package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
)

const (
	workflowCommandName = "workflow"
	reportCommandName   = "report"
)

type workflowPayload struct {
	Subcommand string `json:"subcommand"`
	Workflow   struct {
		Request     string   `json:"request"`
		Scope       string   `json:"scope"`
		Constraints []string `json:"constraints"`
	} `json:"workflow"`
	Acceptance  []state.Acceptance `json:"acceptance"`
	Render      string             `json:"render"`
	Accepted    string             `json:"accepted"`
	AcceptedSet bool               `json:"accepted_set"`
	Changed     []string           `json:"changed"`
	Written     string             `json:"written"`
	DryRun      bool               `json:"dry_run"`
	Notes       []string           `json:"notes"`
	Detail      map[string]any     `json:"detail"`
}

type reportPayload struct {
	Format     string   `json:"format"`
	Path       string   `json:"path"`
	Written    bool     `json:"written"`
	Bytes      int      `json:"bytes"`
	Markdown   string   `json:"markdown"`
	Prev       string   `json:"previous_status"`
	Status     string   `json:"status"`
	Drifted    bool     `json:"drifted"`
	Tasks      int      `json:"tasks"`
	Acceptance int      `json:"acceptance"`
	Mode       string   `json:"mode"`
	DryRun     bool     `json:"dry_run"`
	Notes      []string `json:"notes"`
}

func cliRun(t *testing.T, dir string, args ...string) (int, envelope.Envelope, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(append([]string{"--dir", dir}, args...), &out, &errOut, false)
	raw := bytes.TrimSpace(out.Bytes())
	var env envelope.Envelope
	if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("%v: stdout is not a JSON envelope: %v\n%s", args, err, raw)
		}
	}
	return code, env, out.String(), errOut.String()
}

func workflowRun(t *testing.T, dir string, args ...string) (int, envelope.Envelope, workflowPayload) {
	t.Helper()
	code, env, _, _ := cliRun(t, dir, append([]string{workflowCommandName}, args...)...)
	if env.Command != workflowCommandName {
		t.Fatalf("command = %q, want %q", env.Command, workflowCommandName)
	}
	return code, env, decodeWorkflow(t, env)
}

func decodeWorkflow(t *testing.T, env envelope.Envelope) workflowPayload {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var p workflowPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("workflow payload shape: %v\n%s", err, raw)
	}
	return p
}

func mustWorkflow(t *testing.T, dir string, args ...string) workflowPayload {
	t.Helper()
	code, env, data := workflowRun(t, dir, args...)
	if code != 0 {
		t.Fatalf("ocaw workflow %v: exit %d: %+v", args, code, env.Err)
	}
	return data
}

func reportRunCLITest(t *testing.T, dir string, args ...string) (int, envelope.Envelope, reportPayload, string) {
	t.Helper()
	code, env, out, _ := cliRun(t, dir, append([]string{reportCommandName}, args...)...)
	if env.Command != reportCommandName {
		t.Fatalf("command = %q, want %q", env.Command, reportCommandName)
	}
	var p reportPayload
	if raw, err := json.Marshal(env.Data); err == nil {
		_ = json.Unmarshal(raw, &p)
	}
	return code, env, p, out
}

func mustReport(t *testing.T, dir string, args ...string) reportPayload {
	t.Helper()
	code, env, p, _ := reportRunCLITest(t, dir, args...)
	if code != 0 {
		t.Fatalf("ocaw report %v: exit %d: %+v", args, code, env.Err)
	}
	return p
}

func acceptanceByID(items []state.Acceptance, id string) (state.Acceptance, bool) {
	for _, a := range items {
		if a.ID == id {
			return a, true
		}
	}
	return state.Acceptance{}, false
}

// A field the caller did not mention must survive. The footgun this prevents is
// `ocaw workflow set --request X` silently wiping the constraints.
func TestWorkflowSetTouchesOnlyTheFlagsGiven(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustWorkflow(t, dir, "set",
		"--request", "ship the DAG",
		"--scope", "the state package",
		"--constraint", "stdlib only",
		"--constraint", "no network",
		"--accept", "a1=init twice is clean",
		"--accept", "a2=drift is detected",
	)

	// A request on its own leaves everything else alone.
	data := mustWorkflow(t, dir, "set", "--request", "ship the DAG and the report")
	if data.Workflow.Request != "ship the DAG and the report" {
		t.Errorf("request = %q", data.Workflow.Request)
	}
	if data.Workflow.Scope != "the state package" {
		t.Errorf("scope = %q, want it preserved", data.Workflow.Scope)
	}
	if len(data.Workflow.Constraints) != 2 {
		t.Errorf("constraints = %v, want both preserved", data.Workflow.Constraints)
	}
	if len(data.Acceptance) != 2 {
		t.Errorf("acceptance = %v, want both preserved", data.Acceptance)
	}
	if strings.Join(data.Changed, ",") != "request" {
		t.Errorf("changed = %v, want [request]", data.Changed)
	}
}

// A flag given with an empty value means "clear it", which is why the
// implementation asks which flags were typed rather than what they hold.
func TestWorkflowSetCanClearAField(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustWorkflow(t, dir, "set", "--request", "something", "--scope", "something else")

	data := mustWorkflow(t, dir, "set", "--scope", "")
	if data.Workflow.Scope != "" {
		t.Errorf("scope = %q, want it cleared", data.Workflow.Scope)
	}
	if data.Workflow.Request != "something" {
		t.Errorf("request = %q, want it untouched", data.Workflow.Request)
	}
	// The generated file agrees.
	doc := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md"))
	if strings.Contains(doc, "something else") {
		t.Error("the cleared scope is still in the rendered file")
	}
}

func TestWorkflowAcceptanceIDs(t *testing.T) {
	dir := initialisedTaskRepo(t)
	// A bare string gets the next id, continuing from the highest in use.
	mustWorkflow(t, dir, "set", "--accept", "first")
	mustWorkflow(t, dir, "set", "--accept", "second")
	data := mustWorkflow(t, dir, "set", "--accept", "third")
	if len(data.Acceptance) != 3 {
		t.Fatalf("acceptance = %v, want three", data.Acceptance)
	}
	for i, want := range []string{"a1", "a2", "a3"} {
		if data.Acceptance[i].ID != want {
			t.Errorf("acceptance[%d].id = %q, want %q", i, data.Acceptance[i].ID, want)
		}
	}
	// A value whose left side is already an id updates that item; one whose left
	// side is not is text and gets the next id. See the note on
	// acceptanceIDPattern for why the rule is that tight.
	data = mustWorkflow(t, dir, "set", "--accept", "a2=second, revised")
	if a, ok := acceptanceByID(data.Acceptance, "a2"); !ok || a.Text != "second, revised" {
		t.Errorf("a2 = %+v, want it revised in place", a)
	}
	data = mustWorkflow(t, dir, "set", "--accept", "smoke=the smoke test passes")
	if a, ok := acceptanceByID(data.Acceptance, "a4"); !ok {
		t.Errorf("no item with the next assigned id: %v", data.Acceptance)
	} else if a.Text != "smoke=the smoke test passes" {
		t.Errorf("text = %q, want the whole value kept as text", a.Text)
	}
	// An id that already exists updates in place rather than duplicating.
	data = mustWorkflow(t, dir, "set", "--accept", "a1=first, revised")
	if len(data.Acceptance) != 4 {
		t.Errorf("acceptance = %v, want the existing item updated in place", data.Acceptance)
	}
	if a, _ := acceptanceByID(data.Acceptance, "a1"); a.Text != "first, revised" {
		t.Errorf("a1 text = %q, want the revision", a.Text)
	}
}

// An `=` in real prose must not be mistaken for an id separator. The left side
// has to look like an id before it is treated as one.
func TestAcceptanceTextMayContainAnEqualsSign(t *testing.T) {
	dir := initialisedTaskRepo(t)
	data := mustWorkflow(t, dir, "set", "--accept", "a=1 && b=2 must both hold")
	if len(data.Acceptance) != 1 {
		t.Fatalf("acceptance = %v", data.Acceptance)
	}
	if data.Acceptance[0].ID != "a1" {
		t.Errorf("id = %q, want an assigned one: the left side is not an id", data.Acceptance[0].ID)
	}
	if data.Acceptance[0].Text != "a=1 && b=2 must both hold" {
		t.Errorf("text = %q, want it whole", data.Acceptance[0].Text)
	}
}

func TestWorkflowAcceptTicking(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustWorkflow(t, dir, "set", "--accept", "a1=first", "--accept", "a2=second")

	data := mustWorkflow(t, dir, "accept", "a1")
	if !data.AcceptedSet || data.Accepted != "a1" {
		t.Errorf("accepted = %q, accepted_set = %t", data.Accepted, data.AcceptedSet)
	}
	if a, _ := acceptanceByID(data.Acceptance, "a1"); !a.Done {
		t.Error("a1 was not ticked")
	}
	if a, _ := acceptanceByID(data.Acceptance, "a2"); a.Done {
		t.Error("a2 was ticked by accident")
	}

	// --not-done unticks.
	data = mustWorkflow(t, dir, "accept", "a1", "--not-done")
	if a, _ := acceptanceByID(data.Acceptance, "a1"); a.Done {
		t.Error("--not-done did not untick a1")
	}

	// An unknown id is not found, and names itself in data.
	code, env, _ := workflowRun(t, dir, "accept", "nope")
	if code != 7 {
		t.Fatalf("exit = %d, want 7 (not_found); error = %+v", code, env.Err)
	}
	if env.Err.Code != envelope.CodeEntryNotFound {
		t.Errorf("code = %q, want entry_not_found", env.Err.Code)
	}
	raw, _ := json.Marshal(env.Data)
	if !strings.Contains(string(raw), "nope") {
		t.Errorf("data does not name the id: %s", raw)
	}
}

// The AC the issue is really about: authored text must survive a full
// regeneration byte for byte. Anything less is data loss, and the test
// deliberately uses text that every reasonable renderer would reformat.
func TestAuthoredTextSurvivesAReportWriteByteForByte(t *testing.T) {
	dir := initialisedTaskRepo(t)
	const request = "  Ship | the #1 DAG.\n\n  ```bash\n  ocaw task next\n  ```\n\n  Then stop -  \n"
	const scope = "  - a bullet the author wrote\n\tand a tab-indented line\n"
	constraint := "a | pipe, a #hash, and  trailing space  "
	mustWorkflow(t, dir, "set", "--request", request, "--scope", scope, "--constraint", constraint)
	mustWorkflow(t, dir, "set", "--accept", "a1=keep | me")

	// Change the derived half so the file must be rewritten.
	mustTask(t, dir, "add", "--id", "1", "--title", "a title with | a pipe")
	before := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md"))

	for i := range 3 {
		if code, env := reportRunCLITestErr(t, dir, "--write"); code != 0 {
			t.Fatalf("report --write %d: exit %d: %+v", i, code, env.Err)
		}
		if got := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md")); got != before {
			t.Fatalf("regeneration %d changed the file:\n--- before ---\n%s\n--- after ---\n%s", i, before, got)
		}
	}
	// And the specific fragments are all still there, not merely the hash.
	doc := before
	// Note the unescaped pipes: a bullet is prose, not a table cell, so the
	// author's text is emitted exactly as written.
	for _, want := range []string{request, scope, constraint, "keep | me"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the rendered file lost %q:\n%s", want, doc)
		}
	}
}

func reportRunCLITestErr(t *testing.T, dir string, args ...string) (int, envelope.Envelope) {
	t.Helper()
	code, env, _, _ := cliRun(t, dir, append([]string{reportCommandName}, args...)...)
	return code, env
}

// The documented repair for the error #7 raises must actually clear it.
func TestReportWriteClearsTheRenderDriftFromDoctorsAC7(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	mustTask(t, dir, "add", "--id", "2", "--title", "second", "--dep", "1")

	// A hand-edit inside the derived table, exactly as AC7 does.
	path := filepath.Join(dir, "WORKFLOW_STATE.md")
	edited := strings.Replace(mustRead(t, path), "| 1 | first |", "| 1 | first, by hand |", 1)
	if edited == mustRead(t, path) {
		t.Fatal("the fixture did not edit the derived table")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	// doctor reports it.
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, doctorCommandName}, &out, &errOut, false); code != 4 {
		t.Fatalf("doctor on a drifted file: exit %d, want 4\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "render_drift") {
		t.Errorf("doctor did not report render_drift:\n%s", out.String())
	}
	// Its hint points at the repair.
	if !strings.Contains(out.String(), "ocaw report --write") {
		t.Errorf("doctor's hint does not name the repair:\n%s", out.String())
	}

	// report --write clears it, and doctor is happy again.
	p := mustReport(t, dir, "--write")
	if !p.Written {
		t.Error("data.written = false after --write")
	}
	if p.Prev != "edited" {
		t.Errorf("previous_status = %q, want edited: the repair should say what it replaced", p.Prev)
	}
	if len(p.Notes) == 0 || !strings.Contains(strings.Join(p.Notes, " "), "edited by hand") {
		t.Errorf("notes = %v, want a warning that the hand edit is gone", p.Notes)
	}
	out.Reset()
	if code := cli.Main([]string{"--dir", dir, doctorCommandName}, &out, &errOut, false); code != 0 {
		t.Errorf("doctor after the repair: exit %d\n%s", code, out.String())
	}
}

// --output writes the file and emits nothing on stdout (§4.1).
func TestReportOutputEmitsNoStdout(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustWorkflow(t, dir, "set", "--request", "ship it")
	dest := filepath.Join(dir, "out.md")

	var out, errOut bytes.Buffer
	code := cli.Main([]string{"--dir", dir, reportCommandName, "--format", "md", "--output", dest}, &out, &errOut, false)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Errorf("stdout was not empty:\n%s", out.String())
	}
	written := mustRead(t, dest)
	if !strings.Contains(written, "ship it") {
		t.Errorf("the file does not hold the rendered markdown:\n%s", written)
	}
}

// --format md emits the markdown; --format json (the default) emits the envelope.
func TestReportFormats(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustWorkflow(t, dir, "set", "--request", "ship it")

	// md: raw markdown, not a JSON envelope.
	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, reportCommandName, "--format", "md"}, &out, &errOut, false); code != 0 {
		t.Fatalf("exit %d", code)
	}
	body := out.String()
	if !strings.HasPrefix(body, "# Workflow State") {
		t.Errorf("--format md did not emit the markdown:\n%s", body)
	}
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		t.Error("--format md emitted a JSON envelope")
	}
	if !strings.Contains(body, "ship it") {
		t.Error("the markdown does not hold the request")
	}

	// json: the envelope, with the markdown available as a field.
	p := mustReport(t, dir)
	if p.Format != "json" {
		t.Errorf("default format = %q, want json", p.Format)
	}
	if !strings.HasPrefix(p.Markdown, "# Workflow State") {
		t.Errorf("data.markdown is not the rendered document:\n%s", p.Markdown)
	}
	if p.Bytes != len(p.Markdown) {
		t.Errorf("bytes = %d, len(markdown) = %d", p.Bytes, len(p.Markdown))
	}

	// An unknown format is a usage error naming the set.
	code, env, _, _ := cliRun(t, dir, reportCommandName, "--format", "html")
	if code != 2 {
		t.Errorf("exit = %d, want 2 (usage)", code)
	}
	if env.Err == nil || !strings.Contains(env.Err.Message, "md") {
		t.Errorf("error = %+v, want it to name the formats", env.Err)
	}
}

// report is read-only without --write; --write takes the lock.
func TestReportLockBehaviour(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	before := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md"))

	lock := filepath.Join(dir, ".agent", "state", "lock")
	live := fmt.Sprintf(`{"pid":%d,"host":"somewhere-else","time":%q}`,
		os.Getpid(), time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(lock, []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}

	// A read-only report works while someone else holds the lock.
	if code, env, _, _ := reportRunCLITest(t, dir); code != 0 {
		t.Errorf("read-only report with the lock held: exit %d, want 0: %+v", code, env.Err)
	}
	if _, err := os.Stat(lock); err != nil {
		t.Error("the read-only report took the lock")
	}
	if got := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md")); got != before {
		t.Error("a read-only report changed the file")
	}

	// --write refuses, because it is a mutation.
	code, env, _, _ := reportRunCLITest(t, dir, "--write")
	if code != 6 {
		t.Errorf("report --write with the lock held: exit = %d, want 6; error = %+v", code, env.Err)
	}
	if env.Err == nil || env.Err.Code != envelope.CodeLockHeld {
		t.Errorf("code = %v, want lock_held", env.Err)
	}
}

// --dry-run on workflow set changes nothing, and says what it would have.
func TestWorkflowSetDryRunChangesNothing(t *testing.T) {
	dir := initialisedTaskRepo(t)
	before := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md"))
	stateBefore := mustRead(t, filepath.Join(dir, ".agent", "state", "state.json"))

	code, env, data := workflowRun(t, dir, "set", "--request", "would be this", "--accept", "a1=x", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d: %+v", code, env.Err)
	}
	if !data.DryRun {
		t.Error("data.dry_run = false")
	}
	if strings.Join(data.Changed, ",") != "request,acceptance" {
		t.Errorf("changed = %v, want the plan even on a dry run", data.Changed)
	}
	if got := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md")); got != before {
		t.Error("the dry run changed WORKFLOW_STATE.md")
	}
	if got := mustRead(t, filepath.Join(dir, ".agent", "state", "state.json")); got != stateBefore {
		t.Error("the dry run changed state.json")
	}
	// And the same command for real does change it.
	mustWorkflow(t, dir, "set", "--request", "would be this")
	if got := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md")); got == before {
		t.Error("the real run changed nothing, so the dry-run test proves nothing")
	}
}

func TestReportDryRunWritesNothing(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	before := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md"))

	p := mustReport(t, dir, "--write", "--dry-run")
	if p.Written {
		t.Error("data.written = true on a dry run")
	}
	if p.Bytes == 0 {
		t.Error("the dry run did not report how much it would write")
	}
	if got := mustRead(t, filepath.Join(dir, "WORKFLOW_STATE.md")); got != before {
		t.Error("the dry run changed the file")
	}
}

func TestWorkflowUsageErrors(t *testing.T) {
	dir := initialisedTaskRepo(t)
	cases := [][]string{
		{},
		{"bogus"},
		{"set", "extra"},
		{"set"},
		{"accept"},
		{"accept", "a", "b"},
		{"accept", "a", "--done", "--not-done"},
		{"show", "extra"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			code, env, _ := workflowRun(t, dir, args...)
			if code != 2 {
				t.Errorf("exit = %d, want 2 (usage); error = %+v", code, env.Err)
			}
			if env.Err == nil || env.Err.Code != envelope.CodeUsage {
				t.Errorf("code = %v, want usage", env.Err)
			}
		})
	}
	// An empty --accept value is a caller mistake, not an empty criterion.
	code, env, _ := workflowRun(t, dir, "set", "--accept", "")
	if code != 2 {
		t.Errorf("empty --accept: exit = %d, want 2", code)
	}
	if env.Err == nil || !strings.Contains(env.Err.Message, "empty") {
		t.Errorf("error = %+v", env.Err)
	}
	// And workflow is not a bare "set" with a missing value.
	if code, env, _ := workflowRun(t, dir, "set", "--accept", "a1="); code != 2 {
		t.Errorf("--accept with an id and no text: exit = %d, want 2; error = %+v", code, env.Err)
	}
}

func TestNoNullListsInTheWorkflowPayload(t *testing.T) {
	dir := initialisedTaskRepo(t)
	for _, args := range [][]string{
		{"show"},
		{"set", "--request", "x"},
		{"accept", "missing"},
		{"bogus"},
	} {
		_, env, _ := workflowRun(t, dir, args...)
		raw, err := json.Marshal(env.Data)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), ":null") {
			t.Errorf("workflow %v payload contains a null: %s", args, raw)
		}
	}
	// The report payload has a string slice too.
	_, env, _, _ := cliRun(t, dir, reportCommandName)
	raw, _ := json.Marshal(env.Data)
	if strings.Contains(string(raw), ":null") {
		t.Errorf("report payload contains a null: %s", raw)
	}
}

// The human rendering is a rendering of the envelope.
func TestWorkflowAndReportHumanRendering(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustWorkflow(t, dir, "set", "--request", "ship it", "--constraint", "stdlib only",
		"--accept", "a1=first", "--accept", "a2=second")
	mustWorkflow(t, dir, "accept", "a1")

	var out, errOut bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, workflowCommandName, "show"}, &out, &errOut, true); code != 0 {
		t.Fatalf("workflow show: exit %d", code)
	}
	body := out.String()
	for _, want := range []string{"ship it", "stdlib only", "1 of 2", "[x] a1", "[ ] a2"} {
		if !strings.Contains(body, want) {
			t.Errorf("workflow show is missing %q:\n%s", want, body)
		}
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, reportCommandName}, &out, &errOut, true); code != 0 {
		t.Fatalf("report: exit %d", code)
	}
	if !strings.Contains(out.String(), "current") {
		t.Errorf("report human output does not name the drift state:\n%s", out.String())
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, reportCommandName, "--format", "md"}, &out, &errOut, true); code != 0 {
		t.Fatalf("report --format md: exit %d", code)
	}
	if !strings.HasPrefix(out.String(), "# Workflow State") {
		t.Errorf("human --format md:\n%s", out.String())
	}
}

// Task flags must not leak into the workflow command's package state, and the
// reverse. Both hold package-level flag variables.
func TestWorkflowAndReportFlagsDoNotLeak(t *testing.T) {
	dir := initialisedTaskRepo(t)
	// A JSON-emitting call, so the helper can read the envelope. Neither
	// --format md (markdown on stdout) nor --output (nothing on stdout) leaves
	// anything here to decode, which is why --write alone is used to arm the
	// flags; TestReportFormats covers the other two.
	mustReport(t, dir, "--write")

	// A plain report must not inherit --write or --format.
	p := mustReport(t, dir)
	if p.Written {
		t.Error("--write leaked into the next invocation")
	}
	if p.Format != "json" {
		t.Errorf("format = %q, want json: --format leaked", p.Format)
	}

	mustWorkflow(t, dir, "set", "--request", "x")
	data := mustWorkflow(t, dir, "show")
	if len(data.Changed) != 0 {
		t.Errorf("show reported changed = %v: workflow set's flags leaked", data.Changed)
	}
}

// Ids assigned within one call must not collide with each other or with an
// explicit id in the same command. SetAcceptance merges by id, so a collision is
// not an error — it is one criterion silently replacing another.
func TestAcceptanceIDsDoNotCollideWithinOneCall(t *testing.T) {
	dir := initialisedTaskRepo(t)
	data := mustWorkflow(t, dir, "set",
		"--accept", "a1=first",
		"--accept", "bare second",
		"--accept", "bare third",
		"--accept", "a9=explicit later",
		"--accept", "bare fourth",
	)
	if len(data.Acceptance) != 5 {
		t.Fatalf("acceptance = %d items, want 5:\n%+v", len(data.Acceptance), data.Acceptance)
	}
	seen := map[string]string{}
	for _, a := range data.Acceptance {
		if prev, dup := seen[a.ID]; dup {
			t.Errorf("id %q used twice: %q and %q — one silently replaced the other", a.ID, prev, a.Text)
		}
		seen[a.ID] = a.Text
	}
	for id, want := range map[string]string{
		"a1": "first",
		"a9": "explicit later",
		"a2": "bare second",
		"a3": "bare third",
		"a4": "bare fourth",
	} {
		if seen[id] != want {
			t.Errorf("id %q = %q, want %q (all: %v)", id, seen[id], want, seen)
		}
	}
}
