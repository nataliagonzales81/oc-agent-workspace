// Package workspace resolves the SPEC.md §5 layout, performs every write
// atomically, and owns the single-writer lock.
//
// It is the only package that knows where a file lives, so no command has to
// spell out a path. It also holds the two filesystem guards the rest of ocaw
// relies on: atomic writes (§4.4) and bounded reads and walks (§9.3, §9.6).
// `init` and `--force` semantics live here too, because deciding whether ocaw
// may overwrite a file is a question about the layout, not about a command.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// §5 path components. These are the whole contract: a path that is not built
// from one of these names is a bug, not a convention.
const (
	// AgentDir is the workspace root under the project.
	AgentDir = ".agent"
	// WorkflowDir holds per-run workflow artifacts (§5).
	WorkflowDir = ".workflow"
	// StateMD is the generated human view of the DAG (§6.2).
	StateMD = "WORKFLOW_STATE.md"

	agentYAML   = "agent.yaml"
	rulesMD     = "RULES.md"
	memoryMD    = "MEMORY.md"
	knowledgeMD = "KNOWLEDGE.md"
	indexYAML   = "index.yaml"
	skillsDir   = "skills"
	hooksDir    = "hooks"
	stateDir    = "state"
	stateJSON   = "state.json"
	runsJSONL   = "runs.jsonl"
	lockFile    = "lock"
)

// generatedFiles are the paths `ocaw init` creates and therefore the only ones
// `--force` may overwrite (§5). Everything else in the tree is authored by a
// human or by another command: a skill's SKILL.md, a .workflow packet, and the
// state files are never regenerated from a template, so overwriting them would
// destroy work rather than refresh it.
var generatedFiles = []string{
	filepath.Join(AgentDir, agentYAML),
	filepath.Join(AgentDir, rulesMD),
	filepath.Join(AgentDir, memoryMD),
	filepath.Join(AgentDir, knowledgeMD),
	filepath.Join(AgentDir, "knowledge", indexYAML),
}

// Layout resolves §5 paths against a single project root.
type Layout struct {
	// Root is the absolute project directory. Every other path is derived.
	Root string
}

// Resolve determines the workspace root for a run.
//
// An explicit dir wins. Otherwise the spec's default applies: the current
// directory, or the nearest git root when the current directory sits inside a
// repository. Walking up is deliberate — an agent that runs `ocaw` in a
// subdirectory of a repository means the repository. A caller that needs the
// strict reading can pass FindGitRoot's result or the current directory itself.
func Resolve(dir string) (*Layout, error) {
	if dir != "" {
		return resolveStart(dir, true)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, envelope.Errorf(envelope.CodeInvalidPath, "pass --dir", "cannot determine the current directory: %v", err)
	}
	return resolveStart(cwd, false)
}

// resolveStart validates start and applies the defaulting rule, so the default
// path is testable without changing the process working directory. An explicit
// dir is used as given; only a defaulted one is widened to the git root.
func resolveStart(start string, explicit bool) (*Layout, error) {
	abs, err := filepath.Abs(start)
	if err != nil {
		return nil, envelope.Errorf(envelope.CodeInvalidPath, "pass an absolute --dir", "cannot resolve %q: %v", start, err)
	}
	abs = filepath.Clean(abs)

	info, err := os.Stat(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, envelope.Errorf(envelope.CodeInvalidPath, "ocaw init --dir <new-dir>", "directory %q does not exist", abs)
		}
		return nil, envelope.Errorf(envelope.CodeInvalidPath, "ocaw init --dir <other-dir>", "cannot read %q: %v", abs, err)
	}
	if !info.IsDir() {
		return nil, envelope.Errorf(envelope.CodeInvalidPath, "ocaw init --dir <dir>", "%q is not a directory", abs)
	}

	if !explicit {
		if root, ok := FindGitRoot(abs); ok {
			abs = root
		}
	}
	return &Layout{Root: abs}, nil
}

// FindGitRoot walks up from start looking for a .git entry. A worktree or
// submodule has .git as a file, so presence is enough; content is never read.
// The walk is bounded (maxGitRootHops) so a pathological path cannot loop.
func FindGitRoot(start string) (string, bool) {
	const maxGitRootHops = 64
	dir := filepath.Clean(start)
	for hop := 0; hop < maxGitRootHops; hop++ {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	return "", false
}

func (l *Layout) String() string { return l.Root }

// AgentDir returns the .agent directory (§5).
func (l *Layout) AgentDir() string { return filepath.Join(l.Root, AgentDir) }

// AgentYAML returns the workspace manifest (§5.1).
func (l *Layout) AgentYAML() string { return filepath.Join(l.AgentDir(), agentYAML) }

// RulesMD returns the agent behaviour rules document.
func (l *Layout) RulesMD() string { return filepath.Join(l.AgentDir(), rulesMD) }

// MemoryMD returns the session-local notes document.
func (l *Layout) MemoryMD() string { return filepath.Join(l.AgentDir(), memoryMD) }

// KnowledgeMD returns the project-level knowledge prose document.
func (l *Layout) KnowledgeMD() string { return filepath.Join(l.AgentDir(), knowledgeMD) }

// KnowledgeDir returns the structured knowledge directory.
func (l *Layout) KnowledgeDir() string { return filepath.Join(l.AgentDir(), "knowledge") }

// KnowledgeIndex returns the structured knowledge index (§5.2).
func (l *Layout) KnowledgeIndex() string { return filepath.Join(l.KnowledgeDir(), indexYAML) }

// SkillsDir returns the root of the per-skill directories (§5.3).
func (l *Layout) SkillsDir() string { return filepath.Join(l.AgentDir(), skillsDir) }

// SkillDir returns the directory of one named skill.
func (l *Layout) SkillDir(name string) string { return filepath.Join(l.SkillsDir(), name) }

// SkillFile returns the SKILL.md path of one named skill.
func (l *Layout) SkillFile(name string) string { return filepath.Join(l.SkillDir(name), "SKILL.md") }

// HooksDir returns the hooks directory.
func (l *Layout) HooksDir() string { return filepath.Join(l.AgentDir(), hooksDir) }

// StateDir returns the state directory holding state.json, runs.jsonl, lock.
func (l *Layout) StateDir() string { return filepath.Join(l.AgentDir(), stateDir) }

// StateJSON returns the machine source of truth (§6.1).
func (l *Layout) StateJSON() string { return filepath.Join(l.StateDir(), stateJSON) }

// RunsJSONL returns the append-only verification log.
func (l *Layout) RunsJSONL() string { return filepath.Join(l.StateDir(), runsJSONL) }

// LockPath returns the single-writer lock file.
func (l *Layout) LockPath() string { return filepath.Join(l.StateDir(), lockFile) }

// StateMD returns the generated human view (§6.2).
func (l *Layout) StateMDPath() string { return filepath.Join(l.Root, StateMD) }

// WorkflowRoot returns the .workflow directory.
func (l *Layout) WorkflowRoot() string { return filepath.Join(l.Root, WorkflowDir) }

// WorkflowSlug returns the per-run directory for one workflow slug.
func (l *Layout) WorkflowSlug(slug string) string { return filepath.Join(l.WorkflowRoot(), slug) }

// Dirs lists every directory `ocaw init` must create, parents before children
// so the list can be passed to EnsureDirs in order.
func (l *Layout) Dirs() []string {
	return []string{
		l.AgentDir(),
		l.KnowledgeDir(),
		l.SkillsDir(),
		l.HooksDir(),
		l.StateDir(),
		l.WorkflowRoot(),
	}
}

// Files lists every file `ocaw init` must create, in creation order.
func (l *Layout) Files() []string {
	out := make([]string, 0, len(generatedFiles))
	for _, rel := range generatedFiles {
		out = append(out, filepath.Join(l.Root, rel))
	}
	return out
}

// Generated reports whether rel names a file `ocaw init` writes. Only these
// are force-overwritable (§5).
func Generated(rel string) bool {
	rel = filepath.Clean(rel)
	for _, g := range generatedFiles {
		if rel == g {
			return true
		}
	}
	return false
}

// Rel returns path relative to the workspace root for display, falling back to
// the absolute path when the target is outside the root.
func (l *Layout) Rel(path string) string {
	rel, err := filepath.Rel(l.Root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// Initialized reports whether this root holds a workspace: the .agent directory
// exists and agent.yaml is a readable ocaw manifest.
func (l *Layout) Initialized() bool {
	_, err := l.OcawManifest()
	return err == nil
}

// Require returns workspace_not_initialized when the root is not a workspace.
// Commands that read or mutate state call it first so the precondition is
// stated in one place.
func (l *Layout) Require() *envelope.Error {
	if l.Initialized() {
		return nil
	}
	return envelope.NewError(
		envelope.CodeWorkspaceMissing,
		fmt.Sprintf("no ocaw workspace at %s", l.Root),
		"ocaw init --dir "+l.Root,
	)
}

// IsEmptyDir reports whether dir has no entries, treating a missing directory
// as empty. `init` refuses to write into a non-empty .agent only when the file
// itself is non-empty, so this is about the directory, not a file.
func IsEmptyDir(dir string) (bool, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, envelope.Errorf(envelope.CodeInvalidPath, "check permissions", "cannot read %q: %v", dir, err)
	}
	return len(entries) == 0, nil
}
