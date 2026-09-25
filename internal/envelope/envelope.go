// Package envelope defines the ocaw JSON output contract (SPEC.md §4.2).
//
// Every ocaw command produces exactly one Envelope. The shape is frozen for
// the life of schema major 1: agents branch on Error.Code, never on the
// human-readable Message.
package envelope

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

// SchemaMajor is the current envelope schema major. It never decreases within
// a major series. A command may pin an older major; nothing may exceed this.
const SchemaMajor = 1

// Schema is a stable identifier of a command's data payload, for example
// "ocaw/task.list@1" (SPEC.md §4.2).
type Schema string

// SchemaID builds the stable schema identifier for a command.
func SchemaID(command string, major int) Schema {
	return Schema("ocaw/" + command + "@" + strconv.Itoa(major))
}

// Envelope is the single object every ocaw command emits (SPEC.md §4.2).
type Envelope struct {
	OK       bool      `json:"ok"`
	Command  string    `json:"command"`
	Data     any       `json:"data"`
	Err      *Error    `json:"error"`
	Warnings []Warning `json:"warnings"`
	Schema   Schema    `json:"schema"`
}

// Error is the payload of a failed envelope. Hint is a single actionable next
// command, not prose.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

// Error makes *Error an ordinary error so a command can return it from a
// helper that returns error without erasing the code. The rendering is the one
// an agent reads on stderr, and it deliberately omits the hint: the hint is a
// separate field of the envelope, and repeating it here invites a caller to
// parse prose.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	return string(e.Code) + ": " + e.Message
}

// Warning is a non-fatal observation. Warnings never change OK.
type Warning struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

// Category is the exit-code family an error code belongs to (SPEC.md §8).
type Category string

const (
	CatOK                Category = "ok"
	CatInternal          Category = "internal"
	CatUsage             Category = "usage"
	CatPrecondition      Category = "precondition"
	CatValidation        Category = "validation"
	CatInvariant         Category = "invariant"
	CatLockHeld          Category = "lock_held"
	CatNotFound          Category = "not_found"
	CatNeedsConfirmation Category = "needs_confirmation"
)

// exitCodeByCategory is the closed set of process exit codes (SPEC.md §8).
var exitCodeByCategory = map[Category]int{
	CatOK:                0,
	CatInternal:          1,
	CatUsage:             2,
	CatPrecondition:      3,
	CatValidation:        4,
	CatInvariant:         5,
	CatLockHeld:          6,
	CatNotFound:          7,
	CatNeedsConfirmation: 8,
}

// Exit returns the process exit code for a category. An unknown category is a
// bug in ocaw and is reported as internal rather than silently as success.
func (c Category) Exit() int {
	if code, ok := exitCodeByCategory[c]; ok {
		return code
	}
	return exitCodeByCategory[CatInternal]
}

// Code is a member of the closed error-code set (SPEC.md §4.3, §7).
type Code string

const (
	CodeInternal          Code = "internal"
	CodeUsage             Code = "usage"
	CodeUnknownCommand    Code = "unknown_command"
	CodeNotAProject       Code = "not_a_project"
	CodeWorkspaceMissing  Code = "workspace_not_initialized"
	CodeWriteFailed       Code = "write_failed"
	CodeInvalidPath       Code = "invalid_path"
	CodeValidationFailed  Code = "validation_failed"
	CodeDepsUnmet         Code = "deps_unmet"
	CodeDepCycle          Code = "dep_cycle"
	CodeDepDangling       Code = "dep_dangling"
	CodeInvalidTransition Code = "invalid_transition"
	CodeRenderDrift       Code = "render_drift"
	CodeLockHeld          Code = "lock_held"
	CodeTaskNotFound      Code = "task_not_found"
	CodeGateNotFound      Code = "gate_not_found"
	CodeEntryNotFound     Code = "entry_not_found"
	CodeNeedsConfirmation Code = "needs_confirmation"
	CodeVerifyFailed      Code = "verify_failed"
	CodeVerifyTimeout     Code = "verify_timeout"
)

// Warning codes. Warnings never carry a hint and never change OK.
const (
	WarnFileExists          Code = "file_exists"
	WarnTraversalTruncated  Code = "traversal_truncated"
	WarnVerifyCmdStale      Code = "verify_cmd_stale"
	WarnStaleLockBroken     Code = "stale_lock_broken"
	WarnRenderRegenerated   Code = "render_regenerated"
	WarnKnowledgeAnchorGone Code = "knowledge_anchor_missing"
	WarnTaskStuck           Code = "task_stuck"
)

// categoryByCode maps every declared code to its exit family. It is the single
// place where an error code acquires a process exit status.
var categoryByCode = map[Code]Category{
	CodeInternal:          CatInternal,
	CodeUsage:             CatUsage,
	CodeUnknownCommand:    CatUsage,
	CodeNotAProject:       CatPrecondition,
	CodeWorkspaceMissing:  CatPrecondition,
	CodeWriteFailed:       CatValidation,
	CodeInvalidPath:       CatValidation,
	CodeValidationFailed:  CatValidation,
	CodeVerifyFailed:      CatValidation,
	CodeVerifyTimeout:     CatValidation,
	CodeDepsUnmet:         CatInvariant,
	CodeDepCycle:          CatInvariant,
	CodeDepDangling:       CatInvariant,
	CodeInvalidTransition: CatInvariant,
	CodeRenderDrift:       CatInvariant,
	CodeLockHeld:          CatLockHeld,
	CodeTaskNotFound:      CatNotFound,
	CodeGateNotFound:      CatNotFound,
	CodeEntryNotFound:     CatNotFound,
	CodeNeedsConfirmation: CatNeedsConfirmation,
}

// KnownCode reports whether c is a member of the closed set.
func KnownCode(c Code) bool {
	_, ok := categoryByCode[c]
	return ok
}

// ExitTable returns the full exit-code table keyed by category name.
func ExitTable() map[string]int {
	out := make(map[string]int, len(exitCodeByCategory))
	for cat, code := range exitCodeByCategory {
		out[string(cat)] = code
	}
	return out
}

// Codes returns every declared error code, sorted, for help text and tests.
func Codes() []Code {
	out := make([]Code, 0, len(categoryByCode))
	for c := range categoryByCode {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// WarningCodes returns every declared warning code, sorted.
func WarningCodes() []Code {
	out := make([]Code, 0, len(warningCodes))
	for c := range warningCodes {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

var warningCodes = map[Code]struct{}{
	WarnFileExists:          {},
	WarnTraversalTruncated:  {},
	WarnVerifyCmdStale:      {},
	WarnStaleLockBroken:     {},
	WarnRenderRegenerated:   {},
	WarnKnowledgeAnchorGone: {},
	WarnTaskStuck:           {},
}

// Category returns the exit family for a code. An undeclared code is a bug in
// ocaw, so it falls back to internal rather than pretending to be success.
func (c Code) Category() Category {
	if cat, ok := categoryByCode[c]; ok {
		return cat
	}
	return CatInternal
}

// Exit returns the process exit code for a code.
func (c Code) Exit() int { return c.Category().Exit() }

// NewError builds an error payload.
func NewError(code Code, message, hint string) *Error {
	return &Error{Code: code, Message: message, Hint: hint}
}

// Errorf builds an error payload with a formatted message.
func Errorf(code Code, hint, format string, args ...any) *Error {
	return NewError(code, fmt.Sprintf(format, args...), hint)
}

// Exit returns the process exit code this error demands.
func (e *Error) Exit() int {
	if e == nil {
		return exitCodeByCategory[CatOK]
	}
	return e.Code.Exit()
}

// Result is the value a command implementation returns. It is converted to an
// Envelope exactly once, at the edge, so a command can never emit a shape that
// violates the contract.
type Result struct {
	Command  string
	Major    int
	Data     any
	Err      *Error
	Warnings []Warning
}

// Warn appends a warning to a result.
func (r *Result) Warn(code Code, message string) {
	r.Warnings = append(r.Warnings, Warning{Code: code, Message: message})
}

func (r Result) major() int {
	if r.Major == 0 {
		return SchemaMajor
	}
	return r.Major
}

// Envelope converts a result to the wire shape, normalising the fields that
// must always be present: warnings is an array, never null.
func (r Result) Envelope() Envelope {
	warnings := r.Warnings
	if warnings == nil {
		warnings = []Warning{}
	}
	return Envelope{
		OK:       r.Err == nil,
		Command:  r.Command,
		Data:     r.Data,
		Err:      r.Err,
		Warnings: warnings,
		Schema:   SchemaID(r.Command, r.major()),
	}
}

// ExitCode returns the process exit code for a result.
func (r Result) ExitCode() int { return r.Envelope().ExitCode() }

// ExitCode returns the process exit code an envelope demands: 0 when ok, and
// otherwise the exit family of its error code.
func (e Envelope) ExitCode() int {
	if e.Err == nil {
		return exitCodeByCategory[CatOK]
	}
	return e.Err.Exit()
}

// Marshal renders an envelope. The default is a single line so an agent can
// read it without a JSON parser; pretty output is opt-in. HTML escaping is
// off so paths and shell fragments survive verbatim, and the encoding is
// deterministic for identical input (SPEC.md §9.5).
func (e Envelope) Marshal(pretty bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if pretty {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(e); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RoundTrip validates that an envelope survives encoding/json unchanged in
// shape. It exists so the contract can be asserted in tests without a goldens
// harness.
func (e Envelope) RoundTrip() (Envelope, error) {
	raw, err := e.Marshal(false)
	if err != nil {
		return Envelope{}, err
	}
	var out Envelope
	if err := json.Unmarshal(raw, &out); err != nil {
		return Envelope{}, err
	}
	return out, nil
}
