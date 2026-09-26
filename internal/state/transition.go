package state

import (
	"sort"
	"strings"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// ParseStatus converts a command-line value into a Status.
//
// The vocabulary is closed, so a value outside it is a usage error at the flag
// and a corrupt file inside state.json. Keeping the two apart matters: telling
// an agent it misspelled a flag is a different recovery from telling it its
// state file is broken.
func ParseStatus(raw string) (Status, *Error) {
	for _, s := range Statuses {
		if string(s) == raw {
			return s, nil
		}
	}
	return "", newError(
		envelope.CodeUsage,
		"ocaw task set <id> --status <status>",
		Detail{To: raw},
		"%q is not a task status, want one of: %s", raw, joinStatuses(),
	)
}

// ParseGateStatus converts a command-line value into a GateStatus.
func ParseGateStatus(raw string) (GateStatus, *Error) {
	for _, s := range GateStatuses {
		if string(s) == raw {
			return s, nil
		}
	}
	return "", newError(
		envelope.CodeUsage,
		"ocaw verify run --task <id> --gate <name>",
		Detail{To: raw},
		"%q is not a gate status, want one of: %s", raw, joinGateStatuses(),
	)
}

// AddTask inserts a task, or updates the existing one with the same id.
//
// Upsert by id, not an error on a duplicate (§4.4): the common case is an agent
// re-issuing the same `task add` after a context loss, and refusing it would make
// the command unsafe to retry. Fields the caller left empty keep the stored
// value, so a partial add cannot silently erase a title or a status. A dep
// argument is absolute, not a merge: `add --dep` states the task's full dep set,
// because a caller cannot see the current one and a merge would make the command
// order-dependent.
func (s *State) AddTask(t Task, now time.Time) *Error {
	if strings.TrimSpace(t.ID) == "" {
		return newError(
			envelope.CodeUsage,
			"ocaw task add --id <id> --title <title>",
			Detail{},
			"a task needs an id",
		)
	}
	// An unset status means "not specified", which is pending; a status outside
	// the vocabulary is a usage error. The two are different mistakes and must
	// not read the same, so they are checked separately.
	if t.Status != "" && !validStatus(t.Status) {
		return newError(
			envelope.CodeUsage,
			"ocaw task add --id "+t.ID+" --status "+string(StatusPending),
			Detail{Task: t.ID, To: string(t.Status)},
			"%q is not a task status, want one of: %s", t.Status, joinStatuses(),
		)
	}

	// A supplied dep that is not in the workspace is caught here, before the
	// change is applied, and not left to the general invariant.
	//
	// The two sites see the same shape — a task naming a dep that does not exist
	// — and need opposite advice. Here the dep is *newly supplied*, so it is a
	// typo or a forward reference and the fix is to create the task. In the
	// invariant, the dep may already be recorded, where the fix is to drop the
	// edge, or may be missing because a removal was just refused, where dropping
	// the edge is right and creating the task would resurrect a task the user
	// deleted on purpose. Only this site knows which case it is, so only this site
	// can pick the hint.
	for _, dep := range t.Deps {
		if s.IndexOf(dep) < 0 {
			return newError(
				envelope.CodeDepDangling,
				"ocaw task add --id "+dep+" --title <s>",
				Detail{Task: t.ID, Check: CheckDangling, TaskIDs: []string{dep}},
				"task %q would depend on %q, which is not a task in this workspace", t.ID, dep,
			)
		}
	}

	// The change is applied to a copy and only committed once it is known to be
	// valid, so a refused add leaves no trace in the state.
	proposed := s.clone()
	if existing, found := proposed.Find(t.ID); found {
		mergeString(&existing.Title, t.Title)
		mergeString(&existing.Agent, t.Agent)
		mergeString(&existing.Tier, t.Tier)
		mergeString(&existing.Notes, t.Notes)
		if t.Deps != nil {
			existing.Deps = dedupe(t.Deps)
		}
		if t.Status != "" {
			existing.Status = t.Status
		}
		for _, g := range t.Gates {
			existing.SetGate(g)
		}
		existing.Updated = stamp(now)
	} else {
		task := Task{
			ID:     t.ID,
			Title:  t.Title,
			Agent:  t.Agent,
			Tier:   t.Tier,
			Deps:   dedupe(t.Deps),
			Status: t.Status,
			Gates:  []Gate{},
			Notes:  t.Notes,
		}
		if task.Status == "" {
			task.Status = StatusPending
		}
		for _, g := range t.Gates {
			task.SetGate(g)
		}
		task.Updated = stamp(now)
		proposed.Tasks = append(proposed.Tasks, task)
	}

	if err := proposed.refuseIfIncomplete(t.ID); err != nil {
		return err
	}
	// refuseIfIncomplete covers the checks whose message is worth naming a task
	// for. Validate is the backstop, because AddTask is the one mutation that can
	// introduce a status directly: `add --status in_progress` on a task with
	// unmet deps is invariant 4, and without this it would sail through here and
	// be caught by Save, which reports it as a write failure — the wrong code and
	// the wrong exit class for what is a transition refusal.
	if err := proposed.Validate(); err != nil {
		return err
	}
	s.Tasks = proposed.Tasks
	return nil
}

// SetStatus moves a task to a new status, applying the transition rules.
//
// The two rules the spec names are both refusals rather than silent acceptances,
// and both carry the way out in the hint:
//
//   - done -> in_progress needs --yes. Reopening a done task invalidates the
//     gates that were its evidence, so it is allowed but never accidental.
//   - cancelled -> done is rejected outright. Cancelled means the author decided
//     the work will not happen; marking it done afterwards fabricates an outcome
//     nobody produced.
//
// A transition into in_progress with unmet deps is invariant 4 refusing the
// request, and reports the blocking ids in data so the agent knows what to finish
// first.
func (s *State) SetStatus(id string, to Status, yes bool, now time.Time) *Error {
	task, found := s.Find(id)
	if !found {
		return TaskNotFound(id)
	}
	if !validStatus(to) {
		return newError(
			envelope.CodeUsage,
			"ocaw task set "+id+" --status "+string(StatusPending),
			Detail{Task: id, To: string(to)},
			"%q is not a task status, want one of: %s", to, joinStatuses(),
		)
	}
	from := task.Status
	if from == to {
		return nil
	}
	if from == StatusDone && to == StatusInProgress && !yes {
		return newError(
			envelope.CodeInvalidTransition,
			"ocaw task set "+id+" --status "+string(to)+" --yes",
			Detail{Task: id, From: string(from), To: string(to)},
			"task %q is done; reopening it invalidates its gates, so pass --yes", id,
		)
	}
	if from == StatusCancelled && to == StatusDone {
		return newError(
			envelope.CodeInvalidTransition,
			"ocaw task rm "+id,
			Detail{Task: id, From: string(from), To: string(to)},
			"task %q is cancelled; a cancelled task cannot be completed", id,
		)
	}
	if to == StatusInProgress {
		if blocking := s.UnmetDeps(id); len(blocking) > 0 {
			return newError(
				envelope.CodeDepsUnmet,
				"ocaw task set "+id+" --status "+string(StatusPending),
				Detail{Task: id, From: string(from), To: string(to), Blocking: blocking},
				"task %q cannot start: %s not finished: %s",
				id, plural(len(blocking), "dep is", "deps are"), strings.Join(blocking, ", "),
			)
		}
	}
	if err := s.refuseIfDependentsInFlight(id, to); err != nil {
		return err
	}
	task.Status = to
	task.Updated = stamp(now)
	return nil
}

// refuseIfDependentsInFlight refuses to park a task while a dependent is
// working on the strength of it.
//
// It is the mirror of invariant 4. A dependent may be in_progress only because
// this task is done, cancelled, or under way; setting this task back to pending
// or blocked withdraws that, leaving the dependent in progress with an unmet dep
// — which the next load would then refuse, with a message about a file the
// agent did not hand-edit. Refusing here names the dependent instead.
//
// The dangerous case is a reopen: A is done, B started on A, and A is moved back
// to in_progress. --yes is the author's acknowledgement that reopening
// invalidates A's gates; it is not permission to strand B.
func (s *State) refuseIfDependentsInFlight(id string, to Status) *Error {
	if to != StatusPending && to != StatusBlocked {
		return nil
	}
	var inFlight []string
	for _, dep := range s.Dependents(id) {
		if t, ok := s.Find(dep); ok && t.Status == StatusInProgress {
			inFlight = append(inFlight, dep)
		}
	}
	if len(inFlight) == 0 {
		return nil
	}
	sort.Strings(inFlight)
	// The message names the task being parked and the dependents that are
	// working from it, in that order. Saying "task X is in progress" when X is
	// the one being parked is the kind of message that sends a reader to the
	// wrong task.
	return newError(
		envelope.CodeInvalidTransition,
		"ocaw task set "+strings.Join(inFlight, " ")+" --status done",
		Detail{Task: id, To: string(to), Blocking: inFlight},
		"task %q cannot go back to %s: %s %s already in progress on it",
		id, to, plural(len(inFlight), "task", "tasks"), strings.Join(inFlight, ", "),
	)
}

// AddDeps adds deps to a task, refusing any that would dangle or close a cycle.
//
// The prospective graph is validated as a whole rather than dep by dep, because
// "would this create a cycle" is a question about the whole set: adding two deps
// that are fine individually and a cycle together has to fail.
func (s *State) AddDeps(id string, deps []string, now time.Time) *Error {
	task, found := s.Find(id)
	if !found {
		return TaskNotFound(id)
	}
	if len(deps) == 0 {
		return newError(
			envelope.CodeUsage,
			"ocaw task dep "+id+" --add <id>",
			Detail{Task: id},
			"no dep ids given",
		)
	}
	proposed := s.clone()
	target, _ := proposed.Find(id)
	target.Deps = dedupe(append(target.Deps, deps...))
	target.Updated = stamp(now)
	if err := proposed.Validate(); err != nil {
		return err
	}
	task.Deps = target.Deps
	task.Updated = stamp(now)
	return nil
}

// RemoveDeps removes deps from a task. Removing a dep that is not there is not
// an error: the command is idempotent by contract (§4.4).
func (s *State) RemoveDeps(id string, deps []string, now time.Time) *Error {
	task, found := s.Find(id)
	if !found {
		return TaskNotFound(id)
	}
	drop := make(map[string]bool, len(deps))
	for _, d := range deps {
		drop[d] = true
	}
	kept := make([]string, 0, len(task.Deps))
	for _, d := range task.Deps {
		if !drop[d] {
			kept = append(kept, d)
		}
	}
	task.Deps = kept
	task.Updated = stamp(now)
	return nil
}

// RemoveTask deletes tasks by id.
//
// A task other tasks depend on is refused rather than removed, because deleting
// it would turn those deps dangling and leave the workspace unloadable. A task
// named in the same call does not count as a dependent, so a whole chain can be
// removed from the leaves in one command. `task rm` has no force in v1: the way
// out is to cut the dependents' edges first, and a force flag here would only
// make it easy to lose a DAG by accident.
func (s *State) RemoveTask(ids []string) *Error {
	if len(ids) == 0 {
		return newError(
			envelope.CodeUsage,
			"ocaw task rm <id>",
			Detail{},
			"no task ids given",
		)
	}
	for _, id := range ids {
		if _, found := s.Find(id); !found {
			return TaskNotFound(id)
		}
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		drop[id] = true
	}
	for _, id := range ids {
		var dependents []string
		for _, dep := range s.Dependents(id) {
			if !drop[dep] {
				dependents = append(dependents, dep)
			}
		}
		if len(dependents) > 0 {
			// The blocking task is known, so the hint names it. This used to be a
			// template — `ocaw task dep <id> --rm <id>` — with a literal <id>, so an
			// agent following it ran a command that could not parse. It also read as
			// "drop a dependency called <id>", which is the wrong operation: the fix
			// is to drop the *dependent's* edge on this task.
			//
			// Any one of them breaks the hold, and the list order is the task order,
			// so the first is as good as any and needs no preference rule.
			return newError(
				envelope.CodeDepDangling,
				"ocaw task dep "+dependents[0]+" --rm "+id,
				Detail{Task: id, Blocking: dependents},
				"task %q is still depended on by: %s", id, strings.Join(dependents, ", "),
			)
		}
	}
	kept := make([]Task, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		if !drop[t.ID] {
			kept = append(kept, t)
		}
	}
	s.Tasks = kept
	return nil
}

// SetAcceptance replaces or updates the authored acceptance criteria by id.
func (s *State) SetAcceptance(items []Acceptance) *Error {
	byID := make(map[string]Acceptance, len(s.Workflow.Acceptance))
	for _, a := range s.Workflow.Acceptance {
		byID[a.ID] = a
	}
	order := make([]string, 0, len(s.Workflow.Acceptance))
	for _, a := range s.Workflow.Acceptance {
		order = append(order, a.ID)
	}
	for _, a := range items {
		if strings.TrimSpace(a.ID) == "" {
			return newError(
				envelope.CodeUsage,
				"ocaw workflow set --accept <text>",
				Detail{},
				"an acceptance item needs an id",
			)
		}
		if _, seen := byID[a.ID]; !seen {
			order = append(order, a.ID)
		}
		byID[a.ID] = a
	}
	merged := make([]Acceptance, 0, len(order))
	for _, id := range order {
		merged = append(merged, byID[id])
	}
	proposed := s.clone()
	proposed.Workflow.Acceptance = merged
	if err := proposed.checkAcceptance(); err != nil {
		return err
	}
	s.Workflow.Acceptance = merged
	return nil
}

// SetAcceptanceDone ticks or unticks one acceptance item.
func (s *State) SetAcceptanceDone(id string, done bool) *Error {
	for i := range s.Workflow.Acceptance {
		if s.Workflow.Acceptance[i].ID == id {
			s.Workflow.Acceptance[i].Done = done
			return nil
		}
	}
	return newError(
		envelope.CodeEntryNotFound,
		"ocaw workflow show",
		Detail{Task: id},
		"no acceptance item with id %q", id,
	)
}

// refuseIfIncomplete re-runs the invariants a single mutation can break, so a
// change that would leave an invalid state is refused before it is written
// rather than after the next load complains about a file ocaw itself produced.
//
// Duplicate ids, bad statuses and malformed gates cannot reach here: upsert is
// by id, AddTask validates the status, and gates are only written through
// SetGate on a task that already passed the shape checks.
func (s *State) refuseIfIncomplete(id string) *Error {
	if _, found := s.Find(id); !found {
		return TaskNotFound(id)
	}
	if err := s.checkDangling(); err != nil {
		return err
	}
	return s.checkAcyclic()
}

// clone deep-copies the state so a prospective mutation can be validated before
// it is committed. The copy shares no slice with the original, so a rejected
// proposal leaves the real state untouched.
func (s *State) clone() *State {
	out := *s
	out.Workflow.Constraints = append([]string{}, s.Workflow.Constraints...)
	out.Workflow.Acceptance = append([]Acceptance{}, s.Workflow.Acceptance...)
	out.Tasks = make([]Task, 0, len(s.Tasks))
	for _, t := range s.Tasks {
		copied := t
		copied.Deps = append([]string{}, t.Deps...)
		copied.Gates = append([]Gate{}, t.Gates...)
		out.Tasks = append(out.Tasks, copied)
	}
	return &out
}

// stamp renders a mutation time in the same form the rest of the state uses.
func stamp(now time.Time) string {
	return now.UTC().Format(time.RFC3339)
}

func mergeString(dst *string, v string) {
	if strings.TrimSpace(v) != "" {
		*dst = v
	}
}

func dedupe(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, v := range in {
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}
