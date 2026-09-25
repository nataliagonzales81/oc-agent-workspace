package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

func newLayout(t *testing.T) *workspace.Layout {
	t.Helper()
	return &workspace.Layout{Root: t.TempDir()}
}

func writeRaw(t *testing.T, path, body string) {
	t.Helper()
	if err := workspace.EnsureDir(filepath.Dir(path)); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), workspace.FileMode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	l := newLayout(t)
	s := New()
	s.Workflow.Request = "ship the DAG"
	s.Workflow.Constraints = []string{"stdlib only", "no network"}
	s.Workflow.Acceptance = []Acceptance{{ID: "a1", Text: "init twice is clean", Done: true}}
	exit := 0
	ran := "2026-09-25T11:00:00Z"
	s.Tasks = []Task{{
		ID: "1", Title: "scaffold", Agent: "coder", Tier: "foundation",
		Deps: []string{}, Status: StatusDone, Updated: "2026-09-25T12:00:00Z",
		Gates: []Gate{{Name: "test", Cmd: "go test ./...", Status: GatePass, LastExit: &exit, Attempts: 1, LastRun: &ran}},
	}}
	s.Budget = Budget{MaxTokens: 100000, SpentTokens: 4321}

	if err := Save(l, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(l)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	raw, err := got.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := s.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(raw) != string(want) {
		t.Errorf("a save/load round trip changed the state.\n got: %s\nwant: %s", raw, want)
	}

	task, _ := got.Find("1")
	if task == nil || task.Status != StatusDone {
		t.Fatalf("task 1 did not survive the round trip: %+v", task)
	}
	gate, _ := task.Gate("test")
	if gate.LastExit == nil || *gate.LastExit != 0 || gate.LastRun == nil || *gate.LastRun != ran {
		t.Errorf("the recorded outcome did not survive: %+v", gate)
	}
}

// The same state in, the same bytes out, and a second save that changes nothing
// writes nothing different. This is what makes `ocaw init` twice clean.
func TestSaveIsByteIdenticalForTheSameState(t *testing.T) {
	l := newLayout(t)
	s := New()
	chain(s, "a", "b")
	if err := Save(l, s); err != nil {
		t.Fatalf("Save: %v", err)
	}
	first, err := os.ReadFile(l.StateJSON())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if err := Save(l, s); err != nil {
		t.Fatalf("Save (second): %v", err)
	}
	second, err := os.ReadFile(l.StateJSON())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(first) != string(second) {
		t.Error("saving the same state twice produced different bytes")
	}
}

// An unknown field is refused. Decoding what we recognise and dropping the rest
// means the next save deletes whatever a newer ocaw put there, and the loss is
// silent.
func TestLoadRefusesAnUnknownField(t *testing.T) {
	l := newLayout(t)
	writeRaw(t, l.StateJSON(), `{
  "schema": "ocaw/state@1",
  "workflow": {"request": "", "scope": "", "constraints": [], "acceptance": []},
  "tasks": [],
  "budget": {"max_tokens": 0, "spent_tokens": 0},
  "something_new": {"added": "by a newer ocaw"}
}`)

	_, err := Load(l)
	if err == nil {
		t.Fatal("Load accepted a field this build does not understand")
	}
	var envErr *envelope.Error
	if !asEnvelope(err, &envErr) {
		t.Fatalf("error %v is not an *envelope.Error", err)
	}
	if envErr.Code != envelope.CodeValidationFailed {
		t.Errorf("code = %s, want %s", envErr.Code, envelope.CodeValidationFailed)
	}
	if !strings.Contains(envErr.Message, "something_new") {
		t.Errorf("message = %q, want it to name the field", envErr.Message)
	}
}

func TestLoadRefusesAnUnparsableFile(t *testing.T) {
	l := newLayout(t)
	writeRaw(t, l.StateJSON(), "{not json")
	if _, err := Load(l); err == nil {
		t.Fatal("Load accepted a file that is not JSON")
	}
}

// A missing state.json in an initialised workspace is damage, not a first run.
func TestLoadOfAMissingStateFile(t *testing.T) {
	l := newLayout(t)
	_, err := Load(l)
	var envErr *envelope.Error
	if !asEnvelope(err, &envErr) {
		t.Fatalf("error %v is not an *envelope.Error", err)
	}
	if envErr.Code != envelope.CodeValidationFailed {
		t.Errorf("code = %s, want %s", envErr.Code, envelope.CodeValidationFailed)
	}
	if !strings.Contains(envErr.Hint, "ocaw init") {
		t.Errorf("hint = %q, want it to point at ocaw init", envErr.Hint)
	}
}

// Save refuses a state that would fail to load. ocaw holds the lock precisely so
// two agents cannot interleave, and a write that produced an unloadable file
// would leave the next run with no way back except a hand edit.
func TestSaveRefusesAnInvalidState(t *testing.T) {
	l := newLayout(t)
	s := New()
	s.Tasks = []Task{{ID: "1", Deps: []string{"ghost"}, Status: StatusPending}}
	err := Save(l, s)
	if err == nil {
		t.Fatal("Save wrote a state with a dangling dep")
	}
	if _, statErr := os.Stat(l.StateJSON()); !os.IsNotExist(statErr) {
		t.Error("a refused save must not leave a file behind")
	}
}

func TestSaveIsAtomic(t *testing.T) {
	l := newLayout(t)
	chain(New(), "a")
	if err := Save(l, New()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entries, err := os.ReadDir(l.StateDir())
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), workspace.TempPrefix) {
			t.Errorf("a temp file survived the save: %s", e.Name())
		}
	}
}

func TestLoadRunsAndAppendRun(t *testing.T) {
	l := newLayout(t)

	// A workspace that has never been verified has no log, which is empty rather
	// than an error.
	runs, err := LoadRuns(l)
	if err != nil {
		t.Fatalf("LoadRuns: %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("got %d runs, want none", len(runs))
	}

	run := attempt("1", "test", "FAIL", testNow)
	if err := AppendRun(l, run); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}
	if err := AppendRun(l, attempt("1", "test", "FAIL", testNow)); err != nil {
		t.Fatalf("AppendRun: %v", err)
	}
	body, err := os.ReadFile(l.RunsJSONL())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if n := strings.Count(string(body), "\n"); n != 2 {
		t.Errorf("runs.jsonl has %d lines, want 2: records are appended one per line", n)
	}

	runs, err = LoadRuns(l)
	if err != nil {
		t.Fatalf("LoadRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2", len(runs))
	}
	if runs[0].OutputSHA256 != runs[1].OutputSHA256 {
		t.Error("two identical attempts must hash the same")
	}
}

func TestLoadRunsRefusesAMalformedLog(t *testing.T) {
	l := newLayout(t)
	writeRaw(t, l.RunsJSONL(), "{oops}\n")
	if _, err := LoadRuns(l); err == nil {
		t.Fatal("LoadRuns accepted a malformed log")
	}
}

func TestRunMarshalIsOneLine(t *testing.T) {
	run := attempt("1", "test", "multi\nline\noutput", testNow)
	raw, err := run.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if n := strings.Count(string(raw), "\n"); n != 1 || !strings.HasSuffix(string(raw), "\n") {
		t.Errorf("a record must be exactly one line, got %q", raw)
	}
	// The output's own newlines are escaped, not literal.
	if strings.Count(string(raw), "\n") != 1 {
		t.Errorf("record = %q, want the embedded newlines escaped", raw)
	}
}
