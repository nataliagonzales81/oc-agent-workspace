package cli

import (
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

const statusCommandName = "status"

// statusData is the orientation payload.
//
// Everything here is read from three files — state.json, runs.jsonl, and
// agent.yaml — and the human rendering is a rendering of exactly this. The
// `Task` and `Next` keys are both always present; `task` is null unless
// --task named one, and `next` is null when the queue is empty. Two nullable
// keys that mean different things, rather than one that is overloaded.
type statusData struct {
	Root             string              `json:"root"`
	ProjectType      string              `json:"project_type"`
	VerifyCmd        string              `json:"verify_cmd"`
	Counts           map[string]int      `json:"counts"`
	Total            int                 `json:"total"`
	Ready            []string            `json:"ready"`
	Blocked          map[string][]string `json:"blocked"`
	Next             *state.TaskView     `json:"next"`
	Stuck            []state.Stuck       `json:"stuck"`
	StuckTasks       []string            `json:"stuck_tasks"`
	LastVerification *lastRun            `json:"last_verification"`
	Budget           statusBudget        `json:"budget"`
	Acceptance       acceptanceStatus    `json:"acceptance"`
	Render           string              `json:"render"`
	HistoryTruncated bool                `json:"history_truncated"`
	Task             *taskFocus          `json:"task"`
	Notes            []string            `json:"notes"`
}

// lastRun is the most recent attempt in the log, plus the gate's own attempt
// count. The record alone says what happened once; the count says whether it is
// the first try or the fortieth, which is the difference between a bug and a
// task nobody can finish.
type lastRun struct {
	Task     string `json:"task"`
	Gate     string `json:"gate"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	Exit     *int   `json:"exit"`
	At       string `json:"at"`
	Attempts int    `json:"attempts"`
}

type statusBudget struct {
	MaxTokens     int64 `json:"max_tokens"`
	SpentTokens   int64 `json:"spent_tokens"`
	Declared      bool  `json:"declared"`
	Remaining     int64 `json:"remaining"`
	OverBudget    bool  `json:"over_budget"`
	NoSpendReport bool  `json:"no_spend_report"`
}

type acceptanceStatus struct {
	Total int  `json:"total"`
	Done  int  `json:"done"`
	All   bool `json:"all_done"`
}

// taskFocus is what --task adds: one task with the context needed to decide
// what to do about it. Unmet deps say what is in the way, dependents say what
// will be held up, and the gate history says whether it has been failing the
// same way for a while.
type taskFocus struct {
	Task       state.TaskView `json:"task"`
	UnmetDeps  []string       `json:"unmet_deps"`
	Dependents []string       `json:"dependents"`
	Gates      []gateFocus    `json:"gates"`
	History    []state.Run    `json:"history"`
	Runnable   bool           `json:"runnable"`
	Notes      []string       `json:"notes"`
}

type gateFocus struct {
	Name     string `json:"name"`
	Command  string `json:"command"`
	Status   string `json:"status"`
	Exit     *int   `json:"exit"`
	Attempts int    `json:"attempts"`
	LastRun  string `json:"last_run"`
}

type statusFlags struct {
	task string
}

var statusOpts statusFlags

var statusCommand = &command{
	name:    statusCommandName,
	summary: "one-shot workspace summary; no subprocesses, no network",
	usage:   "ocaw status [--task <id>]",
	setup: func(fs *flag.FlagSet, c *Context) {
		// Reset before binding: flag.StringVar takes the current value as the
		// flag's default, so without this a second invocation in the same
		// process inherits the first one's --task.
		statusOpts = statusFlags{}
		fs.StringVar(&statusOpts.task, "task", statusOpts.task, "focus one task: its unmet deps, dependents, gates, and recent attempts")
	},
	run:   runStatus,
	human: humanStatus,
}

func runStatus(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: statusCommandName}
	if len(args) > 0 {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw status --help",
			"unexpected argument %q", args[0],
		)
		return res
	}

	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		res.Err = asEnvelopeError(err)
		return res
	}
	if missing := layout.Require(); missing != nil {
		res.Err = missing
		return res
	}

	st, loadErr := state.Load(layout)
	if loadErr != nil {
		res.Err = asEnvelopeError(loadErr)
		return res
	}
	// Bounded on purpose. The log is append-only and ocaw does not control how
	// fast it grows; a command whose cost is set by a file it did not write
	// stops being usable exactly when a workspace gets busy.
	runs, truncated, runsErr := state.LoadRunsTail(layout, 0)
	if runsErr != nil {
		// A malformed line in the middle of the log must not make the workspace
		// unorientable. Everything derived from runs.jsonl degrades; the DAG,
		// which is the point of this command, still reads.
		res.Warnings = append(res.Warnings, Warn(envelope.CodeValidationFailed,
			"ignoring the run log: %v", runsErr))
		runs, truncated = nil, false
	}

	data := newStatusData(layout, st, runs, truncated)
	if statusOpts.task != "" {
		focus, focusErr := focusTask(st, runs, statusOpts.task)
		if focusErr != nil {
			res.Err = focusErr
			return res
		}
		data.Task = focus
	}
	res.Data = data
	return res
}

func newStatusData(l *workspace.Layout, st *state.State, runs []state.Run, truncated bool) statusData {
	data := statusData{
		Root:             l.Root,
		Counts:           st.Counts(),
		Ready:            []string{},
		Blocked:          st.Blocked(),
		Stuck:            []state.Stuck{},
		StuckTasks:       []string{},
		HistoryTruncated: truncated,
		Notes:            []string{},
	}
	data.Total = len(st.Tasks)

	for _, task := range st.Ready() {
		data.Ready = append(data.Ready, task.ID)
	}

	// The project facts come from agent.yaml, which init wrote and doctor
	// validates. A workspace whose agent.yaml is unreadable still gets a status:
	// the fields are blank and a note says why, because "ocaw status printed
	// nothing useful" is worse than "project type unknown because agent.yaml is
	// broken".
	if agent, err := l.OcawManifest(); err == nil {
		data.ProjectType = agent.Project.Type
		data.VerifyCmd = agent.Project.VerifyCmd
	} else {
		data.Notes = append(data.Notes, "could not read agent.yaml: "+err.Error())
	}

	// StuckGates and Dependents return nil for "none", which encoding/json
	// writes as null. A caller that ranges over stuck gets nothing and a caller
	// that checks len(nil) gets 0, so it works — until the day one of them
	// marshals it and the other reads it. Normalised here, once.
	stuckGates := state.StuckGates(runs)
	data.Stuck = stuckGates
	if data.Stuck == nil {
		data.Stuck = []state.Stuck{}
	}
	seen := map[string]bool{}
	for _, s := range stuckGates {
		if !seen[s.Task] {
			seen[s.Task] = true
			data.StuckTasks = append(data.StuckTasks, s.Task)
		}
	}
	sort.Strings(data.StuckTasks)

	// Stuck is the single most actionable fact in the file, so it leads the
	// human rendering. An agent reading only `stuck_tasks` gets the one thing
	// worth acting on without parsing anything else.
	if len(data.StuckTasks) > 0 {
		data.Notes = append(data.Notes, fmt.Sprintf(
			"%d task(s) have produced byte-identical gate output %d times running; escalate rather than retry",
			len(data.StuckTasks), state.StuckThreshold))
	}

	stuckMap := state.StuckTasks(runs)
	if next := st.Next(); next != nil {
		view := state.TaskView{Task: *next, Stuck: stuckMap[next.ID]}
		data.Next = &view
	}

	if len(runs) > 0 {
		last := runs[len(runs)-1]
		record := lastRun{
			Task:    last.TaskID,
			Gate:    last.Gate,
			Command: strings.Join(last.Cmd, " "),
			Status:  string(last.Status),
			Exit:    last.ExitCode,
			At:      last.At,
		}
		// The log's own record has no attempt count; the gate does, and it is
		// the gate the caller will look at next.
		if task, found := st.Find(last.TaskID); found {
			if gate, ok := task.Gate(last.Gate); ok {
				record.Attempts = gate.Attempts
			}
		}
		data.LastVerification = &record
	}

	data.Budget = statusBudget{
		MaxTokens:   st.Budget.MaxTokens,
		SpentTokens: st.Budget.SpentTokens,
		Declared:    st.Budget.MaxTokens > 0,
	}
	if data.Budget.Declared {
		data.Budget.Remaining = st.Budget.MaxTokens - st.Budget.SpentTokens
		data.Budget.OverBudget = data.Budget.Remaining < 0
	}
	// ocaw does not track token spend, so a declared budget is a number someone
	// set and nothing is currently adding to. Saying so is more useful than
	// printing a spent figure that will only ever be zero.
	data.Budget.NoSpendReport = true

	data.Acceptance = acceptanceStatus{Total: len(st.Workflow.Acceptance)}
	for _, item := range st.Workflow.Acceptance {
		if item.Done {
			data.Acceptance.Done++
		}
	}
	data.Acceptance.All = data.Acceptance.Total > 0 && data.Acceptance.Done == data.Acceptance.Total

	// Whether the human view still matches the machine one. Cheap, and a
	// mismatched file is exactly the kind of thing an agent reads and believes.
	if doc, found, docErr := report.Load(l); docErr == nil && found {
		if renderStatus, _ := report.Check(doc, st); renderStatus != report.StatusMissing {
			data.Render = string(renderStatus)
		}
	} else {
		data.Render = string(report.StatusMissing)
	}
	if data.Render == string(report.StatusEdited) || data.Render == string(report.StatusForeign) {
		data.Notes = append(data.Notes, "WORKFLOW_STATE.md has been edited by hand; ocaw report --write regenerates it from state.json")
	}
	if data.Render == string(report.StatusStale) {
		data.Notes = append(data.Notes, "state.json has changed since WORKFLOW_STATE.md was rendered")
	}
	if truncated {
		data.Notes = append(data.Notes, fmt.Sprintf(
			"runs.jsonl is larger than the %d-byte read limit, so the history below is the tail of it", state.MaxHistoryBytes))
	}
	return data
}

// focusTask builds the --task view. Its history is the tail of the log for that
// task, newest last, capped so a focused view cannot itself become the slow
// thing about a fast command.
func focusTask(st *state.State, runs []state.Run, id string) (*taskFocus, *envelope.Error) {
	task, found := st.Find(id)
	if !found {
		return nil, envelope.Errorf(
			envelope.CodeTaskNotFound,
			"ocaw task list",
			"no task with id %q", id,
		)
	}
	focus := &taskFocus{
		Task:       state.TaskView{Task: *task},
		UnmetDeps:  []string{},
		Dependents: []string{},
		Gates:      []gateFocus{},
		History:    []state.Run{},
		Notes:      []string{},
	}
	if focus.UnmetDeps = st.UnmetDeps(id); focus.UnmetDeps == nil {
		focus.UnmetDeps = []string{}
	}
	if focus.Dependents = st.Dependents(id); focus.Dependents == nil {
		focus.Dependents = []string{}
	}
	focus.Runnable = task.Status == state.StatusPending && len(focus.UnmetDeps) == 0

	for _, g := range task.Gates {
		gf := gateFocus{
			Name:     g.Name,
			Command:  g.Cmd,
			Status:   string(g.Status),
			Exit:     g.LastExit,
			Attempts: g.Attempts,
			LastRun:  stringOrEmpty(g.LastRun),
		}
		focus.Gates = append(focus.Gates, gf)
	}

	// The trailing attempts for this task, newest last. Stuck detection already
	// looked at the whole window, so this is for a human reading why.
	var mine []state.Run
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].TaskID == id {
			mine = append(mine, runs[i])
			if len(mine) == maxFocusedRuns {
				break
			}
		}
	}
	for i := len(mine) - 1; i >= 0; i-- {
		focus.History = append(focus.History, mine[i])
	}

	if stuck := state.StuckGates(runs); len(stuck) > 0 {
		for _, s := range stuck {
			if s.Task == id {
				focus.Notes = append(focus.Notes, fmt.Sprintf(
					"gate %q has produced identical output %d times since %s", s.Gate, s.Attempts, s.Since))
			}
		}
		focus.Task.Stuck = true
	}
	if len(focus.Dependents) > 0 {
		focus.Notes = append(focus.Notes, "removing or re-opening this task is refused while those dependents exist")
	}
	return focus, nil
}

// maxFocusedRuns caps the history a focused view carries. A command whose cost
// is set by a file it did not write stops being usable exactly when a workspace
// gets busy.
const maxFocusedRuns = 10

func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ---- human rendering ----

func humanStatus(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(statusData)
	if !ok {
		return fmt.Errorf("status: unexpected data type %T", env.Data)
	}

	// Stuck first. It is the one fact worth reading before anything else, and a
	// summary that buries it under a table has failed at the only job it has.
	if len(data.StuckTasks) > 0 {
		if _, err := fmt.Fprintf(w, "STUCK\n"); err != nil {
			return err
		}
		for _, s := range data.Stuck {
			if _, err := fmt.Fprintf(w, "  %s %s  %d identical attempts since %s\n", s.Task, s.Gate, s.Attempts, s.Since); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "\n"); err != nil {
			return err
		}
	}

	head := data.Root
	if data.ProjectType != "" {
		head += "  (" + data.ProjectType + ")"
	}
	if _, err := fmt.Fprintf(w, "%s\n", head); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  %s\n", countsInline(data.Counts)); err != nil {
		return err
	}
	if data.VerifyCmd != "" {
		if _, err := fmt.Fprintf(w, "  verify   %s\n", data.VerifyCmd); err != nil {
			return err
		}
	}

	if data.Next != nil {
		stuck := ""
		if data.Next.Stuck {
			stuck = "  STUCK"
		}
		if _, err := fmt.Fprintf(w, "  next     %s  %s%s\n", data.Next.ID, cellOrDash(data.Next.Title), stuck); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "  next     nothing ready\n"); err != nil {
			return err
		}
	}

	if len(data.Ready) > 0 {
		if _, err := fmt.Fprintf(w, "  ready    %s\n", strings.Join(data.Ready, ", ")); err != nil {
			return err
		}
	}
	if len(data.Blocked) > 0 {
		ids := make([]string, 0, len(data.Blocked))
		for id := range data.Blocked {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if _, err := fmt.Fprintf(w, "  blocked  %s\n", strings.Join(ids, ", ")); err != nil {
			return err
		}
	}
	if data.LastVerification != nil {
		last := data.LastVerification
		exit := "-"
		if last.Exit != nil {
			exit = fmt.Sprintf("%d", *last.Exit)
		}
		if _, err := fmt.Fprintf(w, "  last     %s/%s %s exit %s at %s (attempt %d)\n",
			last.Task, last.Gate, last.Status, exit, last.At, last.Attempts); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "  last     never run\n"); err != nil {
			return err
		}
	}
	if data.Budget.Declared {
		if _, err := fmt.Fprintf(w, "  budget   %d / %d tokens%s\n",
			data.Budget.SpentTokens, data.Budget.MaxTokens, overBudgetSuffix(data.Budget.OverBudget)); err != nil {
			return err
		}
	} else {
		if _, err := fmt.Fprintf(w, "  budget   none declared\n"); err != nil {
			return err
		}
	}
	if data.Acceptance.Total > 0 {
		if _, err := fmt.Fprintf(w, "  accept   %d / %d\n", data.Acceptance.Done, data.Acceptance.Total); err != nil {
			return err
		}
	}
	if data.Render != "" && data.Render != string("current") {
		if _, err := fmt.Fprintf(w, "  render   %s\n", data.Render); err != nil {
			return err
		}
	}

	if data.Task != nil {
		if err := writeFocus(w, *data.Task); err != nil {
			return err
		}
	}
	for _, note := range data.Notes {
		if _, err := fmt.Fprintf(w, "  note     %s\n", note); err != nil {
			return err
		}
	}
	return nil
}

func writeFocus(w io.Writer, focus taskFocus) error {
	if _, err := fmt.Fprintf(w, "\ntask %s  %s\n", focus.Task.ID, cellOrDash(focus.Task.Title)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "  status   %s  runnable=%t\n", focus.Task.Status, focus.Runnable); err != nil {
		return err
	}
	if len(focus.UnmetDeps) > 0 {
		if _, err := fmt.Fprintf(w, "  unmet    %s\n", strings.Join(focus.UnmetDeps, ", ")); err != nil {
			return err
		}
	}
	if len(focus.Dependents) > 0 {
		if _, err := fmt.Fprintf(w, "  dependents %s\n", strings.Join(focus.Dependents, ", ")); err != nil {
			return err
		}
	}
	for _, g := range focus.Gates {
		exit := "-"
		if g.Exit != nil {
			exit = fmt.Sprintf("%d", *g.Exit)
		}
		if _, err := fmt.Fprintf(w, "  gate     %s: %s (%s, exit %s, %d attempt(s))\n", g.Name, g.Command, g.Status, exit, g.Attempts); err != nil {
			return err
		}
	}
	for _, note := range focus.Notes {
		if _, err := fmt.Fprintf(w, "  note     %s\n", note); err != nil {
			return err
		}
	}
	return nil
}

func overBudgetSuffix(over bool) string {
	if over {
		return "  OVER BUDGET"
	}
	return ""
}
