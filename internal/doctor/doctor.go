// Package doctor runs the workspace health check and reports every finding it
// finds (SPEC.md §7).
//
// Two rules shape everything here. First, a health check reports; it does not
// stop at the first problem — an agent that has to re-run the tool after each
// fix will not finish fixing. Second, the only writes it will ever perform are
// the reversible ones, and each one is named in the report, because a tool that
// changes a workspace silently is a tool nobody can trust in a loop.
package doctor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/verify"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

// Severity is the level a finding is reported at. Exit 0 and 4 both come from
// this: an error fails, a warning informs, and --strict promotes the warning to
// an error so a CI gate can insist.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
	SeverityInfo    Severity = "info"
)

// Severities in report order, so counts render the same way every time.
var Severities = []Severity{SeverityError, SeverityWarning, SeverityInfo}

// Check names one of the seven passes. It is the stable identifier: a caller
// pins behaviour to a check, not to a message, which is free to improve.
type Check string

const (
	CheckLayout    Check = "layout"
	CheckAgentYAML Check = "agent_yaml"
	CheckState     Check = "state"
	CheckRender    Check = "render"
	CheckSkills    Check = "skills"
	CheckKnowledge Check = "knowledge"
	CheckVerifyCmd Check = "verify_cmd"
)

// Checks is every check in the order §7 runs them, so two runs of a healthy
// workspace produce findings in the same order.
var Checks = []Check{
	CheckLayout, CheckAgentYAML, CheckState, CheckRender,
	CheckSkills, CheckKnowledge, CheckVerifyCmd,
}

// Finding is one problem. Code is always a member of the closed set (SPEC §8):
// an error carries an error code and a warning carries a warning code, so the
// two never contradict each other and a caller can branch on either. Check and
// Message carry the specificity that the closed set cannot.
type Finding struct {
	Severity Severity      `json:"severity"`
	Code     envelope.Code `json:"code"`
	Check    Check         `json:"check"`
	Message  string        `json:"message"`
	Location string        `json:"location"`
	Hint     string        `json:"hint,omitempty"`
}

// Report is the whole pass. Findings is never nil, so a JSON consumer never has
// to distinguish "no findings" from "the field was not populated".
type Report struct {
	Findings []Finding `json:"findings"`
	// Fixed names what --fix-safe changed, so a fix is as visible as a finding.
	Fixed []string `json:"fixed,omitempty"`
	// VerifyCmd is the recorded command, carried out because the most common
	// thing an agent needs from doctor is "what is it trying to run".
	VerifyCmd string `json:"verify_cmd"`
}

// Add appends a finding. Kept as a method so a check cannot forget to
// initialise the slice.
func (r *Report) Add(f Finding) {
	r.Findings = append(r.Findings, f)
}

func (r *Report) errorf(check Check, code envelope.Code, location, hint, format string, args ...any) {
	r.Add(Finding{
		Severity: SeverityError,
		Code:     code,
		Check:    check,
		Message:  fmt.Sprintf(format, args...),
		Location: location,
		Hint:     hint,
	})
}

func (r *Report) warn(check Check, code envelope.Code, location, hint, format string, args ...any) {
	r.Add(Finding{
		Severity: SeverityWarning,
		Code:     code,
		Check:    check,
		Message:  fmt.Sprintf(format, args...),
		Location: location,
		Hint:     hint,
	})
}

// Counts tallies findings by severity. Every key is always present, including
// the zeros: a caller branching on counts must not have to test for absence.
func (r *Report) Counts() map[string]int {
	out := make(map[string]int, len(Severities))
	for _, s := range Severities {
		out[string(s)] = 0
	}
	for _, f := range r.Findings {
		out[string(f.Severity)]++
	}
	return out
}

// HasErrors reports whether anything failed. With strict set, a warning counts.
// Every exit-code decision goes through here, so the human renderer cannot
// disagree with the process status about whether the run passed.
func (r *Report) HasErrors(strict bool) bool {
	return r.FailingCount(strict) > 0
}

// FailingCount is the number of findings that count as failures under the given
// strictness. --strict promotes warnings to errors, so a CI gate can insist on a
// clean workspace while a human running doctor locally still hears about a stale
// verify command without being blocked by it.
func (r *Report) FailingCount(strict bool) int {
	n := 0
	for _, f := range r.Findings {
		if f.Severity == SeverityError || (strict && f.Severity == SeverityWarning) {
			n++
		}
	}
	return n
}

// Of returns the findings from one check, for a caller that wants to react to a
// specific pass.
func (r *Report) Of(check Check) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

// Options configures a pass.
type Options struct {
	// Strict promotes warnings to errors for the exit code.
	Strict bool
	// FixSafe applies the reversible fixes. It is deliberately not a repair-everything
	// switch: task data, skills, and agent.yaml are never written here.
	FixSafe bool
	// DryRun reports the fixes it would apply and applies none.
	DryRun bool
}

// Run executes every check in §7's order and returns the whole picture.
//
// It takes no lock. A read-only health check that can fail with lock_held is a
// health check that reports the wrong problem, and --fix-safe acquires the lock
// itself only when it has something to write.
func Run(l *workspace.Layout, opts Options) *Report {
	rep := &Report{Findings: []Finding{}}

	checkLayout(l, rep)
	agent, agentOK := checkAgentYAML(l, rep)
	rep.VerifyCmd = agent.Project.VerifyCmd
	st := checkState(l, rep)
	checkRender(l, rep, st)
	checkSkills(l, rep)
	index := checkKnowledge(l, rep)
	if agentOK {
		checkVerifyCmd(l, rep, agent)
	}

	if opts.FixSafe {
		applyFixes(l, rep, st, index, opts.DryRun)
	}
	return rep
}

// ---- check 1: layout presence ---------------------------------------------

// checkLayout reports every missing directory and generated file rather than
// stopping at the first, because the usual cause is someone moving or deleting
// a directory, and that takes several paths with it.
func checkLayout(l *workspace.Layout, rep *Report) {
	if !l.Initialized() {
		rep.errorf(CheckLayout, envelope.CodeWorkspaceMissing, l.Rel(l.AgentDir()),
			"ocaw init --dir "+l.Root,
			"this directory has no .agent/ workspace")
	}
	for _, dir := range l.Dirs() {
		_, err := os.Stat(dir)
		switch {
		case os.IsNotExist(err):
			rep.errorf(CheckLayout, envelope.CodeWorkspaceMissing, l.Rel(dir),
				"ocaw init --dir "+l.Root, "the directory does not exist")
		case err != nil:
			rep.errorf(CheckLayout, envelope.CodeInvalidPath, l.Rel(dir),
				"", "cannot read the directory: %v", err)
		}
	}
	for _, file := range l.Files() {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			rel := l.Rel(file)
			rep.errorf(CheckLayout, envelope.CodeWorkspaceMissing, rel,
				"ocaw init --dir "+l.Root, "the generated file does not exist")
		}
	}
}

// ---- check 2: agent.yaml ---------------------------------------------------

func checkAgentYAML(l *workspace.Layout, rep *Report) (yaml.Agent, bool) {
	rel := l.Rel(l.AgentYAML())
	raw, err := workspace.ReadFile(l.AgentYAML())
	if err != nil {
		if !os.IsNotExist(err) {
			rep.errorf(CheckAgentYAML, envelope.CodeValidationFailed, rel,
				"ocaw init --dir "+l.Root, "cannot read the file: %v", err)
		}
		return yaml.Agent{}, false
	}
	agent, perr := yaml.ParseAgent(string(raw))
	if perr != nil {
		rep.errorf(CheckAgentYAML, envelope.CodeValidationFailed, rel,
			"fix the file, or pass --force to ocaw init to restore the generated version",
			"agent.yaml does not match the ocaw/agent@1 shape: %v", perr)
		return yaml.Agent{}, false
	}
	if !agent.Ocaw {
		rep.errorf(CheckAgentYAML, envelope.CodeValidationFailed, rel,
			"set the \"ocaw\" key to true", "the ocaw marker is false, so this is not an ocaw workspace")
	}
	if agent.Schema != yaml.AgentSchema {
		rep.errorf(CheckAgentYAML, envelope.CodeValidationFailed, rel,
			"upgrade ocaw", "schema is %q, want %q", agent.Schema, yaml.AgentSchema)
	}
	if agent.Name == "" {
		rep.warn(CheckAgentYAML, envelope.WarnVerifyCmdStale, rel,
			"ocaw init --name <name>", "the workspace has no name")
	}
	return agent, true
}

// ---- check 3: state schema and the §6.1 invariants -------------------------

// checkState loads the DAG and reports every invariant violation it can, not
// just the one Load refused to return past. Load stops at the first because it
// has to return a usable state; doctor does not, so it asks for all of them.
func checkState(l *workspace.Layout, rep *Report) *state.State {
	rel := l.Rel(l.StateJSON())
	raw, err := workspace.ReadFile(l.StateJSON())
	if err != nil {
		if os.IsNotExist(err) {
			rep.errorf(CheckState, envelope.CodeWorkspaceMissing, rel,
				"ocaw init --dir "+l.Root, "state.json does not exist")
		} else {
			rep.errorf(CheckState, envelope.CodeValidationFailed, rel,
				"", "cannot read state.json: %v", err)
		}
		return nil
	}
	st, lerr := state.Decode([]byte(raw))
	if lerr != nil {
		rep.errorf(CheckState, lerr.Envelope().Code, rel, lerr.Envelope().Hint,
			"cannot read state.json: %s", lerr.Envelope().Message)
		return nil
	}
	for _, v := range st.Violations() {
		rep.Add(Finding{
			Severity: SeverityError,
			Code:     v.Envelope().Code,
			Check:    CheckState,
			Message:  v.Envelope().Message,
			Location: rel,
			Hint:     hintForViolation(v, l),
		})
	}
	return st
}

// hintForViolation turns an invariant name into the command that fixes it. The
// check name is the stable thing to branch on, so the hint is derived from it
// rather than written per call site where the two could drift apart.
func hintForViolation(v *state.Error, l *workspace.Layout) string {
	switch v.Detail.Check {
	case state.CheckAcyclic:
		return "ocaw task dep --rm, to break the cycle"
	case state.CheckDangling:
		return "ocaw task dep --rm, to drop a dependency on a task that no longer exists"
	case state.CheckInFlight:
		return "finish or cancel the listed dependencies, then set the task to in_progress"
	case state.CheckDependents:
		return "ocaw task set " + v.Detail.Task + " --status in_progress, or finish the dependent"
	case state.CheckGates:
		return "the gate command must be an argv with no shell operators; edit state.json or use ocaw verify"
	case state.CheckUniqueIDs, state.CheckAcceptance:
		return "ocaw task rm, or edit state.json to give each id a unique name"
	case state.CheckStatuses:
		names := make([]string, 0, len(state.Statuses))
		for _, s := range state.Statuses {
			names = append(names, string(s))
		}
		return "set a status from " + strings.Join(names, "|")
	case state.CheckSchema:
		return "upgrade ocaw, or migrate the state by hand"
	}
	return "ocaw --dir " + l.Root + " doctor"
}

// ---- check 4: the generated view -------------------------------------------

func checkRender(l *workspace.Layout, rep *Report, st *state.State) {
	doc, found, err := report.Load(l)
	if err != nil {
		rep.errorf(CheckRender, envelope.CodeValidationFailed, l.Rel(l.StateMDPath()),
			"", "cannot read WORKFLOW_STATE.md: %v", err)
		return
	}
	if !found {
		rep.errorf(CheckRender, envelope.CodeRenderDrift, l.Rel(l.StateMDPath()),
			"ocaw report --write", "WORKFLOW_STATE.md does not exist")
		return
	}
	if st == nil {
		// The state did not load, so there is nothing to render a fresh view
		// from and calling Check would report a second, misleading problem.
		return
	}
	status, derr := report.Check(doc, st)
	if derr == nil {
		return
	}
	rep.errorf(CheckRender, derr.Code, l.Rel(l.StateMDPath()), derr.Hint,
		"%s (%s)", derr.Message, status)
}

// ---- check 5: skill frontmatter -------------------------------------------

// checkSkills validates every skill directory. A skill whose name does not match
// its directory is an error rather than a warning: the directory name is the
// address an agent uses to load it, so a mismatch makes the skill unreachable
// by the name it declares.
func checkSkills(l *workspace.Layout, rep *Report) {
	entries, err := os.ReadDir(l.SkillsDir())
	if err != nil {
		if os.IsNotExist(err) {
			rep.errorf(CheckSkills, envelope.CodeWorkspaceMissing, l.Rel(l.SkillsDir()),
				"ocaw init --dir "+l.Root, "the skills directory does not exist")
			return
		}
		rep.errorf(CheckSkills, envelope.CodeInvalidPath, l.Rel(l.SkillsDir()),
			"", "cannot read the skills directory: %v", err)
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		path := l.SkillFile(name)
		rel := l.Rel(path)
		raw, rerr := workspace.ReadFile(path)
		if rerr != nil {
			rep.errorf(CheckSkills, envelope.CodeValidationFailed, rel,
				"create "+rel, "cannot read the skill: %v", rerr)
			continue
		}
		fm, perr := yaml.ParseFrontmatter(string(raw))
		if perr != nil {
			rep.errorf(CheckSkills, envelope.CodeValidationFailed, rel,
				"the frontmatter must use only the §5.3 subset: name, version, description, allowed-tools, hidden",
				"frontmatter does not parse: %v", perr)
			continue
		}
		if fm.Name != name {
			rep.errorf(CheckSkills, envelope.CodeValidationFailed, rel,
				"set name: "+name, "frontmatter name is %q but the directory is %q", fm.Name, name)
		}
		if strings.TrimSpace(fm.Description) == "" {
			// Description matching is how an agent decides to load a skill, so a
			// skill without one is a skill nothing will ever load.
			rep.errorf(CheckSkills, envelope.CodeValidationFailed, rel,
				"add a description naming the triggers", "the frontmatter has no description")
		}
	}
}

// ---- check 6: knowledge anchors --------------------------------------------

// checkKnowledge validates the index. A malformed entry is an error from the
// parser, which already enforces the two §5.2 rules; an anchor that no longer
// points at a file is a warning, because the entry may be describing something
// that moved rather than something that was wrong.
func checkKnowledge(l *workspace.Layout, rep *Report) yaml.KnowledgeIndex {
	rel := l.Rel(l.KnowledgeIndex())
	raw, err := workspace.ReadFile(l.KnowledgeIndex())
	if err != nil {
		if os.IsNotExist(err) {
			rep.errorf(CheckKnowledge, envelope.CodeWorkspaceMissing, rel,
				"ocaw init --dir "+l.Root, "knowledge/index.yaml does not exist")
		} else {
			rep.errorf(CheckKnowledge, envelope.CodeValidationFailed, rel, "", "cannot read the index: %v", err)
		}
		return yaml.KnowledgeIndex{}
	}
	index, perr := yaml.ParseKnowledgeIndex(string(raw))
	if perr != nil {
		rep.errorf(CheckKnowledge, envelope.CodeValidationFailed, rel,
			"fix the index; anchors must be repo-relative and outside .agent/",
			"knowledge/index.yaml does not match the ocaw/knowledge@1 shape: %v", perr)
		return yaml.KnowledgeIndex{}
	}
	for _, e := range index.Entries {
		if !anchorResolves(l, e.Anchor) {
			rep.warn(CheckKnowledge, envelope.WarnKnowledgeAnchorGone, rel,
				"point the anchor at the file that proves the entry, or delete the entry",
				"entry %q (%s) anchors at %q, which does not exist", e.ID, e.Title, e.Anchor)
		}
	}
	return index
}

// anchorResolves reports whether an anchor names a real file inside the project.
//
// The containment check is not redundant with the parser's rule. The parser
// refuses an absolute path and a path into .agent/, but "../.." is neither, and
// a walk that followed one would be reading outside the project it was asked
// about.
func anchorResolves(l *workspace.Layout, anchor string) bool {
	if strings.TrimSpace(anchor) == "" {
		return false
	}
	full := filepath.Join(l.Root, filepath.FromSlash(anchor))
	if _, inside := relInside(l.Root, full); !inside {
		return false
	}
	info, err := os.Stat(full)
	return err == nil && !info.IsDir()
}

// ---- check 7: the recorded verify command ----------------------------------

// checkVerifyCmd reports a command that would not run, without running it.
//
// Nothing is executed: doctor has to be cheap and safe to call in a loop, and a
// health check that runs the project's test suite is a health check nobody calls
// in a loop. LookPath is a PATH lookup, not a subprocess.
//
// A command whose program is missing is a warning, not an error, because §5.1
// says so explicitly: the project may have moved to Bazel or just, and the human
// knows, and ocaw must not second-guess them into re-running detection.
func checkVerifyCmd(l *workspace.Layout, rep *Report, agent yaml.Agent) {
	rel := l.Rel(l.AgentYAML())
	if !existsFile(l.AgentYAML()) {
		return
	}
	cmd := strings.TrimSpace(agent.Project.VerifyCmd)
	if cmd == "" {
		rep.warn(CheckVerifyCmd, envelope.WarnVerifyCmdStale, rel,
			"ocaw verify detect, or set project.verify_cmd in "+rel,
			"no verify command is recorded, so ocaw verify has nothing to run")
		return
	}
	argv, terr := verify.Tokenize(cmd)
	if terr != nil {
		rep.errorf(CheckVerifyCmd, envelope.CodeValidationFailed, rel,
			"the command must be an argv with no shell operators",
			"verify_cmd %q does not parse: %v", cmd, terr)
		return
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		rep.warn(CheckVerifyCmd, envelope.WarnVerifyCmdStale, rel,
			"ocaw verify detect, or set project.verify_cmd by hand if the project moved to another runner",
			"verify_cmd runs %q, which is not on PATH; ocaw will not re-detect it for you", argv[0])
	}
}

func existsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
