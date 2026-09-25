package state

import (
	"sort"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/verify"
)

// The named checks. doctor reports them by name so an agent can tell "this
// workspace has a cycle" from "this workspace has two tasks called 2", and so a
// test can assert that a specific rule fired rather than merely that something
// did.
const (
	CheckSchema     = "schema"
	CheckStatuses   = "statuses"
	CheckUniqueIDs  = "unique_ids"
	CheckDangling   = "deps_resolve"
	CheckAcyclic    = "acyclic"
	CheckInFlight   = "in_progress_deps"
	CheckDependents = "dependents_in_flight"
	CheckGates      = "gates"
	CheckAcceptance = "acceptance_unique_ids"
)

// Checks is every invariant name, sorted, for `doctor --json` and help text.
var Checks = []string{
	CheckAcceptance,
	CheckAcyclic,
	CheckDependents,
	CheckDangling,
	CheckGates,
	CheckInFlight,
	CheckSchema,
	CheckStatuses,
	CheckUniqueIDs,
}

// Violations runs every invariant and returns all failures, in a fixed order.
//
// All of them, not just the first: doctor exists to report a workspace's whole
// health in one pass, and an agent that has to re-run the tool after fixing
// each problem individually will not.
func (s *State) Violations() []*Error {
	var out []*Error
	for _, check := range []func() *Error{
		s.checkSchema,
		s.checkStatuses,
		s.checkUniqueIDs,
		s.checkDangling,
		s.checkAcyclic,
		s.checkInFlight,
		s.checkDependents,
		s.checkGates,
		s.checkAcceptance,
	} {
		if err := check(); err != nil {
			out = append(out, err)
		}
	}
	return out
}

// Validate returns the first invariant failure, or nil. Load uses it to refuse a
// state file that would be unsafe to reason about; a command uses it before a
// write, so ocaw never persists a state it would refuse to read back.
func (s *State) Validate() *Error {
	violations := s.Violations()
	if len(violations) == 0 {
		return nil
	}
	return violations[0]
}

// checkSchema refuses a state written by a newer or unrelated tool. Reading a
// file whose shape we do not recognise would mean silently dropping fields, and
// the next save would delete them (SPEC §9.1: an unsupported construct is an
// error, never a silent drop).
func (s *State) checkSchema() *Error {
	if s.Schema == StateSchema {
		return nil
	}
	return newError(
		envelope.CodeValidationFailed,
		"upgrade ocaw, or migrate the file by hand",
		Detail{Check: CheckSchema},
		"state.json declares schema %q, this build reads %q", s.Schema, StateSchema,
	)
}

// checkStatuses enforces the closed task and gate vocabularies (SPEC §6.1).
// A value outside them means the file was written by something other than ocaw,
// or was hand-edited into a shape the rules cannot reason about.
func (s *State) checkStatuses() *Error {
	for _, t := range s.Tasks {
		if !validStatus(t.Status) {
			return newError(
				envelope.CodeValidationFailed,
				"ocaw task set "+t.ID+" --status "+string(StatusPending),
				Detail{Task: t.ID, Check: CheckStatuses},
				"task %q has status %q, want one of: %s", t.ID, t.Status, joinStatuses(),
			)
		}
		for _, g := range t.Gates {
			if !validGateStatus(g.Status) {
				return newError(
					envelope.CodeValidationFailed,
					"ocaw verify run --task "+t.ID+" --gate "+g.Name,
					Detail{Task: t.ID, Gate: g.Name, Check: CheckStatuses},
					"task %q gate %q has status %q, want one of: %s",
					t.ID, g.Name, g.Status, joinGateStatuses(),
				)
			}
		}
	}
	return nil
}

func validStatus(s Status) bool {
	for _, v := range Statuses {
		if v == s {
			return true
		}
	}
	return false
}

func validGateStatus(s GateStatus) bool {
	for _, v := range GateStatuses {
		if v == s {
			return true
		}
	}
	return false
}

// checkUniqueIDs enforces invariant 1: task ids are unique.
//
// Ids are compared as opaque strings and integers are never required, so an
// agent that works in slugs ("auth", "cleanup") is never forced to renumber its
// DAG. Two tasks with the same id, though, make every dep reference ambiguous,
// so it is a hard error rather than a last-one-wins merge.
func (s *State) checkUniqueIDs() *Error {
	seen := make(map[string]int, len(s.Tasks))
	for i, t := range s.Tasks {
		if t.ID == "" {
			return newError(
				envelope.CodeValidationFailed,
				"ocaw task add --id <id> --title <title>",
				Detail{Check: CheckUniqueIDs},
				"task at position %d has an empty id", i+1,
			)
		}
		if first, dup := seen[t.ID]; dup {
			return newError(
				envelope.CodeValidationFailed,
				"ocaw task rm "+t.ID,
				Detail{Task: t.ID, Check: CheckUniqueIDs},
				"task id %q is used twice (positions %d and %d)", t.ID, first+1, i+1,
			)
		}
		seen[t.ID] = i
	}
	return nil
}

// checkDangling enforces invariant 2: every dep resolves to an existing task.
func (s *State) checkDangling() *Error {
	for _, t := range s.Tasks {
		for _, dep := range t.Deps {
			if s.IndexOf(dep) < 0 {
				return newError(
					envelope.CodeDepDangling,
					"ocaw task dep "+t.ID+" --rm "+dep,
					Detail{Task: t.ID, Check: CheckDangling},
					"task %q depends on %q, which is not a task in this workspace", t.ID, dep,
				)
			}
		}
	}
	return nil
}

// checkAcyclic enforces invariant 3 and reports the path that closes the cycle,
// so the message is actionable without re-deriving it. A cycle has no valid
// execution order, and the next task would silently never be ready.
//
// The walk is an iterative depth-first search over a colour map: unvisited tasks
// are followed, tasks on the current stack close a cycle, and finished tasks are
// never re-expanded. Tasks are visited in stored order and deps in listed order,
// so the reported path is the same on every run (SPEC §9.5).
func (s *State) checkAcyclic() *Error {
	const (
		unvisited = 0
		onStack   = 1
		finished  = 2
	)
	colour := make(map[string]int, len(s.Tasks))
	var stack []string

	var visit func(id string) []string
	visit = func(id string) []string {
		colour[id] = onStack
		stack = append(stack, id)
		task, ok := s.Find(id)
		if ok {
			for _, dep := range task.Deps {
				switch colour[dep] {
				case unvisited:
					if path := visit(dep); path != nil {
						return path
					}
				case onStack:
					return cycleFrom(stack, dep)
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[id] = finished
		return nil
	}

	for _, t := range s.Tasks {
		if colour[t.ID] == unvisited {
			if path := visit(t.ID); path != nil {
				return newError(
					envelope.CodeDepCycle,
					"ocaw task dep <id> --rm <dep>",
					Detail{Task: t.ID, Check: CheckAcyclic, Cycle: path},
					"dependency cycle: %s", strings.Join(path, " -> "),
				)
			}
		}
	}
	return nil
}

// cycleFrom slices the traversal stack from the first appearance of the id
// that closes the cycle, then appends it again, giving "2 -> 4 -> 2".
func cycleFrom(stack []string, closing string) []string {
	start := 0
	for i, id := range stack {
		if id == closing {
			start = i
			break
		}
	}
	path := append([]string{}, stack[start:]...)
	return append(path, closing)
}

// checkInFlight enforces invariant 4: a task may be in_progress only when every
// dep is done or cancelled.
//
// This is the rule that stops an agent racing ahead of its own DAG. It is a
// hard error, never a warning: a warning is something an agent is free to
// ignore, and an agent that ignores it produces a WORKFLOW_STATE.md that
// describes work that never happened.
func (s *State) checkInFlight() *Error {
	for _, t := range s.Tasks {
		if t.Status != StatusInProgress {
			continue
		}
		if blocking := s.UnmetDeps(t.ID); len(blocking) > 0 {
			return newError(
				envelope.CodeDepsUnmet,
				"ocaw task set "+t.ID+" --status "+string(StatusPending),
				Detail{Task: t.ID, Check: CheckInFlight, Blocking: blocking},
				"task %q is %s but %s not finished: %s",
				t.ID, StatusInProgress, plural(len(blocking), "dep is", "deps are"), strings.Join(blocking, ", "),
			)
		}
	}
	return nil
}

// checkDependents is the mirror of invariant 4, and follows from it: a dependent
// may be in_progress only because its dep is done, cancelled, or under way, so a
// dep that is neither finished nor in flight while a dependent is in_progress is
// already a violation of the rule above, one step removed.
//
// It is checked separately because the message differs. Reporting "task 2 is
// parked while task 3 works on it" names the mistake; reporting a `deps_unmet`
// about task 3 leaves the agent looking at the wrong task.
func (s *State) checkDependents() *Error {
	for _, t := range s.Tasks {
		if t.Status != StatusInProgress {
			continue
		}
		for _, dep := range t.Deps {
			d, found := s.Find(dep)
			if !found || d.Status.Terminal() || d.Status == StatusInProgress {
				continue
			}
			return newError(
				envelope.CodeDepsUnmet,
				"ocaw task set "+t.ID+" --status "+string(StatusPending),
				Detail{Task: t.ID, Check: CheckDependents, Blocking: []string{dep}},
				"task %q is in progress while its dep %q is %s", t.ID, dep, d.Status,
			)
		}
	}
	return nil
}

// checkGates enforces the §6.1 rule that a gate is a real, runnable, single
// command.
//
// The "no unescaped && chain" clause is read strictly rather than literally: ocaw
// never runs a shell (§9.4), so any shell operator left in a gate command is a
// command that would either fail or, worse, be "fixed" by an agent into a shell
// invocation. Refusing the operators is what makes the argv guarantee true.
func (s *State) checkGates() *Error {
	for _, t := range s.Tasks {
		names := make(map[string]int, len(t.Gates))
		for _, g := range t.Gates {
			if g.Name == "" {
				return newError(
					envelope.CodeValidationFailed,
					"ocaw task set "+t.ID+" --note <text>",
					Detail{Task: t.ID, Check: CheckGates},
					"task %q has a gate with an empty name", t.ID,
				)
			}
			if first, dup := names[g.Name]; dup {
				return newError(
					envelope.CodeValidationFailed,
					"ocaw task show "+t.ID,
					Detail{Task: t.ID, Gate: g.Name, Check: CheckGates},
					"task %q has two gates named %q (positions %d and %d)", t.ID, g.Name, first+1, len(names)+1,
				)
			}
			names[g.Name] = 1
			if strings.TrimSpace(g.Cmd) == "" {
				return newError(
					envelope.CodeValidationFailed,
					"ocaw verify detect",
					Detail{Task: t.ID, Gate: g.Name, Check: CheckGates},
					"task %q gate %q has an empty cmd", t.ID, g.Name,
				)
			}
			// The same parser verify will use to run it, so a command ocaw
			// accepts here is one it can actually run, and one it refuses here
			// is one it refuses to run too. Two parsers for one format means a
			// user finds out which by trying.
			if _, terr := verify.Tokenize(g.Cmd); terr != nil {
				return newError(
					envelope.CodeValidationFailed,
					"split the command into separate gates",
					Detail{Task: t.ID, Gate: g.Name, Check: CheckGates},
					"task %q gate %q is not a runnable argv: %v (SPEC §9.4)",
					t.ID, g.Name, terr,
				)
			}
		}
	}
	return nil
}

// checkAcceptance keeps acceptance ids unique. The ids are what `ocaw workflow
// accept` addresses, so a duplicate makes one of them unaddressable.
func (s *State) checkAcceptance() *Error {
	seen := make(map[string]int, len(s.Workflow.Acceptance))
	for i, a := range s.Workflow.Acceptance {
		if a.ID == "" {
			return newError(
				envelope.CodeValidationFailed,
				"ocaw workflow set --accept <text>",
				Detail{Check: CheckAcceptance},
				"acceptance item at position %d has an empty id", i+1,
			)
		}
		if first, dup := seen[a.ID]; dup {
			return newError(
				envelope.CodeValidationFailed,
				"ocaw workflow show",
				Detail{Check: CheckAcceptance, Task: a.ID},
				"acceptance id %q is used twice (positions %d and %d)", a.ID, first+1, i+1,
			)
		}
		seen[a.ID] = i
	}
	return nil
}

// shellOperator returns the first operator found in cmd, or "".

// UnmetDeps returns the deps of a task that are not done or cancelled, in the
// order the task lists them. It is the single answer to "what is blocking this",
// used by the invariant, by the transition rules, and by `ocaw status`.
func (s *State) UnmetDeps(id string) []string {
	task, ok := s.Find(id)
	if !ok {
		return nil
	}
	var blocking []string
	for _, dep := range task.Deps {
		d, found := s.Find(dep)
		if !found || !d.Status.SatisfiesDep() {
			blocking = append(blocking, dep)
		}
	}
	return blocking
}

// Dependents returns the ids of tasks that list id as a dep, in stored order.
func (s *State) Dependents(id string) []string {
	var out []string
	for _, t := range s.Tasks {
		for _, dep := range t.Deps {
			if dep == id {
				out = append(out, t.ID)
				break
			}
		}
	}
	return out
}

// Ready returns the tasks an agent may start now: every dep is done or
// cancelled, and the task itself is still open.
//
// A blocked task is open but not offered as work. Blocked is a deliberate mark
// by a human, and handing it to `task next` anyway would push an agent into the
// exact thing someone flagged; it is reported by `ocaw status` instead.
func (s *State) Ready() []*Task {
	var out []*Task
	for i := range s.Tasks {
		if s.Tasks[i].Status != StatusPending && s.Tasks[i].Status != StatusBlocked {
			continue
		}
		if len(s.UnmetDeps(s.Tasks[i].ID)) == 0 {
			out = append(out, &s.Tasks[i])
		}
	}
	return out
}

// Next returns the highest-priority ready task, or nil. Priority is stored
// order: the DAG's author chose it, and any derived ordering (alphabetical,
// by tier, by insert time) would be a second, invisible priority that disagrees
// with the file a human reads.
//
// A nil result is a valid state, not an error: an empty queue means the work is
// finished, blocked, or waiting, and `ocaw task next` reports
// `data.task: null` with ok true (SPEC §7).
func (s *State) Next() *Task {
	for i := range s.Tasks {
		if s.Tasks[i].Status != StatusPending {
			continue
		}
		if len(s.UnmetDeps(s.Tasks[i].ID)) == 0 {
			return &s.Tasks[i]
		}
	}
	return nil
}

// Blocked returns the tasks held up by their dependencies, with the dep ids
// that are holding them.
func (s *State) Blocked() map[string][]string {
	out := map[string][]string{}
	for _, t := range s.Tasks {
		if !t.Status.Terminal() {
			if blocking := s.UnmetDeps(t.ID); len(blocking) > 0 {
				out[t.ID] = blocking
			}
		}
	}
	return out
}

// Gate returns a task's gate by name.
func (t *Task) Gate(name string) (*Gate, bool) {
	for i := range t.Gates {
		if t.Gates[i].Name == name {
			return &t.Gates[i], true
		}
	}
	return nil, false
}

// GateNames returns a task's gate names in stored order.
func (t *Task) GateNames() []string {
	out := make([]string, 0, len(t.Gates))
	for _, g := range t.Gates {
		out = append(out, g.Name)
	}
	return out
}

// SetGate installs or replaces a gate's definition, preserving the recorded
// outcome when the command is unchanged. A re-detected verify command must not
// look like a fresh failure, and a changed command must not inherit the old
// command's evidence.
//
// This is the definition path. `ocaw verify` records an outcome by mutating the
// gate it gets back from Task.Gate, not through here, so a fresh result is never
// mistaken for a definition change.
func (t *Task) SetGate(g Gate) {
	for i := range t.Gates {
		if t.Gates[i].Name == g.Name {
			if t.Gates[i].Cmd == g.Cmd {
				// The incoming gate is a definition; the outcome belongs to the
				// command that produced it.
				g.Status = t.Gates[i].Status
				g.LastExit = t.Gates[i].LastExit
				g.Attempts = t.Gates[i].Attempts
				g.LastRun = t.Gates[i].LastRun
			} else {
				g.Status = GatePending
				g.LastExit = nil
				g.Attempts = 0
				g.LastRun = nil
			}
			t.Gates[i] = g
			return
		}
	}
	if g.Status == "" {
		g.Status = GatePending
	}
	t.Gates = append(t.Gates, g)
}

// SortedIDs returns the task ids in sorted order, for messages that must not
// depend on stored order.
func (s *State) SortedIDs() []string {
	out := make([]string, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		out = append(out, t.ID)
	}
	sort.Strings(out)
	return out
}

func joinStatuses() string {
	out := make([]string, 0, len(Statuses))
	for _, s := range Statuses {
		out = append(out, string(s))
	}
	return strings.Join(out, "|")
}

func joinGateStatuses() string {
	out := make([]string, 0, len(GateStatuses))
	for _, s := range GateStatuses {
		out = append(out, string(s))
	}
	return strings.Join(out, "|")
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
