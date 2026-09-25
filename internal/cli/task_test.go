package cli_test

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
)

const taskCommandName = "task"

// taskData mirrors the cli payload so the tests read fields rather than
// substrings. It is a test-local copy on purpose: a test that reuses the
// implementation's own struct cannot catch the implementation changing it.
type taskData struct {
	Subcommand string              `json:"subcommand"`
	Task       *state.TaskView     `json:"task"`
	Tasks      []state.TaskView    `json:"tasks"`
	Removed    []string            `json:"removed"`
	Counts     map[string]int      `json:"counts"`
	Ready      []string            `json:"ready"`
	Blocked    map[string][]string `json:"blocked"`
	Created    bool                `json:"created"`
	DryRun     bool                `json:"dry_run"`
}

func taskRun(t *testing.T, dir string, args ...string) (int, envelope.Envelope, taskData) {
	t.Helper()
	var out, errOut bytes.Buffer
	full := append([]string{"--dir", dir, taskCommandName}, args...)
	code := cli.Main(full, &out, &errOut, false)
	raw := bytes.TrimSpace(out.Bytes())
	var env envelope.Envelope
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("task %v: stdout is not a JSON envelope: %v\n%s", args, err, raw)
		}
	}
	if env.Command != taskCommandName {
		t.Fatalf("command = %q, want %q", env.Command, taskCommandName)
	}
	return code, env, decodeTask(t, env)
}

// decodeTask pulls the payload out of the envelope. Unmarshalling the envelope
// itself into this struct would silently produce an all-zero value, because the
// payload is nested under "data" — and a zero value here fails every assertion
// in a way that looks like a bug in the command.
func decodeTask(t *testing.T, env envelope.Envelope) taskData {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var p taskData
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("task payload does not match the documented shape: %v\n%s", err, raw)
	}
	return p
}

func mustTask(t *testing.T, dir string, args ...string) taskData {
	t.Helper()
	code, env, data := taskRun(t, dir, args...)
	if code != 0 {
		t.Fatalf("ocaw task %v: exit %d: %+v", args, code, env.Err)
	}
	return data
}

func taskErr(t *testing.T, dir string, args ...string) (int, envelope.Envelope) {
	t.Helper()
	code, env, _ := taskRun(t, dir, args...)
	if env.Err == nil {
		t.Fatalf("ocaw task %v: expected an error, got ok:true (exit %d)", args, code)
	}
	return code, env
}

func chain(t *testing.T, dir string) {
	t.Helper()
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1")
	mustTask(t, dir, "add", "--id", "3", "--title", "third", "--dep", "2")
}

func initialisedTaskRepo(t *testing.T) string {
	t.Helper()
	dir := gitRepo(t, map[string]string{"go.mod": "module example.com/x\n\ngo 1.22\n"})
	if code, env, _ := initEnvelope(t, dir); code != 0 {
		t.Fatalf("init: exit %d, %+v", code, env.Err)
	}
	return dir
}

// AC4 — tasks 1←2←3, next returns 1; after `set 1 --status done`, next
// returns 2.
func TestAC4NextFollowsTheChainAndAdvances(t *testing.T) {
	dir := initialisedTaskRepo(t)
	chain(t, dir)

	data := mustTask(t, dir, "next")
	if data.Task == nil {
		t.Fatal("next returned no task on an empty chain")
	}
	if data.Task.ID != "1" {
		t.Errorf("next = %q, want 1", data.Task.ID)
	}
	if data.Task.Status != state.StatusPending {
		t.Errorf("next status = %q, want pending", data.Task.Status)
	}

	mustTask(t, dir, "set", "1", "--status", "done")
	data = mustTask(t, dir, "next")
	if data.Task == nil || data.Task.ID != "2" {
		t.Fatalf("after 1 is done, next = %v, want 2", data.Task)
	}

	mustTask(t, dir, "set", "2", "--status", "done")
	data = mustTask(t, dir, "next")
	if data.Task == nil || data.Task.ID != "3" {
		t.Fatalf("after 2 is done, next = %v, want 3", data.Task)
	}
}

// AC5 — `set 3 --status in_progress` while dep 2 is pending exits 5 with
// deps_unmet.
func TestAC5CannotStartATaskAheadOfItsDeps(t *testing.T) {
	dir := initialisedTaskRepo(t)
	chain(t, dir)

	code, env := taskErr(t, dir, "set", "3", "--status", "in_progress")
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (invariant); error = %+v", code, env.Err)
	}
	if env.Err.Code != envelope.CodeDepsUnmet {
		t.Errorf("code = %q, want deps_unmet", env.Err.Code)
	}
	// The blocking deps must be readable as data, not only as message text.
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, `"2"`) {
		t.Errorf("data does not name the blocking dep: %s", text)
	}
	// And the refusal left the task where it was.
	data := mustTask(t, dir, "show", "3")
	if data.Task.Status != state.StatusPending {
		t.Errorf("a refused transition changed the status to %q", data.Task.Status)
	}
}

// The same refusal must hold through `add`, which accepts --status.
func TestAC5AlsoHoldsForAdd(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")

	code, env := taskErr(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1", "--status", "in_progress")
	if code != 5 {
		t.Fatalf("exit = %d, want 5; error = %+v", code, env.Err)
	}
	if env.Err.Code != envelope.CodeDepsUnmet {
		t.Errorf("code = %q, want deps_unmet", env.Err.Code)
	}
	// The whole add was refused, not just the status.
	if code, _, _ := taskRun(t, dir, "show", "2"); code != 7 {
		t.Error("the refused add still created the task")
	}
}

// AC6 — a dep cycle exits 5 with dep_cycle and names the cycle path.
func TestAC6ADepCycleIsRefusedAndNamed(t *testing.T) {
	dir := initialisedTaskRepo(t)
	// Built in an order that never dangles: a dep must already be a task, so
	// the chain is assembled from the far end backwards.
	for _, id := range []string{"1", "2", "3", "4"} {
		mustTask(t, dir, "add", "--id", id, "--title", "task "+id)
	}
	mustTask(t, dir, "dep", "3", "--add", "2")
	mustTask(t, dir, "dep", "2", "--add", "1")
	mustTask(t, dir, "dep", "4", "--add", "3")

	code, env := taskErr(t, dir, "dep", "1", "--add", "4")
	if code != 5 {
		t.Fatalf("exit = %d, want 5 (invariant); error = %+v", code, env.Err)
	}
	if env.Err.Code != envelope.CodeDepCycle {
		t.Fatalf("code = %q, want dep_cycle", env.Err.Code)
	}
	// The path is the part an agent needs. Without it, the only way to find
	// the loop is to re-add every edge by hand.
	if !strings.Contains(env.Err.Message, "1 ->") || strings.Count(env.Err.Message, "->") < 2 {
		t.Errorf("message = %q, want the full cycle path", env.Err.Message)
	}
	raw, _ := json.Marshal(env.Data)
	if !strings.Contains(string(raw), "cycle") {
		t.Errorf("data does not carry the cycle: %s", raw)
	}
	// A refused edge is not recorded.
	data := mustTask(t, dir, "show", "1")
	if len(data.Task.Deps) != 0 {
		t.Errorf("deps = %v, want the refused edge to be absent", data.Task.Deps)
	}
}

// A cycle created entirely by `add` is refused the same way.
// A task may not depend on itself, and a diamond is not a cycle.
func TestSelfDependencyAndDiamond(t *testing.T) {
	dir := initialisedTaskRepo(t)
	code, env := taskErr(t, dir, "add", "--id", "1", "--title", "first", "--dep", "1")
	if code != 5 {
		t.Errorf("self dep: exit = %d, want 5; error = %+v", code, env.Err)
	}

	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1")
	mustTask(t, dir, "add", "--id", "3", "--title", "third", "--dep", "1")
	// 4 depends on 2 and 3, which both depend on 1. That is a diamond, not a loop.
	data := mustTask(t, dir, "add", "--id", "4", "--title", "fourth", "--dep", "2", "--dep", "3")
	if len(data.Task.Deps) != 2 {
		t.Errorf("deps = %v, want two", data.Task.Deps)
	}
}

// AC11 — --dry-run on every mutating subcommand leaves the tree clean.
func TestAC11DryRunWritesNothing(t *testing.T) {
	dir := initialisedTaskRepo(t)
	chain(t, dir)
	commitAll(t, dir)

	// Every mutation below is one that would otherwise succeed, so the test is
	// about the dry run and not about the refusal path. Deps cannot dangle, so
	// the ids a dep points at exist before the dry run that adds the edge.
	mustTask(t, dir, "add", "--id", "4", "--title", "fourth")
	mustTask(t, dir, "add", "--id", "5", "--title", "fifth")
	mustTask(t, dir, "dep", "1", "--add", "4")
	commitAll(t, dir)

	cases := [][]string{
		{"add", "--id", "6", "--title", "sixth"},
		{"set", "1", "--status", "done"},
		{"set", "2", "--title", "renamed"},
		{"dep", "1", "--add", "5"},
		{"dep", "1", "--rm", "4"},
		{"rm", "3"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args[:2], " "), func(t *testing.T) {
			code, env, data := taskRun(t, dir, append(args, "--dry-run")...)
			if code != 0 {
				t.Fatalf("exit = %d: %+v", code, env.Err)
			}
			if !data.DryRun {
				t.Error("data.dry_run = false on a --dry-run")
			}
			if dirty := porcelain(t, dir); dirty != "" {
				t.Errorf("the dry run changed files:\n%s", dirty)
			}
		})
	}

	// The same operations for real must actually change something, or the test
	// above is only proving that the command does nothing at all.
	code, env, data := taskRun(t, dir, "add", "--id", "6", "--title", "sixth")
	if code != 0 {
		t.Fatalf("real add: exit %d: %+v", code, env.Err)
	}
	if !data.Created {
		t.Error("data.created = false on a real add")
	}
	if dirty := porcelain(t, dir); dirty == "" {
		t.Error("the real add changed nothing, so the dry-run test proves nothing")
	}
}

// `add` with an existing id is an upsert, never a duplicate.
func TestAddIsAnUpsert(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first", "--agent", "coder", "--tier", "foundation")

	before := mustTask(t, dir, "list")
	if len(before.Tasks) != 1 {
		t.Fatalf("one task, got %d", len(before.Tasks))
	}

	data := mustTask(t, dir, "add", "--id", "1", "--title", "renamed")
	if data.Created {
		t.Error("data.created = true for an existing id")
	}
	if data.Task.Title != "renamed" {
		t.Errorf("title = %q, want renamed", data.Task.Title)
	}
	// A field the second add did not mention is preserved, not blanked.
	if data.Task.Agent != "coder" || data.Task.Tier != "foundation" {
		t.Errorf("an upsert blanked fields it was not given: %+v", data.Task.Task)
	}

	after := mustTask(t, dir, "list")
	if len(after.Tasks) != 1 {
		t.Errorf("upsert created a duplicate: %d tasks", len(after.Tasks))
	}
}

// done → in_progress needs --yes; cancelled → done is refused always.
func TestTransitionGuards(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second")
	mustTask(t, dir, "add", "--id", "3", "--title", "third")

	mustTask(t, dir, "set", "1", "--status", "done")
	code, env := taskErr(t, dir, "set", "1", "--status", "in_progress")
	if code != 5 {
		t.Fatalf("done to in_progress without --yes: exit = %d, want 5", code)
	}
	if env.Err.Code != envelope.CodeInvalidTransition {
		t.Errorf("code = %q, want invalid_transition", env.Err.Code)
	}
	if !strings.Contains(env.Err.Hint, "--yes") {
		t.Errorf("hint = %q, want it to name --yes", env.Err.Hint)
	}
	// With --yes it is allowed, because re-opening work invalidates its gates.
	mustTask(t, dir, "set", "1", "--status", "in_progress", "--yes")

	mustTask(t, dir, "set", "2", "--status", "cancelled")
	code, env = taskErr(t, dir, "set", "2", "--status", "done")
	if code != 5 {
		t.Fatalf("cancelled to done: exit = %d, want 5", code)
	}
	if env.Err.Code != envelope.CodeInvalidTransition {
		t.Errorf("code = %q, want invalid_transition", env.Err.Code)
	}
	// Not even --yes brings it back.
	code, env = taskErr(t, dir, "set", "2", "--status", "done", "--yes")
	if code != 5 {
		t.Errorf("cancelled to done with --yes: exit = %d, want 5", code)
	}
}

// A task with work in flight may not be parked.
func TestATaskWithADependentsWorkInFlightCannotBeParked(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1")

	mustTask(t, dir, "set", "1", "--status", "done")
	mustTask(t, dir, "set", "2", "--status", "in_progress")

	code, env := taskErr(t, dir, "set", "1", "--status", "pending")
	if code != 5 {
		t.Fatalf("exit = %d, want 5", code)
	}
	if !strings.Contains(env.Err.Message, "2") {
		t.Errorf("message = %q, want it to name the dependent", env.Err.Message)
	}
}

// `rm` refuses a task something still depends on, and a chain goes in one call.
func TestRemoveRefusesADependedOnTask(t *testing.T) {
	dir := initialisedTaskRepo(t)
	chain(t, dir)

	code, env := taskErr(t, dir, "rm", "1")
	if code != 5 {
		t.Fatalf("exit = %d, want 5", code)
	}
	if !strings.Contains(env.Err.Message, "2") {
		t.Errorf("message = %q, want it to name the dependent", env.Err.Message)
	}

	// The whole chain at once, because ids in the same call do not count as
	// dependents of each other.
	data := mustTask(t, dir, "rm", "1", "2", "3")
	if len(data.Removed) != 3 {
		t.Errorf("removed = %v, want all three", data.Removed)
	}
	if left := mustTask(t, dir, "list"); len(left.Tasks) != 0 {
		t.Errorf("%d tasks left, want none", len(left.Tasks))
	}
}

// Removing an id that is not there is the state the caller asked for.
func TestRemoveOfAMissingTaskIsNotAnError(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	data := mustTask(t, dir, "rm", "1", "does-not-exist")
	if len(data.Removed) != 2 {
		t.Errorf("removed = %v, want both ids echoed", data.Removed)
	}
	if left := mustTask(t, dir, "list"); len(left.Tasks) != 0 {
		t.Errorf("%d tasks left, want none", len(left.Tasks))
	}
}

// An empty ready queue is ok:true with a null task, not a failure.
func TestNextOnAnEmptyQueueIsNotAnError(t *testing.T) {
	dir := initialisedTaskRepo(t)
	code, env, data := taskRun(t, dir, "next")
	if code != 0 {
		t.Fatalf("exit = %d, want 0: an empty queue is a valid state: %+v", code, env.Err)
	}
	if !env.OK {
		t.Error("ok = false with nothing to do")
	}
	if data.Task != nil {
		t.Errorf("task = %+v, want null", data.Task)
	}
	// The key must be present and null, not absent.
	raw, _ := json.Marshal(env.Data)
	if !strings.Contains(string(raw), `"task":null`) {
		t.Errorf("data = %s, want an explicit null task", raw)
	}
	// And the counts still travel, so a caller can tell "nothing to do" from
	// "nothing to do because everything is blocked".
	if data.Counts["pending"] != 0 {
		t.Errorf("counts = %v, want all zero", data.Counts)
	}
}

func TestListFilters(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1")
	mustTask(t, dir, "add", "--id", "3", "--title", "third", "--dep", "2")
	mustTask(t, dir, "set", "1", "--status", "done")

	ready := mustTask(t, dir, "list", "--ready")
	if len(ready.Tasks) != 1 || ready.Tasks[0].ID != "2" {
		t.Errorf("--ready = %v, want just 2", idsOf(ready.Tasks))
	}
	blocked := mustTask(t, dir, "list", "--blocked")
	if len(blocked.Tasks) != 1 || blocked.Tasks[0].ID != "3" {
		t.Errorf("--blocked = %v, want just 3", idsOf(blocked.Tasks))
	}
	// The queue is reported even when filtering by something else.
	if len(blocked.Ready) != 1 || blocked.Ready[0] != "2" {
		t.Errorf("ready = %v, want [2] so the caller learns what is runnable", blocked.Ready)
	}
	pending := mustTask(t, dir, "list", "--status", "pending")
	if len(pending.Tasks) != 2 {
		t.Errorf("--status pending = %v, want 2 and 3", idsOf(pending.Tasks))
	}
	code, env := taskErr(t, dir, "list", "--status", "in-progress")
	if code != 2 {
		t.Errorf("an unknown status: exit = %d, want 2 (usage)", code)
	}
	if !strings.Contains(env.Err.Message, "in_progress") {
		t.Errorf("message = %q, want it to list the vocabulary", env.Err.Message)
	}
	code, _ = taskErr(t, dir, "list", "--ready", "--blocked")
	if code != 2 {
		t.Errorf("--ready --blocked: exit = %d, want 2", code)
	}
}

func TestShowOfAMissingTask(t *testing.T) {
	dir := initialisedTaskRepo(t)
	code, env := taskErr(t, dir, "show", "nope")
	if code != 7 {
		t.Fatalf("exit = %d, want 7 (not_found)", code)
	}
	if env.Err.Code != envelope.CodeTaskNotFound {
		t.Errorf("code = %q, want task_not_found", env.Err.Code)
	}
	if !strings.Contains(env.Err.Hint, "ocaw task list") {
		t.Errorf("hint = %q, want it to name a way to see what does exist", env.Err.Hint)
	}
}

// A mutation refreshes the generated view, so doctor does not report staleness
// as the normal outcome of doing work.
func TestMutationsRefreshTheGeneratedView(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	doc := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md"))
	if !strings.Contains(doc, "first") {
		t.Fatalf("WORKFLOW_STATE.md was not regenerated after add:\n%s", doc)
	}
	mustTask(t, dir, "set", "1", "--status", "done")
	doc = mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md"))
	if !strings.Contains(doc, "| 1 | first |") || !strings.Contains(doc, "| done |") {
		t.Errorf("WORKFLOW_STATE.md is stale after the status change:\n%s", doc)
	}
	// And doctor agrees. The shared `run` helper is init-specific, so doctor is
	// invoked directly rather than through it.
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, doctorCommandName}, &out, &errOut, false); code != 0 {
		t.Errorf("doctor after two mutations exits %d:\n%s", code, out.String())
	}
}

// A malformed run log must not make the DAG unwritable.
func TestAMalformedRunLogDoesNotBlockTaskEdits(t *testing.T) {
	dir := initialisedTaskRepo(t)
	log := filepath.Join(dir, ".agent", "state", "runs.jsonl")
	if err := os.WriteFile(log, []byte("this is not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, env, data := taskRun(t, dir, "add", "--id", "1", "--title", "first")
	if code != 0 {
		t.Fatalf("exit = %d: %+v", code, env.Err)
	}
	if data.Task == nil {
		t.Error("the task was not added")
	}
	found := false
	for _, w := range env.Warnings {
		if strings.Contains(w.Message, "run log") {
			found = true
		}
	}
	if !found {
		t.Errorf("want a warning about the run log, got %v", env.Warnings)
	}
}

func TestUsageErrors(t *testing.T) {
	dir := initialisedTaskRepo(t)
	cases := [][]string{
		{},
		{"bogus"},
		{"add"},
		{"add", "--id", "1", "extra-positional"},
		{"set"},
		{"set", "1", "2"},
		{"dep", "1"},
		{"rm"},
		{"show"},
		{"list", "extra"},
		{"next", "extra"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, env := taskErr(t, dir, args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (usage); error = %+v", code, env.Err)
			}
			if env.Err.Code != envelope.CodeUsage {
				t.Errorf("code = %q, want usage", env.Err.Code)
			}
		})
	}
	// set with nothing to change is a usage error, not a silent no-op. The id
	// is checked first, so a typo and a forgotten flag report the typo.
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	code, _ := taskErr(t, dir, "set", "1")
	if code != 2 {
		t.Errorf("set with no flags: exit = %d, want 2", code)
	}
	if code, _ = taskErr(t, dir, "set", "typo"); code != 7 {
		t.Errorf("set with a bad id and no flags: exit = %d, want 7, reporting the typo first", code)
	}
}

// Gates are defined through `task set`, and a gate command ocaw could not run
// is refused at the point it is written rather than at verify time.
func TestGates(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	data := mustTask(t, dir, "set", "1", "--gate", "test=go test ./...", "--gate", "vet=go vet ./...")
	if len(data.Task.Gates) != 2 {
		t.Fatalf("gates = %+v, want two", data.Task.Gates)
	}
	// A gate command with an unquoted shell operator is an invariant violation.
	// A gate whose command ocaw could not run is a validation failure, not a
	// transition refusal: nothing about the task's status is at issue, the
	// command is simply not an argv.
	code, env := taskErr(t, dir, "set", "1", "--gate", "build=go build ./... && go test ./...")
	if code != 4 {
		t.Errorf("--gate with &&: exit = %d, want 4; error = %+v", code, env.Err)
	}
	if !strings.Contains(env.Err.Message, "&&") {
		t.Errorf("message = %q, want it to name the operator", env.Err.Message)
	}
	// A malformed --gate value is the caller's mistake, so usage rather than
	// validation: ocaw did not refuse the command, it could not read it.
	for _, bad := range []string{"x=", "=y", "noequals"} {
		code, env := taskErr(t, dir, "set", "1", "--gate", bad)
		if code != 2 {
			t.Errorf("--gate %q: exit = %d, want 2 (usage); error = %+v", bad, code, env.Err)
		}
	}
	// An operator inside quotes is fine: nothing is interpreted there.
	mustTask(t, dir, "set", "1", "--gate", "sel=go test -run 'TestA && TestB' ./...")
}

// Task flags must not leak between invocations in one process.
func TestTaskFlagsDoNotLeak(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first", "--agent", "coder")
	mustTask(t, dir, "add", "--id", "2", "--title", "second")
	data := mustTask(t, dir, "show", "2")
	if data.Task.Agent != "" {
		t.Errorf("agent = %q, want empty: --agent leaked from the first add", data.Task.Agent)
	}
}

func idsOf(tasks []state.TaskView) []string {
	out := make([]string, 0, len(tasks))
	for _, task := range tasks {
		out = append(out, task.ID)
	}
	return out
}

// --rm on its own must not report a complaint about --add. Both edges are
// guarded separately, because AddDeps refuses an empty list.
func TestDepRemoveWithoutAdd(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1")

	data := mustTask(t, dir, "dep", "2", "--rm", "1")
	if len(data.Task.Deps) != 0 {
		t.Errorf("deps = %v, want none", data.Task.Deps)
	}
	// And both at once, with the remove winning, which is what the flags read as.
	mustTask(t, dir, "dep", "2", "--add", "1")
	data = mustTask(t, dir, "dep", "2", "--add", "1", "--rm", "1")
	if len(data.Task.Deps) != 0 {
		t.Errorf("deps = %v, want the --rm to win over --add", data.Task.Deps)
	}
}

// A payload must never carry a null list, on the failure path either. A caller
// that reads data after an error should not have to handle two shapes.
func TestNoNullsOnTheFailurePath(t *testing.T) {
	dir := initialisedTaskRepo(t)
	_, env := taskErr(t, dir, "show", "missing")
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"tasks":[]`, `"removed":[]`, `"ready":[]`, `"blocked":{}`, `"detail"`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("failure payload is missing %s: %s", key, raw)
		}
	}
	// A null `task` is the answer — there is no such task. A null *list* would be
	// a shape the caller has to guard against, and none may appear.
	if strings.Contains(string(raw), ":null") && !strings.Contains(string(raw), `"task":null`) {
		t.Errorf("failure payload contains a null: %s", raw)
	}
	if strings.Count(string(raw), ":null") > 1 {
		t.Errorf("failure payload has more nulls than the one answer: %s", raw)
	}
}

// The human rendering is a rendering of the envelope. A TTY gets prose.
func TestTaskHumanRendering(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1")

	var out bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, taskCommandName, "next"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "next: 1") || strings.Contains(out.String(), `"command"`) {
		t.Errorf("human next output:\n%s", out.String())
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, taskCommandName, "list"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"ID", "STATUS", "in_progress 0", "pending 2"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human list is missing %q:\n%s", want, out.String())
		}
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, taskCommandName, "rm", "1", "2"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "removed 1, 2") {
		t.Errorf("human rm output:\n%s", out.String())
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, taskCommandName}, &out, io.Discard, true); code != 2 {
		t.Fatalf("bare task: exit %d, want 2", code)
	}
	for _, sub := range []string{"add", "set", "dep", "rm", "show", "list", "next"} {
		if !strings.Contains(out.String(), sub) {
			t.Errorf("bare task does not mention %q:\n%s", sub, out.String())
		}
	}
}
