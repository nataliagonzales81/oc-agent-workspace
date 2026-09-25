// Package state owns .agent/state/state.json: the models, the DAG invariants,
// and the transition rules every other package treats as truth (SPEC.md §6.1,
// §7).
//
// state.json is the single source of truth. WORKFLOW_STATE.md is a generated
// view of it (report), and no command ever parses prose. That separation is the
// reason this package exists: the DAG is a data structure, so the rules about it
// can be unit tested without a filesystem.
//
// The package is split so the rules stay pure. state.go, invariant.go,
// transition.go and runs.go do not touch the disk; only io.go does, and it goes
// through workspace so every write is atomic and every read is bounded.
//
// Per SPEC §12, state must not import cli: a command is one consumer of these
// rules, not the owner of them.
package state

import (
	"encoding/json"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/verify"
)

// StateSchema is the schema id written into every state.json.
const StateSchema = "ocaw/state@1"

// Status is a task's place in the DAG lifecycle. The vocabulary is closed
// (SPEC §6.1): a task is in exactly one of these states, and an unknown value
// is a corrupt file, not a new status.
type Status string

const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusDone       Status = "done"
	StatusBlocked    Status = "blocked"
	StatusCancelled  Status = "cancelled"
)

// Statuses is the closed vocabulary, sorted so help text and error messages are
// byte-identical between runs (SPEC §9.5).
var Statuses = []Status{
	StatusBlocked,
	StatusCancelled,
	StatusDone,
	StatusInProgress,
	StatusPending,
}

// Terminal reports whether a status means the task will not be worked on
// again. A terminal dep satisfies the tasks that depend on it.
func (s Status) Terminal() bool {
	return s == StatusDone || s == StatusCancelled
}

// SatisfiesDep reports whether a dep in this status unblocks a dependent task.
// A cancelled dep counts: the work is abandoned on purpose, so waiting for it
// would be a deadlock the author chose.
func (s Status) SatisfiesDep() bool { return s.Terminal() }

// GateStatus is a verification gate's outcome. It is a separate vocabulary from
// Status because "cancelled" and "blocked" describe scheduling, while gates
// only ever pass, fail, or time out.
//
// The three run outcomes are aliases of the values internal/verify produces
// rather than literals written here. internal/state already depends on
// internal/verify for Tokenize, so the dependency runs one way; defining the
// vocabulary in both places would leave two definitions to keep in step, and the
// step people forget is the one that makes a recorded run unreadable.
type GateStatus string

const (
	GatePending GateStatus = "pending"
	GatePass    GateStatus = GateStatus(verify.Pass)
	GateFail    GateStatus = GateStatus(verify.Fail)
	GateTimeout GateStatus = GateStatus(verify.Timeout)
)

// GateStatuses is the closed gate vocabulary, sorted.
var GateStatuses = []GateStatus{GateFail, GatePass, GatePending, GateTimeout}

// State is the whole of state.json (SPEC §6.1).
type State struct {
	Schema   string   `json:"schema"`
	Workflow Workflow `json:"workflow"`
	Tasks    []Task   `json:"tasks"`
	Budget   Budget   `json:"budget"`
}

// Workflow is the authored, non-derived top of the workflow. It is preserved
// verbatim when WORKFLOW_STATE.md is regenerated (SPEC §6.2).
type Workflow struct {
	Request     string       `json:"request"`
	Scope       string       `json:"scope"`
	Constraints []string     `json:"constraints"`
	Acceptance  []Acceptance `json:"acceptance"`
}

// Acceptance is one checkbox in the authored acceptance criteria.
type Acceptance struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	Done bool   `json:"done"`
}

// Task is one node in the DAG.
type Task struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Agent   string   `json:"agent"`
	Tier    string   `json:"tier"`
	Deps    []string `json:"deps"`
	Status  Status   `json:"status"`
	Gates   []Gate   `json:"gates"`
	Notes   string   `json:"notes"`
	Updated string   `json:"updated"`
}

// Gate is one verification command attached to a task. Cmd is stored as the
// author wrote it and is turned into an argv slice at run time; ocaw never
// hands it to a shell (SPEC §9.4).
type Gate struct {
	Name     string     `json:"name"`
	Cmd      string     `json:"cmd"`
	Status   GateStatus `json:"status"`
	LastExit *int       `json:"last_exit"`
	Attempts int        `json:"attempts"`
	LastRun  *string    `json:"last_run"`
}

// Budget is the token budget. Zero means undeclared, which is the default; a
// missing budget is not an error.
type Budget struct {
	MaxTokens   int64 `json:"max_tokens"`
	SpentTokens int64 `json:"spent_tokens"`
}

// TaskView is a task as a command reports it: the stored fields plus the
// derived stuck flag, which is computed from runs.jsonl and never stored
// (SPEC §7). Embedding keeps the stored shape and the reported shape the same
// object, so the two cannot drift apart.
type TaskView struct {
	Task
	// Stuck is true when some gate of this task has produced byte-identical
	// output for StuckThreshold consecutive attempts. It is a signal to the
	// agent, not an error: escalating instead of retrying is the agent's call.
	Stuck bool `json:"stuck"`
}

// New returns an empty but fully initialised state. Every slice is non-nil so
// encoding/json never writes a null where the spec shows an array, and a second
// init produces a byte-identical file (SPEC §4.4).
func New() *State {
	return &State{
		Schema:   StateSchema,
		Workflow: Workflow{Constraints: []string{}, Acceptance: []Acceptance{}},
		Tasks:    []Task{},
		Budget:   Budget{},
	}
}

// normalise fills in the values encoding/json would otherwise write as null.
// It runs on every decode so a hand-edited state.json missing an optional
// section still produces a well-formed one after a save.
func (s *State) normalise() {
	if s.Schema == "" {
		s.Schema = StateSchema
	}
	if s.Workflow.Constraints == nil {
		s.Workflow.Constraints = []string{}
	}
	if s.Workflow.Acceptance == nil {
		s.Workflow.Acceptance = []Acceptance{}
	}
	if s.Tasks == nil {
		s.Tasks = []Task{}
	}
	for i := range s.Tasks {
		if s.Tasks[i].Deps == nil {
			s.Tasks[i].Deps = []string{}
		}
		if s.Tasks[i].Gates == nil {
			s.Tasks[i].Gates = []Gate{}
		}
	}
}

// Views renders the tasks for a command payload, with stuck derived from the
// run log. Task order is the stored order, which is the DAG's author-chosen
// priority (SPEC §7 `task next`).
func (s *State) Views(stuck map[string]bool) []TaskView {
	out := make([]TaskView, 0, len(s.Tasks))
	for _, task := range s.Tasks {
		out = append(out, TaskView{Task: task, Stuck: stuck[task.ID]})
	}
	return out
}

// Find returns the task with the given id.
func (s *State) Find(id string) (*Task, bool) {
	for i := range s.Tasks {
		if s.Tasks[i].ID == id {
			return &s.Tasks[i], true
		}
	}
	return nil, false
}

// IndexOf returns the position of a task id, or -1.
func (s *State) IndexOf(id string) int {
	for i := range s.Tasks {
		if s.Tasks[i].ID == id {
			return i
		}
	}
	return -1
}

// TaskNotFound is the closed-code error for an unknown id. The hint names the
// commands that would list what does exist.
func TaskNotFound(id string) *Error {
	return newError(
		envelope.CodeTaskNotFound,
		"ocaw task list",
		Detail{Task: id},
		"no task with id %q", id,
	)
}

// GateNotFound is the closed-code error for an unknown gate on a known task.
func GateNotFound(task, gate string) *Error {
	return newError(
		envelope.CodeGateNotFound,
		"ocaw task show "+task,
		Detail{Task: task, Gate: gate},
		"task %q has no gate named %q", task, gate,
	)
}

// Counts returns the number of tasks per status, including statuses with no
// tasks, so a summary never has to test for a missing key.
func (s *State) Counts() map[string]int {
	out := make(map[string]int, len(Statuses))
	for _, st := range Statuses {
		out[string(st)] = 0
	}
	for _, t := range s.Tasks {
		out[string(t.Status)]++
	}
	return out
}

// Marshal renders state.json deterministically: no HTML escaping, so paths and
// command fragments survive verbatim, and a trailing newline so the file is a
// well-formed text file (SPEC §9.5).
func (s *State) Marshal() ([]byte, error) {
	s.normalise()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
