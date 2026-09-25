package state

import (
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// Detail is the machine-readable half of a state failure. It is what a command
// copies into envelope.data, so an agent acts on task ids and dependency ids
// instead of parsing a message (SPEC §4.2, §7: a rejected `in_progress` reports
// what is blocking, in data).
//
// Every field is omitempty because each failure kind carries only what applies
// to it; a `data` full of empty keys is noise an agent has to learn to ignore.
type Detail struct {
	Task string `json:"task,omitempty"`
	// Check names the invariant that refused the operation, for example
	// "unique_ids" or "acyclic".
	Check string `json:"check,omitempty"`
	// Blocking lists the deps that are not done or cancelled.
	Blocking []string `json:"blocking,omitempty"`
	// Cycle is the dependency path that closes a cycle, first id repeated last.
	Cycle []string `json:"cycle,omitempty"`
	// From and To are the two ends of a refused transition.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Gate names the gate a failure belongs to.
	Gate string `json:"gate,omitempty"`
	// TaskIDs and Checks are the full finding lists, for doctor.
	TaskIDs []string `json:"task_ids,omitempty"`
	Checks  []string `json:"checks,omitempty"`
}

// Empty reports whether the detail carries no information, in which case a
// command leaves envelope.data null rather than emitting an object full of empty
// keys.
func (d Detail) Empty() bool {
	return d.Task == "" && d.Check == "" && d.From == "" && d.To == "" &&
		d.Gate == "" && len(d.Blocking) == 0 && len(d.Cycle) == 0 &&
		len(d.TaskIDs) == 0 && len(d.Checks) == 0
}

// Error is a state-layer failure: the envelope payload an agent branches on,
// plus the structured detail the CLI puts in envelope.data.
//
// It holds the envelope error rather than embedding it because the two names
// collide — an embedded *envelope.Error would promote a field called Error over
// the Error() method that makes this an error at all. Composing keeps both
// reachable: Err is the wire payload, Detail is what lands in data.
type Error struct {
	// Err is the envelope error: its Code drives the exit family and its Hint
	// is the one command that recovers. Never nil for an Error built by
	// newError.
	Err *envelope.Error
	// Detail is the structured half, omitted from the message.
	Detail Detail
}

// newError builds a state error. The code is always a member of the closed set
// (SPEC §8); a state failure with an undeclared code would exit 1 and read as an
// ocaw bug rather than as the state problem it is.
func newError(code envelope.Code, hint string, detail Detail, format string, args ...any) *Error {
	if !envelope.KnownCode(code) {
		code = envelope.CodeInternal
	}
	return &Error{
		Err:    envelope.Errorf(code, hint, format, args...),
		Detail: detail,
	}
}

// Error renders the payload as "code: message". The hint stays out of it for the
// same reason it is out of envelope.Error: a hint is a separate field, and
// putting it in the string invites a caller to parse it.
func (e *Error) Error() string {
	if e == nil || e.Err == nil {
		return "<nil>"
	}
	return e.Err.Error()
}

// Envelope returns the wire payload for envelope.error.
func (e *Error) Envelope() *envelope.Error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Data returns the detail for envelope.data, or nil when there is nothing to add
// beyond the message.
func (e *Error) Data() any {
	if e == nil || e.Detail.Empty() {
		return nil
	}
	return e.Detail
}

// Exit returns the process exit code the failure demands (SPEC §8).
func (e *Error) Exit() int {
	if e == nil || e.Err == nil {
		return envelope.CodeInternal.Exit()
	}
	return e.Err.Exit()
}
