package report

import (
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
)

func populated() *state.State {
	s := state.New()
	s.Workflow.Request = "ship the DAG"
	s.Workflow.Scope = "the state package and its invariants"
	s.Workflow.Constraints = []string{"stdlib only", "no network"}
	s.Workflow.Acceptance = []state.Acceptance{
		{ID: "a1", Text: "init twice is clean", Done: true},
		{ID: "a2", Text: "drift is detected", Done: false},
	}
	zero, one := 0, 1
	ran := "2026-09-25T11:00:00Z"
	s.Tasks = []state.Task{
		{
			ID: "1", Title: "scaffold the module", Agent: "coder", Tier: "foundation",
			Deps: []string{}, Status: state.StatusDone, Updated: "2026-09-25T12:00:00Z",
			Gates: []state.Gate{{Name: "test", Cmd: "go test ./...", Status: state.GatePass, LastExit: &zero, Attempts: 1, LastRun: &ran}},
		},
		{
			ID: "2", Title: "the invariants", Agent: "coder", Tier: "state",
			Deps: []string{"1"}, Status: state.StatusPending, Updated: "2026-09-25T12:00:00Z",
			Gates: []state.Gate{{Name: "test", Cmd: "go test ./...", Status: state.GatePending, LastExit: &one, Attempts: 0, LastRun: nil}},
		},
	}
	s.Budget = state.Budget{MaxTokens: 200000, SpentTokens: 18432}
	return s
}

// The §6.2 section order is a contract. A reader who has seen one document knows
// where to look in the next, so a reordered section is a breaking change.
func TestRenderEmitsSectionsInSpecOrder(t *testing.T) {
	doc := Render(populated())
	want := []string{
		"# Workflow State",
		"## Request",
		"## Clarified Scope",
		"## Constraints",
		"## Acceptance Criteria",
		hashMarker,
		"## Workflow DAG",
		"## Subtask Registry",
		"## Verification",
		"## Budget",
	}
	at := -1
	for _, heading := range want {
		next := strings.Index(doc, heading)
		if next < 0 {
			t.Fatalf("missing %q in:\n%s", heading, doc)
		}
		if next < at {
			t.Errorf("%q appears out of order (at %d, after %d)", heading, next, at)
		}
		at = next
	}
	if strings.Count(doc, hashMarker) != 1 {
		t.Errorf("want exactly one hash marker, got %d", strings.Count(doc, hashMarker))
	}
}

func TestRenderTableShape(t *testing.T) {
	doc := Render(populated())
	for _, want := range []string{
		"| ID | Task | Agent | Tier | Deps | Status |",
		"|----|------|-------|------|------|--------|",
		"| 1 | scaffold the module | coder | foundation | - | done |",
		"| 2 | the invariants | coder | state | 1 | pending |",
		"| Task | Gate | Status | Exit | Attempts | Last run |",
		"| 1 | test | pass | 0 | 1 | 2026-09-25T11:00:00Z |",
		"| 2 | test | pending | 1 | 0 | - |",
		"tokens 18432 / 200000 spent",
		"- [x] a1  init twice is clean",
		"- [ ] a2  drift is detected",
		"- stdlib only",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("rendered document is missing %q:\n%s", want, doc)
		}
	}
}

// §9.5: same state in, byte-identical out. Map iteration never reaches the
// output, and no clock is read while rendering.
func TestRenderIsByteIdenticalAcrossRuns(t *testing.T) {
	s := populated()
	first := Render(s)
	for range 5 {
		if got := Render(s); got != first {
			t.Fatal("two renders of the same state differ (SPEC §9.5)")
		}
	}
	// A second, independently built equal state must render identically too.
	other := populated()
	if got := Render(other); got != first {
		t.Error("two equal states rendered differently")
	}
}

// The hash covers the derived block and not the authored half, so authoring a
// sentence does not invalidate the digest, and the digest does not cover itself.
func TestHashCoversOnlyTheDerivedBlock(t *testing.T) {
	s := populated()
	doc := Render(s)
	authored, stored, derived, ok := Split(doc)
	if !ok {
		t.Fatal("Split did not recognise a document ocaw rendered")
	}
	if !strings.Contains(authored, "ship the DAG") {
		t.Error("the authored half must carry the request")
	}
	if strings.Contains(authored, derivedHeading) {
		t.Error("the authored half must not contain the derived heading")
	}
	if !strings.HasPrefix(derived, derivedHeading) {
		t.Errorf("the derived block must start at the derived heading, got %q", derived[:40])
	}
	if hashOf(derived) != stored {
		t.Error("the stored hash is not the hash of the derived block")
	}

	// Re-authoring the request leaves the digest alone.
	s.Workflow.Request = "a different request"
	if hashOf(renderDerived(s)) != stored {
		t.Error("the derived hash changed when only the authored half changed")
	}
	// Moving a task does change it.
	s.Tasks[0].Status = state.StatusInProgress
	if hashOf(renderDerived(s)) == stored {
		t.Error("the derived hash did not change when the state did")
	}
}

// A hand-edit inside the derived block is render_drift, not a warning: the file
// is generated, and an edited one is describing work that did not happen.
func TestHandEditToADerivedSectionIsRenderDrift(t *testing.T) {
	s := populated()
	doc := Render(s)

	edited := strings.Replace(doc, "| 1 | scaffold the module |", "| 1 | scaffold the module (done by hand) |", 1)
	if edited == doc {
		t.Fatal("the fixture did not edit the table")
	}
	status, err := Check(edited, s)
	if got := codeOf(t, err); got != string(envelope.CodeRenderDrift) {
		t.Errorf("code = %s, want %s", got, envelope.CodeRenderDrift)
	}
	if err.Hint != "ocaw report --write" {
		t.Errorf("hint = %q, want %q", err.Hint, "ocaw report --write")
	}
	if status != StatusEdited {
		t.Errorf("status = %q, want %q", status, StatusEdited)
	}
}

// Stale and edited are separated. Both are fixed by the same command, but an
// agent looking at a colleague's commit needs to know whether the file was edited
// or the state simply moved on.
func TestStaleStateIsDistinctFromAHandEdit(t *testing.T) {
	s := populated()
	doc := Render(s)

	// A hand edit: the stored hash no longer matches the bytes on disk.
	edited := strings.Replace(doc, "## Budget", "## Budget (hand written)", 1)
	if status, _ := Check(edited, s); status != StatusEdited {
		t.Errorf("status = %q, want %q", status, StatusEdited)
	}

	// Stale: the file is internally consistent, but the state has moved on.
	s.Tasks[0].Status = state.StatusInProgress
	status, err := Check(doc, s)
	if status != StatusStale {
		t.Errorf("status = %q, want %q", status, StatusStale)
	}
	if got := codeOf(t, err); got != string(envelope.CodeRenderDrift) {
		t.Errorf("code = %s, want %s", got, envelope.CodeRenderDrift)
	}
	if err.Hint != "ocaw report --write" {
		t.Errorf("hint = %q, want %q", err.Hint, "ocaw report --write")
	}
}

func TestCheckOfAnUnchangedDocumentIsCurrent(t *testing.T) {
	s := populated()
	status, err := Check(Render(s), s)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if status != StatusCurrent {
		t.Errorf("status = %q, want %q", status, StatusCurrent)
	}
}

func TestCheckOfAMissingDocument(t *testing.T) {
	status, err := Check("", populated())
	if err != nil {
		t.Fatalf("a missing file is a first render, not drift: %v", err)
	}
	if status != StatusMissing {
		t.Errorf("status = %q, want %q", status, StatusMissing)
	}
}

// A file that is not ocaw's has no marker, so there is nothing to check it
// against. Saying so beats reporting a hash mismatch that means nothing.
func TestCheckOfAForeignDocument(t *testing.T) {
	const hand = "# Workflow State\n\n## Request\nwritten by a human\n\n## Notes\nfree prose\n"
	status, err := Check(hand, populated())
	if got := codeOf(t, err); got != string(envelope.CodeRenderDrift) {
		t.Errorf("code = %s, want %s", got, envelope.CodeRenderDrift)
	}
	if status != StatusForeign {
		t.Errorf("status = %q, want %q", status, StatusForeign)
	}
	if !strings.Contains(err.Message, "rendered_sha256") {
		t.Errorf("message = %q, want it to name what is missing", err.Message)
	}
}

// The authored half is emitted byte for byte. A sentence an agent wrote is never
// reflowed, trimmed, or bullet-converted on the way out — that is the property
// the authored half exists for.
func TestAuthoredTextSurvivesRegenerationUntouched(t *testing.T) {
	s := state.New()
	// Deliberately awkward: a pipe, a hash, leading indentation, a trailing
	// space, a fenced block, an em dash, and CJK.
	s.Workflow.Request = "  Ship | the #1 DAG.\n\n  ```bash\n  ocaw task next\n  ```\n\n  Then stop —  \n"
	s.Workflow.Scope = "  - a bullet the author wrote\n  - and a second\n"
	s.Workflow.Constraints = []string{"a | pipe", "  leading space", "trailing space  "}
	s.Workflow.Acceptance = []state.Acceptance{{ID: "a1", Text: "keep | me", Done: false}}

	first := Render(s)
	if !strings.Contains(first, "Ship | the #1 DAG.") {
		t.Errorf("the request was altered:\n%s", first)
	}
	if !strings.Contains(first, "  ocaw task next") {
		t.Errorf("indentation inside a fenced block was lost:\n%s", first)
	}
	if !strings.Contains(first, "  - a bullet the author wrote") {
		t.Errorf("a bullet in the scope was converted:\n%s", first)
	}
	if !strings.Contains(first, "Then stop —  \n") {
		t.Errorf("a trailing space or the em dash was normalised:\n%s", first)
	}
	for i := range 3 {
		if got := Render(s); got != first {
			t.Fatalf("regeneration %d changed the authored text", i)
		}
	}
}

// A table cell cannot hold a pipe or a newline: a pipe would silently add a
// column and a newline a row, corrupting the file for every reader while leaving
// state.json perfectly correct.
func TestCellValuesCannotCorruptATable(t *testing.T) {
	s := state.New()
	s.Tasks = []state.Task{{
		ID: "1", Title: "a | b", Agent: "coder", Tier: "t1\nt2", Deps: []string{},
		Status: state.StatusPending,
		Notes:  "back\\slash",
	}}
	doc := Render(s)

	row := ""
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "| 1 |") {
			row = line
		}
	}
	if row == "" {
		t.Fatalf("no row for task 1:\n%s", doc)
	}
	if cells := countUnescapedPipes(row); cells != 7 {
		t.Errorf("row has %d unescaped pipes, want 7 (six columns plus the outer pair)\n%s", cells, row)
	}
	if !strings.Contains(row, `a \| b`) {
		t.Errorf("the pipe in the title was not escaped: %s", row)
	}
	if strings.Contains(row, "t1\nt2") {
		t.Error("a newline in a cell stayed a newline")
	}
	if !strings.Contains(row, "t1 t2") {
		t.Errorf("a newline in a cell was not folded to a space: %s", row)
	}
}

// countUnescapedPipes counts the cell delimiters, ignoring the ones a value
// escaped. This is what a markdown reader does, so it is what the test must do.
func countUnescapedPipes(line string) int {
	count, escaped := 0, false
	for _, r := range line {
		switch {
		case escaped:
			escaped = false
		case r == '\\':
			escaped = true
		case r == '|':
			count++
		}
	}
	return count
}

// An empty state still renders every section, so the two tables are never
// missing a header a reader depends on.
func TestRenderOfAnEmptyState(t *testing.T) {
	doc := Render(state.New())
	for _, want := range []string{
		notSet, noneRecorded, derivedHeading, "## Subtask Registry",
		"## Verification", "## Budget", "no budget declared",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("an empty state must still render %q:\n%s", want, doc)
		}
	}
	// A zero budget is undeclared, not exhausted.
	if strings.Contains(doc, "0 / 0") {
		t.Errorf("a zero budget rendered as exhausted:\n%s", doc)
	}
}

func codeOf(t *testing.T, err *envelope.Error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !envelope.KnownCode(err.Code) {
		t.Errorf("code %q is not a member of the closed set", err.Code)
	}
	return string(err.Code)
}

// cell() escapes pipes so a table cell cannot add a column. Applying it to a
// prose section rewrites the author's sentence — `a | pipe` came back as
// `a \| pipe` — which is the data loss the authored half exists to prevent. The
// constraints and acceptance criteria are bullets, not cells.
func TestProseSectionsAreNotCellEscaped(t *testing.T) {
	s := state.New()
	s.Workflow.Constraints = []string{"a | pipe, a #hash, and  trailing space  "}
	s.Workflow.Acceptance = []state.Acceptance{{ID: "a1", Text: "keep | me | this"}}

	doc := Render(s)
	if !strings.Contains(doc, "- a | pipe, a #hash, and  trailing space  ") {
		t.Errorf("the constraint was escaped as if it were a table cell:\n%s", doc)
	}
	if !strings.Contains(doc, "- [ ] a1  keep | me | this") {
		t.Errorf("the acceptance text was escaped as if it were a table cell:\n%s", doc)
	}
	if strings.Contains(doc, `\|`) {
		t.Errorf("a pipe was escaped somewhere in a prose section:\n%s", doc)
	}

	// And a table cell is still protected, or a task title would add a column.
	s.Tasks = []state.Task{{ID: "1", Title: "a | b", Deps: []string{}, Status: state.StatusPending}}
	doc = Render(s)
	if !strings.Contains(doc, `a \| b`) {
		t.Errorf("a table cell is no longer protected:\n%s", doc)
	}
}
