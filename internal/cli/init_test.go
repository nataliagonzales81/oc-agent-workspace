package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

// initCommandName is the subcommand these tests drive. Spelling it out rather
// than exporting it keeps the test honest about what an agent actually types.
const initCommandName = "init"

// run invokes the CLI in-process and returns the exit code, the envelope, and
// both streams. The envelope is decoded so assertions read fields rather than
// substrings, and raw stderr is kept so a test can check that a diagnostic went
// to the right place.
func run(t *testing.T, dir string, args ...string) (int, envelope.Envelope, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]string{"--dir", dir, initCommandName}, args...)
	code := cli.Main(full, &out, &errOut, false)

	raw := bytes.TrimSpace(out.Bytes())
	var env envelope.Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("stdout is not a single-line JSON envelope: %v\n%s", err, raw)
		}
	}
	return code, env, out.String(), errOut.String()
}

func initEnvelope(t *testing.T, dir string, args ...string) (int, envelope.Envelope, string) {
	t.Helper()
	code, env, _, errOut := run(t, dir, args...)
	if env.Command != initCommandName {
		t.Fatalf("command = %q, want %q", env.Command, initCommandName)
	}
	if raw := strings.TrimSpace(errOut); raw != "" && !env.OK {
		t.Logf("stderr on failure: %s", raw)
	}
	return code, env, errOut
}

// gitRepo makes a real repository. AC2 is stated in terms of
// `git status --porcelain`, so asserting it against a directory that is not
// under version control would not be the same test.
func gitRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--quiet", "-m", "fixture"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v failed (%v): %s", args, err, out)
		}
	}
	return dir
}

func porcelain(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "status", "--porcelain")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git status: %v", err)
	}
	return strings.TrimSpace(string(out))
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func warningCodes(env envelope.Envelope) []string {
	var codes []string
	for _, w := range env.Warnings {
		codes = append(codes, string(w.Code))
	}
	return codes
}

func hasWarning(env envelope.Envelope, code envelope.Code) bool {
	for _, w := range env.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// AC1: `ocaw init` on an empty dir with a go.mod produces the full §5 layout,
// sets project.type: go and verify_cmd: go test ./..., and exits 0.
func TestAC1GoRepoLayoutAndDetection(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"})

	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; error = %+v", code, env.Err)
	}
	if !env.OK {
		t.Errorf("ok = false: %+v", env.Err)
	}
	if env.Schema != "ocaw/init@1" {
		t.Errorf("schema = %q, want ocaw/init@1", env.Schema)
	}

	for _, rel := range []string{
		".agent",
		".agent/agent.yaml",
		".agent/RULES.md",
		".agent/MEMORY.md",
		".agent/KNOWLEDGE.md",
		".agent/knowledge/index.yaml",
		".agent/skills",
		".agent/hooks",
		".agent/state",
		".agent/state/state.json",
		".workflow",
		"WORKFLOW_STATE.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("init did not create %s: %v", rel, err)
		}
	}

	agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
	if err != nil {
		t.Fatalf("the agent.yaml init wrote does not parse: %v", err)
	}
	if agent.Project.Type != "go" {
		t.Errorf("project.type = %q, want go", agent.Project.Type)
	}
	if agent.Project.VerifyCmd != "go test ./..." {
		t.Errorf("verify_cmd = %q, want %q", agent.Project.VerifyCmd, "go test ./...")
	}
	if agent.Project.LintCmd != "go vet ./..." {
		t.Errorf("lint_cmd = %q, want %q", agent.Project.LintCmd, "go vet ./...")
	}
	if !agent.Ocaw {
		t.Error("ocaw marker is false, so --force could never overwrite this file")
	}
	if agent.Project.Root != dir {
		t.Errorf("project.root = %q, want %q", agent.Project.Root, dir)
	}
}

// The §9.5 guarantee, stated as git sees it: a second init changes nothing.
func TestAC2InitTwiceLeavesTheTreeClean(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"})

	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("first init: exit %d, %+v", code, env.Err)
	}
	// Commit what init produced, so any change the second run makes is visible
	// rather than hidden among the new files.
	commitAll(t, dir)

	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("second init: exit = %d, want 0; error = %+v", code, env.Err)
	}
	if !env.OK {
		t.Errorf("ok = false on an already-valid workspace: %+v", env.Err)
	}
	if dirty := porcelain(t, dir); dirty != "" {
		t.Errorf("the second init changed files:\n%s", dirty)
	}
	// A second init should also say so, rather than claiming it created things.
	data := env.Data.(map[string]any)
	if initialized, _ := data["initialized"].(bool); !initialized {
		t.Error("data.initialized = false on a re-init, want true")
	}
	if created, _ := data["created"].(bool); created {
		t.Error("data.created = true on a re-init, want false")
	}
}

// AC3: a hand-written RULES.md is left byte-identical and warns.
func TestAC3HandWrittenRulesIsLeftAlone(t *testing.T) {
	const handWritten = "# My rules\n\n- Never touch generated code.\n"
	dir := gitRepo(t, map[string]string{
		"go.mod":          "module example.com/x\n\ngo 1.22\n",
		".agent/RULES.md": handWritten,
	})

	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; error = %+v", code, env.Err)
	}
	if got := mustRead(t, filepath.Join(dir, ".agent", "RULES.md")); got != handWritten {
		t.Errorf("RULES.md was rewritten:\n%s", got)
	}
	if !hasWarning(env, envelope.WarnFileExists) {
		t.Errorf("want a %s warning, got %v", envelope.WarnFileExists, warningCodes(env))
	}
	// The rest of the workspace still got built.
	if _, err := os.Stat(filepath.Join(dir, ".agent", "agent.yaml")); err != nil {
		t.Errorf("init gave up instead of building the rest: %v", err)
	}
}

// --force overwrites a file ocaw wrote, and only those.
func TestForceOverwritesOnlyOcawWrittenFiles(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}

	// Make a file ocaw owns non-empty, and one it does not.
	memory := filepath.Join(dir, ".agent", "MEMORY.md")
	if err := os.WriteFile(memory, []byte("edited memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	skillsFile := filepath.Join(dir, ".agent", "skills", "mine", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skillsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skillsFile, []byte("---\nname: mine\n---\nmine\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, env, _ := initEnvelope(t, dir, "--force")
	if code != 0 {
		t.Fatalf("init --force: exit %d, %+v", code, env.Err)
	}
	if got := mustRead(t, memory); strings.Contains(got, "edited memory") {
		t.Error("--force did not overwrite MEMORY.md, a file ocaw wrote")
	}
	if got := mustRead(t, skillsFile); !strings.Contains(got, "mine") {
		t.Error("--force touched a skill, which ocaw does not own")
	}
}

// A directory that is not a project is exit 3, and the hint is the way out.
func TestNotAProjectExitsPrecondition(t *testing.T) {
	dir := t.TempDir()
	code, env, _ := initEnvelope(t, dir)
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (precondition); error = %+v", code, env.Err)
	}
	if env.Err == nil || env.Err.Code != envelope.CodeNotAProject {
		t.Fatalf("code = %v, want not_a_project", env.Err)
	}
	if !strings.Contains(env.Err.Hint, "--project-type") {
		t.Errorf("hint = %q, want it to name --project-type", env.Err.Hint)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agent")); err == nil {
		t.Error("a refused init created the workspace anyway")
	}
}

// Forcing the type is the documented escape hatch, and it is the only one.
func TestForcedProjectTypeOverridesTheProjectCheck(t *testing.T) {
	for _, forced := range []string{"unknown", "node"} {
		t.Run(forced, func(t *testing.T) {
			dir := t.TempDir()
			code, env, _ := initEnvelope(t, dir, "--project-type", forced)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; error = %+v", code, env.Err)
			}
			agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
			if err != nil {
				t.Fatalf("agent.yaml: %v", err)
			}
			if agent.Project.Type != forced {
				t.Errorf("project.type = %q, want %q", agent.Project.Type, forced)
			}
			// `auto` is a flag sentinel and must never reach a file.
			if agent.Project.Type == "auto" {
				t.Error("the auto sentinel was written to agent.yaml")
			}
		})
	}
}

func TestUnknownProjectTypeIsUsage(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module example.com/x\n"})
	code, env, _ := initEnvelope(t, dir, "--project-type", "cobol")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage); error = %+v", code, env.Err)
	}
	if env.Err == nil || env.Err.Code != envelope.CodeUsage {
		t.Fatalf("code = %v, want usage", env.Err)
	}
}

// A directory that is inside a git repo is a project even with no marker file:
// the repo itself is the evidence.
func TestGitRepoWithoutMarkersIsAProject(t *testing.T) {
	dir := gitRepo(t, map[string]string{"README.md": "hi\n"})
	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; error = %+v", code, env.Err)
	}
	agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
	if err != nil {
		t.Fatalf("agent.yaml: %v", err)
	}
	if agent.Project.Type != "unknown" {
		t.Errorf("project.type = %q, want unknown with no marker file", agent.Project.Type)
	}
	if agent.Project.VerifyCmd != "" {
		t.Errorf("verify_cmd = %q, want empty with no marker file", agent.Project.VerifyCmd)
	}
}

func TestProjectTypeDetectionPerMarker(t *testing.T) {
	cases := []struct {
		marker string
		body   string
		type_  string
		verify string
	}{
		{"go.mod", "module x\n", "go", "go test ./..."},
		{"Cargo.toml", "[package]\nname=\"x\"\n", "rust", "cargo test"},
		{"pyproject.toml", "[project]\nname=\"x\"\n", "python", "pytest"},
		{"package.json", "{\"name\":\"x\"}\n", "node", "npm test"},
		{"pom.xml", "<project/>\n", "jvm", "mvn test"},
		{"build.gradle", "plugins {}\n", "jvm", "gradle test"},
		{"Makefile", "test:\n\techo\n", "make", "make test"},
	}
	for _, tc := range cases {
		t.Run(tc.marker, func(t *testing.T) {
			dir := gitRepo(t, map[string]string{tc.marker: tc.body})
			if code, env, _ := initEnvelope(t, dir); code != 0 {
				t.Fatalf("init: exit %d, %+v", code, env.Err)
			}
			agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
			if err != nil {
				t.Fatalf("agent.yaml: %v", err)
			}
			if agent.Project.Type != tc.type_ {
				t.Errorf("project.type = %q, want %q", agent.Project.Type, tc.type_)
			}
			if agent.Project.VerifyCmd != tc.verify {
				t.Errorf("verify_cmd = %q, want %q", agent.Project.VerifyCmd, tc.verify)
			}
			if agent.Project.LintCmd != "" && tc.type_ != "go" {
				t.Errorf("lint_cmd = %q: a lint command is only recorded where the toolchain guarantees it", agent.Project.LintCmd)
			}
		})
	}
}

// A repo can hold several markers. The answer has to be the same every time, or
// two machines disagree about what the project is.
func TestMarkerPriorityIsDeterministic(t *testing.T) {
	dir := gitRepo(t, map[string]string{
		"go.mod":   "module x\n",
		"Makefile": "test:\n\techo\n",
		"pom.xml":  "<project/>\n",
	})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}
	agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
	if err != nil {
		t.Fatalf("agent.yaml: %v", err)
	}
	if agent.Project.Type != "go" {
		t.Errorf("project.type = %q, want go: a Go repo with a Makefile is a Go repo", agent.Project.Type)
	}
	if agent.Project.VerifyCmd != "go test ./..." {
		t.Errorf("verify_cmd = %q, want the go one, not make test", agent.Project.VerifyCmd)
	}
}

// §5.1: the verify command is written once. A hand edit to it is the only way
// to change it, and a second init must not quietly revert that.
func TestVerifyCmdIsWrittenOnceAndNotRefreshed(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}

	path := filepath.Join(dir, ".agent", "agent.yaml")
	edited := strings.Replace(mustRead(t, path), "go test ./...", "just test", 1)
	if edited == mustRead(t, path) {
		t.Fatal("could not find the verify command to edit")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("re-init: exit %d, %+v", code, env.Err)
	}
	agent, err := yaml.ParseAgent(mustRead(t, path))
	if err != nil {
		t.Fatalf("agent.yaml: %v", err)
	}
	if agent.Project.VerifyCmd != "just test" {
		t.Errorf("verify_cmd = %q, want the hand-chosen %q", agent.Project.VerifyCmd, "just test")
	}
}

// The DAG is work. init must never replace it with a template.
func TestInitPreservesAnExistingDAG(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}
	statePath := filepath.Join(dir, ".agent", "state", "state.json")
	const dag = `{"schema":"ocaw/state@1","workflow":{"request":"keep me"},"tasks":[],"budget":{}}`
	if err := os.WriteFile(statePath, []byte(dag), 0o644); err != nil {
		t.Fatal(err)
	}

	code, env, _ := initEnvelope(t, dir, "--force")
	if code != 0 {
		t.Fatalf("init --force: exit %d, %+v", code, env.Err)
	}
	if got := mustRead(t, statePath); got != dag {
		t.Errorf("init rewrote state.json:\n%s", got)
	}
	// And the generated view follows the state, not the old render.
	if !strings.Contains(mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md")), "keep me") {
		t.Error("WORKFLOW_STATE.md was not regenerated from the current state")
	}
}

// --dry-run reports the same thing and writes nothing at all.
func TestDryRunWritesNothing(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})

	code, env, _ := initEnvelope(t, dir, "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; error = %+v", code, env.Err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agent")); err == nil {
		t.Fatal("--dry-run created .agent")
	}
	if _, err := os.Stat(filepath.Join(dir, "WORKFLOW_STATE.md")); err == nil {
		t.Fatal("--dry-run created WORKFLOW_STATE.md")
	}
	data := env.Data.(map[string]any)
	targets, _ := data["targets"].([]any)
	if len(targets) == 0 {
		t.Error("--dry-run reported no targets; it must report the plan")
	}
	for _, raw := range targets {
		target := raw.(map[string]any)
		if action, _ := target["action"].(string); action != "create" {
			t.Errorf("target %v: action = %q, want create in a dry run", target["rel"], action)
		}
	}

	// The real run then produces the same plan, and writes it.
	realCode, realEnv, _ := initEnvelope(t, dir)
	if realCode != 0 {
		t.Fatalf("real init: exit %d, %+v", realCode, realEnv.Err)
	}
	if got, want := summariseTargets(t, realEnv.Data), summariseTargets(t, env.Data); got != want {
		t.Errorf("the dry run and the real run disagree:\n dry: %s\nreal: %s", want, got)
	}
}

func summariseTargets(t *testing.T, data any) string {
	t.Helper()
	m := data.(map[string]any)
	var b strings.Builder
	targets, _ := m["targets"].([]any)
	for _, raw := range targets {
		target := raw.(map[string]any)
		b.WriteString(target["rel"].(string))
		b.WriteString("=")
		b.WriteString(target["action"].(string))
		b.WriteString(" ")
	}
	return b.String()
}

// The lock is the single-writer guarantee. It has to be taken even for the
// first init, when the directory holding it did not exist a moment ago.
func TestInitHoldsTheLockWhileRunning(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}
	lock := filepath.Join(dir, ".agent", "state", "lock")
	if _, err := os.Stat(lock); err == nil {
		t.Error("init left the lock behind after it finished")
	}

	// A lock left by someone else must stop a second init. The timestamp has to
	// be now: the staleness check reads the recorded time, not the mtime, so a
	// midnight timestamp would be hours old and the lock would be broken.
	liveLock := fmt.Sprintf(`{"pid":%d,"host":"somewhere-else","time":%q}`, os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(lock, []byte(liveLock), 0o644); err != nil {
		t.Fatal(err)
	}
	code, env, _ := initEnvelope(t, dir)
	if code != 6 {
		t.Fatalf("exit = %d, want 6 (lock_held); error = %+v", code, env.Err)
	}
	if env.Err == nil || env.Err.Code != envelope.CodeLockHeld {
		t.Fatalf("code = %v, want lock_held", env.Err)
	}
	if !strings.Contains(env.Err.Message, "held by") {
		t.Errorf("message = %q, want it to name the holder", env.Err.Message)
	}

	// An abandoned lock from a dead process on this host is a different case:
	// it is broken with a warning rather than refused, so a crashed run does not
	// block the workspace forever.
	deadLock := fmt.Sprintf(`{"pid":999999,"host":%q,"time":%q}`, hostName(t), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(lock, []byte(deadLock), 0o644); err != nil {
		t.Fatal(err)
	}
	code, env, _ = initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: a dead holder must not block the workspace; error = %+v", code, env.Err)
	}
	if !hasWarning(env, envelope.WarnStaleLockBroken) {
		t.Errorf("want a %s warning, got %v", envelope.WarnStaleLockBroken, warningCodes(env))
	}
}

func hostName(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}
	return host
}

// A corrupt agent.yaml is a validation error naming the file, not a silent
// rewrite. Replacing someone's hand-written config would lose whatever it held.
func TestCorruptAgentYAMLIsReportedNotOverwritten(t *testing.T) {
	dir := gitRepo(t, map[string]string{
		"go.mod":            "module x\n",
		".agent/agent.yaml": "ocaw: true\nschema: ocaw/agent@1\nname: [unclosed\n",
	})
	before := mustRead(t, filepath.Join(dir, ".agent", "agent.yaml"))
	code, env, _ := initEnvelope(t, dir)
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (validation); error = %+v", code, env.Err)
	}
	if env.Err == nil || env.Err.Code != envelope.CodeValidationFailed {
		t.Fatalf("code = %v, want validation_failed", env.Err)
	}
	if !strings.Contains(env.Err.Message, "agent.yaml") {
		t.Errorf("message = %q, want it to name agent.yaml", env.Err.Message)
	}
	if after := mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")); after != before {
		t.Error("a rejected init modified the file it could not parse")
	}
}

// --name is recorded, and an empty name falls back to the directory name.
func TestNameFlag(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir, "--name", "acme"); code != 0 {
		t.Fatalf("exit %d, %+v", code, env.Err)
	}
	agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name != "acme" {
		t.Errorf("name = %q, want acme", agent.Name)
	}
}

// A flag leak between invocations would make a test pass alone and fail in a
// suite, so it gets its own case: run init with flags, then without.
func TestInitFlagsDoNotLeakBetweenInvocations(t *testing.T) {
	first := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, first, "--name", "leaky", "--project-type", "node", "--force"); code != 0 {
		t.Fatalf("init with flags: exit %d, %+v", code, env.Err)
	}
	second := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, second); code != 0 {
		t.Fatalf("plain init: exit %d, %+v", code, env.Err)
	}
	agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(second, ".agent", "agent.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name == "leaky" {
		t.Error("--name leaked into the next invocation")
	}
	if agent.Project.Type != "go" {
		t.Errorf("project.type = %q, want go: --project-type leaked", agent.Project.Type)
	}
}

// Generated files carry no timestamp. A template that stamped the clock would
// make two inits differ, which is exactly what AC2 rules out, so the rule is
// checked directly on the rendered content rather than only through AC2.
//
// The year pattern is the proxy: a real timestamp contains one, and no prose
// template legitimately needs to mention a year.
func TestGeneratedFilesCarryNoTimestamp(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("exit %d, %+v", code, env.Err)
	}
	year := regexp.MustCompile(`\b(19|20)\d{2}-\d{2}-\d{2}`)
	for _, rel := range []string{".agent/RULES.md", ".agent/MEMORY.md", ".agent/KNOWLEDGE.md"} {
		body := mustRead(t, filepath.Join(dir, rel))
		if year.MatchString(body) {
			t.Errorf("%s contains a date:\n%s", rel, body)
		}
	}
	// agent.yaml is the one file that does carry one, and AC2 is what proves it
	// is preserved rather than regenerated.
	if !year.MatchString(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml"))) {
		t.Error("agent.yaml has no created stamp; the preservation test above would be vacuous")
	}
}

func commitAll(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"add", "-A"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "--quiet", "-m", "after init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// A field that is sometimes null and sometimes [] is a field a script gets
// wrong. The envelope normalises warnings; init's own payload does the same.
func TestListFieldsAreNeverNull(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("exit %d, %+v", code, env.Err)
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	// dirs is empty on a real run because the directories are made before the
	// payload is built; targets holds the five generated files.
	if !strings.Contains(string(raw), `"dirs":[]`) {
		t.Errorf("dirs is not an empty array: %s", raw)
	}
	if !strings.Contains(string(raw), `"targets":[{`) {
		t.Errorf("targets is not a populated array: %s", raw)
	}
	if strings.Contains(string(raw), ":null") {
		t.Errorf("the init payload contains a null: %s", raw)
	}
}

// Warnings that fire on every healthy run are warnings an agent learns to
// ignore. A re-init of a managed workspace has nothing to report.
func TestReinitOfAManagedWorkspaceIsQuiet(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("first init: exit %d, %+v", code, env.Err)
	}
	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("second init: exit %d, %+v", code, env.Err)
	}
	if len(env.Warnings) != 0 {
		t.Errorf("a healthy re-init warned: %v", warningCodes(env))
	}
}

// The warning must be true. init rewrites agent.yaml on every run, so a
// file_exists warning saying it was "left alone" would be a lie about the one
// file that is most worth trusting.
func TestNoFileExistsWarningNamesAgentYAML(t *testing.T) {
	dir := gitRepo(t, map[string]string{
		"go.mod":            "module x\n",
		".agent/RULES.md":   "mine\n",
		".agent/MEMORY.md":  "mine\n",
		".agent/agent.yaml": "ocaw: true\nschema: ocaw/agent@1\nname: existing\ncreated: 2026-01-01T00:00:00Z\nproject:\n  type: go\n  root: /old/path\n  verify_cmd: just test\n  verify_detected: 2026-01-01T00:00:00Z\n  lint_cmd: \"\"\n  test_cmd: \"\"\n",
	})
	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("exit %d, %+v", code, env.Err)
	}
	for _, w := range env.Warnings {
		if strings.Contains(w.Message, "agent.yaml") {
			t.Errorf("warning about agent.yaml, which init rewrites: %s", w.Message)
		}
	}
	// The human's files still warn, and the metadata is still updated.
	kept := 0
	for _, w := range env.Warnings {
		if w.Code == envelope.WarnFileExists {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("got %d file_exists warnings, want 2 (RULES.md and MEMORY.md)", kept)
	}
	agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if agent.Name != "existing" || agent.Project.VerifyCmd != "just test" {
		t.Errorf("init overwrote hand-set metadata: %+v", agent.Project)
	}
	if agent.Project.Root != dir {
		t.Errorf("project.root = %q, want it corrected to %q", agent.Project.Root, dir)
	}
}

// The distinction the warning turns on: a file still holding ocaw's template
// is a no-op, and a file a human wrote or edited is worth hearing about even
// when the workspace is already initialised.
func TestWarningsDistinguishOcawContentFromHumanContent(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}
	// A human edits one generated file and leaves the rest alone.
	rules := filepath.Join(dir, ".agent", "RULES.md")
	edited := mustRead(t, rules) + "\n- Also: never run the tests on Friday.\n"
	if err := os.WriteFile(rules, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("re-init: exit %d, %+v", code, env.Err)
	}
	if len(env.Warnings) != 1 {
		t.Fatalf("want exactly one warning about the edited file, got %v", warningCodes(env))
	}
	if !strings.Contains(env.Warnings[0].Message, "RULES.md") {
		t.Errorf("warning = %q, want it to name RULES.md", env.Warnings[0].Message)
	}
	// And the human's edit survived, as AC3 requires.
	if got := mustRead(t, rules); got != edited {
		t.Error("init overwrote the human's edit to a generated file")
	}
}

// Help discovers a command's flags by running its setup, which means --help has
// a side effect on the command's flag state. It must not be able to change what
// the next real run does.
func TestHelpDoesNotDisturbTheNextRun(t *testing.T) {
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})

	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, "init", "--help"}, &out, &errOut, false); code != 0 {
		t.Fatalf("init --help: exit %d", code)
	}
	if !strings.Contains(out.String(), "--project-type") {
		t.Errorf("init --help does not list init's own flags:\n%s", out.String())
	}
	if strings.Contains(out.String(), "--project-type\n") {
		t.Error("the global flag block leaked a command flag")
	}

	code, env, _ := initEnvelope(t, dir)
	if code != 0 {
		t.Fatalf("init after --help: exit %d, %+v", code, env.Err)
	}
	agent, err := yaml.ParseAgent(mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	if agent.Project.Type != "go" {
		t.Errorf("project.type = %q, want go: --help changed what the next run detected", agent.Project.Type)
	}
}
