package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/verify"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

const verifyCommandName = "verify"

// verifyData is the payload for all three subcommands. One shape, every key
// always present, for the same reason `ocaw task` has one: a caller should not
// have to know which subcommand it ran before it can read a field.
type verifyData struct {
	Subcommand string        `json:"subcommand"`
	Command    string        `json:"command"`
	Argv       []string      `json:"argv"`
	Project    string        `json:"project_type"`
	Detected   string        `json:"detected_from"`
	Saved      bool          `json:"saved"`
	Attempts   []attemptData `json:"attempts"`
	Runs       []state.Run   `json:"runs"`
	Truncated  bool          `json:"history_truncated"`
	Timeout    string        `json:"timeout"`
	Skipped    []string      `json:"skipped"`
	Stuck      []state.Stuck `json:"stuck"`
	Notes      []string      `json:"notes"`
	DryRun     bool          `json:"dry_run"`
}

// attemptData is one gate's outcome. It is the same fields as the record in
// runs.jsonl plus the flag ocaw adds: a timeout gets its own error code, and a
// skipped gate says why rather than being absent.
type attemptData struct {
	Task      string           `json:"task"`
	Gate      string           `json:"gate"`
	Command   string           `json:"command"`
	Status    state.GateStatus `json:"status"`
	Exit      *int             `json:"exit"`
	At        string           `json:"at"`
	Attempts  int              `json:"attempts"`
	Stuck     bool             `json:"stuck"`
	TimedOut  bool             `json:"timed_out"`
	Skipped   bool             `json:"skipped"`
	Reason    string           `json:"reason,omitempty"`
	Output    string           `json:"output,omitempty"`
	Truncated bool             `json:"output_truncated,omitempty"`
	SHA256    string           `json:"output_sha256,omitempty"`
}

type verifyFlags struct {
	task    string
	gate    string
	limit   int
	timeout string
	force   bool
	save    bool
	rawCmd  string
	hasCmd  bool
	shell   bool
	verbose bool
}

var verifyFlagsState verifyFlags

// hasCommandMarker is the `--` the caller types to end ocaw's own flags.
const hasCommandMarker = "--"

// verifySub is one subcommand. It is not taskSub: the two commands share a
// dispatch shape but nothing else, and a single struct with fields only one of
// them uses is a struct both of them are tempted to misuse.
type verifySub struct {
	name    string
	summary string
	setup   func(fs *flag.FlagSet)
	run     func(c *Context) (verifyData, []envelope.Warning, *envelope.Error)
	human   humanFunc
}

var verifySubs = []*verifySub{
	{
		name:    "detect",
		summary: "re-detect the project's verification command",
		setup: func(fs *flag.FlagSet) {
			fs.BoolVar(&verifyFlagsState.force, "force", false, "overwrite a verify_cmd the project already chose")
			fs.BoolVar(&verifyFlagsState.save, "save", false, "write the detected command back to agent.yaml")
		},
		run:   runVerifyDetect,
		human: humanVerifyDetect,
	},
	{
		name:    "run",
		summary: "run a gate and record the attempt",
		setup: func(fs *flag.FlagSet) {
			fs.StringVar(&verifyFlagsState.task, "task", "", "only this task's gates")
			fs.StringVar(&verifyFlagsState.gate, "gate", "", "only this gate")
			fs.StringVar(&verifyFlagsState.timeout, "timeout", "", "deadline per gate: 90s, 10m, or a whole number of seconds")
			fs.BoolVar(&verifyFlagsState.force, "force", false, "re-run gates that already pass")
			fs.BoolVar(&verifyFlagsState.verbose, "verbose", false, "include captured output in data")
		},
		run:   runVerifyRun,
		human: humanVerifyRun,
	},
	{
		name:    "history",
		summary: "show recorded attempts",
		setup: func(fs *flag.FlagSet) {
			fs.StringVar(&verifyFlagsState.task, "task", "", "only this task's attempts")
			fs.IntVar(&verifyFlagsState.limit, "limit", 20, "most recent N records")
		},
		run:   runVerifyHistory,
		human: humanVerifyHistory,
	},
}

var verifyCommand = &command{
	name:      verifyCommandName,
	summary:   "run the project's verification and record what happened",
	usage:     "ocaw verify <" + verifySubNames() + "> [flags]",
	takesArgs: true,
	run:       runVerify,
	human:     humanVerifyDispatch,
}

func verifySubNames() string {
	names := make([]string, 0, len(verifySubs))
	for _, sub := range verifySubs {
		names = append(names, sub.name)
	}
	return strings.Join(names, "|")
}

func verifySubByName(name string) *verifySub {
	for _, sub := range verifySubs {
		if sub.name == name {
			return sub
		}
	}
	return nil
}

func newVerifyData(sub string, dryRun bool) verifyData {
	return verifyData{
		Subcommand: sub,
		Argv:       []string{},
		Attempts:   []attemptData{},
		Runs:       []state.Run{},
		Skipped:    []string{},
		Stuck:      []state.Stuck{},
		Notes:      []string{},
		DryRun:     dryRun,
	}
}

func runVerify(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: verifyCommandName}

	if len(args) == 0 {
		res.Err = envelope.Errorf(envelope.CodeUsage, "ocaw verify --help",
			"verify needs a subcommand: %s", verifySubNames())
		res.Data = map[string]any{"available_subcommands": []string{"detect", "history", "run"}}
		return res
	}
	sub := verifySubByName(args[0])
	if sub == nil {
		res.Err = envelope.Errorf(envelope.CodeUsage, "ocaw verify --help",
			"unknown subcommand %q; expected %s", args[0], verifySubNames())
		res.Data = map[string]any{"available_subcommands": []string{"detect", "history", "run"}}
		return res
	}

	fs := newFlagSet("ocaw verify " + sub.name)
	registerGlobals(fs, &c.Options)
	verifyFlagsState = verifyFlags{}
	fs.BoolVar(&verifyFlagsState.shell, "shell", false, "run the command through a shell — always refused (§9.4)")
	if sub.setup != nil {
		sub.setup(fs)
	}
	// The `-- <cmd>` tail is split off before the parse, and that ordering is
	// forced rather than chosen: Go's flag package treats a bare `--` as the end
	// of the flags and does not return it, so a tail separated afterwards is
	// already gone. Splitting first is also what makes a gate's own flags
	// impossible to confuse with ocaw's.
	head, tail, hasTail, uerr := splitCommandArg(args[1:])
	if uerr != nil {
		res.Err = uerr
		return res
	}
	verifyFlagsState.rawCmd = tail
	verifyFlagsState.hasCmd = hasTail

	rest, parseErr := parseInterspersed(fs, head)
	if parseErr != nil {
		res.Err = envelope.Errorf(envelope.CodeUsage,
			"ocaw verify "+sub.name+" --help", "%s", parseErr.Error())
		return res
	}
	if len(rest) > 0 {
		res.Err = envelope.Errorf(envelope.CodeUsage,
			"ocaw verify "+sub.name+" --help", "unexpected argument %q", rest[0])
		return res
	}

	data, warnings, err := dispatchVerify(c, sub)
	if data.Subcommand == "" {
		data = newVerifyData(sub.name, c.Options.DryRun)
	}
	res.Data = data
	res.Warnings = warnings
	if err != nil {
		res.Err = err
	}
	return res
}

// splitCommandArg separates the `-- <cmd>` tail from ocaw's own flags, and is
// called before the FlagSet ever sees the arguments.
func splitCommandArg(args []string) (head []string, tail string, has bool, uerr *envelope.Error) {
	at := -1
	for i, arg := range args {
		if arg == hasCommandMarker {
			at = i
			break
		}
	}
	if at < 0 {
		return args, "", false, nil
	}
	head = args[:at]
	tail = strings.TrimSpace(strings.Join(args[at+1:], " "))
	if tail == "" {
		return nil, "", true, envelope.Errorf(envelope.CodeUsage, "ocaw verify run -- <cmd>",
			"-- was given but no command follows it")
	}
	return head, tail, true, nil
}

func dispatchVerify(c *Context, sub *verifySub) (verifyData, []envelope.Warning, *envelope.Error) {
	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		return verifyData{}, nil, asEnvelopeError(err)
	}
	if missing := layout.Require(); missing != nil {
		return verifyData{}, nil, missing
	}
	switch sub.name {
	case "detect":
		return runVerifyDetect(c)
	case "run":
		return runVerifyRun(c)
	default:
		return runVerifyHistory(c)
	}
}

// ---- detect ----

// runVerifyDetect re-detects the project type and the conventional verify
// command.
//
// It never overwrites a verify_cmd the project already chose unless --force is
// given: §5.1 is explicit that the recorded command is changed only by explicit
// request, and a project that has moved to Bazel or just has a correct answer
// that detection would replace with a guess.
func runVerifyDetect(c *Context) (verifyData, []envelope.Warning, *envelope.Error) {
	data := newVerifyData("detect", c.Options.DryRun)

	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		return data, nil, asEnvelopeError(err)
	}
	raw, readErr := workspace.ReadFile(layout.AgentYAML())
	if readErr != nil {
		return data, nil, asEnvelopeError(readErr)
	}
	agent, perr := yaml.ParseAgent(string(raw))
	if perr != nil {
		return data, nil, yamlErr(perr, layout.Rel(layout.AgentYAML()))
	}

	mark := markFor(agent.Project.Type)
	data.Project = agent.Project.Type
	data.Detected = mark.Marker

	// Checked before tokenising, so the message names the actual problem — a
	// project type with no convention — rather than reporting the empty string
	// that stands in for one.
	if mark.Verify == "" {
		return data, nil, envelope.Errorf(
			envelope.CodeNotAProject,
			"set project.verify_cmd in .agent/agent.yaml",
			"no verification command is conventional for project type %q; set one by hand", agent.Project.Type,
		)
	}
	argv, terr := verify.Tokenize(mark.Verify)
	if terr != nil {
		return data, nil, asEnvelopeError(terr)
	}
	data.Command = mark.Verify
	data.Argv = argv

	switch {
	case agent.Project.VerifyCmd == mark.Verify:
		// Nothing to do. Saying so is more useful than reporting a change that
		// did not happen.
		return data, nil, nil
	case !verifyFlagsState.save && !verifyFlagsState.force:
		data.Notes = []string{"the project already has a verify_cmd; pass --save to replace it or --force --save to replace it with the detected one"}
		return data, nil, nil
	}

	if c.Options.DryRun {
		data.Notes = append(data.Notes, "dry run: agent.yaml was not written")
		return data, nil, nil
	}
	agent.Project.VerifyCmd = mark.Verify
	agent.Project.VerifyDetected = time.Now().UTC().Format(time.RFC3339)
	if err := workspace.WriteFileAtomic(layout.AgentYAML(), []byte(agent.Encode())); err != nil {
		return data, nil, writeFailure("write agent.yaml", err)
	}
	data.Saved = true
	return data, nil, nil
}

// markFor finds the detection rule for a project type. The rules are the ones
// `ocaw init` used, reached from the other direction: init writes the type,
// detect reads it back and finds the command that type implies.
func markFor(projectType string) projectMark {
	for _, m := range markerTable {
		if m.Type == projectType {
			return m
		}
	}
	return projectMark{Type: projectType}
}

// ---- run ----

// runVerifyRun runs gates and records every attempt.
//
// A gate that already passes is not re-run without --force. A gate is a claim
// that something passed; re-running it to collect another identical record
// would make the history unreadable and, worse, would make "it has passed
// recently" mean less each time.
func runVerifyRun(c *Context) (verifyData, []envelope.Warning, *envelope.Error) {
	data := newVerifyData("run", c.Options.DryRun)

	timeout, terr := verify.ParseTimeout(verifyFlagsState.timeout)
	if terr != nil {
		return data, nil, envelope.Errorf(envelope.CodeUsage, "ocaw verify run --help", "%s", terr.Error())
	}
	data.Timeout = timeout.String()

	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		return data, nil, asEnvelopeError(err)
	}
	st, loadErr := state.Load(layout)
	if loadErr != nil {
		return data, nil, asEnvelopeError(loadErr)
	}

	requests, planErr := planRuns(c, layout, st)
	if planErr != nil {
		return data, nil, planErr
	}
	if len(requests) == 0 {
		data.Skipped = []string{"no gates are defined; add one with `ocaw task set <id> --gate name=command`"}
		return data, nil, nil
	}

	if c.Options.DryRun {
		// A dry run says which gates it would run, and runs none of them. The
		// recorded history is untouched, which is the whole point.
		for _, req := range requests {
			data.Attempts = append(data.Attempts, attemptData{
				Task: req.TaskID, Gate: req.Gate, Command: strings.Join(req.Argv, " "),
				Status: state.GatePending, Skipped: true, Reason: "dry run",
			})
		}
		data.Notes = []string{"dry run: nothing was executed and no attempt was recorded"}
		return data, nil, nil
	}

	// The lock is held across every gate in the run, not released between them.
	// A run is one decision about the workspace, and interleaving another
	// agent's `task set` into the middle of it would leave the recorded attempt
	// describing a state that never existed.
	lock, warnings, lockErr := workspace.Acquire(layout)
	if lockErr != nil {
		return data, nil, asEnvelopeError(lockErr)
	}
	defer func() {
		if relErr := lock.Release(); relErr != nil {
			warnings = append(warnings, Warn(envelope.CodeLockHeld,
				"could not release the workspace lock at %s: %v", lock.Path(), relErr))
		}
	}()

	prior, priorErr := state.LoadRuns(layout)
	if priorErr != nil {
		return data, warnings, asEnvelopeError(priorErr)
	}
	recorded := append([]state.Run{}, prior...)

	// A gate named by an ad-hoc `-- <cmd>` is defined on the task first, so the
	// task and the run log cannot disagree about which gates exist. A record
	// naming a gate the task has never heard of would leave stuck detection
	// flagging a gate with no definition, which doctor then reports.
	if verifyFlagsState.hasCmd {
		if serr := defineAdhocGate(layout, st, requests[0]); serr != nil {
			return data, warnings, serr
		}
	}

	for _, req := range requests {
		attempt := verify.Run(req)
		record := state.NewRun(attempt.TaskID, attempt.Gate, attempt.Argv,
			state.GateStatus(attempt.Status), attempt.Exit, attempt.Output, attempt.At)
		recorded = append(recorded, record)
		if err := state.AppendRun(layout, record); err != nil {
			return data, warnings, writeFailure("append to runs.jsonl", err)
		}
		if gate, ok := st.Find(attempt.TaskID); ok {
			if g, ok := gate.Gate(attempt.Gate); ok {
				// Recorded by mutating the pointer, not through SetGate: SetGate
				// is the *definition* installer and would reset the outcome of a
				// gate whose command has not changed.
				g.Status = state.GateStatus(attempt.Status)
				g.LastExit = attempt.Exit
				g.LastRun = &record.At
				g.Attempts++
			}
		}
		entry := attemptToData(attempt, record, false, "", verifyFlagsState.verbose)
		// The count is the total, not the number this run produced: "attempt 3"
		// is only useful if it means the third, ever.
		entry.Attempts = countRuns(recorded, attempt.TaskID, attempt.Gate)
		data.Attempts = append(data.Attempts, entry)
	}

	if err := state.Save(layout, st); err != nil {
		return data, warnings, writeFailure("write state.json", err)
	}
	if _, err := report.Write(layout, st); err != nil {
		return data, warnings, writeFailure("write "+layout.Rel(layout.StateMDPath()), err)
	}

	stuck := state.StuckGates(recorded)
	data.Stuck = stuck
	for i := range data.Attempts {
		for _, s := range stuck {
			if s.Task == data.Attempts[i].Task && s.Gate == data.Attempts[i].Gate {
				data.Attempts[i].Stuck = true
			}
		}
	}
	return data, warnings, nil
}

// planRuns decides which gates to run, and reports the ones being skipped.
func planRuns(c *Context, layout *workspace.Layout, st *state.State) ([]verify.Request, *envelope.Error) {
	// An explicit `-- <cmd>` overrides the recorded gates for the named task.
	// It is the escape hatch for a one-off check, and the record still names a
	// task and a gate so the history stays readable.
	if verifyFlagsState.hasCmd {
		argv, err := commandArgv(verifyFlagsState.rawCmd)
		if err != nil {
			return nil, err
		}
		if verifyFlagsState.task == "" {
			return nil, envelope.Errorf(
				envelope.CodeUsage,
				"ocaw verify run --task <id> -- <cmd>",
				"a command given with -- has to be recorded against a task; pass --task",
			)
		}
		taskID, gateName := verifyFlagsState.task, verifyFlagsState.gate
		if gateName == "" {
			gateName = "adhoc"
		}
		return []verify.Request{{
			TaskID: taskID, Gate: gateName, Argv: argv,
			Dir: layout.Root, Timeout: timeoutOrDefault(),
		}}, nil
	}

	if verifyFlagsState.task != "" {
		task, found := st.Find(verifyFlagsState.task)
		if !found {
			return nil, envelope.Errorf(envelope.CodeTaskNotFound, "ocaw task list",
				"no task with id %q", verifyFlagsState.task)
		}
		return gatesFor(layout, task, verifyFlagsState.gate)
	}

	var out []verify.Request
	for i := range st.Tasks {
		reqs, err := gatesFor(layout, &st.Tasks[i], verifyFlagsState.gate)
		if err != nil {
			return nil, err
		}
		out = append(out, reqs...)
	}
	return out, nil
}

func gatesFor(layout *workspace.Layout, task *state.Task, only string) ([]verify.Request, *envelope.Error) {
	var out []verify.Request
	for _, g := range task.Gates {
		if only != "" && g.Name != only {
			continue
		}
		if g.Status == state.GatePass && exitIsZero(g.LastExit) && !verifyFlagsState.force {
			// Skipped, and said so. A gate that is silently absent from a run
			// looks like a gate that was forgotten.
			out = append(out, verify.Request{})
			continue
		}
		argv, err := verify.Tokenize(g.Cmd)
		if err != nil {
			return nil, asEnvelopeError(err)
		}
		out = append(out, verify.Request{
			TaskID: task.ID, Gate: g.Name, Argv: argv,
			Dir: layout.Root, Timeout: timeoutOrDefault(),
		})
	}
	kept := out[:0]
	for _, r := range out {
		if len(r.Argv) == 0 {
			continue
		}
		kept = append(kept, r)
	}
	return kept, nil
}

func exitIsZero(exit *int) bool { return exit != nil && *exit == 0 }

// defineAdhocGate installs the gate an explicit `-- <cmd>` names. SetGate
// keeps the recorded outcome when the command has not changed, so running the
// same ad-hoc command twice does not reset its history.
func defineAdhocGate(layout *workspace.Layout, st *state.State, req verify.Request) *envelope.Error {
	task, found := st.Find(req.TaskID)
	if !found {
		return envelope.Errorf(envelope.CodeTaskNotFound, "ocaw task list",
			"no task with id %q", req.TaskID)
	}
	task.SetGate(state.Gate{Name: req.Gate, Cmd: strings.Join(req.Argv, " ")})
	return nil
}

func countRuns(runs []state.Run, taskID, gate string) int {
	n := 0
	for _, r := range runs {
		if r.TaskID == taskID && r.Gate == gate {
			n++
		}
	}
	return n
}

func timeoutOrDefault() time.Duration {
	d, err := verify.ParseTimeout(verifyFlagsState.timeout)
	if err != nil {
		return verify.DefaultTimeout
	}
	return d
}

// commandArgv turns the `-- <cmd>` tail into an argv, or refuses it.
//
// --shell is accepted and then refused. §9.4 says a shell string is permitted
// only under an explicit --shell, and also that no built-in gate ever uses it;
// a flag that is parseable and always errors is worse than one that is not
// there, because a caller who typed it will read the error as a bug in ocaw
// rather than as the rule it is.
func commandArgv(raw string) ([]string, *envelope.Error) {
	if verifyFlagsState.shell {
		return nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw verify run -- <cmd>",
			"--shell is refused: ocaw runs an argv, never a shell string (SPEC §9.4)",
		)
	}
	argv, err := verify.Tokenize(raw)
	if err != nil {
		return nil, asEnvelopeError(err)
	}
	if len(argv) == 0 {
		return nil, envelope.Errorf(envelope.CodeUsage, "ocaw verify run -- <cmd>",
			"the command is empty")
	}
	return argv, nil
}

func attemptToData(a verify.Attempt, record state.Run, skipped bool, reason string, verbose bool) attemptData {
	out := attemptData{
		Task:      a.TaskID,
		Gate:      a.Gate,
		Command:   strings.Join(a.Argv, " "),
		Status:    state.GateStatus(a.Status),
		Exit:      a.Exit,
		At:        record.At,
		Skipped:   skipped,
		Reason:    reason,
		TimedOut:  a.TimedOut,
		SHA256:    record.OutputSHA256,
		Truncated: record.Truncated,
	}
	if verbose {
		out.Output = record.Output
	}
	return out
}

// ---- history ----

func runVerifyHistory(c *Context) (verifyData, []envelope.Warning, *envelope.Error) {
	data := newVerifyData("history", c.Options.DryRun)
	limit := verifyFlagsState.limit
	if limit <= 0 {
		limit = 20
	}

	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		return data, nil, asEnvelopeError(err)
	}
	runs, runsErr := state.LoadRuns(layout)
	if runsErr != nil {
		return data, nil, asEnvelopeError(runsErr)
	}
	if verifyFlagsState.task != "" {
		// The task has to exist. "The state loaded" is not the same question,
		// and answering the easier one reports an empty history for a typo as
		// though nothing had ever run.
		st, loadErr := state.Load(layout)
		if loadErr != nil {
			return data, nil, asEnvelopeError(loadErr)
		}
		if _, found := st.Find(verifyFlagsState.task); !found {
			return data, nil, envelope.Errorf(envelope.CodeTaskNotFound, "ocaw task list",
				"no task with id %q", verifyFlagsState.task)
		}
		filtered := make([]state.Run, 0, len(runs))
		for _, r := range runs {
			if r.TaskID == verifyFlagsState.task {
				filtered = append(filtered, r)
			}
		}
		runs = filtered
	}

	// Most recent last, and the limit keeps the most recent. A history that
	// starts at the newest and truncates the oldest reads backwards, which is
	// how an agent reads it.
	if len(runs) > limit {
		runs = runs[len(runs)-limit:]
		data.Truncated = true
	}
	data.Runs = runs
	data.Stuck = state.StuckGates(runs)
	return data, nil, nil
}

// ---- human renderers ----

func humanVerifyDispatch(c *Context, env envelope.Envelope, w io.Writer) error {
	if data, ok := env.Data.(verifyData); ok {
		if sub := verifySubByName(data.Subcommand); sub != nil {
			return sub.human(c, env, w)
		}
	}
	_, err := io.WriteString(w, "Subcommands:\n  detect  re-detect the project's verification command\n  run     run a gate and record the attempt\n  history show recorded attempts\n\n")
	return err
}

func humanVerifyDetect(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(verifyData)
	if !ok {
		return fmt.Errorf("verify: unexpected data type %T", env.Data)
	}
	if data.Command == "" {
		_, err := fmt.Fprintf(w, "no conventional verify command for project type %q\n", data.Project)
		return err
	}
	if data.Saved {
		_, err := fmt.Fprintf(w, "verify_cmd set to %s\n", data.Command)
		return err
	}
	_, err := fmt.Fprintf(w, "detected %s (from %s)\n", data.Command, cellOrDash(data.Detected))
	return err
}

func humanVerifyRun(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(verifyData)
	if !ok {
		return fmt.Errorf("verify: unexpected data type %T", env.Data)
	}
	if len(data.Attempts) == 0 {
		_, err := fmt.Fprintf(w, "nothing to verify\n")
		return err
	}
	failed, timedOut, stuck := 0, 0, 0
	for _, a := range data.Attempts {
		mark := "ok"
		switch {
		case a.TimedOut:
			mark, timedOut = "TIMEOUT", timedOut+1
		case a.Status == state.GateFail:
			mark, failed = "FAIL", failed+1
		}
		if a.Stuck {
			mark += " STUCK"
			stuck++
		}
		exit := "-"
		if a.Exit != nil {
			exit = fmt.Sprintf("%d", *a.Exit)
		}
		if _, err := fmt.Fprintf(w, "%-8s %-10s %-8s %-4s attempt %d  %s\n",
			a.Task, a.Gate, mark, exit, a.Attempts, a.Command); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "%d run, %d failed, %d timed out, %d stuck\n",
		len(data.Attempts), failed, timedOut, stuck)
	return err
}

func humanVerifyHistory(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(verifyData)
	if !ok {
		return fmt.Errorf("verify: unexpected data type %T", env.Data)
	}
	if len(data.Runs) == 0 {
		_, err := fmt.Fprintf(w, "no attempts recorded\n")
		return err
	}
	for _, r := range data.Runs {
		exit := "-"
		if r.ExitCode != nil {
			exit = fmt.Sprintf("%d", *r.ExitCode)
		}
		if _, err := fmt.Fprintf(w, "%-20s %-8s %-10s %-8s %-4s %s\n",
			r.At, r.TaskID, r.Gate, r.Status, exit, strings.Join(r.Cmd, " ")); err != nil {
			return err
		}
	}
	if data.Truncated {
		_, err := fmt.Fprintf(w, "(most recent %d of the recorded attempts)\n", len(data.Runs))
		return err
	}
	return nil
}
