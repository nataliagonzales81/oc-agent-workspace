package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// newLayout returns a layout rooted at a fresh temp directory.
func newLayout(t *testing.T) *Layout {
	t.Helper()
	return &Layout{Root: t.TempDir()}
}

// managedLayout returns a layout whose agent.yaml carries the ocaw: true
// marker, which is what makes a workspace overwritable under --force.
func managedLayout(t *testing.T) *Layout {
	t.Helper()
	l := newLayout(t)
	writeManifest(t, l, true)
	return l
}

func writeManifest(t *testing.T, l *Layout, ocaw bool) {
	t.Helper()
	if err := EnsureDir(l.AgentDir()); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	body := "ocaw: " + boolText(ocaw) + "\nschema: ocaw/agent@1\nname: fixture\ncreated: 2026-09-25T00:00:00Z\nproject:\n  type: go\n"
	if err := os.WriteFile(l.AgentYAML(), []byte(body), FileMode); err != nil {
		t.Fatalf("write agent.yaml: %v", err)
	}
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), FileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func codeOf(t *testing.T, err error) envelope.Code {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var envErr *envelope.Error
	if !asEnvelopeError(err, &envErr) {
		t.Fatalf("error %v is not an *envelope.Error", err)
	}
	return envErr.Code
}

func asEnvelopeError(err error, dst **envelope.Error) bool {
	e, ok := err.(*envelope.Error)
	if ok {
		*dst = e
	}
	return ok
}

// TestLayoutPathsMatchSpec pins every §5 path component. A rename here would
// silently orphan an existing workspace, so the names are asserted literally
// rather than derived.
func TestLayoutPathsMatchSpec(t *testing.T) {
	l := &Layout{Root: "/repo"}
	cases := map[string]string{
		l.AgentDir():        "/repo/.agent",
		l.AgentYAML():       "/repo/.agent/agent.yaml",
		l.RulesMD():         "/repo/.agent/RULES.md",
		l.MemoryMD():        "/repo/.agent/MEMORY.md",
		l.KnowledgeMD():     "/repo/.agent/KNOWLEDGE.md",
		l.KnowledgeIndex():  "/repo/.agent/knowledge/index.yaml",
		l.SkillsDir():       "/repo/.agent/skills",
		l.SkillDir("x"):     "/repo/.agent/skills/x",
		l.SkillFile("x"):    "/repo/.agent/skills/x/SKILL.md",
		l.HooksDir():        "/repo/.agent/hooks",
		l.StateDir():        "/repo/.agent/state",
		l.StateJSON():       "/repo/.agent/state/state.json",
		l.RunsJSONL():       "/repo/.agent/state/runs.jsonl",
		l.LockPath():        "/repo/.agent/state/lock",
		l.StateMDPath():     "/repo/WORKFLOW_STATE.md",
		l.WorkflowRoot():    "/repo/.workflow",
		l.WorkflowSlug("s"): "/repo/.workflow/s",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

func TestDirsCoverTheSpecTree(t *testing.T) {
	l := &Layout{Root: "/repo"}
	got := l.Dirs()
	want := []string{
		"/repo/.agent",
		"/repo/.agent/knowledge",
		"/repo/.agent/skills",
		"/repo/.agent/hooks",
		"/repo/.agent/state",
		"/repo/.workflow",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Dirs() = %v, want %v", got, want)
	}
}

func TestFilesAreTheGeneratedSet(t *testing.T) {
	l := &Layout{Root: "/repo"}
	files := l.Files()
	if len(files) != len(generatedFiles) {
		t.Fatalf("Files() = %d entries, want %d", len(files), len(generatedFiles))
	}
	for i, rel := range generatedFiles {
		if want := filepath.Join("/repo", rel); files[i] != want {
			t.Errorf("Files()[%d] = %q, want %q", i, files[i], want)
		}
	}
}

// Generated is the whole permission set for --force, so the exclusions matter
// as much as the inclusions: a skill or a workflow packet is authored work.
func TestGeneratedCoversOnlyInitFiles(t *testing.T) {
	generated := []string{
		filepath.Join(AgentDir, "agent.yaml"),
		filepath.Join(AgentDir, "RULES.md"),
		filepath.Join(AgentDir, "MEMORY.md"),
		filepath.Join(AgentDir, "KNOWLEDGE.md"),
		filepath.Join(AgentDir, "knowledge", "index.yaml"),
	}
	for _, rel := range generated {
		if !Generated(rel) {
			t.Errorf("Generated(%q) = false, want true", rel)
		}
	}
	notGenerated := []string{
		StateMD,
		filepath.Join(AgentDir, "state", "state.json"),
		filepath.Join(AgentDir, "state", "runs.jsonl"),
		filepath.Join(AgentDir, "state", "lock"),
		filepath.Join(AgentDir, "skills", "s", "SKILL.md"),
		filepath.Join(WorkflowDir, "slug", "plan.md"),
		"README.md",
	}
	for _, rel := range notGenerated {
		if Generated(rel) {
			t.Errorf("Generated(%q) = true, want false", rel)
		}
	}
}

func TestResolveExplicitDir(t *testing.T) {
	dir := t.TempDir()
	l, err := Resolve(dir)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if l.Root != dir {
		t.Errorf("Root = %q, want %q", l.Root, dir)
	}
}

// The spec's default is "cwd, or the nearest git root", so a directory inside
// a repository resolves to the repository root.
func TestResolveDefaultsToNearestGitRoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	nested := filepath.Join(root, "cmd", "ocaw")
	if err := os.MkdirAll(nested, DirMode); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	l, err := resolveStart(nested, false)
	if err != nil {
		t.Fatalf("resolveStart: %v", err)
	}
	if l.Root != root {
		t.Errorf("Root = %q, want the git root %q", l.Root, root)
	}
}

// An explicit --dir is taken literally: an agent that names a directory means
// that directory, and silently walking up to a repository root would write
// somewhere the caller did not ask for.
func TestResolveKeepsAnExplicitSubdirectory(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "HEAD"), "ref: refs/heads/main\n")
	nested := filepath.Join(root, "services", "api")
	if err := os.MkdirAll(nested, DirMode); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	l, err := Resolve(nested)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if l.Root != nested {
		t.Errorf("Root = %q, want the explicit dir %q", l.Root, nested)
	}
}

func TestFindGitRootTreatsAFileAsARoot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git"), "gitdir: /elsewhere/.git/worktrees/wt\n")
	got, ok := FindGitRoot(filepath.Join(root, "deep", "deeper"))
	if !ok {
		t.Fatal("FindGitRoot found no root for a worktree .git file")
	}
	if got != root {
		t.Errorf("FindGitRoot = %q, want %q", got, root)
	}
}

func TestFindGitRootStopsAtFilesystemRoot(t *testing.T) {
	if _, ok := FindGitRoot(t.TempDir()); ok {
		t.Error("FindGitRoot claimed a root inside a temp dir with no .git")
	}
}

func TestResolveRejectsBadDirs(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "f.txt")
	writeFile(t, file, "x")

	if _, err := Resolve(filepath.Join(root, "missing")); codeOf(t, err) != envelope.CodeInvalidPath {
		t.Errorf("missing dir: code = %s, want %s", codeOf(t, err), envelope.CodeInvalidPath)
	}
	if _, err := Resolve(file); codeOf(t, err) != envelope.CodeInvalidPath {
		t.Errorf("a file: code = %s, want %s", codeOf(t, err), envelope.CodeInvalidPath)
	}
}

func TestRelFallsBackToAbsoluteOutsideRoot(t *testing.T) {
	l := &Layout{Root: "/repo"}
	if got := l.Rel("/repo/.agent/agent.yaml"); got != filepath.Join(AgentDir, "agent.yaml") {
		t.Errorf("Rel inside root = %q", got)
	}
	if got := l.Rel("/elsewhere/x"); got != "/elsewhere/x" {
		t.Errorf("Rel outside root = %q, want the absolute path", got)
	}
}

func TestRequireReportsMissingWorkspace(t *testing.T) {
	l := newLayout(t)
	if l.Initialized() {
		t.Error("Initialized() = true for an empty dir")
	}
	if got := codeOf(t, l.Require()); got != envelope.CodeWorkspaceMissing {
		t.Errorf("code = %s, want %s", got, envelope.CodeWorkspaceMissing)
	}
	if hint := l.Require().Hint; !strings.Contains(hint, "ocaw init") {
		t.Errorf("hint = %q, want it to name ocaw init", hint)
	}
}

func TestRequirePassesOnAManagedWorkspace(t *testing.T) {
	l := managedLayout(t)
	if !l.Initialized() || !l.Managed() {
		t.Error("a workspace with a valid agent.yaml should be initialized and managed")
	}
	if err := l.Require(); err != nil {
		t.Errorf("Require() = %v, want nil", err)
	}
}

func TestRequireRejectsAForeignManifest(t *testing.T) {
	l := newLayout(t)
	writeManifest(t, l, false)
	if l.Initialized() {
		t.Error("Initialized() = true for an agent.yaml with ocaw: false")
	}
}

func TestIsEmptyDir(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, DirMode); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	ok, err := IsEmptyDir(empty)
	if err != nil || !ok {
		t.Errorf("IsEmptyDir(empty) = %v, %v; want true, nil", ok, err)
	}

	gone := filepath.Join(root, "gone")
	ok, err = IsEmptyDir(gone)
	if err != nil || !ok {
		t.Errorf("IsEmptyDir(missing) = %v, %v; want true, nil", ok, err)
	}

	writeFile(t, filepath.Join(empty, "a"), "x")
	ok, err = IsEmptyDir(empty)
	if err != nil || ok {
		t.Errorf("IsEmptyDir(non-empty) = %v, %v; want false, nil", ok, err)
	}
}
