package acceptance

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// moduleRoot is the repository root, found from this file rather than from the
// working directory. `go test` runs a package's tests in that package's
// directory, so a relative ".." is the only thing that has to be right and it
// is asserted rather than assumed.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join(".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

// result is one command invocation: the exit code, the decoded envelope, the
// raw stdout, and the payload as a map.
type result struct {
	code   int
	env    envelope.Envelope
	stdout string
	data   map[string]any
}

// run invokes ocaw in-process with a non-TTY stdout, which is the mode the
// contract is written for.
func run(t *testing.T, dir string, args ...string) result {
	t.Helper()
	var out, errOut bytes.Buffer
	argv := append([]string{"--dir", dir}, args...)
	code := cli.Main(argv, &out, &errOut, false)
	res := result{code: code, stdout: out.String()}
	raw := bytes.TrimSpace(out.Bytes())
	if len(raw) == 0 {
		return res
	}
	if err := json.Unmarshal(raw, &res.env); err != nil {
		t.Fatalf("ocaw %v: stdout is not a JSON envelope: %v\n%s", args, err, raw)
	}
	if res.env.Data != nil {
		encoded, err := json.Marshal(res.env.Data)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &res.data); err != nil {
			t.Fatalf("ocaw %v: data is not an object: %v", args, err)
		}
	}
	return res
}

func mustRun(t *testing.T, dir string, args ...string) result {
	t.Helper()
	res := run(t, dir, args...)
	if res.code != 0 {
		t.Fatalf("ocaw %v: exit %d, want 0: %+v", args, res.code, res.env.Err)
	}
	return res
}

func mustFail(t *testing.T, dir string, wantExit int, args ...string) result {
	t.Helper()
	res := run(t, dir, args...)
	if res.code != wantExit {
		t.Fatalf("ocaw %v: exit %d, want %d: %+v", args, res.code, wantExit, res.env.Err)
	}
	if res.env.Err == nil {
		t.Fatalf("ocaw %v: exit %d with no error in the envelope", args, res.code)
	}
	if !envelope.KnownCode(res.env.Err.Code) {
		t.Errorf("ocaw %v: code %q is not in the closed set", args, res.env.Err.Code)
	}
	return res
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

func gate(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "gate.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// project is a directory ocaw will accept as a project: a marker file is enough,
// and using a temporary directory rather than a git repository keeps the suite
// from depending on git being installed or configured.
func project(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module example.com/acceptance\n\ngo 1.22\n")
	return dir
}

func initialised(t *testing.T) string {
	t.Helper()
	dir := project(t)
	mustRun(t, dir, "init")
	return dir
}

func chain(t *testing.T) string {
	t.Helper()
	dir := initialised(t)
	mustRun(t, dir, "task", "add", "--id", "1", "--title", "scaffold", "--agent", "coder")
	mustRun(t, dir, "task", "add", "--id", "2", "--title", "invariants", "--agent", "coder", "--dep", "1")
	mustRun(t, dir, "task", "add", "--id", "3", "--title", "render", "--agent", "coder", "--dep", "2")
	return dir
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, found := m[key].(string)
	if !found {
		t.Fatalf("%q is %T, want a string", key, m[key])
	}
	return v
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, found := m[key].(float64)
	if !found {
		t.Fatalf("%q is %T, want a number", key, m[key])
	}
	return v
}

func list(t *testing.T, m map[string]any, key string) []any {
	t.Helper()
	v, found := m[key].([]any)
	if !found {
		t.Fatalf("%q is %T, want an array", key, m[key])
	}
	return v
}

// object pulls a nested object out of a payload.
func object(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, found := m[key].(map[string]any)
	if !found {
		t.Fatalf("%q is %T, want an object", key, m[key])
	}
	return v
}

// firstTask is the id of `data.task`, or "" when it is null.
func firstTask(t *testing.T, m map[string]any) string {
	t.Helper()
	task, found := m["task"].(map[string]any)
	if !found {
		return ""
	}
	return str(t, task, "id")
}

// ---------------------------------------------------------------------------
// AC1 — init builds the layout and detects the project
// ---------------------------------------------------------------------------

func TestAC1InitBuildsTheLayoutAndDetectsTheProject(t *testing.T) {
	dir := project(t)
	res := mustRun(t, dir, "init")

	if got := str(t, res.data, "project_type"); got != "go" {
		t.Errorf("project_type = %q, want go", got)
	}
	if got := str(t, res.data, "verify_cmd"); got != "go test ./..." {
		t.Errorf("verify_cmd = %q, want %q", got, "go test ./...")
	}
	for _, rel := range []string{
		".agent/agent.yaml", ".agent/RULES.md", ".agent/MEMORY.md", ".agent/KNOWLEDGE.md",
		".agent/knowledge/index.yaml", ".agent/state/state.json",
		".agent/skills", ".agent/hooks", ".workflow", "WORKFLOW_STATE.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, rel)); err != nil {
			t.Errorf("init did not create %s: %v", rel, err)
		}
	}
	agent := read(t, filepath.Join(dir, ".agent", "agent.yaml"))
	for _, want := range []string{"ocaw: true", "schema: ocaw/agent@1", "type: go", "verify_cmd: go test ./..."} {
		if !strings.Contains(agent, want) {
			t.Errorf("agent.yaml is missing %q:\n%s", want, agent)
		}
	}
}

// ---------------------------------------------------------------------------
// AC2 — init is idempotent
// ---------------------------------------------------------------------------

func TestAC2InitTwiceChangesNothing(t *testing.T) {
	dir := initialised(t)
	before := snapshot(t, dir)

	res := mustRun(t, dir, "init")
	if got, _ := res.data["initialized"].(bool); !got {
		t.Error("data.initialized = false on a re-init, want true")
	}
	if got, _ := res.data["created"].(bool); got {
		t.Error("data.created = true on a re-init, want false")
	}
	after := snapshot(t, dir)
	for name, sum := range before {
		if after[name] != sum {
			t.Errorf("%s changed on a second init", name)
		}
	}
	for name := range after {
		if _, seen := before[name]; !seen {
			t.Errorf("a second init created %s", name)
		}
	}
}

// snapshot hashes every file ocaw owns, which is the closest thing to
// `git status --porcelain` that does not require git. AC2 is about content, and
// this checks content.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		out[rel] = string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// ---------------------------------------------------------------------------
// AC3 — a hand-written RULES.md survives
// ---------------------------------------------------------------------------

func TestAC3HandWrittenRulesSurvives(t *testing.T) {
	const mine = "# Mine\n\n- Never touch generated code.\n"
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module example.com/acceptance\n")
	write(t, filepath.Join(dir, ".agent", "RULES.md"), mine)

	res := mustRun(t, dir, "init")
	if got := read(t, filepath.Join(dir, ".agent", "RULES.md")); got != mine {
		t.Errorf("RULES.md was rewritten:\n%s", got)
	}
	found := false
	for _, w := range res.env.Warnings {
		if w.Code == envelope.WarnFileExists {
			found = true
		}
	}
	if !found {
		t.Errorf("want a %s warning about the hand-written file", envelope.WarnFileExists)
	}
	// The rest of the workspace still got built.
	if _, err := os.Stat(filepath.Join(dir, ".agent", "agent.yaml")); err != nil {
		t.Errorf("init gave up instead of building the rest: %v", err)
	}
}

// ---------------------------------------------------------------------------
// AC4 — next follows the DAG
// ---------------------------------------------------------------------------

func TestAC4NextFollowsTheChain(t *testing.T) {
	dir := chain(t)
	if got := firstTask(t, mustRun(t, dir, "task", "next").data); got != "1" {
		t.Errorf("next = %q, want 1", got)
	}
	mustRun(t, dir, "task", "set", "1", "--status", "done")
	if got := firstTask(t, mustRun(t, dir, "task", "next").data); got != "2" {
		t.Errorf("after 1 is done, next = %q, want 2", got)
	}
	mustRun(t, dir, "task", "set", "2", "--status", "done")
	if got := firstTask(t, mustRun(t, dir, "task", "next").data); got != "3" {
		t.Errorf("after 2 is done, next = %q, want 3", got)
	}
	// An empty queue is a valid state, not a failure.
	mustRun(t, dir, "task", "set", "3", "--status", "done")
	res := mustRun(t, dir, "task", "next")
	if got := firstTask(t, res.data); got != "" {
		t.Errorf("with everything done, next = %q, want null", got)
	}
	if !strings.Contains(res.stdout, `"task":null`) {
		t.Errorf("an empty queue must report an explicit null, not an absent key:\n%s", res.stdout)
	}
}

// ---------------------------------------------------------------------------
// AC5 — a task cannot start ahead of its dependencies
// ---------------------------------------------------------------------------

func TestAC5InProgressWithUnmetDepsIsRefused(t *testing.T) {
	dir := chain(t)
	res := mustFail(t, dir, 5, "task", "set", "3", "--status", "in_progress")
	if res.env.Err.Code != envelope.CodeDepsUnmet {
		t.Errorf("code = %q, want deps_unmet", res.env.Err.Code)
	}
	// The blocking dependency is readable as data, not only as message text.
	detail := object(t, res.data, "detail")
	blocking, _ := detail["blocking"].([]any)
	if len(blocking) == 0 || blocking[0] != "2" {
		t.Errorf("detail.blocking = %v, want [2]", detail["blocking"])
	}
	// And the refusal changed nothing.
	if got := firstTask(t, mustRun(t, dir, "task", "show", "3").data); got != "3" {
		t.Errorf("the refused transition removed the task: %q", got)
	}
	show := object(t, mustRun(t, dir, "task", "show", "3").data, "task")
	if got := str(t, show, "status"); got != "pending" {
		t.Errorf("status = %q, want pending: a refusal must not half-apply", got)
	}
}

// ---------------------------------------------------------------------------
// AC6 — a cycle is refused and named
// ---------------------------------------------------------------------------

func TestAC6CycleIsRefusedAndNamed(t *testing.T) {
	dir := initialised(t)
	mustRun(t, dir, "task", "add", "--id", "1", "--title", "a")
	mustRun(t, dir, "task", "add", "--id", "2", "--title", "b", "--dep", "1")

	res := mustFail(t, dir, 5, "task", "dep", "1", "--add", "2")
	if res.env.Err.Code != envelope.CodeDepCycle {
		t.Fatalf("code = %q, want dep_cycle", res.env.Err.Code)
	}
	// The path is the part an agent needs. Without it, finding the loop means
	// re-adding every edge by hand.
	if n := strings.Count(res.env.Err.Message, "->"); n < 2 {
		t.Errorf("message = %q, want the full cycle path", res.env.Err.Message)
	}
	// detail.cycle carries the path as ids, so a caller can walk it without
	// parsing the message's arrow syntax.
	detail := object(t, res.data, "detail")
	cycle, _ := detail["cycle"].([]any)
	if len(cycle) < 3 {
		t.Errorf("detail.cycle = %v, want the path as ids", detail["cycle"])
	}
	if cycle[0] != cycle[len(cycle)-1] {
		t.Errorf("detail.cycle = %v, want the first id repeated last", cycle)
	}
	// The refused edge was not recorded.
	show := object(t, mustRun(t, dir, "task", "show", "1").data, "task")
	if deps, _ := show["deps"].([]any); len(deps) != 0 {
		t.Errorf("deps = %v, want the refused edge to be absent", deps)
	}
}

// ---------------------------------------------------------------------------
// AC7 — a hand-edited render is drift, and report --write repairs it
// ---------------------------------------------------------------------------

func TestAC7RenderDriftIsDetectedAndRepaired(t *testing.T) {
	dir := initialised(t)
	mustRun(t, dir, "task", "add", "--id", "1", "--title", "scaffold")

	path := filepath.Join(dir, "WORKFLOW_STATE.md")
	doc := read(t, path)
	edited := strings.Replace(doc, "| 1 | scaffold |", "| 1 | scaffold (done by hand) |", 1)
	if edited == doc {
		t.Fatal("could not edit the generated table")
	}
	write(t, path, edited)

	res := mustFail(t, dir, 4, "doctor")
	found := false
	for _, raw := range list(t, res.data, "findings") {
		f := raw.(map[string]any)
		if f["code"] == string(envelope.CodeRenderDrift) {
			found = true
			if !strings.Contains(str(t, f, "hint"), "report --write") {
				t.Errorf("hint = %q, want it to name the repair", f["hint"])
			}
		}
	}
	if !found {
		t.Fatalf("doctor found no render_drift finding:\n%s", res.stdout)
	}

	// The documented repair, then a clean bill of health.
	mustRun(t, dir, "report", "--write")
	mustRun(t, dir, "doctor")
}

// ---------------------------------------------------------------------------
// AC8 — a failing gate is recorded and exits non-zero
// ---------------------------------------------------------------------------

func TestAC8FailingGateIsRecordedAndExitsNonZero(t *testing.T) {
	dir := initialised(t)
	script := gate(t, dir, "echo 'FAIL: two things are wrong'\nexit 3")
	mustRun(t, dir, "task", "add", "--id", "1", "--title", "scaffold", "--gate", "test="+script)

	res := mustFail(t, dir, 4, "verify", "run")
	if res.env.Err.Code != envelope.CodeVerifyFailed {
		t.Errorf("code = %q, want verify_failed", res.env.Err.Code)
	}

	// The record is written even though the command reports failure, and it
	// carries the real exit code rather than one of ocaw's own.
	log := read(t, filepath.Join(dir, ".agent", "state", "runs.jsonl"))
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 1 {
		t.Fatalf("runs.jsonl has %d records, want exactly 1:\n%s", len(lines), log)
	}
	var record map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("the record is not JSON: %v\n%s", err, lines[0])
	}
	if got := num(t, record, "exit"); got != 3 {
		t.Errorf("recorded exit = %v, want the gate's own 3", got)
	}
	if got := str(t, record, "status"); got != "fail" {
		t.Errorf("recorded status = %q, want fail", got)
	}

	// The gate is left `fail`, and nothing was retried on ocaw's initiative.
	task := object(t, mustRun(t, dir, "task", "show", "1").data, "task")
	gateList, _ := task["gates"].([]any)
	if len(gateList) != 1 {
		t.Fatalf("gates = %v, want one", gateList)
	}
	g := gateList[0].(map[string]any)
	if got := str(t, g, "status"); got != "fail" {
		t.Errorf("gate status = %q, want fail", got)
	}
	if got := num(t, g, "attempts"); got != 1 {
		t.Errorf("gate attempts = %v, want 1: ocaw never retries", got)
	}
	if lines := len(strings.Split(strings.TrimSpace(read(t, filepath.Join(dir, ".agent", "state", "runs.jsonl"))), "\n")); lines != 1 {
		t.Errorf("runs.jsonl has %d records after one invocation, want 1", lines)
	}
}

// ---------------------------------------------------------------------------
// AC9 — three identical failures are stuck, and status says so
// ---------------------------------------------------------------------------

func TestAC9ThreeIdenticalFailuresAreStuckAndVisible(t *testing.T) {
	dir := initialised(t)
	script := gate(t, dir, "echo 'FAIL: same message every time'\nexit 1")
	mustRun(t, dir, "task", "add", "--id", "1", "--title", "broken", "--gate", "test="+script)
	mustRun(t, dir, "task", "set", "1", "--status", "in_progress")

	// Two failures are not yet a pattern: StuckThreshold is 3, and the verdict
	// is a comparison of consecutive attempts, so it cannot be reached in fewer.
	mustFail(t, dir, 4, "verify", "run")
	mustFail(t, dir, 4, "verify", "run")
	if stuck, _ := mustRun(t, dir, "status").data["stuck"].([]any); len(stuck) != 0 {
		t.Errorf("stuck after two failures = %v, want none", stuck)
	}
	mustFail(t, dir, 4, "verify", "run")

	res := mustRun(t, dir, "status")
	stuck, _ := res.data["stuck"].([]any)
	if len(stuck) == 0 {
		t.Fatalf("three identical failures did not set stuck:\n%s", res.stdout)
	}
	entry := stuck[0].(map[string]any)
	if str(t, entry, "task") != "1" || str(t, entry, "gate") != "test" {
		t.Errorf("stuck entry = %v, want task 1 gate test", entry)
	}
	if got := num(t, entry, "attempts"); got < 3 {
		t.Errorf("stuck attempts = %v, want at least 3", got)
	}
	// And the task itself is flagged, so `ocaw task list` shows it too.
	for _, raw := range list(t, mustRun(t, dir, "task", "list").data, "tasks") {
		view := raw.(map[string]any)
		if str(t, view, "id") == "1" && view["stuck"] != true {
			t.Errorf("task 1 stuck = %v, want true", view["stuck"])
		}
	}
}

// ---------------------------------------------------------------------------
// AC10 — one line of valid JSON from every command
// ---------------------------------------------------------------------------

// everyCommand is the surface §7 defines. AC10 is a statement about all of them,
// so the test walks all of them rather than a convenient few.
var everyCommand = [][]string{
	{"version"},
	{"schema"},
	// `ocaw schema <key>` writes the *document*, not the envelope, so that
	// `ocaw schema task > task.json` produces a loadable file. It is the one
	// documented exception to AC10 and TestSchemaWritesADocumentRatherThanAnEnvelope
	// in internal/cli pins it.
	{"schema", "--json"},
	{"schema", "task", "--json"},
	{"init"},
	{"doctor"},
	{"status"},
	{"status", "--task", "1"},
	{"task", "add", "--id", "9", "--title", "ninth"},
	{"task", "list"},
	{"task", "list", "--ready"},
	{"task", "list", "--blocked"},
	{"task", "next"},
	{"task", "show", "1"},
	{"task", "set", "1", "--title", "renamed"},
	{"task", "dep", "9", "--add", "1"},
	{"workflow", "set", "--request", "acceptance"},
	{"workflow", "show"},
	{"workflow", "accept", "a1"},
	{"report"},
	{"verify", "history"},
	{"verify", "run"},
	{"verify", "detect"},
}

func TestAC10EveryCommandEmitsOneLineOfValidJSON(t *testing.T) {
	dir := chain(t)
	// A gate that passes, so `verify run` has something to run and succeed.
	mustRun(t, dir, "task", "set", "1", "--gate", "test="+gate(t, dir, "echo ok"))
	mustRun(t, dir, "workflow", "set", "--accept", "a criterion")

	for _, argv := range everyCommand {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := cli.Main(append([]string{"--dir", dir}, argv...), &out, &errOut, false)
			raw := out.Bytes()

			trimmed := bytes.TrimRight(raw, "\n")
			if n := bytes.Count(trimmed, []byte("\n")); n != 0 {
				t.Fatalf("stdout is %d lines, want exactly 1 (exit %d):\n%s", n+1, code, trimmed)
			}
			var env envelope.Envelope
			if err := json.Unmarshal(trimmed, &env); err != nil {
				t.Fatalf("stdout does not round-trip through encoding/json: %v\n%s", err, trimmed)
			}
			// The documented envelope keys, all present, on every path.
			for _, key := range []string{"ok", "command", "schema"} {
				if !bytes.Contains(trimmed, []byte(`"`+key+`":`)) {
					t.Errorf("envelope is missing %q:\n%s", key, trimmed)
				}
			}
			if !bytes.Contains(trimmed, []byte(`"error":`)) || !bytes.Contains(trimmed, []byte(`"warnings":`)) {
				t.Errorf("envelope is missing error or warnings:\n%s", trimmed)
			}
			if env.Command == "" {
				t.Errorf("envelope.command is empty:\n%s", trimmed)
			}
			// schema is `<command>@<major>` and the major is the one ocaw has.
			if want := envelope.Schema("ocaw/" + env.Command + "@1"); env.Schema != want {
				t.Errorf("schema = %q, want %q", env.Schema, want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC11 — --dry-run writes nothing
// ---------------------------------------------------------------------------

func TestAC11DryRunWritesNothingOnAnyMutatingCommand(t *testing.T) {
	dir := chain(t)
	mustRun(t, dir, "workflow", "set", "--request", "before", "--accept", "a1=first")
	before := snapshot(t, dir)

	cases := [][]string{
		{"init"},
		{"task", "add", "--id", "9", "--title", "ninth"},
		{"task", "set", "1", "--status", "done"},
		{"task", "set", "1", "--title", "renamed"},
		{"task", "dep", "3", "--add", "1"},
		{"task", "dep", "3", "--rm", "2"},
		{"task", "rm", "3"},
		{"task", "rm", "1", "2", "3"},
		{"workflow", "set", "--request", "after"},
		{"workflow", "accept", "a1"},
		{"workflow", "accept", "a1", "--not-done"},
		{"report", "--write"},
		{"verify", "run"},
		{"doctor", "--fix-safe"},
	}
	for _, argv := range cases {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			res := mustRun(t, dir, append(argv, "--dry-run")...)
			if got, _ := res.data["dry_run"].(bool); !got {
				t.Errorf("data.dry_run = false on a --dry-run:\n%s", res.stdout)
			}
			after := snapshot(t, dir)
			for name, sum := range before {
				if after[name] != sum {
					t.Errorf("%s changed during a --dry-run of %v", name, argv)
				}
			}
			for name := range after {
				if _, seen := before[name]; !seen {
					t.Errorf("a --dry-run of %v created %s", argv, name)
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// AC12 — a skill whose name does not match its directory is an error
// ---------------------------------------------------------------------------

func TestAC12SkillNameMismatchIsADoctorError(t *testing.T) {
	dir := initialised(t)
	write(t, filepath.Join(dir, ".agent", "skills", "correct", "SKILL.md"),
		"---\nname: wrong\nversion: 1.0.0\ndescription: \"A skill whose name is not its directory.\"\n---\n\nbody\n")

	res := mustFail(t, dir, 4, "doctor")
	found := false
	for _, raw := range list(t, res.data, "findings") {
		f := raw.(map[string]any)
		if f["check"] == "skills" && f["severity"] == "error" {
			found = true
			if !strings.Contains(str(t, f, "message"), "wrong") {
				t.Errorf("message = %q, want it to name the offending name", f["message"])
			}
		}
	}
	if !found {
		t.Fatalf("doctor did not report the mismatched skill:\n%s", res.stdout)
	}

	// A correctly named one is not a finding, so the check is not just always-on.
	write(t, filepath.Join(dir, ".agent", "skills", "second", "SKILL.md"),
		"---\nname: second\ndescription: \"A well-formed skill.\"\n---\n\nbody\n")
	// The first one is still wrong, so fix it too and then the workspace is clean.
	write(t, filepath.Join(dir, ".agent", "skills", "correct", "SKILL.md"),
		"---\nname: correct\ndescription: \"A well-formed skill.\"\n---\n\nbody\n")
	mustRun(t, dir, "doctor")
}

// ---------------------------------------------------------------------------
// AC13 — the toolchain checks
// ---------------------------------------------------------------------------

func TestAC13TheToolchainChecksPass(t *testing.T) {
	root := moduleRoot(t)

	t.Run("go vet is clean", func(t *testing.T) {
		out, err := exec.Command("go", "vet", "./...").CombinedOutput()
		if err != nil {
			t.Fatalf("go vet: %v\n%s", err, out)
		}
	})

	t.Run("CGO_ENABLED=0 produces a static binary", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "ocaw")
		cmd := exec.Command("go", "build", "-o", bin, "./cmd/ocaw")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("CGO_ENABLED=0 go build: %v\n%s", err, out)
		}
		info, err := os.Stat(bin)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() < 1<<20 {
			t.Errorf("binary is %d bytes, which is too small to be a real build", info.Size())
		}
		// Asserting the build setting rather than trusting the environment: a
		// pure-Go darwin binary still references libSystem for syscalls, so
		// looking for a dynamic loader in the bytes proves nothing. The
		// recorded setting is what the toolchain actually did.
		recorded, err := exec.Command("go", "version", "-m", bin).Output()
		if err != nil {
			t.Fatalf("go version -m: %v", err)
		}
		if !bytes.Contains(recorded, []byte("CGO_ENABLED=0")) {
			t.Errorf("the binary was not built with CGO_ENABLED=0:\n%s", recorded)
		}
		if bytes.Contains(recorded, []byte("dep\t")) {
			t.Errorf("the binary has third-party dependencies (SPEC §9.1):\n%s", recorded)
		}
	})

	t.Run("go list -m all shows only the main module", func(t *testing.T) {
		cmd := exec.Command("go", "list", "-m", "all")
		cmd.Dir = root
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -m all: %v", err)
		}
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) != 1 {
			t.Errorf("go list -m all reports %d modules, want 1 (SPEC §9.1, stdlib only):\n%s", len(lines), out)
		}
		if !strings.Contains(lines[0], "nataliagonzales81/oc-agent-workspace") {
			t.Errorf("the only module is %q, want the main module", lines[0])
		}
	})

	t.Run("go.mod has no require block", func(t *testing.T) {
		for _, line := range strings.Split(read(t, filepath.Join(root, "go.mod")), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "require") {
				t.Errorf("go.mod has a require block: %q", line)
			}
		}
	})

	// SPEC §9.1: no network. Nothing ocaw ships may reach it, including the two
	// packages that legitimately run or resolve a command.
	//
	// The narrower rule — only internal/verify may build a process, and only
	// internal/doctor may resolve one on PATH — is checked where it belongs, by
	// TestStatusCannotReachOSExec in internal/cli, which has the source-level
	// detail to say so. Repeating it here would only dilute it.
	t.Run("nothing shipped reaches the network", func(t *testing.T) {
		for _, dir := range []string{
			"cmd", "internal/cli", "internal/doctor", "internal/envelope", "internal/report",
			"internal/schema", "internal/state", "internal/verify", "internal/workspace", "internal/yaml",
		} {
			entries, err := os.ReadDir(filepath.Join(root, dir))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
					continue
				}
				body := read(t, filepath.Join(root, dir, entry.Name()))
				for _, forbidden := range []string{"net/http", "net/url", "net/rpc", "net/smtp"} {
					if strings.Contains(body, `"`+forbidden+`"`) {
						t.Errorf("%s/%s imports %s; ocaw is air-gap safe (SPEC §2, §9.1)", dir, entry.Name(), forbidden)
					}
				}
			}
		}
	})
}

// ---------------------------------------------------------------------------
// AC14 — the goldens exist, one per command, one line each
// ---------------------------------------------------------------------------

func TestAC14GoldensExistForEveryCommand(t *testing.T) {
	root := moduleRoot(t)
	dir := filepath.Join(root, "testdata", "goldens")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("no goldens directory: %v", err)
	}

	covered := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".golden") {
			t.Errorf("unexpected file in testdata/goldens: %s", name)
			continue
		}
		raw := read(t, filepath.Join(dir, name))
		if n := strings.Count(strings.TrimRight(raw, "\n"), "\n"); n != 0 {
			t.Errorf("%s is %d lines, want 1", name, n+1)
		}
		var env envelope.Envelope
		if err := json.Unmarshal([]byte(raw), &env); err != nil {
			t.Errorf("%s is not a JSON envelope: %v", name, err)
			continue
		}
		if env.Command == "" {
			t.Errorf("%s has no command", name)
			continue
		}
		covered[env.Command] = true
	}

	// The comparison itself lives in internal/cli's TestGoldens, which owns the
	// harness and the -update flag. What is checked here is that the record
	// exists at all and is well formed, so a suite run from a clean checkout
	// fails on a missing contract rather than on nothing.
	for _, name := range []string{"doctor", "init", "report", "schema", "status", "task", "verify", "version", "workflow"} {
		if !covered[name] {
			t.Errorf("no golden covers %q", name)
		}
	}
	if len(covered) < len(everyCommand) {
		t.Logf("note: %d commands have goldens for %d subcommand forms", len(covered), len(everyCommand))
	}
}

// ---------------------------------------------------------------------------
// The README's examples, run as written
// ---------------------------------------------------------------------------

// ocawLine matches a shell line that invokes ocaw, so the examples can be run
// without also running `go install`.
var ocawLine = regexp.MustCompile("(?m)^ocaw .*$")

func TestReadmeExamplesRunAsWritten(t *testing.T) {
	root := moduleRoot(t)
	readme := read(t, filepath.Join(root, "README.md"))

	var lines []string
	for _, line := range ocawLine.FindAllString(readme, -1) {
		lines = append(lines, strings.TrimSpace(line))
	}
	if len(lines) == 0 {
		t.Fatal("the README has no `ocaw` examples to check")
	}

	// One workspace, in the order the README presents them: the examples are a
	// session, not a set of independent snippets, and running them out of order
	// would be testing something the README does not claim.
	dir := project(t)
	// The session runs `ocaw verify run` against a `go test ./...` gate, so the
	// fixture has to be a module that actually compiles and passes. A gate that
	// cannot succeed is not an example anyone can run.
	write(t, filepath.Join(dir, "smoke.go"), "package acceptance\n\nfunc Smoke() string { return \"ok\" }\n")
	write(t, filepath.Join(dir, "smoke_test.go"), "package acceptance\n\nimport \"testing\"\n\nfunc TestSmoke(t *testing.T) {\n\tif Smoke() != \"ok\" {\n\t\tt.Fatal(\"not ok\")\n\t}\n}\n")
	for _, line := range lines {
		fields, err := splitArgs(line)
		if err != nil {
			t.Fatalf("cannot parse %q: %v", line, err)
		}
		// A trailing `> path` is a shell operator, not part of the command.
		// The example uses it to show that a redirect produces a loadable file,
		// so the redirect is carried out here rather than skipped.
		// `>` is its own token once the line is split, so the redirect is found
		// by position rather than by looking at the last field.
		redirect := ""
		for i, f := range fields {
			if f != ">" {
				continue
			}
			if i+1 >= len(fields) {
				t.Errorf("README example %q redirects to nothing", line)
				return
			}
			redirect = fields[i+1]
			fields = fields[:i]
			break
		}
		// The line reads `ocaw init`; argv[0] is the program.
		fields = fields[1:]

		var out, errOut bytes.Buffer
		code := cli.Main(append([]string{"--dir", dir}, fields...), &out, &errOut, false)
		if code != 0 {
			t.Errorf("README example %q exited %d:\n%s\n%s", line, code, out.String(), errOut.String())
			continue
		}
		if redirect == "" {
			continue
		}
		path := filepath.Join(dir, redirect)
		// The redirect has to be the *document*, so that what lands in the file
		// loads as JSON Schema rather than as a description of one.
		body := out.String()
		var probe map[string]any
		if err := json.Unmarshal([]byte(body), &probe); err != nil {
			t.Errorf("README example %q redirected something that is not JSON: %v", line, err)
			continue
		}
		if _, isEnvelope := probe["command"]; isEnvelope {
			t.Errorf("README example %q redirected an envelope, want the document", line)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// splitArgs is a shell-ish word splitter: it honours double quotes so an example
// containing a quoted title parses the way a shell would, and it does not try to
// be a shell. ocaw takes argv, not a command line, so an example that needed
// anything more would not be runnable by an agent either.
func splitArgs(line string) ([]string, error) {
	var (
		out     []string
		current strings.Builder
		inQuote bool
		started bool
	)
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			started = true
		case r == ' ' && !inQuote:
			if started {
				out = append(out, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if inQuote {
		return nil, errUnbalancedQuote
	}
	if started {
		out = append(out, current.String())
	}
	return out, nil
}

type quoteError struct{}

func (quoteError) Error() string { return "unbalanced double quote" }

var errUnbalancedQuote = quoteError{}

// ---------------------------------------------------------------------------
// Every criterion maps to a named test
// ---------------------------------------------------------------------------

// criteria is §10 as an executable list. The map is the deliverable: a criterion
// nobody wrote a test for is a criterion nobody checked, and the only way to
// notice is to read the tests and count.
var criteria = []struct{ ac, test string }{
	{"AC1", "TestAC1InitBuildsTheLayoutAndDetectsTheProject"},
	{"AC2", "TestAC2InitTwiceChangesNothing"},
	{"AC3", "TestAC3HandWrittenRulesSurvives"},
	{"AC4", "TestAC4NextFollowsTheChain"},
	{"AC5", "TestAC5InProgressWithUnmetDepsIsRefused"},
	{"AC6", "TestAC6CycleIsRefusedAndNamed"},
	{"AC7", "TestAC7RenderDriftIsDetectedAndRepaired"},
	{"AC8", "TestAC8FailingGateIsRecordedAndExitsNonZero"},
	{"AC9", "TestAC9ThreeIdenticalFailuresAreStuckAndVisible"},
	{"AC10", "TestAC10EveryCommandEmitsOneLineOfValidJSON"},
	{"AC11", "TestAC11DryRunWritesNothingOnAnyMutatingCommand"},
	{"AC12", "TestAC12SkillNameMismatchIsADoctorError"},
	{"AC13", "TestAC13TheToolchainChecksPass"},
	{"AC14", "TestAC14GoldensExistForEveryCommand"},
}

// TestEveryAcceptanceCriterionIsMapped checks the map against the source rather
// than against the test binary's symbol table, so it also catches a test that
// was deleted or renamed and left behind in the list.
func TestEveryAcceptanceCriterionIsMapped(t *testing.T) {
	root := moduleRoot(t)
	declared := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		for _, m := range regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`).FindAllStringSubmatch(string(body), -1) {
			declared[m[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// §10 is numbered 1..14 and the numbering has not changed since the spec
	// was written, so a missing entry is a missing criterion rather than a
	// renumbering.
	for i := 1; i <= 14; i++ {
		ac := "AC" + strconv.Itoa(i)
		var mapped string
		for _, c := range criteria {
			if c.ac == ac {
				mapped = c.test
			}
		}
		if mapped == "" {
			t.Errorf("SPEC §10 %s has no entry in the criteria map", ac)
			continue
		}
		where, found := declared[mapped]
		if !found {
			t.Errorf("SPEC §10 %s maps to %s, which does not exist", ac, mapped)
			continue
		}
		if !strings.HasPrefix(filepath.Base(where), "acceptance") {
			t.Errorf("SPEC §10 %s maps to %s in %s; the acceptance suite belongs in acceptance/", ac, mapped, where)
		}
	}
}

// TestReadmeInstallPathResolvesToAPackage closes the gap that let a broken
// install command ship in v0.1.0.
//
// TestReadmeExamplesRunAsWritten executes every `ocaw` line in the README, and
// it could not catch that one, because `go install` is not an `ocaw` line. The
// line a first-time user runs *first* was therefore the line nothing tested, and
// it named a path that does not resolve:
//
//	go: .../oc-agent-workspace@v0.1.0: module ... found, but does not contain
//	package github.com/nataliagonzales81/oc-agent-workspace
//
// The module root has no Go package, because the tool lives under cmd/. This
// resolves the path the README actually prints against the repository and
// requires a `package main` in the directory it names, so the mistake cannot be
// made again without the test failing.
func TestReadmeInstallPathResolvesToAPackage(t *testing.T) {
	root := moduleRoot(t)
	readme := read(t, filepath.Join(root, "README.md"))

	install := regexp.MustCompile(`(?m)^go install (\S+?)@\S+\s*$`)
	matches := install.FindAllStringSubmatch(readme, -1)
	if len(matches) == 0 {
		t.Fatal("the README has no `go install` line to check")
	}

	module := ""
	for _, line := range strings.Split(read(t, filepath.Join(root, "go.mod")), "\n") {
		if strings.HasPrefix(line, "module ") {
			module = strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	if module == "" {
		t.Fatal("could not read the module path from go.mod")
	}

	seen := map[string]bool{}
	for _, m := range matches {
		path := m[1]
		seen[path] = true

		rel, found := strings.CutPrefix(path, module)
		if !found {
			t.Errorf("install path %q is not under the module %q", path, module)
			continue
		}
		rel = strings.Trim(rel, "/")
		dir := root
		if rel != "" {
			dir = filepath.Join(root, rel)
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("install path %q does not resolve to a directory: %v", path, err)
			continue
		}
		main := false
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			if strings.Contains(read(t, filepath.Join(dir, e.Name())), "package main") {
				main = true
			}
		}
		if !main {
			t.Errorf("install path %q resolves to %s, which has no package main; "+
				"go install would fail with \"does not contain package\"", path, rel)
		}
	}
	if len(seen) == 1 {
		t.Log("note: every install line names the same path")
	}
}
