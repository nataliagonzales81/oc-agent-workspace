package envelope

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSuccessEnvelopeIsExactlyOneLine(t *testing.T) {
	r := Result{Command: "version", Data: map[string]any{"version": "dev"}}
	raw, err := r.Envelope().Marshal(false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	want := `{"ok":true,"command":"version","data":{"version":"dev"},"error":null,"warnings":[],"schema":"ocaw/version@1"}` + "\n"
	if got != want {
		t.Fatalf("envelope mismatch\n got: %s\nwant: %s", got, want)
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("expected exactly one newline, got %d", strings.Count(got, "\n"))
	}
}

func TestFailureEnvelopeShape(t *testing.T) {
	r := Result{
		Command: "task.show",
		Err:     NewError(CodeTaskNotFound, "no task with id 99", "ocaw task list"),
	}
	raw, err := r.Envelope().Marshal(false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(raw)
	want := `{"ok":false,"command":"task.show","data":null,` +
		`"error":{"code":"task_not_found","message":"no task with id 99","hint":"ocaw task list"},` +
		`"warnings":[],"schema":"ocaw/task.show@1"}` + "\n"
	if got != want {
		t.Fatalf("envelope mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestRoundTrip(t *testing.T) {
	in := Result{
		Command: "doctor",
		Data:    map[string]any{"counts": map[string]int{"error": 1, "warning": 0}},
		Warnings: []Warning{
			{Code: WarnFileExists, Message: "RULES.md already exists"},
		},
	}
	env := in.Envelope()
	out, err := env.RoundTrip()
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if out.OK != env.OK || out.Command != env.Command || out.Schema != env.Schema {
		t.Fatalf("scalars changed: %+v vs %+v", out, env)
	}
	if out.Err != nil {
		t.Fatalf("unexpected error in success envelope: %+v", out.Err)
	}
	if len(out.Warnings) != 1 || out.Warnings[0].Code != WarnFileExists {
		t.Fatalf("warnings lost: %+v", out.Warnings)
	}
	again, err := out.Marshal(false)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	first, err := env.Marshal(false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(again) != string(first) {
		t.Fatalf("not byte-identical after round trip:\n%s\n%s", first, again)
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	r := Result{Command: "status", Data: map[string]any{
		"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6, "g": 7, "h": 8,
	}}
	env := r.Envelope()
	first, err := env.Marshal(false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for i := 0; i < 64; i++ {
		again, err := env.Marshal(false)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(again) != string(first) {
			t.Fatalf("iteration %d differed", i)
		}
	}
}

func TestMarshalIsValidJSON(t *testing.T) {
	env := Result{
		Command: "report",
		Data:    map[string]any{"path": "a<b>c&d", "argv": []string{"go", "test", "./..."}},
	}.Envelope()
	raw, err := env.Marshal(false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("invalid JSON: %s", raw)
	}
	if !strings.Contains(string(raw), "a<b>c&d") {
		t.Fatalf("HTML escaping leaked into output: %s", raw)
	}
}

func TestPrettyIsOptIn(t *testing.T) {
	env := Result{Command: "version", Data: map[string]any{"ok": true}}.Envelope()
	raw, err := env.Marshal(true)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), "\n  \"ok\"") {
		t.Fatalf("pretty output not indented: %s", raw)
	}
	if !json.Valid(raw) {
		t.Fatalf("pretty output is invalid JSON: %s", raw)
	}
}

func TestSchemaID(t *testing.T) {
	cases := map[string]struct {
		command string
		major   int
		want    Schema
	}{
		"list":   {"task.list", 1, "ocaw/task.list@1"},
		"show":   {"task.show", 1, "ocaw/task.show@1"},
		"verify": {"verify.run", 1, "ocaw/verify.run@1"},
	}
	for name, tc := range cases {
		if got := SchemaID(tc.command, tc.major); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

func TestResultMajorDefaultsToSchemaMajor(t *testing.T) {
	env := Result{Command: "init"}.Envelope()
	if env.Schema != "ocaw/init@1" {
		t.Fatalf("got %q", env.Schema)
	}
	env = Result{Command: "init", Major: 1}.Envelope()
	if env.Schema != "ocaw/init@1" {
		t.Fatalf("got %q", env.Schema)
	}
}

func TestWarningsAreNeverNull(t *testing.T) {
	raw, err := Result{Command: "status"}.Envelope().Marshal(false)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"warnings":[]`) {
		t.Fatalf("warnings was not an empty array: %s", raw)
	}
}

// specExitTable is SPEC.md §8 transcribed. Changing it means changing the
// spec, which is a breaking change for every agent parsing exit codes.
var specExitTable = map[Category]int{
	"ok":                 0,
	"internal":           1,
	"usage":              2,
	"precondition":       3,
	"validation":         4,
	"invariant":          5,
	"lock_held":          6,
	"not_found":          7,
	"needs_confirmation": 8,
}

func TestExitTableMatchesSpec(t *testing.T) {
	for cat, want := range specExitTable {
		if got := cat.Exit(); got != want {
			t.Errorf("category %q: got exit %d want %d", cat, got, want)
		}
	}
}

func TestEveryErrorCodeHasAnExitCode(t *testing.T) {
	for _, code := range Codes() {
		if !KnownCode(code) {
			t.Errorf("code %q not known", code)
		}
		cat := code.Category()
		want, ok := specExitTable[cat]
		if !ok {
			t.Errorf("code %q maps to category %q which is not in the spec exit table", code, cat)
			continue
		}
		if got := code.Exit(); got != want {
			t.Errorf("code %q (category %q): got exit %d want %d", code, cat, got, want)
		}
	}
}

func TestUndeclaredCodeIsInternal(t *testing.T) {
	bogus := Code("no_such_code")
	if KnownCode(bogus) {
		t.Fatal("bogus code reported as known")
	}
	if got := bogus.Category(); got != CatInternal {
		t.Errorf("category: got %q want %q", got, CatInternal)
	}
	if got := bogus.Exit(); got != 1 {
		t.Errorf("exit: got %d want 1", got)
	}
}

func TestNilErrorIsSuccess(t *testing.T) {
	var e *Error
	if got := e.Exit(); got != 0 {
		t.Fatalf("got %d want 0", got)
	}
	env := Result{Command: "task.next"}.Envelope()
	if env.ExitCode() != 0 {
		t.Fatalf("empty ready queue must exit 0, got %d", env.ExitCode())
	}
	if !env.OK {
		t.Fatal("expected ok:true")
	}
}

func TestErrorCodesAreSortedAndNonEmpty(t *testing.T) {
	codes := Codes()
	if len(codes) == 0 {
		t.Fatal("no error codes declared")
	}
	for i := 1; i < len(codes); i++ {
		if codes[i-1] >= codes[i] {
			t.Fatalf("codes not sorted or duplicated at %d: %q %q", i, codes[i-1], codes[i])
		}
	}
}

func TestWarningCodesDisjointFromErrorCodes(t *testing.T) {
	errs := make(map[Code]bool)
	for _, c := range Codes() {
		errs[c] = true
	}
	for _, c := range WarningCodes() {
		if errs[c] {
			t.Errorf("code %q is both an error and a warning", c)
		}
	}
}

func TestErrorf(t *testing.T) {
	e := Errorf(CodeDepCycle, "ocaw task dep 1 --rm 3", "tasks %s -> %s", "1", "3")
	if e.Message != "tasks 1 -> 3" {
		t.Fatalf("got %q", e.Message)
	}
	if e.Code.Exit() != 5 {
		t.Fatalf("dep_cycle must exit 5, got %d", e.Code.Exit())
	}
}

// *Error must satisfy error so a package can return it from a helper without
// losing the code. The rendering is what reaches a human on stderr, so it is
// pinned: code and message, never the hint, which lives in its own field.
func TestErrorSatisfiesError(t *testing.T) {
	var err error = NewError(CodeLockHeld, "the workspace lock is held by pid 42 on host h since 2026-09-25T00:00:00Z", "retry later")
	if got := err.Error(); got != "lock_held: the workspace lock is held by pid 42 on host h since 2026-09-25T00:00:00Z" {
		t.Errorf("Error() = %q", got)
	}
	var nilErr *Error
	if got := nilErr.Error(); got != "<nil>" {
		t.Errorf("nil Error() = %q, want <nil>", got)
	}
	if !errors.Is(err, err) {
		t.Error("errors.Is must find the error itself")
	}
}
