package state

import (
	"strings"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

var testNow = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

func TestParseStatusRejectsAnythingOutsideTheVocabulary(t *testing.T) {
	for _, s := range Statuses {
		got, err := ParseStatus(string(s))
		if err != nil {
			t.Errorf("ParseStatus(%q) = %v, want it accepted", s, err)
		}
		if got != s {
			t.Errorf("ParseStatus(%q) = %q", s, got)
		}
	}
	for _, bad := range []string{"", "in-progress", "DONE", "done ", "complete"} {
		_, err := ParseStatus(bad)
		// A value from the command line is a usage error, not a broken state
		// file: telling an agent it misspelled a flag is a different recovery.
		if got := codeOf(t, err); got != string(envelope.CodeUsage) {
			t.Errorf("ParseStatus(%q) code = %s, want %s", bad, got, envelope.CodeUsage)
		}
	}
}

// AddTask is upsert by id, so re-issuing the same add after a context loss is
// safe. A partial add must not erase fields the caller left out.
func TestAddTaskIsUpsertByID(t *testing.T) {
	s := New()
	if err := s.AddTask(Task{ID: "1", Title: "scaffold", Agent: "coder", Notes: "keep me"}, testNow); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := s.AddTask(Task{ID: "1", Title: "scaffold, renamed"}, testNow); err != nil {
		t.Fatalf("AddTask (second): %v", err)
	}
	if len(s.Tasks) != 1 {
		t.Fatalf("got %d tasks, want 1: add must upsert, not duplicate", len(s.Tasks))
	}
	task, _ := s.Find("1")
	if task.Title != "scaffold, renamed" {
		t.Errorf("title = %q, want the new value", task.Title)
	}
	if task.Agent != "coder" || task.Notes != "keep me" {
		t.Errorf("a partial add erased fields: agent=%q notes=%q", task.Agent, task.Notes)
	}
	if task.Status != StatusPending {
		t.Errorf("status = %q, want pending by default", task.Status)
	}
	if task.Updated != "2026-09-25T12:00:00Z" {
		t.Errorf("updated = %q, want the mutation time", task.Updated)
	}
}

// A dep argument is the task's full dep set, not a merge: a caller cannot see
// the current one, and a merge would make the command's effect depend on what
// happened to be there.
func TestAddTaskDepsAreAbsoluteAndDeduplicated(t *testing.T) {
	s := New()
	for _, id := range []string{"a", "b"} {
		if err := s.AddTask(Task{ID: id}, testNow); err != nil {
			t.Fatalf("AddTask %s: %v", id, err)
		}
	}
	if err := s.AddTask(Task{ID: "1", Deps: []string{"a", "b", "a"}}, testNow); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	task, _ := s.Find("1")
	if len(task.Deps) != 2 || task.Deps[0] != "a" || task.Deps[1] != "b" {
		t.Errorf("deps = %v, want [a b] with the duplicate dropped", task.Deps)
	}

	if err := s.AddTask(Task{ID: "1", Deps: []string{"b"}}, testNow); err != nil {
		t.Fatalf("AddTask with new deps: %v", err)
	}
	task, _ = s.Find("1")
	if len(task.Deps) != 1 || task.Deps[0] != "b" {
		t.Errorf("deps = %v, want [b]: the argument is the whole set, not a merge", task.Deps)
	}
}

func TestAddTaskRefusesADanglingDep(t *testing.T) {
	s := New()
	err := s.AddTask(Task{ID: "1", Deps: []string{"ghost"}}, testNow)
	if got := codeOf(t, err); got != string(envelope.CodeDepDangling) {
		t.Errorf("code = %s, want %s", got, envelope.CodeDepDangling)
	}
	if _, found := s.Find("1"); found {
		t.Error("a refused add must not leave the task in the state")
	}
}

func TestAddTaskRefusesAnEmptyID(t *testing.T) {
	s := New()
	if got := codeOf(t, s.AddTask(Task{Title: "no id"}, testNow)); got != string(envelope.CodeUsage) {
		t.Errorf("code = %s, want %s", got, envelope.CodeUsage)
	}
}

// done -> in_progress reopens work and invalidates the gates that were its
// evidence, so it needs --yes and is never accidental.
func TestDoneToInProgressNeedsYes(t *testing.T) {
	s := New()
	if err := s.AddTask(Task{ID: "1", Status: StatusDone}, testNow); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	err := s.SetStatus("1", StatusInProgress, false, testNow)
	if got := codeOf(t, err); got != string(envelope.CodeInvalidTransition) {
		t.Errorf("code = %s, want %s", got, envelope.CodeInvalidTransition)
	}
	if err.Detail.From != "done" || err.Detail.To != "in_progress" {
		t.Errorf("detail = %+v, want from=done to=in_progress", err.Detail)
	}
	if !strings.Contains(err.Err.Hint, "--yes") {
		t.Errorf("hint = %q, want it to name --yes", err.Err.Hint)
	}
	task, _ := s.Find("1")
	if task.Status != StatusDone {
		t.Errorf("status = %q, want the refusal to leave it alone", task.Status)
	}

	if err := s.SetStatus("1", StatusInProgress, true, testNow); err != nil {
		t.Fatalf("SetStatus with --yes: %v", err)
	}
	task, _ = s.Find("1")
	if task.Status != StatusInProgress {
		t.Errorf("status = %q, want in_progress after --yes", task.Status)
	}
}

// cancelled -> done is rejected outright: cancelled means the author decided the
// work will not happen, and marking it done fabricates an outcome.
func TestCancelledToDoneIsRejected(t *testing.T) {
	s := New()
	if err := s.AddTask(Task{ID: "1", Status: StatusCancelled}, testNow); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	err := s.SetStatus("1", StatusDone, true, testNow)
	if got := codeOf(t, err); got != string(envelope.CodeInvalidTransition) {
		t.Errorf("code = %s, want %s", got, envelope.CodeInvalidTransition)
	}
	if !strings.Contains(err.Err.Message, "cancelled") {
		t.Errorf("message = %q, want it to name the cancelled state", err.Err.Message)
	}
	task, _ := s.Find("1")
	if task.Status != StatusCancelled {
		t.Errorf("status = %q, want the refusal to leave it cancelled", task.Status)
	}
}

// Setting the status a task already has is a no-op, because every command is
// idempotent by contract (§4.4).
func TestSetStatusToTheSameValueIsANoOp(t *testing.T) {
	s := New()
	if err := s.AddTask(Task{ID: "1", Status: StatusPending}, testNow); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if err := s.SetStatus("1", StatusPending, false, testNow); err != nil {
		t.Fatalf("SetStatus to the same value: %v", err)
	}
}

// The transition into in_progress carries the blocking dep ids in the error
// detail, which is what a command copies into envelope.data. The point is that
// an agent never has to parse the message to find out what to finish first.
func TestInProgressWithUnmetDepsReportsBlockingIDs(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c")
	err := s.SetStatus("2", StatusInProgress, false, testNow)
	if got := codeOf(t, err); got != string(envelope.CodeDepsUnmet) {
		t.Errorf("code = %s, want %s", got, envelope.CodeDepsUnmet)
	}
	if len(err.Detail.Blocking) != 1 || err.Detail.Blocking[0] != "1" {
		t.Errorf("detail.blocking = %v, want [1]", err.Detail.Blocking)
	}
	if got := err.Data(); got == nil {
		t.Fatal("Data() is nil, but a deps_unmet failure must carry the blocking ids")
	}
	if err.Exit() != 5 {
		t.Errorf("Exit = %d, want 5 (invariant)", err.Exit())
	}
}

// Pausing a task another task is working on would leave that dependent in
// progress with an unmet dep, which invariant 4 then refuses on the next load.
// Cancelling is the honest move, because a cancelled dep satisfies its
// dependents by design.
//
// The reachable path to the bad state is a reopen: 1 is done, 2 started on it,
// and 1 is moved back to in_progress. --yes acknowledges that reopening
// invalidates 1's gates; it is not permission to strand 2.
func TestParkingATaskWithWorkInFlightIsRefused(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c")
	if err := s.SetStatus("1", StatusDone, false, testNow); err != nil {
		t.Fatalf("finish 1: %v", err)
	}
	if err := s.SetStatus("2", StatusInProgress, false, testNow); err != nil {
		t.Fatalf("start 2: %v", err)
	}
	// Reopen 1: 2 is mid-flight, so 1 must not be parked again.
	if err := s.SetStatus("1", StatusInProgress, true, testNow); err != nil {
		t.Fatalf("reopen 1: %v", err)
	}
	err := s.SetStatus("1", StatusPending, false, testNow)
	if got := codeOf(t, err); got != string(envelope.CodeInvalidTransition) {
		t.Errorf("code = %s, want %s", got, envelope.CodeInvalidTransition)
	}
	if len(err.Detail.Blocking) != 1 || err.Detail.Blocking[0] != "2" {
		t.Errorf("detail.blocking = %v, want the in-flight dependent [2]", err.Detail.Blocking)
	}
	// Cancelling is allowed, and then invariant 4 is satisfied again.
	if err := s.SetStatus("1", StatusCancelled, false, testNow); err != nil {
		t.Fatalf("cancelling a task with work in flight: %v", err)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("a cancelled dep must satisfy its dependents, got %v", err)
	}
}

// The corollary is checked in the state too, so a hand-edited file with the same
// shape is reported rather than refused with a message about the wrong task.
func TestInvariantFlagsAParkedDepWithWorkInFlight(t *testing.T) {
	s := New()
	chain(s, "a", "b")
	s.Tasks[0].Status = StatusDone
	s.Tasks[1].Status = StatusInProgress
	if err := s.checkDependents(); err != nil {
		t.Fatalf("a done dep with an in-flight dependent is valid, got %v", err)
	}
	s.Tasks[0].Status = StatusPending
	err := s.checkDependents()
	if got := codeOf(t, err); got != string(envelope.CodeDepsUnmet) {
		t.Errorf("code = %s, want %s", got, envelope.CodeDepsUnmet)
	}
	if err.Detail.Task != "2" {
		t.Errorf("detail.task = %q, want the in-flight task, not the parked dep", err.Detail.Task)
	}
}

func TestSetStatusOnAnUnknownTask(t *testing.T) {
	s := New()
	err := s.SetStatus("ghost", StatusDone, false, testNow)
	if got := codeOf(t, err); got != string(envelope.CodeTaskNotFound) {
		t.Errorf("code = %s, want %s", got, envelope.CodeTaskNotFound)
	}
	if err.Exit() != 7 {
		t.Errorf("Exit = %d, want 7 (not_found)", err.Exit())
	}
}

func TestAddDepsRefusesWhatWouldDangleOrCycle(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c") // 1:[], 2:[1], 3:[2]

	if got := codeOf(t, s.AddDeps("1", []string{"ghost"}, testNow)); got != string(envelope.CodeDepDangling) {
		t.Errorf("dangling code = %s, want %s", got, envelope.CodeDepDangling)
	}
	err := s.AddDeps("1", []string{"3"}, testNow)
	if got := codeOf(t, err); got != string(envelope.CodeDepCycle) {
		t.Errorf("cycle code = %s, want %s", got, envelope.CodeDepCycle)
	}
	if len(err.Detail.Cycle) == 0 {
		t.Error("a refused cycle must carry the path in data")
	}

	// The refused dep must not have landed.
	task, _ := s.Find("1")
	if len(task.Deps) != 0 {
		t.Errorf("deps = %v, want the refusal to leave the state alone", task.Deps)
	}

	if err := s.AddDeps("1", nil, testNow); codeOf(t, err) != string(envelope.CodeUsage) {
		t.Error("adding no deps must be a usage error")
	}
}

func TestRemoveDepsIsIdempotent(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c")
	if err := s.RemoveDeps("2", []string{"absent"}, testNow); err != nil {
		t.Errorf("removing a dep that is not there must be a no-op, got %v", err)
	}
	if err := s.RemoveDeps("2", []string{"1"}, testNow); err != nil {
		t.Fatalf("RemoveDeps: %v", err)
	}
	task, _ := s.Find("2")
	if len(task.Deps) != 0 {
		t.Errorf("deps = %v, want none", task.Deps)
	}
	// Now 3 depends on a pending 2, and 2 has no deps, so the graph is still valid.
	if err := s.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// Deleting a task others depend on would turn those deps dangling and leave the
// workspace unloadable, so it is refused. There is no force in v1: the way out
// is to cut the dependents' edges first.
func TestRemoveTaskRefusesWhenOthersDependOnIt(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c")

	if got := codeOf(t, s.RemoveTask([]string{"1"})); got != string(envelope.CodeDepDangling) {
		t.Errorf("code = %s, want %s", got, envelope.CodeDepDangling)
	}
	if len(s.Dependents("1")) == 0 {
		t.Fatal("the refusal removed the task anyway")
	}

	if got := codeOf(t, s.RemoveTask([]string{"ghost"})); got != string(envelope.CodeTaskNotFound) {
		t.Errorf("unknown id code = %s, want %s", got, envelope.CodeTaskNotFound)
	}
	if got := codeOf(t, s.RemoveTask(nil)); got != string(envelope.CodeUsage) {
		t.Errorf("empty code = %s, want %s", got, envelope.CodeUsage)
	}

	if err := s.RemoveTask([]string{"3", "2", "1"}); err != nil {
		t.Fatalf("removing a whole chain from the leaves: %v", err)
	}
	if len(s.Tasks) != 0 {
		t.Errorf("got %d tasks, want none", len(s.Tasks))
	}
}

// Ready is the work an agent may start. A blocked task with its deps satisfied
// is open but is not offered: blocked is a deliberate mark, and handing it to
// `task next` anyway would push an agent into the exact thing someone flagged.
func TestReadyExcludesBlockedAndFinished(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c", "d") // 1:[], 2:[1], 3:[2], 4:[2]
	s.Tasks[3].Deps = []string{} // 4 is now independently ready
	s.Tasks[3].Status = StatusBlocked

	ready := s.Ready()
	if len(ready) != 2 {
		t.Fatalf("got %d ready tasks, want 2 (%v)", len(ready), ids(ready))
	}
	if ready[0].ID != "1" || ready[1].ID != "4" {
		t.Errorf("ready = %v, want 1 and 4", ids(ready))
	}
	if s.Next() == nil || s.Next().ID != "1" {
		t.Errorf("Next = %v, want 1: a blocked task is never offered as next work", s.Next())
	}
}

// Priority is stored order, so a later pending task is chosen over an earlier
// blocked one, and an earlier pending task over a later one.
func TestNextIsStoredOrderAndPendingOnly(t *testing.T) {
	s := New()
	s.Tasks = []Task{
		{ID: "1", Title: "parked", Deps: []string{}, Status: StatusBlocked},
		{ID: "2", Title: "ready", Deps: []string{}, Status: StatusPending},
		{ID: "3", Title: "waits", Deps: []string{"2"}, Status: StatusPending},
	}
	next := s.Next()
	if next == nil || next.ID != "2" {
		t.Fatalf("Next = %v, want task 2", next)
	}
	if err := s.SetStatus("2", StatusDone, false, testNow); err != nil {
		t.Fatalf("finish 2: %v", err)
	}
	if next := s.Next(); next == nil || next.ID != "3" {
		t.Errorf("Next = %v, want task 3 once 2 is done", next)
	}

	// An empty queue is a valid state, not an error (SPEC §7).
	empty := New()
	if got := empty.Next(); got != nil {
		t.Errorf("Next on an empty state = %v, want nil", got)
	}
	for i := range s.Tasks {
		s.Tasks[i].Status = StatusDone
	}
	if got := s.Next(); got != nil {
		t.Errorf("Next with everything done = %v, want nil", got)
	}
}

func TestBlockedReportsTheHoldingDeps(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c")
	got := s.Blocked()
	if len(got) != 2 {
		t.Fatalf("Blocked = %v, want tasks 2 and 3", got)
	}
	if len(got["2"]) != 1 || got["2"][0] != "1" {
		t.Errorf("Blocked[2] = %v, want [1]", got["2"])
	}
	s.Tasks[0].Status = StatusDone
	if _, still := s.Blocked()["2"]; still {
		t.Error("task 2 must be unblocked once 1 is done")
	}
}

// A re-detected verify command must not look like a fresh failure, and a changed
// command must not inherit the old command's evidence.
func TestSetGateKeepsTheOutcomeWhenTheCommandIsUnchanged(t *testing.T) {
	task := Task{ID: "1"}
	exit := 1
	ran := "2026-09-25T11:00:00Z"
	task.SetGate(Gate{Name: "test", Cmd: "go test ./...", Status: GateFail, LastExit: &exit, Attempts: 2, LastRun: &ran})

	task.SetGate(Gate{Name: "test", Cmd: "go test ./..."})
	got, _ := task.Gate("test")
	if got.Status != GateFail || got.LastExit == nil || *got.LastExit != 1 || got.Attempts != 2 || got.LastRun == nil {
		t.Errorf("outcome was lost on a same-command reinstall: %+v", got)
	}

	// A different command is a different gate's worth of evidence.
	task.SetGate(Gate{Name: "test", Cmd: "go test -race ./..."})
	got, _ = task.Gate("test")
	if got.Status != GatePending || got.LastExit != nil || got.Attempts != 0 || got.LastRun != nil {
		t.Errorf("a changed command must reset the outcome, got %+v", got)
	}
}

// verify records an outcome by mutating the gate it gets back, not through
// SetGate, so a fresh result is never mistaken for a definition change.
func TestGateMutationDoesNotGoThroughSetGate(t *testing.T) {
	task := Task{ID: "1"}
	task.SetGate(Gate{Name: "test", Cmd: "go test ./..."})
	gate, ok := task.Gate("test")
	if !ok {
		t.Fatal("Gate(test) did not find the gate")
	}
	exit := 0
	ran := "2026-09-25T11:30:00Z"
	gate.Status = GatePass
	gate.LastExit = &exit
	gate.Attempts = 1
	gate.LastRun = &ran

	task.SetGate(Gate{Name: "test", Cmd: "go test ./..."})
	got, _ := task.Gate("test")
	if got.Status != GatePass || got.Attempts != 1 {
		t.Errorf("a recorded pass was lost to a definition reinstall: %+v", got)
	}
}

func TestSetGateDefaultsAnEmptyStatusToPending(t *testing.T) {
	task := Task{ID: "1"}
	task.SetGate(Gate{Name: "test", Cmd: "go test ./..."})
	got, _ := task.Gate("test")
	if got.Status != GatePending {
		t.Errorf("status = %q, want pending: a gate that has never run is pending", got.Status)
	}
}

func TestSetAcceptanceMergesByID(t *testing.T) {
	s := New()
	if err := s.SetAcceptance([]Acceptance{{ID: "a1", Text: "one"}, {ID: "a2", Text: "two"}}); err != nil {
		t.Fatalf("SetAcceptance: %v", err)
	}
	if err := s.SetAcceptance([]Acceptance{{ID: "a1", Text: "one, revised", Done: true}}); err != nil {
		t.Fatalf("SetAcceptance (update): %v", err)
	}
	if len(s.Workflow.Acceptance) != 2 {
		t.Fatalf("got %d items, want 2: an update must not append", len(s.Workflow.Acceptance))
	}
	if s.Workflow.Acceptance[0].Text != "one, revised" || !s.Workflow.Acceptance[0].Done {
		t.Errorf("item a1 = %+v, want the revised, ticked version", s.Workflow.Acceptance[0])
	}
	if err := s.SetAcceptance([]Acceptance{{Text: "no id"}}); codeOf(t, err) != string(envelope.CodeUsage) {
		t.Error("an acceptance item with no id must be a usage error")
	}
}

func TestSetAcceptanceDone(t *testing.T) {
	s := New()
	if err := s.SetAcceptance([]Acceptance{{ID: "a1", Text: "one"}}); err != nil {
		t.Fatalf("SetAcceptance: %v", err)
	}
	if err := s.SetAcceptanceDone("a1", true); err != nil {
		t.Fatalf("SetAcceptanceDone: %v", err)
	}
	if !s.Workflow.Acceptance[0].Done {
		t.Error("a1 is not ticked")
	}
	if err := s.SetAcceptanceDone("a1", false); err != nil {
		t.Fatalf("unticking must work too, got %v", err)
	}
	if s.Workflow.Acceptance[0].Done {
		t.Error("a1 is still ticked")
	}
	if got := codeOf(t, s.SetAcceptanceDone("ghost", true)); got != string(envelope.CodeEntryNotFound) {
		t.Errorf("code = %s, want %s", got, envelope.CodeEntryNotFound)
	}
}

func ids(tasks []*Task) []string {
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, task.ID)
	}
	return out
}
