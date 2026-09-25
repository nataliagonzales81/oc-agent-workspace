package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

const initCommandName = "init"

// projectTypeAuto is the default for --project-type. It is a flag sentinel, not
// a member of the closed set agent.yaml allows: `auto` is never written to a
// file, it is resolved away before anything is stored.
const projectTypeAuto = "auto"

// initFlags are the ones beyond the global set.
type initFlags struct {
	name        string
	projectType string
	force       bool
}

var initOpts initFlags

// projectMark is one detection rule. Order in markerTable is the priority: a
// repo can hold several markers, and the answer has to be the same every time.
type projectMark struct {
	Marker string
	Type   string
	// Verify is the command `ocaw verify` runs. It is a convention, not a
	// discovery — §5.1 records it so it is computed once, and §7 makes a stale
	// one a doctor warning rather than a silent second-guess.
	Verify string
	// Lint is filled only where the toolchain guarantees it. `go vet` ships with
	// Go; `npm run lint` and `ruff check .` do not ship with anything, so
	// recording them would write a field naming a command the project may not
	// have. An empty field is discoverable, a wrong one is a bug.
	Lint string
	Test string
}

// markerTable is ordered by specificity. A Makefile is last because every C
// project has one, and a Go project with a Makefile is still a Go project.
var markerTable = []projectMark{
	{Marker: "go.mod", Type: yaml.ProjectTypeGo, Verify: "go test ./...", Lint: "go vet ./...", Test: "go test ./..."},
	{Marker: "Cargo.toml", Type: yaml.ProjectTypeRust, Verify: "cargo test", Test: "cargo test"},
	{Marker: "pyproject.toml", Type: yaml.ProjectTypePython, Verify: "pytest", Test: "pytest"},
	{Marker: "package.json", Type: yaml.ProjectTypeNode, Verify: "npm test", Test: "npm test"},
	{Marker: "pom.xml", Type: yaml.ProjectTypeJvm, Verify: "mvn test", Test: "mvn test"},
	{Marker: "build.gradle", Type: yaml.ProjectTypeJvm, Verify: "gradle test", Test: "gradle test"},
	{Marker: "Makefile", Type: yaml.ProjectTypeMake, Verify: "make test", Test: "make test"},
}

var initCommand = &command{
	name:    initCommandName,
	summary: "create the workspace and detect the project type",
	usage:   "ocaw init [--dir <path>] [--name <string>] [--project-type <type>] [--force]",
	setup: func(fs *flag.FlagSet, c *Context) {
		// Reset before binding. flag.StringVar takes the *current* value as the
		// flag's default, so without this a second invocation in the same
		// process inherits the first one's --name and --project-type. The tests
		// run many invocations per process, and a leak there would show up as a
		// test that passes alone and fails in a suite.
		initOpts = initFlags{}
		fs.StringVar(&initOpts.name, "name", initOpts.name, "workspace name recorded in agent.yaml (default: the directory name)")
		fs.StringVar(&initOpts.projectType, "project-type", initOpts.projectType,
			"project type: auto|"+strings.Join(yaml.ProjectTypes, "|"))
		fs.BoolVar(&initOpts.force, "force", initOpts.force,
			"overwrite files ocaw previously wrote; never touches anything else")
	},
	run:   runInit,
	human: humanInit,
}

func runInit(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: initCommandName}

	requested := initOpts.projectType
	if requested == "" {
		requested = projectTypeAuto
	}
	accepted := append([]string{projectTypeAuto}, yaml.ProjectTypes...)
	if !contains(accepted, requested) {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw init --help",
			"--project-type %q is not one of %s", requested, strings.Join(accepted, "|"),
		)
		return res
	}

	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		res.Err = asEnvelopeError(err)
		return res
	}

	detected := detectProject(layout, requested)
	_, inGit := workspace.FindGitRoot(layout.Root)
	if requested == projectTypeAuto && detected.Type == yaml.ProjectTypeUnknown && !inGit {
		res.Err = envelope.Errorf(
			envelope.CodeNotAProject,
			"ocaw init --project-type unknown",
			"%s holds no project marker and is not inside a git repository", layout.Root,
		)
		return res
	}

	// The lock lives inside .agent/state, so the directories have to exist
	// before it can be taken. Two concurrent inits therefore both create the
	// directories — EnsureDirs is idempotent — and one of them loses the lock
	// and reports lock_held instead of interleaving writes.
	//
	// A dry run takes neither the directories nor the lock. Acquire creates the
	// directory it lives in, so locking in a dry run would write to the
	// filesystem the whole flag exists to leave alone.
	var missingDirs []string
	if c.Options.DryRun {
		missingDirs, err = missingWorkspaceDirs(layout)
		if err != nil {
			res.Err = asEnvelopeError(err)
			return res
		}
		data, warns, initErr := initWorkspace(c, layout, detected, missingDirs)
		res.Data = data
		res.Warnings = append(res.Warnings, warns...)
		if initErr != nil {
			res.Err = initErr
		}
		return res
	}
	if err := workspace.EnsureDirs(layout.Dirs()); err != nil {
		res.Err = writeFailure("create the workspace directories", err)
		return res
	}

	lock, warnings, err := workspace.Acquire(layout)
	if err != nil {
		res.Err = asEnvelopeError(err)
		res.Warnings = warnings
		return res
	}
	defer func() {
		if relErr := lock.Release(); relErr != nil {
			// A lock that outlives its command blocks every later command
			// until it goes stale, so a failure to remove it is worth
			// reporting even though the command itself succeeded.
			res.Warnings = append(res.Warnings, Warn(envelope.CodeLockHeld,
				"could not release the workspace lock at %s: %v; it will be broken once it is %s old",
				lock.Path(), relErr, workspace.StaleLockAge))
		}
	}()

	data, warns, initErr := initWorkspace(c, layout, detected, missingDirs)
	res.Data = data
	res.Warnings = append(warnings, warns...)
	if initErr != nil {
		res.Err = initErr
	}
	return res
}

// missingWorkspaceDirs lists the §5 directories that do not exist yet, so
// --dry-run can name what it would create without creating anything.
func missingWorkspaceDirs(l *workspace.Layout) ([]string, error) {
	var missing []string
	for _, dir := range l.Dirs() {
		_, err := os.Stat(dir)
		switch {
		case errors.Is(err, os.ErrNotExist):
			missing = append(missing, dir)
		case err != nil:
			return nil, err
		}
	}
	return missing, nil
}

// initData is the init payload. Every field is something a script can branch on
// without parsing a human rendering of it.
type initData struct {
	Root        string             `json:"root"`
	Initialized bool               `json:"initialized"`
	Created     bool               `json:"created"`
	ProjectType string             `json:"project_type"`
	Detected    string             `json:"detected_from"`
	VerifyCmd   string             `json:"verify_cmd"`
	Force       bool               `json:"force"`
	DryRun      bool               `json:"dry_run"`
	Targets     []workspace.Target `json:"targets"`
	Dirs        []string           `json:"dirs"`
	StateMD     string             `json:"workflow_state_md"`
	AgentYAML   string             `json:"agent_yaml"`
	Notes       []string           `json:"notes,omitempty"`
}

func initWorkspace(c *Context, l *workspace.Layout, detected projectMark, missingDirs []string) (initData, []envelope.Warning, *envelope.Error) {
	data := initData{
		Root:        l.Root,
		ProjectType: detected.Type,
		Detected:    detected.Marker,
		VerifyCmd:   detected.Verify,
		Force:       initOpts.force,
		DryRun:      c.Options.DryRun,
		StateMD:     l.Rel(l.StateMDPath()),
		AgentYAML:   l.Rel(l.AgentYAML()),
		// Never null. The envelope's contract is that a list field is a list,
		// and a script that has to handle both is a script that will eventually
		// handle only one.
		Targets: []workspace.Target{},
		Dirs:    []string{},
	}
	var warnings []envelope.Warning

	name := initOpts.name
	if name == "" {
		name = filepath.Base(l.Root)
	}
	now := time.Now().UTC().Format(time.RFC3339)

	agent, agentExisted, aerr := loadOrNewAgent(l, name, now)
	if aerr != nil {
		return data, warnings, aerr
	}
	data.Initialized = agentExisted
	// §5.1: verify_cmd is written once and thereafter only changed by explicit
	// request. An existing value is never refreshed here, so a project that has
	// moved to just or Bazel keeps its choice.
	if agent.Project.VerifyCmd == "" {
		agent.Project.VerifyCmd = detected.Verify
		agent.Project.VerifyDetected = now
	}
	if agent.Project.TestCmd == "" {
		agent.Project.TestCmd = detected.Test
	}
	if agent.Project.LintCmd == "" {
		agent.Project.LintCmd = detected.Lint
	}
	if agent.Project.Type == "" {
		agent.Project.Type = detected.Type
	}
	if agent.Project.Root != l.Root {
		// The root is derived, so a workspace moved on disk is corrected here
		// rather than left pointing at a path that no longer exists.
		agent.Project.Root = l.Root
	}
	data.VerifyCmd = agent.Project.VerifyCmd
	data.Notes = append(data.Notes, "agent.yaml records a conventional verify command, not a discovered one; ocaw doctor reports it as stale if it stops working")

	// Decide every file before writing any of them, so a refusal half way
	// through does not leave a half-created workspace behind.
	type plannedWrite struct {
		path    string
		content string
		action  workspace.Action
	}
	var writes []plannedWrite
	for _, path := range l.Files() {
		target, err := l.Inspect(path, initOpts.force)
		if err != nil {
			return data, warnings, asEnvelopeError(err)
		}
		data.Targets = append(data.Targets, target)
		if path == l.AgentYAML() {
			// agent.yaml is metadata, not content: §7 updates it on every run
			// and leaves everything else alone, so it is rewritten even when
			// Inspect says keep. Warning that it was "left alone" would be
			// untrue, and it is the one file a caller most needs to trust.
			if target.Action == workspace.ActionRefuse {
				warnings = append(warnings, Warn(envelope.WarnFileExists,
					"left %s alone: %s", target.Rel, target.Reason))
			}
			continue
		}
		content, cerr := templateFor(l, path, agent)
		if cerr != nil {
			return data, warnings, asEnvelopeError(cerr)
		}
		if target.Action == workspace.ActionKeep {
			// Kept is not the same as "nothing to say". A file still holding
			// ocaw's own template is exactly what a re-init should find, and
			// warning about it every time is how an agent learns to ignore
			// warnings. A file that no longer matches the template was written
			// or edited by a human, and that is worth reporting: init kept it,
			// so only the human knows.
			if unchanged, uerr := unchangedFromTemplate(path, content); uerr != nil {
				return data, warnings, asEnvelopeError(uerr)
			} else if unchanged {
				continue
			}
			warnings = append(warnings, Warn(envelope.WarnFileExists,
				"left %s alone: it has content ocaw did not write (pass --force to restore the generated version)",
				target.Rel))
			continue
		}
		if target.Action == workspace.ActionRefuse {
			warnings = append(warnings, Warn(envelope.WarnFileExists,
				"left %s alone: %s", target.Rel, target.Reason))
			continue
		}
		writes = append(writes, plannedWrite{path: path, content: content, action: target.Action})
	}
	for _, rel := range missingDirs {
		data.Dirs = append(data.Dirs, l.Rel(rel))
	}

	if c.Options.DryRun {
		data.Notes = append(data.Notes, "dry run: no directory, file, or lock was created")
		return data, warnings, nil
	}

	if err := workspace.WriteFileAtomic(l.AgentYAML(), []byte(agent.Encode())); err != nil {
		return data, warnings, writeFailure("write agent.yaml", err)
	}
	for _, w := range writes {
		if w.path == l.AgentYAML() {
			continue
		}
		if err := workspace.WriteFileAtomic(w.path, []byte(w.content)); err != nil {
			return data, warnings, writeFailure("write "+l.Rel(w.path), err)
		}
		data.Created = true
	}

	st, serr := ensureState(l)
	if serr != nil {
		return data, warnings, serr
	}
	if doc, found, err := report.Load(l); err == nil && found {
		// state.json is the source of truth, so the generated view is
		// refreshed. A file someone edited by hand is still replaced, but not
		// silently: the warning says what happened before it did.
		if status, _ := report.Check(doc, st); status == report.StatusEdited || status == report.StatusForeign {
			warnings = append(warnings, Warn(envelope.CodeRenderDrift,
				"regenerated %s from state.json; it had been edited by hand", data.StateMD))
		}
	} else if err != nil {
		return data, warnings, asEnvelopeError(err)
	}
	if _, err := report.Write(l, st); err != nil {
		return data, warnings, writeFailure("write "+data.StateMD, err)
	}
	return data, warnings, nil
}

// unchangedFromTemplate reports whether a kept file still holds exactly what
// ocaw would have written. It is the only honest way to tell "init already did
// this" from "a human wrote this and init is politely staying out of the way",
// and the difference decides whether the user hears about it.
func unchangedFromTemplate(path, template string) (bool, error) {
	raw, err := workspace.ReadFile(path)
	if err != nil {
		return false, err
	}
	return string(raw) == template, nil
}

// ensureState loads the DAG or creates an empty one. An existing state.json is
// never rewritten from a template — it holds the work. Its absence is checked
// directly rather than through the load error, which reports a missing file as
// a validation failure with an init hint: correct for a read command, wrong here,
// where a missing file is the normal first-run case.
func ensureState(l *workspace.Layout) (*state.State, *envelope.Error) {
	if exists(l.StateJSON()) {
		st, err := state.Load(l)
		if err != nil {
			return nil, asEnvelopeError(err)
		}
		return st, nil
	}
	st := state.New()
	if err := state.Save(l, st); err != nil {
		return nil, writeFailure("write state.json", err)
	}
	return st, nil
}

// loadOrNewAgent reads an existing agent.yaml or starts a fresh one. A workspace
// that is already initialised keeps its Created stamp and its recorded verify
// command, because both are part of what makes a second init a no-op on disk.
func loadOrNewAgent(l *workspace.Layout, name, now string) (yaml.Agent, bool, *envelope.Error) {
	raw, err := workspace.ReadFile(l.AgentYAML())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return yaml.Agent{Ocaw: true, Schema: yaml.AgentSchema, Name: name, Created: now}, false, nil
		}
		return yaml.Agent{}, false, asEnvelopeError(err)
	}
	existing, perr := yaml.ParseAgent(string(raw))
	if perr != nil {
		return yaml.Agent{}, false, yamlErr(perr, l.Rel(l.AgentYAML()))
	}
	if !existing.Ocaw {
		return yaml.Agent{}, false, envelope.Errorf(
			envelope.CodeValidationFailed,
			"ocaw init --dir "+l.Root,
			"%s exists but is not ocaw's: the \"ocaw\" key must be true", l.Rel(l.AgentYAML()),
		)
	}
	return existing, true, nil
}

// templateFor produces the content of a generated file that does not exist yet.
// There is no timestamp in any of them: a template that stamps the clock would
// make two inits differ, which is exactly what AC2 rules out.
func templateFor(l *workspace.Layout, path string, agent yaml.Agent) (string, error) {
	switch path {
	case l.AgentYAML():
		return agent.Encode(), nil
	case l.RulesMD():
		return rulesTemplate(agent), nil
	case l.MemoryMD():
		return memoryTemplate, nil
	case l.KnowledgeMD():
		return knowledgeTemplate, nil
	case l.KnowledgeIndex():
		return (yaml.KnowledgeIndex{Schema: yaml.KnowledgeSchema, Entries: []yaml.KnowledgeEntry{}}).Encode(), nil
	}
	return "", fmt.Errorf("no template for %s", l.Rel(path))
}

// detectProject returns the marker that matched and, when the caller named a
// type explicitly, that type. An explicit choice wins: someone who passed
// --project-type knows something the marker files do not.
func detectProject(l *workspace.Layout, requested string) projectMark {
	if requested != projectTypeAuto {
		// A forced type still picks up the verify command when the marker for
		// that type is actually on disk, so `--project-type go` in a Go repo
		// records `go test ./...` rather than nothing.
		for _, m := range markerTable {
			if m.Type == requested {
				if exists(filepath.Join(l.Root, m.Marker)) {
					return m
				}
				return projectMark{Type: requested}
			}
		}
		return projectMark{Type: requested}
	}
	for _, m := range markerTable {
		if exists(filepath.Join(l.Root, m.Marker)) {
			return m
		}
	}
	return projectMark{Type: yaml.ProjectTypeUnknown}
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func asEnvelopeError(err error) *envelope.Error {
	var envErr *envelope.Error
	if errors.As(err, &envErr) {
		return envErr
	}
	return envelope.Errorf(
		envelope.CodeWriteFailed,
		"",
		"%s", err.Error(),
	)
}

// writeFailure turns a filesystem error into exit 4 with a path, because
// "permission denied" without saying where is not an error an agent can act on.
func writeFailure(what string, err error) *envelope.Error {
	return envelope.Errorf(
		envelope.CodeWriteFailed,
		"check the path exists and is writable",
		"could not %s: %s", what, err.Error(),
	)
}

func yamlErr(err error, rel string) *envelope.Error {
	return envelope.Errorf(
		envelope.CodeValidationFailed,
		"fix the file, or pass --force if it is a file ocaw wrote",
		"%s: %s", rel, err.Error(),
	)
}

func humanInit(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(initData)
	if !ok {
		return fmt.Errorf("init: unexpected data type %T", env.Data)
	}
	verb := "Initialised"
	if data.DryRun {
		verb = "Would initialise"
	} else if data.Initialized {
		verb = "Updated"
	}
	if _, err := fmt.Fprintf(w, "%s %s\n", verb, data.Root); err != nil {
		return err
	}
	if data.ProjectType != "" {
		if _, err := fmt.Fprintf(w, "  project   %s%s\n", data.ProjectType, detectedSuffix(data.Detected)); err != nil {
			return err
		}
	}
	if data.VerifyCmd != "" {
		if _, err := fmt.Fprintf(w, "  verify    %s\n", data.VerifyCmd); err != nil {
			return err
		}
	}
	created, kept, refused := 0, 0, 0
	for _, t := range data.Targets {
		switch t.Action {
		case workspace.ActionCreate, workspace.ActionOverwrite:
			created++
		case workspace.ActionKeep:
			kept++
		case workspace.ActionRefuse:
			refused++
		}
	}
	if _, err := fmt.Fprintf(w, "  files     %d written, %d kept, %d refused\n", created, kept, refused); err != nil {
		return err
	}
	for _, t := range data.Targets {
		if t.Action == workspace.ActionRefuse {
			if _, err := fmt.Fprintf(w, "  refused   %s: %s\n", t.Rel, t.Reason); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(w, "  next      ocaw doctor\n")
	return err
}

func detectedSuffix(marker string) string {
	if marker == "" {
		return ""
	}
	return " (from " + marker + ")"
}
