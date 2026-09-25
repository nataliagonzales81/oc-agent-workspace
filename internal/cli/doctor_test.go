package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

const doctorCommandName = "doctor"

func doctorRun(t *testing.T, dir string, args ...string) (int, envelope.Envelope, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(append([]string{"--dir", dir, doctorCommandName}, args...), &out, &errOut, false)
	raw := bytes.TrimSpace(out.Bytes())
	var env envelope.Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, raw)
		}
	}
	if env.Command != doctorCommandName {
		t.Fatalf("command = %q, want %q", env.Command, doctorCommandName)
	}
	return code, env, errOut.String()
}

type doctorPayload struct {
	Findings []struct {
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Check    string `json:"check"`
		Message  string `json:"message"`
		Location string `json:"location"`
		Hint     string `json:"hint"`
	} `json:"findings"`
	Fixed     []string       `json:"fixed"`
	Counts    map[string]int `json:"counts"`
	Checks    []string       `json:"checks"`
	VerifyCmd string         `json:"verify_cmd"`
	Strict    bool           `json:"strict"`
	FixSafe   bool           `json:"fix_safe"`
	DryRun    bool           `json:"dry_run"`
}

func decodeDoctor(t *testing.T, env envelope.Envelope) doctorPayload {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var p doctorPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("doctor payload does not match the documented shape: %v\n%s", err, raw)
	}
	return p
}

func initialisedRepo(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}
	return dir
}

// The contract from §7: a healthy workspace exits 0 with no findings.
func TestDoctorOnAHealthyWorkspaceExitsZero(t *testing.T) {
	dir := initialisedRepo(t)
	code, env, _ := doctorRun(t, dir)
	if code != 0 {
		t.Fatalf("exit = %d, want 0; error = %+v", code, env.Err)
	}
	if !env.OK {
		t.Errorf("ok = false: %+v", env.Err)
	}
	if env.Schema != "ocaw/doctor@1" {
		t.Errorf("schema = %q, want ocaw/doctor@1", env.Schema)
	}
	p := decodeDoctor(t, env)
	if len(p.Findings) != 0 {
		t.Errorf("got %d findings, want 0: %+v", len(p.Findings), p.Findings)
	}
	if p.Counts["error"] != 0 || p.Counts["warning"] != 0 {
		t.Errorf("counts = %v, want zeros", p.Counts)
	}
	if p.VerifyCmd != "go test ./..." {
		t.Errorf("verify_cmd = %q, want the recorded one", p.VerifyCmd)
	}
	if p.Fixed == nil {
		t.Error("data.fixed is null, want an array")
	}
}

// AC7: the edit is exit 4, --fix-safe repairs it, and the next doctor is exit 0.
func TestAC7ThroughTheCLI(t *testing.T) {
	dir := initialisedRepo(t)
	path := filepath.Join(dir, "WORKFLOW_STATE.md")
	doc := mustRead(t, path)
	if err := os.WriteFile(path, []byte(strings.Replace(doc, "## Budget", "## Budget (by hand)", 1)), 0o644); err != nil {
		t.Fatal(err)
	}

	code, env, _ := doctorRun(t, dir)
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (validation); error = %+v", code, env.Err)
	}
	if env.Err == nil {
		t.Fatal("exit 4 with ok:true would break the contract that a non-zero exit is always explained")
	}
	p := decodeDoctor(t, env)
	if !hasFindingCode(p, "render_drift") {
		t.Errorf("want a render_drift finding: %+v", p.Findings)
	}
	if p.Counts["error"] != 1 {
		t.Errorf("counts.error = %d, want 1: %v", p.Counts["error"], p.Counts)
	}
	if len(p.Checks) != 1 || p.Checks[0] != "render" {
		t.Errorf("checks = %v, want [render]", p.Checks)
	}

	// The repairing run still exits 4, because its findings and its exit code
	// describe the same moment: the state it found. A run that reported ok:true
	// while carrying an error in `findings` would be the worse contract. What it
	// does is say what it did and what to do next.
	code, fixEnv, _ := doctorRun(t, dir, "--fix-safe")
	if code != 4 {
		t.Fatalf("the repairing run exits %d, want 4: the findings it carries are real", code)
	}
	fp := decodeDoctor(t, fixEnv)
	if len(fp.Fixed) == 0 {
		t.Error("--fix-safe reported no fixes")
	}
	if !strings.Contains(fixEnv.Err.Hint, "again") {
		t.Errorf("hint = %q, want it to say the fixes were applied and to re-run", fixEnv.Err.Hint)
	}
	// And the confirming run is clean.
	if code, env, _ := doctorRun(t, dir); code != 0 {
		t.Errorf("doctor after the fix exits %d: %+v", code, env.Err)
	}
}

// AC12 through the CLI: a misnamed skill is an error and exit 4.
func TestAC12ThroughTheCLI(t *testing.T) {
	dir := initialisedRepo(t)
	skill := filepath.Join(dir, ".agent", "skills", "verify-first", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: totally-different\ndescription: \"x\"\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, env, _ := doctorRun(t, dir)
	if code != 4 {
		t.Fatalf("exit = %d, want 4; error = %+v", code, env.Err)
	}
	p := decodeDoctor(t, env)
	if !hasFindingCheck(p, "skills") {
		t.Errorf("want a finding from the skills check: %+v", p.Findings)
	}
}

// Several findings, one array. §7 is explicit that doctor reports all of them.
func TestMultipleFindingsInOneArray(t *testing.T) {
	dir := initialisedRepo(t)
	// Three unrelated problems across three checks.
	if err := os.RemoveAll(filepath.Join(dir, ".agent", "hooks")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agent", "knowledge", "index.yaml"),
		[]byte("schema: ocaw/knowledge@1\nentries:\n  - id: k1\n    title: gone\n    kind: fact\n    anchor: gone.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, ".agent", "skills", "bad", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("---\nname: other\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, env, _ := doctorRun(t, dir)
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	p := decodeDoctor(t, env)
	checks := map[string]bool{}
	for _, f := range p.Findings {
		checks[f.Check] = true
	}
	for _, want := range []string{"layout", "skills", "knowledge"} {
		if !checks[want] {
			t.Errorf("no finding from %q; got %v", want, checks)
		}
	}
	if p.Counts["error"]+p.Counts["warning"] != len(p.Findings) {
		t.Errorf("counts %v do not add up to %d findings", p.Counts, len(p.Findings))
	}
	// Every finding carries a location, so a reader can go and look.
	for _, f := range p.Findings {
		if f.Location == "" {
			t.Errorf("finding %+v has no location", f)
		}
		if f.Severity == "error" && f.Hint == "" {
			t.Errorf("error finding %+v has no hint", f)
		}
	}
}

// --strict changes the exit code, and only the exit code.
func TestStrictChangesTheExitCode(t *testing.T) {
	dir := initialisedRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".agent", "knowledge", "index.yaml"),
		[]byte("schema: ocaw/knowledge@1\nentries:\n  - id: k1\n    title: gone\n    kind: fact\n    anchor: gone.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	lenientCode, lenientEnv, _ := doctorRun(t, dir)
	if lenientCode != 0 {
		t.Errorf("a warning alone must not fail the run, got exit %d: %+v", lenientCode, lenientEnv.Err)
	}
	strictCode, strictEnv, _ := doctorRun(t, dir, "--strict")
	if strictCode != 4 {
		t.Errorf("--strict exit = %d, want 4; error = %+v", strictCode, strictEnv.Err)
	}
	lp := decodeDoctor(t, lenientEnv)
	sp := decodeDoctor(t, strictEnv)
	if len(lp.Findings) != len(sp.Findings) {
		t.Errorf("--strict changed what was found: %d vs %d", len(lp.Findings), len(sp.Findings))
	}
	if !sp.Strict || lp.Strict {
		t.Error("the strict flag is not reported back in data")
	}
}

// A missing workspace is reported, not a crash. doctor is the command someone
// runs precisely because something is wrong.
func TestDoctorOnAMissingWorkspace(t *testing.T) {
	dir := t.TempDir()
	code, env, _ := doctorRun(t, dir)
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	p := decodeDoctor(t, env)
	if !hasFindingCode(p, "workspace_not_initialized") {
		t.Errorf("want workspace_not_initialized: %+v", p.Findings)
	}
	found := false
	for _, f := range p.Findings {
		if strings.Contains(f.Hint, "ocaw init") {
			found = true
		}
	}
	if !found {
		t.Errorf("no finding says how to fix it: %+v", p.Findings)
	}
}

// A read-only health check must never take the lock, or it reports the wrong
// problem while another agent is legitimately working.
func TestDoctorDoesNotTakeTheLock(t *testing.T) {
	dir := initialisedRepo(t)
	lock := filepath.Join(dir, ".agent", "state", "lock")
	live := `{"pid":1,"host":"somewhere-else","time":"` + timeNow() + `"}`
	if err := os.WriteFile(lock, []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	code, env, _ := doctorRun(t, dir)
	if code != 0 {
		t.Errorf("doctor failed with a live lock held: exit %d, %+v", code, env.Err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("doctor removed a lock it did not own")
	}
}

func hasFindingCode(p doctorPayload, code string) bool {
	for _, f := range p.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func hasFindingCheck(p doctorPayload, check string) bool {
	for _, f := range p.Findings {
		if f.Check == check {
			return true
		}
	}
	return false
}

func timeNow() string { return time.Now().UTC().Format(time.RFC3339) }

// The human rendering is a rendering of the envelope, not a second source of
// truth, so it is worth a test of its own: the formatting is where a payload
// that is fine in JSON turns into something unreadable.
func TestDoctorHumanRendering(t *testing.T) {
	dir := initialisedRepo(t)
	bad := filepath.Join(dir, ".agent", "skills", "verify-first", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(bad), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("---\nname: other\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := cli.Main([]string{"--dir", dir, doctorCommandName}, &out, io.Discard, true)
	if code != 4 {
		t.Fatalf("exit = %d, want 4", code)
	}
	text := out.String()
	for _, want := range []string{
		"error", "validation_failed",
		"frontmatter name is",
		"at .agent/skills/verify-first/SKILL.md",
		"try set name: verify-first",
		"2 error, 0 warning",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("human output is missing %q:\n%s", want, text)
		}
	}
	// A TTY gets prose, not the envelope.
	if strings.Contains(text, `"command":"doctor"`) {
		t.Errorf("human mode printed the JSON envelope:\n%s", text)
	}
}

func TestDoctorHumanRenderingOfAHealthyWorkspace(t *testing.T) {
	dir := initialisedRepo(t)
	var out bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, doctorCommandName}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(out.String(), "no findings") {
		t.Errorf("want a one-line summary, got:\n%s", out.String())
	}
}
