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
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
)

const verifyCommandName = "verify"

type attemptData struct {
	Task      string           `json:"task"`
	Gate      string           `json:"gate"`
	Command   string           `json:"command"`
	Status    state.GateStatus `json:"status"`
	Exit      *int             `json:"exit"`
	At        string           `json:"at"`
	Attempts  int              `json:"attempts"`
	Stuck     bool             `json:"stuck"`
	TimedOut  bool             `json:"timed_out"`
	Skipped   bool             `json:"skipped"`
	Reason    string           `json:"reason"`
	Output    string           `json:"output"`
	Truncated bool             `json:"output_truncated"`
	SHA256    string           `json:"output_sha256"`
}

type verifyPayload struct {
	Subcommand string        `json:"subcommand"`
	Command    string        `json:"command"`
	Argv       []string      `json:"argv"`
	Project    string        `json:"project_type"`
	Detected   string        `json:"detected_from"`
	Saved      bool          `json:"saved"`
	Attempts   []attemptData `json:"attempts"`
	Runs       []state.Run   `json:"runs"`
	Truncated  bool          `json:"history_truncated"`
	Timeout    string        `json:"timeout"`
	Skipped    []string      `json:"skipped"`
	Stuck      []state.Stuck `json:"stuck"`
	Notes      []string      `json:"notes"`
	DryRun     bool          `json:"dry_run"`
}

func verifyRun(t *testing.T, dir string, args ...string) (int, envelope.Envelope, verifyPayload) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]string{"--dir", dir, verifyCommandName}, args...)
	code := cli.Main(full, &out, &errOut, false)
	raw := bytes.TrimSpace(out.Bytes())
	var env envelope.Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("verify %v: stdout is not a JSON envelope: %v\n%s", args, err, raw)
		}
	}
	if env.Command != verifyCommandName {
		t.Fatalf("command = %q, want %q", env.Command, verifyCommandName)
	}
	var p verifyPayload
	if env.Data != nil {
		encoded, err := json.Marshal(env.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &p); err != nil {
			t.Fatalf("verify payload does not match the documented shape: %v\n%s", err, encoded)
		}
	}
	if !env.OK && env.Err != nil {
		if raw := strings.TrimSpace(errOut.String()); raw != "" {
			t.Logf("stderr on failure: %s", raw)
		}
	}
	return code, env, p
}

func mustVerify(t *testing.T, dir string, args ...string) verifyPayload {
	t.Helper()
	code, env, p := verifyRun(t, dir, args...)
	if code != 0 {
		t.Fatalf("ocaw verify %v: exit %d: %+v", args, code, env.Err)
	}
	return p
}

// gateScript writes an executable gate and returns its path. The gates in these
// tests are real programs so the runner is exercised end to end rather than
// against a mock of itself.
func gateScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "gate.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// verifiedRepo is an initialised workspace with one task, plus the path of a
// gate script the caller can install on it.
func verifiedRepo(t *testing.T, gateBody string) (string, string) {
	t.Helper()
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	return dir, gateScript(t, gateBody)
}

func gatedTask(t *testing.T, dir, gateName, script string) {
	t.Helper()
	code, env, _ := taskRun(t, dir, "set", "1", "--gate", gateName+"="+script)
	if code != 0 {
		t.Fatalf("set gate: exit %d: %+v", code, env.Err)
	}
}

// AC8 — a failing gate gives a non-zero exit, records the real exit code, leaves
// the gate `fail`, and does not retry.
func TestAC8AFailingGate(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 3")
	gatedTask(t, dir, "test", script)

	code, env, data := verifyRun(t, dir, "run")
	if code != 0 {
		t.Fatalf("exit = %d, want 0: a failing gate is an attempt, not a command failure: %+v", code, env.Err)
	}
	if len(data.Attempts) != 1 {
		t.Fatalf("attempts = %d, want exactly one: nothing may be retried", len(data.Attempts))
	}
	att := data.Attempts[0]
	if att.Status != state.GateFail {
		t.Errorf("status = %q, want fail", att.Status)
	}
	if att.Exit == nil || *att.Exit != 3 {
		t.Errorf("exit = %v, want the command's own 3", att.Exit)
	}
	if att.Task != "1" || att.Gate != "test" {
		t.Errorf("recorded as %s/%s, want 1/test", att.Task, att.Gate)
	}

	// The gate in state.json is fail, with the real code.
	shown := mustTask(t, dir, "show", "1")
	if len(shown.Task.Gates) != 1 {
		t.Fatalf("gates = %+v", shown.Task.Gates)
	}
	g := shown.Task.Gates[0]
	if g.Status != state.GateFail {
		t.Errorf("gate status = %q, want fail", g.Status)
	}
	if g.LastExit == nil || *g.LastExit != 3 {
		t.Errorf("gate last_exit = %v, want 3", g.LastExit)
	}
	if g.Attempts != 1 {
		t.Errorf("gate attempts = %d, want 1", g.Attempts)
	}

	// Exactly one record in runs.jsonl, and its output is the real output.
	hist := mustVerify(t, dir, "history")
	if len(hist.Runs) != 1 {
		t.Errorf("runs.jsonl holds %d records, want 1", len(hist.Runs))
	}
	if hist.Runs[0].OutputSHA256 == "" {
		t.Error("the record has no output hash, so stuck detection cannot compare it")
	}
}

// AC9 — three consecutive byte-identical failing runs set stuck.
func TestAC9ThreeIdenticalFailuresAreStuck(t *testing.T) {
	dir, script := verifiedRepo(t, "echo 'same failure every time'; exit 1")
	gatedTask(t, dir, "test", script)

	for i := range 3 {
		data := mustVerify(t, dir, "run")
		if len(data.Attempts) != 1 {
			t.Fatalf("attempt %d: %d attempts, want 1", i, len(data.Attempts))
		}
		att := data.Attempts[0]
		if att.Attempts != i+1 {
			t.Errorf("attempt %d: data says %d, want %d", i, att.Attempts, i+1)
		}
		wantStuck := i >= 2
		if att.Stuck != wantStuck {
			t.Errorf("after %d attempt(s), stuck = %t, want %t", i+1, att.Stuck, wantStuck)
		}
	}

	data := mustVerify(t, dir, "run", "--force")
	if !data.Attempts[0].Stuck {
		t.Error("a fourth identical failure did not leave the gate stuck")
	}
	if len(data.Stuck) != 1 {
		t.Errorf("stuck = %+v, want one entry", data.Stuck)
	}
	if data.Stuck[0].Task != "1" || data.Stuck[0].Gate != "test" {
		t.Errorf("stuck entry = %+v, want 1/test", data.Stuck[0])
	}
	if data.Stuck[0].Attempts < state.StuckThreshold {
		t.Errorf("stuck attempts = %d, want at least %d", data.Stuck[0].Attempts, state.StuckThreshold)
	}
}

// Different output is not stuck, even three times over. Stuck means "the same
// thing keeps happening", not "this keeps failing".
func TestDifferentFailuresAreNotStuck(t *testing.T) {
	dir, _ := verifiedRepo(t, "true")
	counter := filepath.Join(filepath.Dir(dir), "counter")
	script := gateScript(t, "n=$(cat "+counter+" 2>/dev/null || echo 0); n=$((n+1)); echo $n > "+counter+"; echo attempt $n; exit 1")
	gatedTask(t, dir, "test", script)

	for range 3 {
		data := mustVerify(t, dir, "run", "--force")
		if data.Attempts[0].Stuck {
			t.Fatalf("output changed every run, so nothing is stuck: %+v", data.Attempts[0])
		}
	}
}

func TestAStuckGateIsAlsoFlaggedOnTheTask(t *testing.T) {
	dir, script := verifiedRepo(t, "echo identical; exit 1")
	gatedTask(t, dir, "test", script)
	for range 3 {
		mustVerify(t, dir, "run")
	}
	shown := mustTask(t, dir, "show", "1")
	if !shown.Task.Stuck {
		t.Error("ocaw task show does not flag a stuck gate")
	}
	listed := mustTask(t, dir, "list")
	if len(listed.Tasks) != 1 || !listed.Tasks[0].Stuck {
		t.Error("ocaw task list does not flag a stuck gate")
	}
	// And it is visible where an agent actually meets it: the human rendering
	// of the task it is about to pick up.
	var out bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, "task", "next"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("task next: exit %d", code)
	}
	if !strings.Contains(out.String(), "STUCK") {
		t.Errorf("human `task next` does not say STUCK:\n%s", out.String())
	}
}

// A gate that already passes is not re-run without --force. Re-running it to
// collect another identical record makes "it has passed recently" mean less
// each time.
func TestAPassingGateIsNotReRunWithoutForce(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 0")
	gatedTask(t, dir, "test", script)

	mustVerify(t, dir, "run")
	before := mustVerify(t, dir, "history")
	if len(before.Runs) != 1 {
		t.Fatalf("runs = %d, want 1", len(before.Runs))
	}

	// A second plain run must record nothing: the gate passes and is skipped.
	after := mustVerify(t, dir, "run")
	if len(after.Attempts) != 0 {
		t.Errorf("attempts = %+v, want none: a passing gate is skipped", after.Attempts)
	}
	hist := mustVerify(t, dir, "history")
	if len(hist.Runs) != 1 {
		t.Errorf("runs = %d after a skipped gate, want 1: nothing was re-run", len(hist.Runs))
	}

	// --force does re-run it, and the attempt count grows.
	forced := mustVerify(t, dir, "run", "--force")
	if len(forced.Attempts) != 1 {
		t.Errorf("--force attempts = %+v, want one", forced.Attempts)
	}
	if forced.Attempts[0].Attempts != 2 {
		t.Errorf("attempt count = %d, want 2", forced.Attempts[0].Attempts)
	}
}

// The deadline is what makes a hang an attempt.
func TestATimeoutIsNeverAHang(t *testing.T) {
	dir, script := verifiedRepo(t, "sleep 5")
	gatedTask(t, dir, "test", script)

	code, env, data := verifyRun(t, dir, "run", "--timeout", "200ms")
	if code != 0 {
		t.Fatalf("exit = %d, want 0: a timeout is an attempt, not a command failure: %+v", code, env.Err)
	}
	att := data.Attempts[0]
	if att.Status != state.GateTimeout {
		t.Errorf("status = %q, want timeout", att.Status)
	}
	if !att.TimedOut {
		t.Error("timed_out = false on a timed-out attempt")
	}
	if att.Exit != nil {
		t.Errorf("exit = %v, want nil: nothing exited", *att.Exit)
	}
	// The gate is recorded as timed out, and the record says so.
	shown := mustTask(t, dir, "show", "1")
	if shown.Task.Gates[0].Status != state.GateTimeout {
		t.Errorf("gate status = %q, want timeout", shown.Task.Gates[0].Status)
	}
	hist := mustVerify(t, dir, "history")
	if len(hist.Runs) != 1 || hist.Runs[0].Status != state.GateTimeout {
		t.Errorf("history = %+v, want one timeout record", hist.Runs)
	}
	if data.Timeout != "200ms" {
		t.Errorf("data.timeout = %q, want the effective deadline", data.Timeout)
	}
}

func TestAnUnparsableTimeoutIsUsage(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 0")
	gatedTask(t, dir, "test", script)
	for _, bad := range []string{"soon", "0", "-1s"} {
		code, env, _ := verifyRun(t, dir, "run", "--timeout", bad)
		if code != 2 {
			t.Errorf("--timeout %q: exit = %d, want 2 (usage)", bad, code)
		}
		if env.Err == nil || env.Err.Code != envelope.CodeUsage {
			t.Errorf("--timeout %q: code = %v, want usage", bad, env.Err)
		}
	}
}

// §9.4: --shell is parseable and always refused. A flag that is accepted and
// then errors reads as a bug in ocaw rather than as the rule it is.
func TestShellIsRefused(t *testing.T) {
	dir := initialisedTaskRepo(t)
	code, env, _ := verifyRun(t, dir, "run", "--shell", "--", "echo hi")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
	if env.Err == nil || !strings.Contains(env.Err.Message, "never a shell") {
		t.Errorf("message = %v, want it to name the rule", env.Err)
	}
}

func TestAShellStringIsNotEvenTokenised(t *testing.T) {
	dir := initialisedTaskRepo(t)
	code, env, _ := verifyRun(t, dir, "run", "--", "go test ./... && rm -rf /")
	if code != 4 {
		t.Fatalf("exit = %d, want 4 (validation)", code)
	}
	if env.Err == nil || !strings.Contains(env.Err.Message, "&&") {
		t.Errorf("message = %v, want it to name the operator", env.Err)
	}
}

// --dry-run runs nothing and records nothing.
func TestDryRunRecordsNothing(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 0")
	gatedTask(t, dir, "test", script)
	commitAll(t, dir)

	data := mustVerify(t, dir, "run", "--dry-run")
	if !data.DryRun {
		t.Error("data.dry_run = false")
	}
	if len(data.Attempts) != 1 || !data.Attempts[0].Skipped {
		t.Errorf("attempts = %+v, want one skipped entry naming what it would run", data.Attempts)
	}
	if dirty := porcelain(t, dir); dirty != "" {
		t.Errorf("the dry run changed files:\n%s", dirty)
	}
	hist := mustVerify(t, dir, "history")
	if len(hist.Runs) != 0 {
		t.Errorf("history = %+v, want empty: a dry run records no attempt", hist.Runs)
	}
}

func TestARunWithNoGatesSaysSo(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	data := mustVerify(t, dir, "run")
	if len(data.Attempts) != 0 {
		t.Errorf("attempts = %+v, want none", data.Attempts)
	}
	if len(data.Skipped) == 0 || !strings.Contains(data.Skipped[0], "no gates") {
		t.Errorf("skipped = %v, want it to say there are no gates rather than reporting nothing", data.Skipped)
	}
}

// An explicit command overrides the recorded gates for the named task, and is
// still recorded under a task and a gate so the history stays readable.
func TestAnExplicitCommandIsRecorded(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "adhoc", "--title", "one-off")
	data := mustVerify(t, dir, "run", "--task", "adhoc", "--gate", "oneoff", "--", "/bin/echo hello")
	if len(data.Attempts) != 1 {
		t.Fatalf("attempts = %+v, want one", data.Attempts)
	}
	if data.Attempts[0].Command != "/bin/echo hello" {
		t.Errorf("command = %q", data.Attempts[0].Command)
	}
	if data.Attempts[0].Task != "adhoc" || data.Attempts[0].Gate != "oneoff" {
		t.Errorf("recorded as %s/%s", data.Attempts[0].Task, data.Attempts[0].Gate)
	}
	if data.Attempts[0].Status != state.GatePass {
		t.Errorf("status = %q, want pass", data.Attempts[0].Status)
	}
	// And the gate it was recorded under now exists on the task.
	shown := mustTask(t, dir, "show", "adhoc")
	if len(shown.Task.Gates) != 1 || shown.Task.Gates[0].Name != "oneoff" {
		t.Errorf("gates = %+v, want the ad-hoc gate to be defined", shown.Task.Gates)
	}
}

func TestAnExplicitCommandNeedsATask(t *testing.T) {
	dir := initialisedTaskRepo(t)
	code, env, _ := verifyRun(t, dir, "run", "--", "/bin/echo hi")
	if code != 2 {
		t.Fatalf("exit = %d, want 2 (usage)", code)
	}
	if !strings.Contains(env.Err.Hint, "--task") {
		t.Errorf("hint = %q, want it to name --task", env.Err.Hint)
	}
}

func TestAnUnknownTaskIsNotFound(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 0")
	gatedTask(t, dir, "test", script)
	for _, args := range [][]string{
		{"run", "--task", "nope"},
		{"history", "--task", "nope"},
	} {
		code, env, _ := verifyRun(t, dir, args...)
		if code != 7 {
			t.Errorf("%v: exit = %d, want 7 (not_found); %+v", args, code, env.Err)
		}
		if env.Err == nil || env.Err.Code != envelope.CodeTaskNotFound {
			t.Errorf("%v: code = %v, want task_not_found", args, env.Err)
		}
	}
}

func TestRunIsScopedToATaskAndAGate(t *testing.T) {
	dir := initialisedTaskRepo(t)
	pass := gateScript(t, "exit 0")
	fail := gateScript(t, "exit 1")
	mustTask(t, dir, "add", "--id", "1", "--title", "one")
	mustTask(t, dir, "add", "--id", "2", "--title", "two")
	mustTask(t, dir, "set", "1", "--gate", "test="+pass)
	mustTask(t, dir, "set", "1", "--gate", "lint="+fail)
	mustTask(t, dir, "set", "2", "--gate", "test="+pass)

	all := mustVerify(t, dir, "run")
	if len(all.Attempts) != 3 {
		t.Errorf("all tasks: %d attempts, want 3", len(all.Attempts))
	}
	one := mustVerify(t, dir, "run", "--force", "--task", "1")
	if len(one.Attempts) != 2 {
		t.Errorf("one task: %d attempts, want 2", len(one.Attempts))
	}
	oneGate := mustVerify(t, dir, "run", "--force", "--task", "1", "--gate", "lint")
	if len(oneGate.Attempts) != 1 || oneGate.Attempts[0].Gate != "lint" {
		t.Errorf("one gate: %+v, want just lint", oneGate.Attempts)
	}
}

func TestHistoryFiltersAndLimits(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 1")
	gatedTask(t, dir, "test", script)
	mustTask(t, dir, "add", "--id", "2", "--title", "two")
	other := gateScript(t, "exit 2")
	mustTask(t, dir, "set", "2", "--gate", "test="+other)

	for range 3 {
		mustVerify(t, dir, "run", "--task", "1", "--force")
	}
	mustVerify(t, dir, "run", "--task", "2", "--force")

	all := mustVerify(t, dir, "history")
	if len(all.Runs) != 4 {
		t.Errorf("all history = %d records, want 4", len(all.Runs))
	}
	if all.Truncated {
		t.Error("truncated = true with only four records and a default limit of 20")
	}
	one := mustVerify(t, dir, "history", "--task", "1")
	if len(one.Runs) != 3 {
		t.Errorf("task 1 history = %d records, want 3", len(one.Runs))
	}
	limited := mustVerify(t, dir, "history", "--limit", "2")
	if len(limited.Runs) != 2 {
		t.Errorf("--limit 2 = %d records, want 2", len(limited.Runs))
	}
	if !limited.Truncated {
		t.Error("truncated = false when records were dropped")
	}
	// The limit keeps the most recent, so the last record is the newest.
	if limited.Runs[len(limited.Runs)-1].TaskID != "2" {
		t.Errorf("the limit kept the oldest records: %+v", limited.Runs)
	}
}

func TestAMalformedRunLogIsAnError(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 0")
	gatedTask(t, dir, "test", script)
	log := filepath.Join(dir, ".agent", "state", "runs.jsonl")
	if err := os.WriteFile(log, []byte("not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A broken log must not be silently ignored: it is the only record of what
	// was tried, and a skipped line would erase an attempt.
	code, env, _ := verifyRun(t, dir, "history")
	if code != 4 {
		t.Errorf("exit = %d, want 4 (validation); %+v", code, env.Err)
	}
	if env.Err == nil || !strings.Contains(env.Err.Message, "line 1") {
		t.Errorf("message = %v, want it to name the line", env.Err)
	}
}

// ---- detect ----

func TestDetectReportsWithoutSaving(t *testing.T) {
	dir := initialisedTaskRepo(t)
	before := mustRead(t, filepath.Join(dir, ".agent", "agent.yaml"))
	data := mustVerify(t, dir, "detect")
	if data.Command != "go test ./..." {
		t.Errorf("command = %q, want go test ./...", data.Command)
	}
	if len(data.Argv) != 3 || data.Argv[0] != "go" {
		t.Errorf("argv = %v, want a tokenised command", data.Argv)
	}
	if data.Saved {
		t.Error("saved = true without --save")
	}
	if after := mustRead(t, filepath.Join(dir, ".agent", "agent.yaml")); after != before {
		t.Error("detect wrote agent.yaml without --save")
	}
}

// A project that chose its own runner keeps it. §5.1: the recorded command
// changes only by explicit request.
func TestDetectDoesNotOverrideAChosenCommand(t *testing.T) {
	dir := initialisedTaskRepo(t)
	path := filepath.Join(dir, ".agent", "agent.yaml")
	edited := strings.Replace(mustRead(t, path), "verify_cmd: go test ./...", "verify_cmd: just test", 1)
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	data := mustVerify(t, dir, "detect")
	if data.Saved {
		t.Error("saved = true without --save")
	}
	if len(data.Notes) == 0 {
		t.Error("no note explaining that the project's own command was kept")
	}
	agent := mustRead(t, path)
	if !strings.Contains(agent, "just test") {
		t.Error("detect replaced a command the project had chosen")
	}

	// --save replaces it, because that is the explicit request.
	saved := mustVerify(t, dir, "detect", "--save")
	if !saved.Saved {
		t.Error("saved = false with --save")
	}
	if !strings.Contains(mustRead(t, path), "go test ./...") {
		t.Error("--save did not write the detected command")
	}
}

func TestDetectOnATypeWithNoConvention(t *testing.T) {
	// A workspace whose type has no conventional command, so detection has
	// nothing to offer. A go repo would detect fine and prove nothing.
	dir := gitRepo(t, map[string]string{"go.mod": "module x\n"})
	if code, env, _ := initEnvelope(t, dir, "--project-type", "unknown"); code != 0 {
		t.Fatalf("init: %d %+v", code, env.Err)
	}
	code, env, _ := verifyRun(t, dir, "detect")
	if code != 3 {
		t.Fatalf("exit = %d, want 3 (precondition); %+v", code, env.Err)
	}
	if !strings.Contains(env.Err.Message, "unknown") {
		t.Errorf("message = %v, want it to name the project type", env.Err)
	}
}

func TestVerifyUsageErrors(t *testing.T) {
	dir := initialisedTaskRepo(t)
	for _, args := range [][]string{
		{},
		{"bogus"},
		{"run", "--"},
	} {
		code, env, _ := verifyRun(t, dir, args...)
		if code != 2 {
			t.Errorf("%v: exit = %d, want 2; %+v", args, code, env.Err)
		}
		if env.Err == nil || env.Err.Code != envelope.CodeUsage {
			t.Errorf("%v: code = %v, want usage", args, env.Err)
		}
	}
}

// Every list in the payload is a list, on the failure path too.
func TestVerifyPayloadHasNoNulls(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 0")
	gatedTask(t, dir, "test", script)
	code, env, _ := verifyRun(t, dir, "run", "--task", "nope")
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	if code != 7 {
		t.Fatalf("exit = %d, want 7", code)
	}
	if strings.Contains(string(raw), ":null") {
		t.Errorf("payload contains a null: %s", raw)
	}
}

func TestVerifyHumanRendering(t *testing.T) {
	dir, script := verifiedRepo(t, "exit 3")
	gatedTask(t, dir, "test", script)

	var out bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, verifyCommandName, "run"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "attempt 1") {
		t.Errorf("human run output:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"command"`) {
		t.Error("a TTY got JSON")
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, verifyCommandName, "history"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "fail") {
		t.Errorf("human history output:\n%s", out.String())
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, verifyCommandName, "detect"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "go test ./...") {
		t.Errorf("human detect output:\n%s", out.String())
	}
}

// A gate that spawns children must still respect the deadline. Killing only the
// gate leaves the grandchild holding the output pipe, and the 200ms deadline
// becomes however long the child runs.
func TestTheCLIDeadlineSurvivesAGrandchild(t *testing.T) {
	dir, script := verifiedRepo(t, "sleep 30")
	gatedTask(t, dir, "test", script)
	commitAll(t, dir)

	start := time.Now()
	data := mustVerify(t, dir, "run", "--timeout", "200ms")
	elapsed := time.Since(start)
	if len(data.Attempts) != 1 {
		t.Fatalf("attempts = %+v, want one", data.Attempts)
	}
	if data.Attempts[0].Status != state.GateTimeout {
		t.Errorf("status = %q, want timeout", data.Attempts[0].Status)
	}
	if elapsed > 5*time.Second {
		t.Errorf("took %s for a 200ms deadline", elapsed.Round(time.Millisecond))
	}
	// The attempt is recorded, so the deadline is visible in the history rather
	// than only in how long the command took.
	hist := mustVerify(t, dir, "history")
	if len(hist.Runs) != 1 || hist.Runs[0].Status != state.GateTimeout {
		t.Errorf("history = %+v, want one timeout record", hist.Runs)
	}
}
