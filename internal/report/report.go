// Package report renders WORKFLOW_STATE.md from state.json and detects when the
// two have diverged (SPEC.md §6.2).
//
// The file has two halves and the split is the whole design. Above the hash
// marker the text is authored — Request, Clarified Scope, Constraints,
// Acceptance Criteria — and is emitted from state.json byte for byte, so a
// sentence an agent wrote is never reflowed, trimmed, or escaped on the way
// out. Below the marker everything is derived, and a SHA-256 of that block
// travels inside the file. A hash that does not match the block is
// render_drift, which is a different failure from a stale render and gets a
// different message, because the fix is the same and the cause is not.
//
// Nothing here parses prose. The renderer reads state; the checker re-renders
// and compares bytes. Per SPEC §12, report must not import cli.
package report

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

// The section order is fixed by §6.2 and is part of the contract: a reader who
// has seen one WORKFLOW_STATE.md knows where to look in the next one.
const (
	titleHeading   = "# Workflow State"
	hashMarker     = "<!-- ocaw:rendered_sha256="
	hashMarkerEnd  = " -->"
	derivedHeading = "## Workflow DAG"
	notSet         = "_(not set)_"
	noneRecorded   = "_(none recorded)_"
)

// derivedColumns and verificationColumns pin the table headers, so a diff
// between two renders shows changed data and never a changed header.
const (
	derivedColumns     = "| ID | Task | Agent | Tier | Deps | Status |"
	derivedSeparator   = "|----|------|-------|------|------|--------|"
	verificationHeader = "| Task | Gate | Status | Exit | Attempts | Last run |"
	verifySeparator    = "|------|------|--------|------|----------|----------|"
)

// Render produces the whole document. The result is byte-identical for equal
// state: no map is ever ranged over to produce output, and every section is
// written unconditionally so an empty one cannot shift the ones after it
// (SPEC §9.5).
func Render(s *state.State) string {
	derived := renderDerived(s)
	var b strings.Builder

	b.WriteString(titleHeading + "\n\n")
	writeSection(&b, "## Request", s.Workflow.Request)
	writeSection(&b, "## Clarified Scope", s.Workflow.Scope)
	writeList(&b, "## Constraints", s.Workflow.Constraints)
	writeAcceptance(&b, s.Workflow.Acceptance)

	// The hash sits between the two halves, outside the block it covers, so a
	// document can carry its own digest without the digest covering itself.
	b.WriteString(hashMarker + hashOf(derived) + hashMarkerEnd + "\n\n")
	b.WriteString(derived)
	return b.String()
}

// renderDerived produces everything below the authored sections. It is a
// separate function because it is also what gets hashed, and hashing the whole
// document would make the digest depend on the text it is stored in.
func renderDerived(s *state.State) string {
	var b strings.Builder

	b.WriteString(derivedHeading + "\n")
	b.WriteString(derivedColumns + "\n")
	b.WriteString(derivedSeparator + "\n")
	for _, t := range s.Tasks {
		b.WriteString(taskRow(t) + "\n")
	}

	b.WriteString("\n## Subtask Registry\n")
	b.WriteString(derivedColumns + "\n")
	b.WriteString(derivedSeparator + "\n")
	for _, t := range s.Tasks {
		b.WriteString(taskRow(t) + "\n")
	}

	b.WriteString("\n## Verification\n")
	b.WriteString(verificationHeader + "\n")
	b.WriteString(verifySeparator + "\n")
	for _, t := range s.Tasks {
		for _, g := range t.Gates {
			b.WriteString(verificationRow(t.ID, g) + "\n")
		}
	}

	b.WriteString("\n## Budget\n")
	b.WriteString(budgetLine(s.Budget) + "\n")
	return b.String()
}

func taskRow(t state.Task) string {
	return "| " + strings.Join([]string{
		cell(t.ID),
		cell(t.Title),
		cell(t.Agent),
		cell(t.Tier),
		cell(deps(t.Deps)),
		cell(string(t.Status)),
	}, " | ") + " |"
}

func verificationRow(taskID string, g state.Gate) string {
	return "| " + strings.Join([]string{
		cell(taskID),
		cell(g.Name),
		cell(string(g.Status)),
		exitCell(g.LastExit),
		fmt.Sprintf("%d", g.Attempts),
		cell(stringOrDash(g.LastRun)),
	}, " | ") + " |"
}

func deps(list []string) string {
	if len(list) == 0 {
		return "-"
	}
	return strings.Join(list, ",")
}

func budgetLine(b state.Budget) string {
	if b.MaxTokens == 0 {
		// A zero budget means undeclared, not exhausted. Saying "0 / 0" would
		// read as a workspace that has spent everything.
		return fmt.Sprintf("tokens %d spent, no budget declared", b.SpentTokens)
	}
	return fmt.Sprintf("tokens %d / %d spent", b.SpentTokens, b.MaxTokens)
}

// cell makes a value safe to drop into a markdown table cell.
//
// A title containing a pipe would silently add a column, and one containing a
// newline would silently add a row — both corrupt the file for every reader
// while leaving state.json perfectly correct. Backslashes are escaped first so
// the escaping of a pipe cannot itself be escaped.
//
// Table cells only. Prose sections are emitted verbatim, because a pipe in a
// sentence is not a table delimiter and rewriting it loses the author's text.
func cell(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	v = strings.ReplaceAll(v, "|", "\\|")
	// A cell is one line by construction; fold any embedded newline to a space
	// rather than emitting a row the renderer did not intend.
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(v)
}

func exitCell(exit *int) string {
	if exit == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *exit)
}

func stringOrDash(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// writeSection emits a heading and one body paragraph. The body is written
// exactly as stored: no trimming, no reflow, no bullet conversion. Authored text
// surviving regeneration untouched is the property the authored half exists for.
func writeSection(b *strings.Builder, heading, body string) {
	b.WriteString(heading + "\n")
	if strings.TrimSpace(body) == "" {
		b.WriteString(notSet + "\n\n")
		return
	}
	b.WriteString(body + "\n\n")
}

// writeList emits the constraints as plain bullets.
//
// Not cell(): cell() escapes pipes so a value cannot add a column to a markdown
// table, and a bullet in a prose section is not a table cell. Escaping there
// rewrites the author's text — `a | pipe` comes back as `a \| pipe` — which is
// the data loss the authored half exists to prevent.
func writeList(b *strings.Builder, heading string, items []string) {
	b.WriteString(heading + "\n")
	if len(items) == 0 {
		b.WriteString(noneRecorded + "\n\n")
		return
	}
	for _, item := range items {
		b.WriteString("- " + item + "\n")
	}
	b.WriteString("\n")
}

// writeAcceptance keeps the stored id visible, because `ocaw workflow accept a1`
// addresses items by id and a reader who cannot see it cannot tick the right box.
// The text is not cell()-escaped, for the reason in writeList.
func writeAcceptance(b *strings.Builder, items []state.Acceptance) {
	b.WriteString("## Acceptance Criteria\n")
	if len(items) == 0 {
		b.WriteString(noneRecorded + "\n\n")
		return
	}
	for _, a := range items {
		mark := " "
		if a.Done {
			mark = "x"
		}
		b.WriteString(fmt.Sprintf("- [%s] %s  %s\n", mark, a.ID, a.Text))
	}
	b.WriteString("\n")
}

// hashOf is the digest of a derived block.
func hashOf(derived string) string {
	sum := sha256.Sum256([]byte(derived))
	return hex.EncodeToString(sum[:])
}

// Split separates a document into its authored half, its stored hash, and its
// derived block.
//
// The split is by the marker rather than by counting sections because the marker
// is the one thing in the file that states where the boundary is. A document
// without one did not come from ocaw.
func Split(doc string) (authored, hash string, derived string, ok bool) {
	markerAt := strings.Index(doc, hashMarker)
	if markerAt < 0 {
		return "", "", "", false
	}
	rest := doc[markerAt:]
	endOfMarker := strings.Index(rest, hashMarkerEnd)
	if endOfMarker < 0 {
		return "", "", "", false
	}
	hash = rest[len(hashMarker):endOfMarker]
	afterMarker := rest[endOfMarker+len(hashMarkerEnd):]
	headingAt := strings.Index(afterMarker, derivedHeading)
	if headingAt < 0 {
		return "", "", "", false
	}
	return doc[:markerAt], hash, afterMarker[headingAt:], true
}

// Status is the relationship between the file on disk and a fresh render.
type Status string

const (
	// StatusMissing means there is no WORKFLOW_STATE.md yet.
	StatusMissing Status = "missing"
	// StatusCurrent means the derived block is exactly what the state renders to.
	StatusCurrent Status = "current"
	// StatusStale means the state moved on since the last render.
	StatusStale Status = "stale"
	// StatusEdited means the derived block was changed by hand.
	StatusEdited Status = "edited"
	// StatusForeign means the file is not one ocaw generated.
	StatusForeign Status = "foreign"
)

// Check compares the file on disk against a fresh render of the state.
//
// Stale and edited are separated because the two hash comparisons fall out in
// order: the stored hash is checked against the block actually on disk first, so
// a hand-edit is named as a hand-edit rather than reported as a stale render.
// Both carry the same hint, because `ocaw report --write` is the fix for both,
// but an agent debugging a colleague's commit wants to know whether someone
// edited the file or the state simply advanced.
func Check(doc string, s *state.State) (Status, *envelope.Error) {
	if doc == "" {
		return StatusMissing, nil
	}
	_, stored, onDisk, ok := Split(doc)
	if !ok {
		return StatusForeign, envelope.Errorf(
			envelope.CodeRenderDrift,
			"ocaw report --write",
			"WORKFLOW_STATE.md was not generated by ocaw: it has no %s marker", strings.TrimSuffix(hashMarker, "<!-- "),
		)
	}
	if hashOf(onDisk) != stored {
		return StatusEdited, envelope.Errorf(
			envelope.CodeRenderDrift,
			"ocaw report --write",
			"the generated sections of WORKFLOW_STATE.md were edited by hand; the file no longer matches its own hash",
		)
	}
	if onDisk != renderDerived(s) {
		return StatusStale, envelope.Errorf(
			envelope.CodeRenderDrift,
			"ocaw report --write",
			"state.json has changed since WORKFLOW_STATE.md was last rendered",
		)
	}
	return StatusCurrent, nil
}

// Write regenerates WORKFLOW_STATE.md atomically and returns the document it
// wrote. A missing file is a normal first write, not drift.
func Write(l *workspace.Layout, s *state.State) (string, error) {
	doc := Render(s)
	if err := workspace.WriteFileAtomic(l.StateMDPath(), []byte(doc)); err != nil {
		return "", err
	}
	return doc, nil
}

// Load reads WORKFLOW_STATE.md. A workspace that has never rendered one reports
// no document and no error, which is a state the caller can act on rather than a
// failure to distinguish from one.
func Load(l *workspace.Layout) (string, bool, error) {
	raw, err := workspace.ReadFile(l.StateMDPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return string(raw), true, nil
}
