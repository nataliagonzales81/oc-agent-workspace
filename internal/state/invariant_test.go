package state

import (
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// TestInvariantSchemaIsChecked: a state from a newer build is refused, because
// decoding it would drop the fields this build does not know and the next save
// would delete them.
func TestInvariantSchemaIsChecked(t *testing.T) {
	s := New()
	if err := s.checkSchema(); err != nil {
		t.Fatalf("the current schema must pass: %v", err)
	}
	s.Schema = "ocaw/state@2"
	if got := codeOf(t, s.checkSchema()); got != string(envelope.CodeValidationFailed) {
		t.Errorf("code = %s, want %s", got, envelope.CodeValidationFailed)
	}
	if err := s.checkSchema(); err != nil && err.Detail.Check != CheckSchema {
		t.Errorf("detail.check = %q, want %q", err.Detail.Check, CheckSchema)
	}
}

// TestInvariantStatusVocabularyIsClosed: only the five §6.1 statuses are
// accepted, for tasks and for gates.
func TestInvariantStatusVocabularyIsClosed(t *testing.T) {
	for _, st := range Statuses {
		s := New()
		s.Tasks = []Task{{ID: "1", Status: st}}
		if err := s.checkStatuses(); err != nil {
			t.Errorf("status %q must be accepted, got %v", st, err)
		}
	}
	for _, bad := range []Status{"", "in-progress", "DONE", "finished", "wip"} {
		s := New()
		s.Tasks = []Task{{ID: "1", Status: bad}}
		if got := codeOf(t, s.checkStatuses()); got != string(envelope.CodeValidationFailed) {
			t.Errorf("status %q code = %s, want %s", bad, got, envelope.CodeValidationFailed)
		}
	}
	s := New()
	s.Tasks = []Task{{ID: "1", Status: StatusPending, Gates: []Gate{{Name: "lint", Cmd: "go vet ./...", Status: "ok"}}}}
	if got := codeOf(t, s.checkStatuses()); got != string(envelope.CodeValidationFailed) {
		t.Errorf("gate status code = %s, want %s", got, envelope.CodeValidationFailed)
	}
	if !strings.Contains(s.checkStatuses().Err.Message, "lint") {
		t.Error("a bad gate status must name the gate it belongs to")
	}
}

// TestInvariantTaskIDsAreUnique: a duplicate id makes every dep reference
// ambiguous, so it is a hard error.
func TestInvariantTaskIDsAreUnique(t *testing.T) {
	s := New()
	s.Tasks = []Task{{ID: "1"}, {ID: "2"}, {ID: "1"}}
	err := s.checkUniqueIDs()
	if got := codeOf(t, err); got != string(envelope.CodeValidationFailed) {
		t.Errorf("code = %s, want %s", got, envelope.CodeValidationFailed)
	}
	if err.Detail.Task != "1" {
		t.Errorf("detail.task = %q, want the duplicated id", err.Detail.Task)
	}
	if !strings.Contains(err.Err.Message, "twice") {
		t.Errorf("message = %q, want it to say the id is used twice", err.Err.Message)
	}
}

// TestInvariantIDsMayBeSlugs is the other half of invariant 1: ids are compared
// as opaque strings, so an agent working in names is never forced to renumber.
func TestInvariantIDsMayBeSlugs(t *testing.T) {
	s := New()
	s.Tasks = []Task{
		{ID: "auth", Title: "extract the auth module", Deps: []string{}, Status: StatusPending},
		{ID: "cleanup", Title: "delete dead code", Deps: []string{"auth"}, Status: StatusPending},
		{ID: "007", Title: "leading zeros are a string, not a number", Deps: []string{}, Status: StatusPending},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("slug ids must be accepted, got %v", err)
	}
	if got := s.IndexOf("007"); got != 2 {
		t.Errorf("IndexOf(007) = %d, want 2; ids are not integers", got)
	}
}

// TestInvariantDanglingDepsAreErrors: a dep naming a task that is not there is
// dep_dangling, and the message names both ends.
func TestInvariantDanglingDepsAreErrors(t *testing.T) {
	s := New()
	s.Tasks = []Task{{ID: "2", Deps: []string{"1"}}}
	err := s.checkDangling()
	if got := codeOf(t, err); got != string(envelope.CodeDepDangling) {
		t.Errorf("code = %s, want %s", got, envelope.CodeDepDangling)
	}
	if err.Detail.Task != "2" {
		t.Errorf("detail.task = %q, want the dependent task", err.Detail.Task)
	}
	if !strings.Contains(err.Err.Message, "1") {
		t.Error("message must name the dep that does not resolve")
	}
}

// TestInvariantGraphIsAcyclic: a cycle is dep_cycle and the detail carries the
// path, so an agent can see which edges to cut.
func TestInvariantGraphIsAcyclic(t *testing.T) {
	cases := []struct {
		name  string
		tasks []Task
		want  string
	}{
		{
			name:  "two tasks",
			tasks: []Task{{ID: "2", Deps: []string{"4"}}, {ID: "4", Deps: []string{"2"}}},
			want:  "2 -> 4 -> 2",
		},
		{
			name: "three tasks with a clean entry point",
			tasks: []Task{
				{ID: "1", Deps: []string{}},
				{ID: "2", Deps: []string{"1", "3"}},
				{ID: "3", Deps: []string{"4"}},
				{ID: "4", Deps: []string{"2"}},
			},
			want: "2 -> 3 -> 4 -> 2",
		},
		{
			name:  "a task that depends on itself",
			tasks: []Task{{ID: "5", Deps: []string{"5"}}},
			want:  "5 -> 5",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New()
			s.Tasks = tc.tasks
			err := s.checkAcyclic()
			if got := codeOf(t, err); got != string(envelope.CodeDepCycle) {
				t.Errorf("code = %s, want %s", got, envelope.CodeDepCycle)
			}
			if got := strings.Join(err.Detail.Cycle, " -> "); got != tc.want {
				t.Errorf("cycle = %q, want %q", got, tc.want)
			}
			if !strings.Contains(err.Err.Message, tc.want) {
				t.Errorf("message = %q, want it to contain the cycle path", err.Err.Message)
			}
		})
	}
}

// A diamond is the case a naive cycle check gets wrong: 2 and 3 both depend on 1,
// and 4 depends on both. It is not a cycle, and the closure check must not treat
// the second visit to 1 as one.
func TestInvariantDiamondIsNotACycle(t *testing.T) {
	s := New()
	s.Tasks = []Task{
		{ID: "1", Deps: []string{}},
		{ID: "2", Deps: []string{"1"}},
		{ID: "3", Deps: []string{"1"}},
		{ID: "4", Deps: []string{"2", "3"}},
	}
	if err := s.checkAcyclic(); err != nil {
		t.Fatalf("a diamond is a valid DAG, got %v", err)
	}
}

// TestInvariantInProgressNeedsDoneDeps is invariant 4: the rule that stops an
// agent racing ahead of its own DAG. It is an error, not a warning.
func TestInvariantInProgressNeedsDoneDeps(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c")
	s.Tasks[0].Status = StatusDone
	s.Tasks[1].Status = StatusInProgress
	if err := s.checkInFlight(); err != nil {
		t.Fatalf("an in_progress task whose deps are done must pass, got %v", err)
	}

	// Now reopen task 1, leaving 2 in progress on top of it.
	s.Tasks[0].Status = StatusPending
	err := s.checkInFlight()
	if got := codeOf(t, err); got != string(envelope.CodeDepsUnmet) {
		t.Errorf("code = %s, want %s", got, envelope.CodeDepsUnmet)
	}
	if len(err.Detail.Blocking) != 1 || err.Detail.Blocking[0] != "1" {
		t.Errorf("detail.blocking = %v, want the dep that is not finished", err.Detail.Blocking)
	}
	if err.Detail.Task != "2" {
		t.Errorf("detail.task = %q, want the task that raced ahead", err.Detail.Task)
	}
}

// A cancelled dep unblocks its dependents. Waiting for work that was abandoned on
// purpose would be a deadlock the author chose, not one to report.
func TestInvariantCancelledDepSatisfiesDependents(t *testing.T) {
	s := New()
	chain(s, "a", "b")
	s.Tasks[0].Status = StatusCancelled
	s.Tasks[1].Status = StatusInProgress
	if err := s.checkInFlight(); err != nil {
		t.Fatalf("a cancelled dep must satisfy its dependents, got %v", err)
	}
	if blocking := s.UnmetDeps("2"); len(blocking) != 0 {
		t.Errorf("UnmetDeps(2) = %v, want none", blocking)
	}
}

// TestInvariantGatesAreRunnableCommands: a gate must be named, uniquely, with a
// non-empty command that ocaw can run without a shell.
func TestInvariantGatesAreRunnableCommands(t *testing.T) {
	base := func() *State {
		s := New()
		s.Tasks = []Task{{
			ID: "1", Status: StatusPending,
			Gates: []Gate{{Name: "test", Cmd: "go test ./...", Status: GatePending}},
		}}
		return s
	}
	if err := base().checkGates(); err != nil {
		t.Fatalf("a well-formed gate must pass, got %v", err)
	}

	empty := base()
	empty.Tasks[0].Gates[0].Cmd = "   "
	if got := codeOf(t, empty.checkGates()); got != string(envelope.CodeValidationFailed) {
		t.Errorf("empty cmd code = %s, want %s", got, envelope.CodeValidationFailed)
	}

	dupe := base()
	dupe.Tasks[0].Gates = append(dupe.Tasks[0].Gates, Gate{Name: "test", Cmd: "go vet ./..."})
	if got := codeOf(t, dupe.checkGates()); got != string(envelope.CodeValidationFailed) {
		t.Errorf("duplicate gate code = %s, want %s", got, envelope.CodeValidationFailed)
	}
	if !strings.Contains(dupe.checkGates().Err.Message, "two gates") {
		t.Error("message must say the gate name is used twice")
	}
}

// §9.4 makes argv, not a shell, the execution model, so a shell operator left in
// a gate is a command that could not be honoured. Quoting does not defeat the
// rule: ocaw tokenises the string itself.
func TestInvariantGatesRefuseShellOperators(t *testing.T) {
	for _, cmd := range []string{
		"go test ./... && go vet ./...",
		"go test ./... || true",
		"go test ./... | tee log",
		"go test ./...; echo done",
		"go test ./... && echo hi > out.txt",
		"go run $(cat go.mod)",
		"go test `id`",
		"go build ./... &",
		`go test "a && b"`,
	} {
		s := New()
		s.Tasks = []Task{{
			ID: "1", Status: StatusPending,
			Gates: []Gate{{Name: "test", Cmd: cmd, Status: GatePending}},
		}}
		err := s.checkGates()
		if got := codeOf(t, err); got != string(envelope.CodeValidationFailed) {
			t.Errorf("cmd %q code = %s, want %s", cmd, got, envelope.CodeValidationFailed)
		}
		if !strings.Contains(err.Err.Message, "argv") {
			t.Errorf("cmd %q: message = %q, want it to explain the argv rule", cmd, err.Err.Message)
		}
	}
}

// The negative half: a gate that merely looks dangerous must still pass, or the
// rule becomes "refuse anything with a special character".
func TestInvariantGateCommandsWithSafePunctuationPass(t *testing.T) {
	for _, cmd := range []string{
		"go test ./...",
		`go test -run 'TestThing' ./internal/...`,
		`python -m pytest tests/test_x.py`,
		"npm run test -- --watch=false",
		"cargo test --all-features",
		"make check",
		"./scripts/verify.sh --strict",
	} {
		s := New()
		s.Tasks = []Task{{
			ID: "1", Status: StatusPending,
			Gates: []Gate{{Name: "test", Cmd: cmd, Status: GatePending}},
		}}
		if err := s.checkGates(); err != nil {
			t.Errorf("cmd %q must be accepted, got %v", cmd, err)
		}
	}
}

func TestInvariantAcceptanceIDsAreUnique(t *testing.T) {
	s := New()
	s.Workflow.Acceptance = []Acceptance{{ID: "a1", Text: "one"}, {ID: "a1", Text: "one again"}}
	if got := codeOf(t, s.checkAcceptance()); got != string(envelope.CodeValidationFailed) {
		t.Errorf("code = %s, want %s", got, envelope.CodeValidationFailed)
	}

	s.Workflow.Acceptance = []Acceptance{{ID: "a1"}, {ID: "a2"}}
	if err := s.checkAcceptance(); err != nil {
		t.Errorf("distinct acceptance ids must pass, got %v", err)
	}
}

// Validate reports the first failure in a fixed order, so the same broken file
// always produces the same message. An agent that retries must not see a
// different error each time.
func TestValidateIsOrderedAndDeterministic(t *testing.T) {
	broken := func() *State {
		s := New()
		s.Tasks = []Task{
			{ID: "1", Status: StatusPending, Gates: []Gate{{Name: "t", Cmd: "go test ./...", Status: GatePending}}},
			{ID: "1", Status: StatusPending, Deps: []string{"9"}},
		}
		return s
	}
	first := broken()
	second := broken()
	for i := range 3 {
		a := first.Validate()
		b := second.Validate()
		if a == nil || b == nil {
			t.Fatal("a broken state must not validate")
		}
		if a.Err.Code != b.Err.Code || a.Err.Message != b.Err.Message {
			t.Fatalf("run %d: %v vs %v", i, a, b)
		}
	}
	// Unique ids are checked before dep resolution, so the duplicate is reported
	// first: fixing it is a prerequisite for the rest being meaningful.
	if got := codeOf(t, first.Validate()); got != string(envelope.CodeValidationFailed) {
		t.Errorf("code = %s, want %s", got, envelope.CodeValidationFailed)
	}
	if first.Validate().Detail.Check != CheckUniqueIDs {
		t.Errorf("first check = %q, want %q", first.Validate().Detail.Check, CheckUniqueIDs)
	}
}

// Violations reports every failure, because doctor exists to describe a
// workspace's health in one pass.
func TestViolationsReportsEveryFailure(t *testing.T) {
	s := New()
	s.Schema = "ocaw/state@9"
	s.Tasks = []Task{
		{ID: "1", Status: StatusInProgress, Deps: []string{"2"},
			Gates: []Gate{{Name: "t", Cmd: "go test ./... && go vet ./...", Status: GatePending}}},
		{ID: "2", Status: StatusPending, Deps: []string{"nope"}},
		{ID: "3", Status: "bogus", Deps: []string{"2"}},
	}
	got := s.Violations()
	checks := make([]string, 0, len(got))
	for _, v := range got {
		if v.Detail.Check == "" {
			t.Errorf("violation %q does not name the check that fired", v.Err.Message)
		}
		checks = append(checks, v.Detail.Check)
	}
	for _, want := range []string{CheckSchema, CheckStatuses, CheckDangling, CheckInFlight, CheckGates} {
		found := false
		for _, c := range checks {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("check %q did not fire; got %v", want, checks)
		}
	}
}

// Every check name is exported so doctor can list them, and the list is sorted so
// `doctor --json` is byte-identical between runs.
func TestChecksAreSortedAndComplete(t *testing.T) {
	for i := 1; i < len(Checks); i++ {
		if Checks[i-1] >= Checks[i] {
			t.Fatalf("Checks is not sorted: %q then %q", Checks[i-1], Checks[i])
		}
	}
	if len(Checks) != 9 {
		t.Errorf("got %d checks, want 9: %v", len(Checks), Checks)
	}
}

// UnmetDeps is the single answer to "what is blocking this", shared by the
// invariant, the transition rules, and status.
func TestUnmetDepsAndDependents(t *testing.T) {
	s := New()
	chain(s, "a", "b", "c") // 1:[], 2:[1], 3:[2]
	s.Tasks[1].Status = StatusDone
	s.Tasks[2].Status = StatusCancelled

	if got := s.UnmetDeps("1"); len(got) != 0 {
		t.Errorf("UnmetDeps(1) = %v, want none", got)
	}
	if got := s.UnmetDeps("2"); len(got) != 1 || got[0] != "1" {
		t.Errorf("UnmetDeps(2) = %v, want [1]: a pending dep blocks", got)
	}
	if got := s.UnmetDeps("3"); len(got) != 0 {
		t.Errorf("UnmetDeps(3) = %v, want none: a done dep and a cancelled task block nothing", got)
	}
	if got := s.UnmetDeps("absent"); got != nil {
		t.Errorf("UnmetDeps(absent) = %v, want nil", got)
	}

	if got := s.Dependents("1"); len(got) != 1 || got[0] != "2" {
		t.Errorf("Dependents(1) = %v, want [2]", got)
	}
	if got := s.Dependents("2"); len(got) != 1 || got[0] != "3" {
		t.Errorf("Dependents(2) = %v, want [3]", got)
	}
	if got := s.Dependents("3"); len(got) != 0 {
		t.Errorf("Dependents(3) = %v, want none", got)
	}
}
