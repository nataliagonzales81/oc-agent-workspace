package state

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

func encodeRun(t *testing.T, r Run) string {
	t.Helper()
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeRuns(t *testing.T, l *workspace.Layout, records ...Run) {
	t.Helper()
	if err := workspace.EnsureDir(l.StateDir()); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, r := range records {
		b.WriteString(encodeRun(t, r))
		b.WriteString("\n")
	}
	if err := os.WriteFile(l.RunsJSONL(), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(id, gate, status string, exit int, at string) Run {
	when, err := time.Parse(time.RFC3339, at)
	if err != nil {
		panic(err)
	}
	return NewRun(id, gate, []string{"go", "test"}, GateStatus(status), &exit, []byte(status), when)
}

func TestLoadRunsTailOnAMissingLog(t *testing.T) {
	l := newLayout(t)
	runs, truncated, err := LoadRunsTail(l, 0)
	if err != nil {
		t.Fatalf("a workspace that has never been verified is not an error: %v", err)
	}
	if truncated || len(runs) != 0 {
		t.Errorf("runs = %v, truncated = %t; want empty and not truncated", runs, truncated)
	}
}

func TestLoadRunsTailReadsEverythingWhenItFits(t *testing.T) {
	l := newLayout(t)
	want := []Run{
		run("1", "test", "pass", 0, "2026-09-25T10:00:00Z"),
		run("2", "test", "fail", 1, "2026-09-25T10:05:00Z"),
	}
	writeRuns(t, l, want...)

	runs, truncated, err := LoadRunsTail(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated {
		t.Error("truncated = true on a log well under the limit")
	}
	if len(runs) != 2 {
		t.Fatalf("read %d records, want 2", len(runs))
	}
	if runs[1].TaskID != "2" || runs[1].Status != GateFail {
		t.Errorf("second record = %+v, want task 2 fail", runs[1])
	}
}

// The window starts at a line boundary. A record whose first bytes were cut off
// would otherwise parse into a Run with an empty task and gate, and that phantom
// is indistinguishable from a real record in the stuck comparison.
func TestLoadRunsTailNeverReturnsAPartialRecord(t *testing.T) {
	l := newLayout(t)
	if err := workspace.EnsureDir(l.StateDir()); err != nil {
		t.Fatal(err)
	}
	// One huge record followed by small ones, so a naive tail read lands inside
	// the first of them.
	lines := []string{
		recordJSON(t, "early", "test", strings.Repeat("x", 4096)),
		recordJSON(t, "middle", "test", "m"),
		recordJSON(t, "late", "test", "l"),
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(l.RunsJSONL(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, truncated, err := LoadRunsTail(l, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("truncated = false with a 4 KiB record and a 200-byte window")
	}
	for _, r := range runs {
		if r.TaskID == "" || r.Gate == "" {
			t.Errorf("a record with no task or gate came out of the window: %+v", r)
		}
	}
	// The last record is the last record, whichever end of the file it is on.
	if len(runs) == 0 || runs[len(runs)-1].TaskID != "late" {
		t.Errorf("last record = %+v, want the one stamped late", runs[len(runs)-1:])
	}
}

// A single record larger than the whole window yields nothing rather than a
// fragment. Reporting a half-read record is worse than reporting none.
func TestLoadRunsTailWithOneRecordLargerThanTheWindow(t *testing.T) {
	l := newLayout(t)
	if err := workspace.EnsureDir(l.StateDir()); err != nil {
		t.Fatal(err)
	}
	body := recordJSON(t, "huge", "test", strings.Repeat("y", 8192)) + "\n"
	if err := os.WriteFile(l.RunsJSONL(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	runs, truncated, err := LoadRunsTail(l, 64)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Error("truncated = false when everything was dropped")
	}
	if len(runs) != 0 {
		t.Errorf("runs = %+v, want none: there was no whole record in the window", runs)
	}
}

// Truncating the log must not change what stuck detection concludes. The
// trailing run of identical hashes is at the end of the file, which is exactly
// the part a tail read keeps, so as long as the window holds StuckThreshold
// records the verdict is identical.
func TestStuckDetectionSurvivesTruncation(t *testing.T) {
	l := newLayout(t)
	var records []Run
	for i := range 10 {
		records = append(records, run("1", "test", "fail", 1, fmt.Sprintf("2026-09-25T10:00:%02dZ", i)))
	}
	writeRuns(t, l, records...)

	full, truncated, err := LoadRunsTail(l, 0)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(full) != 10 {
		t.Fatalf("full read = %d records, truncated = %t", len(full), truncated)
	}
	if got := StuckTasks(full); !got["1"] {
		t.Fatal("ten identical failures are not stuck")
	}

	// A window wide enough to hold the trailing run of identical attempts and
	// narrow enough to drop the earlier records.
	const window = 1024
	tail, truncated, err := LoadRunsTail(l, window)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatalf("truncated = false with a %d-byte window over 10 records of about %d bytes each",
			window, len(encodeRun(t, records[0])))
	}
	if len(tail) < StuckThreshold {
		t.Fatalf("the window kept only %d records, so this case is not reachable at %d bytes; "+
			"widen the window rather than letting the assertion skip", len(tail), window)
	}
	if len(tail) >= len(full) {
		t.Fatalf("the window kept %d of %d records, so nothing was actually truncated", len(tail), len(full))
	}
	if got := StuckTasks(tail); !got["1"] {
		t.Error("stuck detection changed even though the window held the trailing run")
	}
}

// The limit on truncation: a window that cannot hold StuckThreshold records
// cannot reach a stuck verdict, because the verdict is a comparison of
// consecutive attempts. This is a real limitation, documented rather than
// papered over, and it is why the default is a megabyte rather than kilobytes —
// a record is at most MaxOutputBytes plus a line of fields, so the default
// always holds hundreds.
func TestTruncationBelowStuckThresholdCannotReachAVerdict(t *testing.T) {
	l := newLayout(t)
	var records []Run
	for i := range 10 {
		records = append(records, run("1", "test", "fail", 1, fmt.Sprintf("2026-09-25T10:00:%02dZ", i)))
	}
	writeRuns(t, l, records...)

	tail, truncated, err := LoadRunsTail(l, 90)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("truncated = false")
	}
	if len(tail) >= StuckThreshold {
		t.Skipf("the window held %d records, which is enough; the limitation is not reachable at this size", len(tail))
	}
	if got := StuckTasks(tail); got["1"] {
		t.Error("stuck was reported from fewer than StuckThreshold records")
	}
	// The default limit holds far more than enough.
	maxRecord := MaxOutputBytes + 1024
	if MaxHistoryBytes/maxRecord < StuckThreshold*10 {
		t.Errorf("the default limit holds %d maximum-size records; that is too few to detect a stuck run comfortably",
			MaxHistoryBytes/maxRecord)
	}
}

func TestLoadRunsTailReportsAMalformedLine(t *testing.T) {
	l := newLayout(t)
	if err := workspace.EnsureDir(l.StateDir()); err != nil {
		t.Fatal(err)
	}
	good := encodeRun(t, run("1", "test", "pass", 0, "2026-09-25T10:00:00Z"))
	if err := os.WriteFile(l.RunsJSONL(), []byte(good+"\nnot json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadRunsTail(l, 0); err == nil {
		t.Fatal("a malformed line was accepted; dropping it would erase an attempt from the only record of what was tried")
	}
}

func TestLoadRunsTailIgnoresAZeroLimit(t *testing.T) {
	l := newLayout(t)
	writeRuns(t, l, run("1", "test", "pass", 0, "2026-09-25T10:00:00Z"))
	// A negative limit would seek backwards and fail; zero means the default.
	runs, truncated, err := LoadRunsTail(l, -1)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || len(runs) != 1 {
		t.Errorf("runs = %v, truncated = %t; want the default limit applied", runs, truncated)
	}
}

func TestMaxHistoryBytesIsBounded(t *testing.T) {
	// The whole point is that the number is finite. A regression here would be a
	// number large enough to be a typo that looks like a deliberate budget.
	if MaxHistoryBytes <= 0 || MaxHistoryBytes > 64<<20 {
		t.Errorf("MaxHistoryBytes = %d, want a positive value under 64 MiB", MaxHistoryBytes)
	}
}

func recordJSON(t *testing.T, task, gate, output string) string {
	t.Helper()
	one := 1
	r := NewRun(task, gate, []string{"go", "test"}, GateFail, &one, []byte(output), time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	return encodeRun(t, r)
}
