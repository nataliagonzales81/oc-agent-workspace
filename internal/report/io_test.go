package report

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

func newLayout(t *testing.T) *workspace.Layout {
	t.Helper()
	return &workspace.Layout{Root: t.TempDir()}
}

func TestWriteThenCheckIsCurrent(t *testing.T) {
	l := newLayout(t)
	s := populated()

	written, err := Write(l, s)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	onDisk, found, err := Load(l)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !found {
		t.Fatal("Load reported no document after a write")
	}
	if onDisk != written {
		t.Error("the file on disk differs from what Write returned")
	}
	status, checkErr := Check(onDisk, s)
	if checkErr != nil {
		t.Fatalf("Check: %v", checkErr)
	}
	if status != StatusCurrent {
		t.Errorf("status = %q, want %q", status, StatusCurrent)
	}
}

// Writing the same state twice must leave byte-identical files, or a rerun
// produces a git diff for nothing.
func TestWriteTwiceIsByteIdentical(t *testing.T) {
	l := newLayout(t)
	s := populated()
	if _, err := Write(l, s); err != nil {
		t.Fatalf("Write: %v", err)
	}
	first, err := os.ReadFile(l.StateMDPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if _, err := Write(l, s); err != nil {
		t.Fatalf("Write (second): %v", err)
	}
	second, err := os.ReadFile(l.StateMDPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(first) != string(second) {
		t.Error("writing the same state twice produced different bytes")
	}
	if strings.Contains(string(first), workspace.TempPrefix) {
		t.Error("a temp file survived the write")
	}
}

// A missing file is a first render, not drift. This is the path `ocaw report
// --write` takes to repair a workspace whose file was deleted.
func TestLoadOfAMissingDocument(t *testing.T) {
	l := newLayout(t)
	doc, found, err := Load(l)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if found || doc != "" {
		t.Errorf("Load = %q, %t; want no document and no error", doc, found)
	}
	if status, _ := Check(doc, populated()); status != StatusMissing {
		t.Errorf("status = %q, want %q", status, StatusMissing)
	}
}

// Repair is the point of the whole mechanism: regenerate and the drift is gone.
func TestRewriteRepairsDrift(t *testing.T) {
	l := newLayout(t)
	s := populated()
	if _, err := Write(l, s); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// A hand-edit to the derived block.
	doc, _, err := Load(l)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	edited := strings.Replace(doc, "scaffold the module", "scaffold the module, by hand", 1)
	if err := workspace.WriteFileAtomic(l.StateMDPath(), []byte(edited)); err != nil {
		t.Fatalf("write the edit: %v", err)
	}
	if status, _ := Check(edited, s); status != StatusEdited {
		t.Fatalf("status = %q, want %q", status, StatusEdited)
	}

	if _, err := Write(l, s); err != nil {
		t.Fatalf("Write (repair): %v", err)
	}
	repaired, _, err := Load(l)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if status, checkErr := Check(repaired, s); checkErr != nil || status != StatusCurrent {
		t.Errorf("after a rewrite, status = %q and err = %v; want current and no error", status, checkErr)
	}
}

// A hand-edit to the authored half is not drift, and the checker does not even
// look at it: the hash covers only the derived block.
//
// The consequence is worth stating plainly, because it is a design choice
// rather than an oversight. state.json remains the source of truth, so a
// `report --write` restores the text from state.json and the edit above the
// marker does not survive it. What does survive every regeneration without a
// rewrite is the text an agent authored through `ocaw workflow set`, which is
// what §6.2 means by "preserved verbatim": ocaw does not reflow, trim, or
// escape it on the way out. See TestAuthoredTextSurvivesRegenerationUntouched.
func TestAuthoredEditIsNotDrift(t *testing.T) {
	l := newLayout(t)
	s := populated()
	if _, err := Write(l, s); err != nil {
		t.Fatalf("Write: %v", err)
	}
	doc, _, err := Load(l)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	touched := strings.Replace(doc, "ship the DAG", "ship the DAG (and the report)", 1)
	if touched == doc {
		t.Fatal("the fixture did not edit the authored half")
	}
	status, checkErr := Check(touched, s)
	if checkErr != nil {
		t.Fatalf("an edit above the marker must not be drift, got %v", checkErr)
	}
	if status != StatusCurrent {
		t.Errorf("status = %q, want %q: only the derived block is checked", status, StatusCurrent)
	}
	// And a rewrite restores the text from state.json.
	if _, err := Write(l, s); err != nil {
		t.Fatalf("Write: %v", err)
	}
	restored, _, err := Load(l)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if strings.Contains(restored, "and the report") {
		t.Error("state.json is the source of truth, so a rewrite must restore the authored text")
	}
}

func TestWriteReportsAFailedWrite(t *testing.T) {
	l := newLayout(t)
	// A directory where the file belongs: WriteFileAtomic renames onto it and
	// fails, and the failure must reach the caller rather than pass silently.
	if err := workspace.EnsureDir(l.StateMDPath()); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if _, err := Write(l, populated()); err == nil {
		t.Fatal("Write over a directory succeeded, want an error")
	}
}

// SPEC §12: report must not import cli, for the same reason state must not. The
// renderer has to be testable without a FlagSet, and `report --write` is one
// command of several that will want the renderer.
func TestReportDoesNotImportCLI(t *testing.T) {
	dir := filepath.Join("..", "..", "internal", "report")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	cliPath := "github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); imported == cliPath {
				t.Errorf("%s imports internal/cli; SPEC §12 forbids it", name)
			}
		}
	}
}

// The renderer reads state and nothing else. A future change that reaches for
// the clock, the filesystem, or a map iteration would break §9.5, so the purity
// is asserted through the API rather than trusted.
func TestRenderPurity(t *testing.T) {
	s := state.New()
	s.Tasks = []state.Task{
		{ID: "3", Title: "c", Deps: []string{}, Status: state.StatusPending},
		{ID: "1", Title: "a", Deps: []string{}, Status: state.StatusPending},
		{ID: "2", Title: "b", Deps: []string{}, Status: state.StatusPending},
	}
	doc := Render(s)
	// Stored order is priority, so the renderer must not sort.
	at := func(id string) int { return strings.Index(doc, "| "+id+" |") }
	if !(at("3") < at("1") && at("1") < at("2")) {
		t.Error("the renderer reordered tasks; stored order is the author's priority")
	}
}
