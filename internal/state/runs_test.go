package state

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

func intPtr(n int) *int       { return &n }
func strPtr(s string) *string { return &s }

// attempt builds a run with a distinct output so it does not accidentally trip
// the stuck threshold.
func attempt(task, gate, output string, at time.Time) Run {
	return NewRun(task, gate, []string{"go", "test", "./..."}, GateFail, intPtr(1), []byte(output), at)
}

func TestNewRunRecordsTheFullOutputHash(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	run := attempt("1", "test", "FAIL\n", at)
	if run.At != "2026-09-25T12:00:00Z" {
		t.Errorf("at = %q, want RFC3339 UTC", run.At)
	}
	if run.OutputSHA256 != HashOutput([]byte("FAIL\n")) {
		t.Error("output hash does not match the output")
	}
	if run.Status != GateFail || run.ExitCode == nil || *run.ExitCode != 1 {
		t.Errorf("run = %+v, want a recorded failure", run)
	}
}

// §9.6 bounds every read and every write. A gate that prints a megabyte must not
// be able to grow runs.jsonl without limit, and a truncated prefix must never be
// mistaken for the whole output.
func TestNewRunBoundsStoredOutputButNotTheHash(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	huge := []byte(strings.Repeat("x", MaxOutputBytes+5000))

	run := NewRun("1", "test", []string{"go", "test"}, GateFail, intPtr(1), huge, at)
	if !run.Truncated {
		t.Error("a record past the cap must say it was truncated")
	}
	if len(run.Output) > MaxOutputBytes+len(TruncationMarker) {
		t.Errorf("stored output is %d bytes, want at most the cap plus the marker", len(run.Output))
	}
	if !strings.HasSuffix(run.Output, TruncationMarker) {
		t.Errorf("a truncated record must end with %q", TruncationMarker)
	}
	if run.OutputSHA256 != HashOutput(huge) {
		t.Error("the hash must cover the whole output, not the stored prefix, or two different failures would compare equal")
	}
	// Two records whose prefixes are identical but whose full outputs differ must
	// not be seen as the same failure.
	other := append([]byte{}, huge...)
	other = append(other, '!')
	if NewRun("1", "test", nil, GateFail, nil, other, at).OutputSHA256 == run.OutputSHA256 {
		t.Error("outputs that differ after the cap hash the same")
	}
}

// A malformed line names its line number. Silently dropping it would erase an
// attempt from the only record of what was tried.
func TestParseRunsRefusesAMalformedLine(t *testing.T) {
	raw := []byte(`{"task":"1","gate":"test","status":"fail"}` + "\n" + `{"task":"1",}` + "\n")
	_, err := ParseRuns(raw)
	if err == nil {
		t.Fatal("ParseRuns accepted a malformed line")
	}
	var envErr *envelope.Error
	if !asEnvelope(err, &envErr) {
		t.Fatalf("error %v is not an *envelope.Error", err)
	}
	if envErr.Code != envelope.CodeValidationFailed {
		t.Errorf("code = %s, want %s", envErr.Code, envelope.CodeValidationFailed)
	}
	if !strings.Contains(envErr.Message, "line 2") {
		t.Errorf("message = %q, want it to name line 2", envErr.Message)
	}
}

func TestParseRunsSkipsBlankLinesAndDefaultsCmd(t *testing.T) {
	runs, err := ParseRuns([]byte("\n" + `{"task":"1","gate":"test"}` + "\n\n"))
	if err != nil {
		t.Fatalf("ParseRuns: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("got %d runs, want 1", len(runs))
	}
	if runs[0].Cmd == nil {
		t.Error("cmd must be an empty array, never null, so a reader can index it")
	}
}

func TestParseRunsOfAnEmptyFile(t *testing.T) {
	runs, err := ParseRuns(nil)
	if err != nil || len(runs) != 0 {
		t.Errorf("ParseRuns(nil) = %v, %v; want no runs and no error", runs, err)
	}
}

// Three identical attempts in a row is the threshold. Fewer is not stuck: two
// runs of the same failure is what a normal failing test looks like.
func TestStuckGatesNeedsThreeConsecutiveIdenticalAttempts(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	step := time.Minute

	two := []Run{
		attempt("1", "test", "same", at),
		attempt("1", "test", "same", at.Add(step)),
	}
	if got := StuckGates(two); len(got) != 0 {
		t.Errorf("two identical attempts must not be stuck, got %v", got)
	}

	three := append(two, attempt("1", "test", "same", at.Add(2*step)))
	got := StuckGates(three)
	if len(got) != 1 {
		t.Fatalf("got %v, want one stuck gate", got)
	}
	if got[0].Task != "1" || got[0].Gate != "test" {
		t.Errorf("stuck = %+v, want task 1 gate test", got[0])
	}
	if got[0].Since != "2026-09-25T12:00:00Z" {
		t.Errorf("since = %q, want the start of the identical run", got[0].Since)
	}
	if got[0].Attempts != StuckThreshold {
		t.Errorf("attempts = %d, want %d", got[0].Attempts, StuckThreshold)
	}
}

// A changed output clears the flag. Three identical failures with a different one
// in between is a gate that changed behaviour, which is not stuck.
func TestStuckGatesNeedsTheIdenticalRunsToBeConsecutive(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	step := time.Minute
	runs := []Run{
		attempt("1", "test", "same", at),
		attempt("1", "test", "different", at.Add(step)),
		attempt("1", "test", "same", at.Add(2*step)),
		attempt("1", "test", "same", at.Add(3*step)),
	}
	if got := StuckGates(runs); len(got) != 0 {
		t.Errorf("got %v, want not stuck: the run is not consecutive", got)
	}
}

// A run longer than the threshold reports how far past it the gate has gone, so
// an agent can see whether this is new or has been going for an hour.
func TestStuckGatesReportsTheWholeIdenticalRun(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	step := time.Minute
	var runs []Run
	for i := range 5 {
		runs = append(runs, attempt("1", "test", "same", at.Add(time.Duration(i)*step)))
	}
	got := StuckGates(runs)
	if len(got) != 1 {
		t.Fatalf("got %v, want one stuck gate", got)
	}
	if got[0].Attempts != 5 {
		t.Errorf("attempts = %d, want 5", got[0].Attempts)
	}
}

// An empty output still hashes, so a gate that fails silently three times is
// detectable.
func TestStuckGatesDetectsAnEmptyOutput(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	var runs []Run
	for i := range StuckThreshold {
		runs = append(runs, attempt("1", "test", "", at.Add(time.Duration(i)*time.Minute)))
	}
	if got := StuckGates(runs); len(got) != 1 {
		t.Errorf("got %v, want a gate that failed silently to be stuck", got)
	}
}

// StuckGates is sorted by task then gate, so `ocaw status` is byte-identical
// between runs.
func TestStuckGatesIsSorted(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	var runs []Run
	for _, key := range [][2]string{{"2", "b"}, {"1", "b"}, {"2", "a"}, {"1", "a"}} {
		for range StuckThreshold {
			runs = append(runs, attempt(key[0], key[1], "same", at))
		}
	}
	got := StuckGates(runs)
	want := []string{"1/a", "1/b", "2/a", "2/b"}
	if len(got) != len(want) {
		t.Fatalf("got %d stuck gates, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Task+"/"+got[i].Gate != w {
			t.Errorf("stuck[%d] = %s/%s, want %s", i, got[i].Task, got[i].Gate, w)
		}
	}
}

func TestStuckTasksReducesToTaskIDs(t *testing.T) {
	at := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	var runs []Run
	for _, gate := range []string{"build", "test"} {
		for range StuckThreshold {
			runs = append(runs, attempt("1", gate, "same", at))
		}
	}
	got := StuckTasks(runs)
	if len(got) != 1 || !got["1"] {
		t.Errorf("StuckTasks = %v, want only task 1", got)
	}
}

// asEnvelope recovers the wire error from whatever a state-layer call returned.
// It uses errors.As rather than a type assertion because a state error wraps an
// envelope error: Load can return either shape depending on which layer failed,
// and a caller that only knows the wire contract should not need a type switch.
func asEnvelope(err error, dst **envelope.Error) bool {
	return errors.As(err, dst)
}
