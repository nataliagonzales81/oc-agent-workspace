package cli_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/cli"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

const statusCommandName = "status"

type statusPayload struct {
	Root             string              `json:"root"`
	ProjectType      string              `json:"project_type"`
	VerifyCmd        string              `json:"verify_cmd"`
	Counts           map[string]int      `json:"counts"`
	Total            int                 `json:"total"`
	Ready            []string            `json:"ready"`
	Blocked          map[string][]string `json:"blocked"`
	Next             *taskViewPayload    `json:"next"`
	Stuck            []stuckPayload      `json:"stuck"`
	StuckTasks       []string            `json:"stuck_tasks"`
	LastVerification *lastRunPayload     `json:"last_verification"`
	Budget           budgetPayload       `json:"budget"`
	Acceptance       acceptancePayload   `json:"acceptance"`
	Render           string              `json:"render"`
	HistoryTruncated bool                `json:"history_truncated"`
	Task             *taskFocusPayload   `json:"task"`
	Notes            []string            `json:"notes"`
}

type taskViewPayload struct {
	ID     string   `json:"id"`
	Title  string   `json:"title"`
	Status string   `json:"status"`
	Deps   []string `json:"deps"`
	Stuck  bool     `json:"stuck"`
}

type stuckPayload struct {
	Task     string `json:"task"`
	Gate     string `json:"gate"`
	Since    string `json:"since"`
	Attempts int    `json:"attempts"`
}

type lastRunPayload struct {
	Task     string `json:"task"`
	Gate     string `json:"gate"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	Exit     *int   `json:"exit"`
	At       string `json:"at"`
	Attempts int    `json:"attempts"`
}

type budgetPayload struct {
	MaxTokens   int64 `json:"max_tokens"`
	SpentTokens int64 `json:"spent_tokens"`
	Declared    bool  `json:"declared"`
	Remaining   int64 `json:"remaining"`
	OverBudget  bool  `json:"over_budget"`
}

type acceptancePayload struct {
	Total int  `json:"total"`
	Done  int  `json:"done"`
	All   bool `json:"all_done"`
}

type taskFocusPayload struct {
	Task       taskViewPayload `json:"task"`
	UnmetDeps  []string        `json:"unmet_deps"`
	Dependents []string        `json:"dependents"`
	Gates      []struct {
		Name     string `json:"name"`
		Command  string `json:"command"`
		Status   string `json:"status"`
		Exit     *int   `json:"exit"`
		Attempts int    `json:"attempts"`
	} `json:"gates"`
	History []struct {
		Task   string `json:"task"`
		Gate   string `json:"gate"`
		Status string `json:"status"`
		Exit   *int   `json:"exit"`
	} `json:"history"`
	Runnable bool     `json:"runnable"`
	Notes    []string `json:"notes"`
}

func statusRun(t *testing.T, dir string, args ...string) (int, envelope.Envelope, statusPayload) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.Main(append([]string{"--dir", dir, statusCommandName}, args...), &out, &errOut, false)
	raw := bytes.TrimSpace(out.Bytes())
	if len(raw) == 0 {
		return code, envelope.Envelope{}, statusPayload{}
	}
	var env envelope.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("status: stdout is not a JSON envelope: %v\n%s", err, raw)
	}
	if env.Command != statusCommandName {
		t.Fatalf("command = %q, want %q", env.Command, statusCommandName)
	}
	payload := decodeStatus(t, env)
	return code, env, payload
}

func decodeStatus(t *testing.T, env envelope.Envelope) statusPayload {
	t.Helper()
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var p statusPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("status payload does not match the documented shape: %v\n%s", err, raw)
	}
	return p
}

func mustStatus(t *testing.T, dir string, args ...string) statusPayload {
	t.Helper()
	code, env, p := statusRun(t, dir, args...)
	if code != 0 {
		t.Fatalf("ocaw status %v: exit %d: %+v", args, code, env.Err)
	}
	return p
}

// populated builds a chain with one done task, one ready, one blocked, and a
// gate that has failed identically three times, which is the state the command
// exists to summarise.
func populated(t *testing.T) string {
	t.Helper()
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "scaffold", "--agent", "coder", "--gate", "test="+gateScript(t, "exit 1"))
	mustTask(t, dir, "add", "--id", "2", "--title", "invariants", "--agent", "coder", "--dep", "1")
	mustTask(t, dir, "add", "--id", "3", "--title", "render", "--agent", "coder", "--dep", "2")
	mustTask(t, dir, "set", "1", "--status", "done")
	return dir
}

func TestStatusOnAFreshWorkspace(t *testing.T) {
	dir := initialisedTaskRepo(t)
	p := mustStatus(t, dir)

	if p.ProjectType != "go" {
		t.Errorf("project_type = %q, want go", p.ProjectType)
	}
	if p.VerifyCmd != "go test ./..." {
		t.Errorf("verify_cmd = %q, want go test ./...", p.VerifyCmd)
	}
	if p.Total != 0 {
		t.Errorf("total = %d, want 0", p.Total)
	}
	if p.Next != nil {
		t.Errorf("next = %+v, want null on an empty DAG", p.Next)
	}
	// Every key present, every list an array. A status a caller has to guard
	// against nulls is a status that gets the nulls wrong.
	_, env, _ := statusRun(t, dir)
	raw, _ := json.Marshal(env.Data)
	for _, key := range []string{`"ready":[]`, `"stuck":[]`, `"stuck_tasks":[]`, `"notes":[]`, `"blocked":{}`, `"last_verification":null`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("status payload is missing %s: %s", key, raw)
		}
	}
	if !strings.Contains(string(raw), `"last_verification":null`) {
		t.Error("last_verification should be explicitly null, not absent")
	}
	if p.Budget.Declared {
		t.Error("budget.declared = true with no budget set")
	}
	if p.Render != "current" {
		t.Errorf("render = %q, want current right after init", p.Render)
	}
}

func TestStatusSummarisesTheDAG(t *testing.T) {
	dir := populated(t)
	p := mustStatus(t, dir)

	if p.Total != 3 {
		t.Errorf("total = %d, want 3", p.Total)
	}
	if p.Counts["done"] != 1 || p.Counts["pending"] != 2 {
		t.Errorf("counts = %v, want 1 done and 2 pending", p.Counts)
	}
	if len(p.Counts) != 5 {
		t.Errorf("counts has %d keys, want one per status: %v", len(p.Counts), p.Counts)
	}
	if p.Next == nil || p.Next.ID != "2" {
		t.Errorf("next = %+v, want 2", p.Next)
	}
	if len(p.Ready) != 1 || p.Ready[0] != "2" {
		t.Errorf("ready = %v, want [2]", p.Ready)
	}
	if len(p.Blocked) != 1 || len(p.Blocked["3"]) != 1 {
		t.Errorf("blocked = %v, want 3 blocked by 2", p.Blocked)
	}
	if p.Acceptance.Total != 0 {
		t.Errorf("acceptance = %+v, want none recorded yet", p.Acceptance)
	}
}

func TestStatusReadyAndBlockedAgreeWithTaskList(t *testing.T) {
	dir := populated(t)
	p := mustStatus(t, dir)

	readyList := mustTask(t, dir, "list", "--ready")
	blockedList := mustTask(t, dir, "list", "--blocked")

	if len(readyList.Tasks) != len(p.Ready) {
		t.Fatalf("status ready = %v, task list --ready = %d tasks", p.Ready, len(readyList.Tasks))
	}
	for i, task := range readyList.Tasks {
		if p.Ready[i] != task.ID {
			t.Errorf("ready[%d] = %q, task list says %q", i, p.Ready[i], task.ID)
		}
	}
	if len(blockedList.Tasks) != len(p.Blocked) {
		t.Fatalf("status blocked = %v, task list --blocked = %d tasks", p.Blocked, len(blockedList.Tasks))
	}
	for _, task := range blockedList.Tasks {
		if _, found := p.Blocked[task.ID]; !found {
			t.Errorf("status does not list %q as blocked", task.ID)
		}
	}
	// And the next task agrees with `task next`.
	nextList := mustTask(t, dir, "next")
	if p.Next == nil || nextList.Task == nil || p.Next.ID != nextList.Task.ID {
		t.Errorf("status next = %+v, task next = %+v", p.Next, nextList.Task)
	}
}

// AC9: three identical failing runs set stuck, and it is visible in status --json.
func TestAC9StuckIsVisibleInStatus(t *testing.T) {
	dir := populated(t)
	// One gate, three identical failures. `verify run` exits 0 when the run
	// happened: each run is recorded in full before the exit code is decided
	// (AC8), so three failing runs leave three records and three exit-4s.
	for range 3 {
		code, env, payload := verifyRun(t, dir, "run", "--task", "1", "--gate", "test", "--force")
		if code != 4 {
			t.Fatalf("verify run: exit %d, want 4: %+v", code, env.Err)
		}
		if env.Err == nil || env.Err.Code != envelope.CodeVerifyFailed {
			t.Fatalf("code = %v, want verify_failed", env.Err)
		}
		if len(payload.Attempts) != 1 || payload.Attempts[0].Status != "fail" {
			t.Fatalf("attempt = %+v, want one fail", payload.Attempts)
		}
	}
	p := mustStatus(t, dir)
	if len(p.StuckTasks) != 1 || p.StuckTasks[0] != "1" {
		t.Fatalf("stuck_tasks = %v, want [1]", p.StuckTasks)
	}
	if len(p.Stuck) != 1 {
		t.Fatalf("stuck = %+v, want one entry", p.Stuck)
	}
	s := p.Stuck[0]
	if s.Task != "1" || s.Gate != "test" || s.Attempts < 3 {
		t.Errorf("stuck entry = %+v, want task 1 gate test with at least 3 attempts", s)
	}
	// A null `task` key is not enough: it has to be a list a caller can range.
	_, env, _ := statusRun(t, dir)
	raw, _ := json.Marshal(env.Data)
	if !strings.Contains(string(raw), `"stuck_tasks":["1"]`) {
		t.Errorf("stuck_tasks is not a populated array: %s", raw)
	}
	// And the next task is not silently the stuck one.
	if p.Next != nil && p.Next.ID == "1" {
		t.Error("a stuck task should not be presented as ready work")
	}
}

func TestStatusLastVerification(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first", "--gate", "test="+gateScript(t, "exit 0"))

	// Before anything ran: explicitly null, not absent and not empty.
	p := mustStatus(t, dir)
	if p.LastVerification != nil {
		t.Errorf("last_verification = %+v, want null before any run", p.LastVerification)
	}

	code, env, _ := verifyRun(t, dir, "run", "--task", "1", "--gate", "test")
	if code != 0 {
		t.Fatalf("verify: exit %d: %+v", code, env.Err)
	}
	p = mustStatus(t, dir)
	if p.LastVerification == nil {
		t.Fatal("last_verification is null after a run")
	}
	last := *p.LastVerification
	if last.Task != "1" || last.Gate != "test" || last.Status != "pass" {
		t.Errorf("last_verification = %+v, want task 1 gate test pass", last)
	}
	if last.Exit == nil || *last.Exit != 0 {
		t.Errorf("last_verification exit = %v, want 0", last.Exit)
	}
	if last.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", last.Attempts)
	}
	if last.At == "" {
		t.Error("last_verification has no timestamp")
	}
}

func TestStatusFocusOnATask(t *testing.T) {
	dir := populated(t)
	p := mustStatus(t, dir, "--task", "3")
	if p.Task == nil {
		t.Fatal("--task produced no task")
	}
	focus := *p.Task
	if focus.Task.ID != "3" {
		t.Errorf("focus id = %q, want 3", focus.Task.ID)
	}
	if len(focus.UnmetDeps) != 1 || focus.UnmetDeps[0] != "2" {
		t.Errorf("unmet_deps = %v, want [2]", focus.UnmetDeps)
	}
	if focus.Runnable {
		t.Error("runnable = true for a task whose dep is pending")
	}
	if len(focus.Dependents) != 0 {
		t.Errorf("dependents = %v, want none", focus.Dependents)
	}
	if len(focus.History) != 0 {
		t.Errorf("history = %+v, want none", focus.History)
	}
	// A focused view of a task with no gate history still has the full shape.
	_, env, _ := statusRun(t, dir, "--task", "3")
	raw, _ := json.Marshal(env.Data)
	for _, key := range []string{`"unmet_deps":["2"]`, `"dependents":[]`, `"gates":[]`, `"history":[]`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("focus payload is missing %s: %s", key, raw)
		}
	}
	// And without --task the key is null.
	p = mustStatus(t, dir)
	if p.Task != nil {
		t.Errorf("task = %+v, want null without --task", p.Task)
	}

	code, env2, _ := statusRun(t, dir, "--task", "nope")
	if code != 7 {
		t.Errorf("unknown id: exit = %d, want 7", code)
	}
	if env2.Err == nil || env2.Err.Code != envelope.CodeTaskNotFound {
		t.Errorf("code = %v, want task_not_found", env2.Err)
	}
}

func TestStatusFocusCarriesGateHistory(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first", "--gate", "test="+gateScript(t, "exit 1"))
	for range 3 {
		if code, env, _ := verifyRun(t, dir, "run", "--task", "1", "--gate", "test", "--force"); code != 4 {
			t.Fatalf("verify: exit %d, want 4: %+v", code, env.Err)
		}
	}
	p := mustStatus(t, dir, "--task", "1")
	if p.Task == nil {
		t.Fatal("no focus")
	}
	focus := *p.Task
	if !focus.Task.Stuck {
		t.Error("focus task is not flagged stuck after three identical failures")
	}
	if len(focus.Gates) != 1 {
		t.Fatalf("gates = %+v, want one", focus.Gates)
	}
	if focus.Gates[0].Attempts < 3 {
		t.Errorf("gate attempts = %d, want at least 3", focus.Gates[0].Attempts)
	}
	if len(focus.History) != 3 {
		t.Errorf("history = %d records, want 3", len(focus.History))
	}
	if len(focus.Notes) == 0 {
		t.Error("a stuck task's focus carries no note saying so")
	}
}

// The whole point of the command. Asserted, not assumed.
func TestStatusSpawnsNoSubprocesses(t *testing.T) {
	dir := populated(t)
	// The gate above has already been run, so the log is populated and the
	// stuck path is live. Poison PATH so that any program ocaw shells out to
	// leaves a marker behind.
	marker := filepath.Join(t.TempDir(), "ran-a-subprocess")
	poison := t.TempDir()
	for _, name := range []string{"git", "go", "sh", "bash", "make", "npm", "node", "pytest", "cargo"} {
		script := fmt.Sprintf("#!/bin/sh\ntouch %q\nexit 0\n", marker)
		if err := os.WriteFile(filepath.Join(poison, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", poison)

	p := mustStatus(t, dir)
	if p.Total != 3 {
		t.Errorf("total = %d, want 3: the poisoned PATH should not have broken the read", p.Total)
	}
	mustStatus(t, dir, "--task", "2")

	if _, err := os.Stat(marker); err == nil {
		t.Error("status ran a subprocess")
	}
}

// The source-level counterpart, which is the one a runtime path cannot defeat.
// The five packages status reads its entire payload from must not be able to
// reach os/exec at all, and this file must not either.
func TestStatusDataPackagesDoNotImportOSExec(t *testing.T) {
	for _, pkg := range []string{"state", "report", "workspace", "yaml", "envelope"} {
		assertNoImport(t, filepath.Join("..", pkg), "os/exec", pkg)
	}
	// The cli package must not import it either: the verify command calls
	// internal/verify.Run, and that call is the boundary. If cli imported
	// os/exec, every command in it would be one refactor away from shelling out.
	assertNoImport(t, ".", "os/exec", "cli")
}

// assertNoImport fails when a non-test file in dir imports forbidden. Test
// files are exempt: building a git fixture legitimately runs git.
func assertNoImport(t *testing.T, dir, forbidden, label string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, spec := range parsed.Imports {
			imported, _ := strconv.Unquote(spec.Path.Value)
			if imported == forbidden {
				t.Errorf("internal/%s/%s imports %s; only internal/verify and internal/doctor may", label, name, forbidden)
			}
		}
	}
}

// Bounded on a large workspace. runs.jsonl is the only file whose size ocaw
// does not control, so it is the one that can make a read command slow.
func TestStatusIsBoundedOnALargeLog(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first", "--gate", "test="+gateScript(t, "exit 0"))

	// 40k records, each a few hundred bytes: several megabytes, far past the
	// 1 MiB read budget.
	log := filepath.Join(dir, ".agent", "state", "runs.jsonl")
	f, err := os.Create(log)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40_000 {
		fmt.Fprintf(f, `{"task":"1","gate":"test","cmd":["go","test"],"status":"fail","exit":1,"at":"2026-09-25T00:00:00Z","output":"%s","output_sha256":"%064d"}`+"\n",
			strings.Repeat("x", 120), i)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("log is %d bytes", info.Size())

	p := mustStatus(t, dir)
	if !p.HistoryTruncated {
		t.Error("history_truncated = false on a log well past the read limit")
	}
	if p.LastVerification == nil {
		t.Fatal("no last_verification from a large log")
	}
	// A note has to say so, or the caller is reading a tail and believes it is
	// reading everything.
	found := false
	for _, n := range p.Notes {
		if strings.Contains(n, "runs.jsonl is larger than") {
			found = true
		}
	}
	if !found {
		t.Errorf("truncation is not reported in notes: %v", p.Notes)
	}
}

// The last record is the last record, whichever end of the file it is on.
func TestStatusHistoryTruncationKeepsWholeRecords(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "first")
	log := filepath.Join(dir, ".agent", "state", "runs.jsonl")
	var b strings.Builder
	b.WriteString(`{"task":"1","gate":"test","cmd":["go","test"],"status":"pass","exit":0,"at":"2026-01-01T00:00:00Z","output":"early","output_sha256":"a"}` + "\n")
	// Push the interesting record past the read window with a long record.
	b.WriteString(`{"task":"1","gate":"test","cmd":["go","test"],"status":"fail","exit":1,"at":"2026-01-02T00:00:00Z","output":"` + strings.Repeat("y", 2<<20) + `","output_sha256":"b"}` + "\n")
	b.WriteString(`{"task":"1","gate":"test","cmd":["go","test"],"status":"fail","exit":1,"at":"2026-01-03T00:00:00Z","output":"late","output_sha256":"c"}` + "\n")
	if err := os.WriteFile(log, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	p := mustStatus(t, dir)
	if !p.HistoryTruncated {
		t.Fatal("expected truncation")
	}
	if p.LastVerification == nil {
		t.Fatal("no last_verification")
	}
	if p.LastVerification.Task != "1" || p.LastVerification.Gate != "test" {
		t.Errorf("last_verification = %+v, want the final record, not a fragment", p.LastVerification)
	}
	if p.LastVerification.Status != "fail" {
		t.Errorf("status = %q, want fail from the final record", p.LastVerification.Status)
	}
}

func TestStatusHumanRendering(t *testing.T) {
	dir := initialisedTaskRepo(t)
	mustTask(t, dir, "add", "--id", "1", "--title", "scaffold", "--agent", "coder", "--gate", "test="+gateScript(t, "exit 1"))
	mustTask(t, dir, "add", "--id", "2", "--title", "invariants", "--agent", "coder", "--dep", "1")
	for range 3 {
		if code, env, _ := verifyRun(t, dir, "run", "--task", "1", "--gate", "test", "--force"); code != 4 {
			t.Fatalf("verify: exit %d, want 4: %+v", code, env.Err)
		}
	}

	var out bytes.Buffer
	if code := cli.Main([]string{"--dir", dir, statusCommandName}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	text := out.String()
	if strings.Contains(text, `"command"`) {
		t.Fatalf("human status emitted JSON:\n%s", text)
	}
	// Stuck leads. A summary that buries the one actionable fact under a table
	// has failed at the only job it has.
	if idxStuck, idxRoot := strings.Index(text, "STUCK"), strings.Index(text, dir); idxStuck < 0 || idxStuck > idxRoot {
		t.Errorf("stuck does not lead the rendering:\n%s", text)
	}
	for _, want := range []string{"next     1", "ready    1", "verify   go test ./...", "last     1/test"} {
		if !strings.Contains(text, want) {
			t.Errorf("human status is missing %q:\n%s", want, text)
		}
	}

	out.Reset()
	if code := cli.Main([]string{"--dir", dir, statusCommandName, "--task", "2"}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "unmet    1") {
		t.Errorf("focused human status:\n%s", out.String())
	}

	// An empty workspace says so rather than printing nothing.
	empty := initialisedTaskRepo(t)
	out.Reset()
	if code := cli.Main([]string{"--dir", empty, statusCommandName}, &out, io.Discard, true); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "nothing ready") {
		t.Errorf("empty human status:\n%s", out.String())
	}
}

func TestStatusUsageErrors(t *testing.T) {
	dir := initialisedTaskRepo(t)
	code, env, _ := statusRun(t, dir, "extra")
	if code != 2 || env.Err == nil || env.Err.Code != envelope.CodeUsage {
		t.Errorf("exit = %d, code = %v, want 2 and usage", code, env.Err)
	}
	// An uninitialised directory is a precondition failure, not a usage one.
	bare := t.TempDir()
	code, env, _ = statusRun(t, bare)
	if code != 3 {
		t.Errorf("uninitialised: exit = %d, want 3", code)
	}
	if env.Err == nil || env.Err.Code != envelope.CodeWorkspaceMissing {
		t.Errorf("code = %v, want workspace_not_initialized", env.Err)
	}
}

func TestStatusFlagsDoNotLeak(t *testing.T) {
	first := initialisedTaskRepo(t)
	mustTask(t, first, "add", "--id", "1", "--title", "first")
	second := initialisedTaskRepo(t)
	mustTask(t, second, "add", "--id", "1", "--title", "first")
	mustTask(t, second, "add", "--id", "2", "--title", "second")

	mustStatus(t, first, "--task", "1")
	p := mustStatus(t, second)
	if p.Task != nil {
		t.Errorf("task = %+v, want null: --task leaked between invocations", p.Task)
	}
}

// status must take no lock, or an agent orienting while another works is told
// the wrong thing.
func TestStatusTakesNoLock(t *testing.T) {
	dir := populated(t)
	lock := filepath.Join(dir, ".agent", "state", "lock")
	held := fmt.Sprintf(`{"pid":%d,"host":"somewhere-else","time":%q}`, os.Getpid(), nowRFC())
	if err := os.WriteFile(lock, []byte(held), 0o644); err != nil {
		t.Fatal(err)
	}
	code, env, p := statusRun(t, dir)
	if code != 0 {
		t.Errorf("exit = %d, want 0: a read-only orientation must not fail because a write is in progress: %+v", code, env.Err)
	}
	if p.Total != 3 {
		t.Errorf("total = %d, want the full DAG read while locked", p.Total)
	}
	// A mutating command, by contrast, is refused.
	if code, _, _ := taskRun(t, dir, "add", "--id", "9", "--title", "nine"); code != 6 {
		t.Errorf("task add while locked: exit = %d, want 6", code)
	}
	// And status did not release someone else's lock on its way out.
	if _, err := os.Stat(lock); err != nil {
		t.Error("status removed a lock it did not take")
	}
}

// ocaw's entire process surface, in one place: exactly one file may build a
// command, and one may only look one up.
//
// internal/verify/run.go is the only file that may call exec.Command. It is what
// `ocaw verify` exists to do. internal/doctor may call exec.LookPath, which
// resolves a name against PATH and starts nothing — that is how it reports a
// stale verify_cmd without running it. Every other package must be unable to
// reach os/exec at all.
func TestOnlyVerifyBuildsCommands(t *testing.T) {
	root := ".."
	pkgs := []string{"state", "report", "workspace", "yaml", "envelope", "cli", "doctor", "verify"}
	for _, pkg := range pkgs {
		dir := filepath.Join(root, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			rel := "internal/" + pkg + "/" + name
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			body := string(raw)
			if !strings.Contains(body, `"os/exec"`) {
				continue
			}
			for _, call := range []string{"exec.Command(", "exec.CommandContext("} {
				if !strings.Contains(body, call) {
					continue
				}
				if rel != "internal/verify/run.go" {
					t.Errorf("%s calls %s; only internal/verify/run.go may build a command", rel, call)
				}
			}
			// LookPath starts nothing, so it is allowed anywhere.
			if strings.Contains(body, "exec.LookPath") && rel != "internal/doctor/doctor.go" && rel != "internal/verify/run.go" {
				t.Errorf("%s calls exec.LookPath; a program lookup belongs in internal/doctor only", rel)
			}
		}
	}
}
