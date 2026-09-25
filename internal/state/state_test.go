package state

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// newState returns a state with no tasks, ready for a test to fill in.
func newState() *State { return New() }

// chain builds tasks 1..n, each depending on the one before it, so a test gets
// a valid DAG in stored order without repeating the literal seven times.
func chain(s *State, titles ...string) {
	for i, title := range titles {
		id := strconv.Itoa(i + 1)
		deps := []string{}
		if i > 0 {
			deps = []string{strconv.Itoa(i)}
		}
		s.Tasks = append(s.Tasks, Task{ID: id, Title: title, Deps: deps, Status: StatusPending})
	}
}

// codeOf is the assertion every invariant test ends with: the exact closed code
// the failure must carry, so a test cannot pass by returning the wrong failure
// for the right reason.
func codeOf(t *testing.T, err *Error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a state error, got nil")
	}
	if err.Err == nil {
		t.Fatal("state error has no envelope payload")
	}
	if !envelope.KnownCode(err.Err.Code) {
		t.Errorf("code %q is not a member of the closed set (SPEC §8)", err.Err.Code)
	}
	if err.Err.Hint == "" {
		t.Error("error has no hint; every failure must name the way out")
	}
	return string(err.Err.Code)
}

// TestNewIsValidAndEmpty pins the two properties every command depends on: a
// fresh state has no nulls, so the JSON is well-formed, and it passes every
// invariant, so `ocaw init` can write it without a second thought.
func TestNewIsValidAndEmpty(t *testing.T) {
	s := New()
	if err := s.Validate(); err != nil {
		t.Fatalf("a new state must be valid, got %v", err)
	}
	raw, err := s.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "null") {
		t.Errorf("state.json must not contain null for a new workspace:\n%s", raw)
	}
	for _, want := range []string{`"schema": "ocaw/state@1"`, `"tasks": []`, `"constraints": []`, `"acceptance": []`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}
}

// The spec shows state.json byte for byte. A drift in field order or a null in
// place of an empty list would break every agent that diffs the file.
func TestMarshalIsDeterministicAndSpecShaped(t *testing.T) {
	s := New()
	s.Workflow.Request = "ship ocaw"
	s.Workflow.Constraints = []string{"stdlib only"}
	s.Workflow.Acceptance = []Acceptance{{ID: "a1", Text: "init twice is clean", Done: false}}
	s.Tasks = []Task{{
		ID: "1", Title: "scaffold", Deps: []string{}, Status: StatusPending,
		Gates: []Gate{{Name: "gate", Cmd: "go test ./...", Status: GatePending}},
		Notes: "", Updated: "2026-09-25T00:00:00Z",
	}}

	first, err := s.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	second, err := s.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Error("two marshals of the same state differ (SPEC §9.5)")
	}
	if !strings.HasSuffix(string(first), "\n") {
		t.Error("state.json must end with a newline")
	}
	if strings.Contains(string(first), `<`) {
		t.Error("state.json is HTML-escaped; paths must survive verbatim")
	}

	// Round trip through the wire form: nulls must not appear for omitted
	// pointers, and must appear for absent last_exit / last_run.
	var probe struct {
		Tasks []struct {
			Gates []struct {
				LastExit *int    `json:"last_exit"`
				LastRun  *string `json:"last_run"`
			} `json:"gates"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(first, &probe); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	gate := probe.Tasks[0].Gates[0]
	if gate.LastExit != nil || gate.LastRun != nil {
		t.Errorf("an unrun gate must record null exit and run, got %v / %v", gate.LastExit, gate.LastRun)
	}
}

func TestCountsIncludesEveryStatus(t *testing.T) {
	s := newState()
	chain(s, "a", "b", "c")
	s.Tasks[0].Status = StatusDone
	s.Tasks[1].Status = StatusInProgress

	got := s.Counts()
	if len(got) != len(Statuses) {
		t.Errorf("Counts has %d keys, want one per status (%d)", len(got), len(Statuses))
	}
	if got[string(StatusDone)] != 1 || got[string(StatusInProgress)] != 1 || got[string(StatusPending)] != 1 {
		t.Errorf("Counts = %v", got)
	}
	if got[string(StatusCancelled)] != 0 {
		t.Error("a status with no tasks must still be present, so a summary needs no nil checks")
	}
}

func TestViewsCarryStuck(t *testing.T) {
	s := newState()
	chain(s, "a", "b")
	views := s.Views(map[string]bool{"2": true})
	if len(views) != 2 {
		t.Fatalf("got %d views, want 2", len(views))
	}
	if views[0].Stuck {
		t.Error("task 1 is not in the stuck set")
	}
	if !views[1].Stuck {
		t.Error("task 2 is in the stuck set but reported as not stuck")
	}
	raw, err := json.Marshal(views[1])
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"stuck":true`) {
		t.Errorf("a view must always carry the stuck flag, got %s", raw)
	}
}

func TestTaskGateHelpers(t *testing.T) {
	task := Task{ID: "1", Gates: []Gate{{Name: "build", Cmd: "go build ./..."}, {Name: "test", Cmd: "go test ./..."}}}
	if got := task.GateNames(); len(got) != 2 || got[0] != "build" || got[1] != "test" {
		t.Errorf("GateNames = %v, want stored order", got)
	}
	if _, ok := task.Gate("nope"); ok {
		t.Error("Gate(nope) reported a gate that does not exist")
	}
	if _, ok := task.Gate("test"); !ok {
		t.Error("Gate(test) did not find the gate")
	}
}

// SPEC §12: state must not import cli. The DAG rules are the model's, and a
// command is one consumer of them; the moment a command type reaches in here,
// doctor, status and task all start depending on whichever command happened to
// be written first, and the invariants can no longer be tested without a FlagSet.
func TestStateDoesNotImportCLI(t *testing.T) {
	root := filepath.Join("..", "..")
	dir := filepath.Join(root, "internal", "state")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	cliPath := "github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if imported == cliPath {
				t.Errorf("%s imports internal/cli; SPEC §12 forbids it", name)
			}
		}
	}
}
