package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

const taskCommandName = "task"

// taskData is the payload for every `ocaw task` subcommand.
//
// One shape for all seven, with every key always present. A caller that reads
// `ocaw task next` should not have to know which subcommand it ran before it can
// check whether `task` is null, and a field that appears in one subcommand and
// not another is a field a caller ends up guarding twice.
type taskData struct {
	Subcommand string              `json:"subcommand"`
	Task       *state.TaskView     `json:"task"`
	Tasks      []state.TaskView    `json:"tasks"`
	Removed    []string            `json:"removed"`
	Counts     map[string]int      `json:"counts"`
	Ready      []string            `json:"ready"`
	Blocked    map[string][]string `json:"blocked"`
	Created    bool                `json:"created"`
	DryRun     bool                `json:"dry_run"`
	// Detail carries the structured half of a refusal: the blocking dep ids, the
	// cycle path, the dependent that would be stranded. An agent that has to
	// parse the message to find out what is in its way is back to parsing prose,
	// which is the one thing the JSON contract exists to stop.
	Detail map[string]any `json:"detail"`
}

// newTaskData starts from the full shape so a command that returns early — a
// usage error before the state was read, a refusal from the rule set — still
// emits the same keys. A payload that is sometimes a map and sometimes a struct
// is a payload a caller ends up guarding twice, and the guard will be wrong half
// the time.
func newTaskData(sub string, dryRun bool) taskData {
	counts := make(map[string]int, len(state.Statuses))
	for _, status := range state.Statuses {
		counts[string(status)] = 0
	}
	return taskData{
		Subcommand: sub,
		Tasks:      []state.TaskView{},
		Removed:    []string{},
		Ready:      []string{},
		Blocked:    map[string][]string{},
		Counts:     counts,
		DryRun:     dryRun,
		Detail:     map[string]any{},
	}
}

// taskSub is one subcommand. The rules live in internal/state; this is the
// surface that parses flags, takes the lock, and renders.
type taskSub struct {
	name    string
	summary string
	// mutate marks a subcommand that writes. Only those take the lock, and only
	// those honour --dry-run by not saving.
	mutate bool
	setup  func(fs *flag.FlagSet, t *taskFlags)
	run    func(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error)
	human  func(c *Context, env envelope.Envelope, w io.Writer) error
}

// taskEnv is what every subcommand gets: the state, already loaded and
// validated once by the dispatcher, and the stuck map from the run log.
//
// One load, not one per subcommand. Two loads would mean the subcommand mutates
// a different State instance from the one the dispatcher saves, and the save
// would persist nothing at all — which is exactly the kind of bug that passes a
// read-only test.
type taskEnv struct {
	st    *state.State
	stuck map[string]bool
}

// taskFlags holds every subcommand's flags. One struct rather than seven keeps
// the plumbing in one place, and every field is reset before each parse so a
// second invocation in the same process cannot inherit the first one's values.
type taskFlags struct {
	id      string
	title   string
	agent   string
	tier    string
	note    string
	status  string
	deps    stringList
	gates   stringList
	addDeps stringList
	rmDeps  stringList
	ready   bool
	blocked bool
}

// stringList is a repeatable flag. flag has no built-in one, and a
// comma-separated value would be ambiguous for a task title.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }

func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

var taskFlagsState taskFlags

var taskSubs = []*taskSub{
	{
		name:    "add",
		summary: "add a task, or update the one with this id",
		mutate:  true,
		setup: func(fs *flag.FlagSet, t *taskFlags) {
			fs.StringVar(&t.id, "id", "", "task id (required); re-using an id updates in place")
			fs.StringVar(&t.title, "title", "", "what the task is")
			fs.StringVar(&t.agent, "agent", "", "which agent or role owns it")
			fs.StringVar(&t.tier, "tier", "", "free-form tier label, e.g. foundation or cli")
			fs.StringVar(&t.status, "status", "", "initial status; an omitted one means pending")
			fs.StringVar(&t.note, "note", "", "a note for whoever picks this up")
			fs.Var(&t.deps, "dep", "a task id this one depends on; repeatable")
			fs.Var(&t.gates, "gate", "a gate as name=command; repeatable")
		},
		run:   runTaskAdd,
		human: humanTaskWrite,
	},
	{
		name:    "set",
		summary: "update a task's fields or status",
		mutate:  true,
		setup: func(fs *flag.FlagSet, t *taskFlags) {
			fs.StringVar(&t.title, "title", "", "new title")
			fs.StringVar(&t.agent, "agent", "", "new agent")
			fs.StringVar(&t.tier, "tier", "", "new tier")
			fs.StringVar(&t.status, "status", "", "new status")
			fs.StringVar(&t.note, "note", "", "new note")
			fs.Var(&t.gates, "gate", "a gate as name=command; repeatable")
		},
		run:   runTaskSet,
		human: humanTaskWrite,
	},
	{
		name:    "dep",
		summary: "add or remove a task's dependencies",
		mutate:  true,
		setup: func(fs *flag.FlagSet, t *taskFlags) {
			fs.Var(&t.addDeps, "add", "a dependency to add; repeatable")
			fs.Var(&t.rmDeps, "rm", "a dependency to remove; repeatable")
		},
		run:   runTaskDep,
		human: humanTaskWrite,
	},
	{
		name:    "rm",
		summary: "remove tasks",
		mutate:  true,
		run:     runTaskRemove,
		human:   humanTaskWrite,
	},
	{
		name:    "show",
		summary: "print one task",
		run:     runTaskShow,
		human:   humanTaskWrite,
	},
	{
		name:    "list",
		summary: "list tasks, optionally filtered",
		setup: func(fs *flag.FlagSet, t *taskFlags) {
			fs.StringVar(&t.status, "status", "", "only tasks in this status")
			fs.BoolVar(&t.ready, "ready", false, "only tasks whose dependencies are all satisfied")
			fs.BoolVar(&t.blocked, "blocked", false, "only tasks that cannot start yet")
		},
		run:   runTaskList,
		human: humanTaskList,
	},
	{
		name:    "next",
		summary: "print the highest-priority ready task",
		run:     runTaskNext,
		human:   humanTaskNext,
	},
}

var taskCommand = &command{
	name:      taskCommandName,
	summary:   "drive the task DAG",
	usage:     "ocaw task <" + taskSubNames() + "> [flags]",
	takesArgs: true,
	run:       runTask,
	human:     humanTask,
}

func taskSubNames() string {
	names := make([]string, 0, len(taskSubs))
	for _, sub := range taskSubs {
		names = append(names, sub.name)
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

func taskSubByName(name string) *taskSub {
	for _, sub := range taskSubs {
		if sub.name == name {
			return sub
		}
	}
	return nil
}

func runTask(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: taskCommandName}

	if len(args) == 0 {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task --help",
			"task needs a subcommand: %s", taskSubNames(),
		)
		res.Data = map[string]any{"available_subcommands": sortedSubNames()}
		printSubcommandHelp(c, res)
		return res
	}

	sub := taskSubByName(args[0])
	if sub == nil {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task --help",
			"unknown subcommand %q; expected %s", args[0], taskSubNames(),
		)
		res.Data = map[string]any{"available_subcommands": sortedSubNames()}
		printSubcommandHelp(c, res)
		return res
	}

	// Parse the subcommand's flags over a fresh set. taskFlagsState is reset
	// first: flag.StringVar takes the current value as the flag's default, so
	// without this a second invocation in the same process inherits the first
	// one's --title.
	fs := newFlagSet("ocaw task " + sub.name)
	registerGlobals(fs, &c.Options)
	taskFlagsState = taskFlags{}
	if sub.setup != nil {
		sub.setup(fs, &taskFlagsState)
	}
	rest, parseErr := parseInterspersed(fs, args[1:])
	if parseErr != nil {
		// A leaf asking for help is a success. It used to be reported as a
		// `usage` refusal whose hint was the argv that had just failed, so the
		// envelope offered a fix that reproduced the envelope.
		if errors.Is(parseErr, flag.ErrHelp) {
			return subcommandHelp(c, res, taskCommandName, sub.name, sub.summary, fs)
		}
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task "+sub.name+" --help",
			"%s", parseErr.Error(),
		)
		return res
	}

	data, warnings, err := dispatchTask(c, sub, rest)
	if data.Subcommand == "" {
		// The dispatcher failed before the subcommand ran. The payload still has
		// to be the full shape: a caller that reads data after a failure should
		// find empty lists, not nulls, or it ends up guarding two shapes.
		data = newTaskData(sub.name, c.Options.DryRun)
	}
	res.Data = data
	res.Warnings = warnings
	if err != nil {
		res.Err = err
	}
	return res
}

// printSubcommandHelp is the task equivalent of the root command's emitUsage: a
// caller mistake is best answered on a terminal by the list of what was
// available, on stdout alongside the error on stderr. In JSON mode the envelope
// already carries available_subcommands, so this is a no-op.
func printSubcommandHelp(c *Context, res envelope.Result) {
	if c.mode() != ModeHuman || res.Err == nil {
		return
	}
	var b strings.Builder
	b.WriteString("Subcommands:\n")
	width := 0
	for _, sub := range taskSubs {
		if len(sub.name) > width {
			width = len(sub.name)
		}
	}
	for _, sub := range taskSubs {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, sub.name, sub.summary)
	}
	b.WriteString("\n")
	_, _ = io.WriteString(c.Stdout, b.String())
}

// parseInterspersed parses flags that appear before, between, and after
// positional arguments.
//
// flag.Parse stops at the first non-flag argument, so a single pass would leave
// `set 1 --status done` with everything after `1` unread and `--status` silently
// ignored — the command would then report "nothing to change". The spec's usage
// lines put flags after the positional, so the leftovers are consumed one at a
// time and the remainder re-parsed. Each pass sees only what it has not already
// consumed, so a repeatable flag is not collected twice.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		rest = rest[1:]
	}
}

// dispatchTask loads the state, runs the subcommand against it, and saves when
// the subcommand mutated something. The lock is taken for mutating
// subcommands only, including under --dry-run: a dry run that reports a verdict
// about a state another agent is halfway through changing is reporting fiction.
func dispatchTask(c *Context, sub *taskSub, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		return taskData{}, nil, asEnvelopeError(err)
	}
	if missing := layout.Require(); missing != nil {
		return taskData{}, nil, missing
	}

	var warnings []envelope.Warning
	release := func() {}
	if sub.mutate {
		lock, lockWarnings, lockErr := workspace.Acquire(layout)
		if lockErr != nil {
			return taskData{}, nil, asEnvelopeError(lockErr)
		}
		warnings = lockWarnings
		release = func() {
			if relErr := lock.Release(); relErr != nil {
				warnings = append(warnings, Warn(envelope.CodeLockHeld,
					"could not release the workspace lock at %s: %v", lock.Path(), relErr))
			}
		}
	}
	defer func() { release() }()

	st, loadErr := state.Load(layout)
	if loadErr != nil {
		return taskData{}, warnings, asEnvelopeError(loadErr)
	}
	runs, runsErr := state.LoadRuns(layout)
	if runsErr != nil {
		// A malformed run log must not stop a task edit. The log is a record of
		// what was tried, not the work itself, and refusing to record a task
		// because an old log line is broken would make the broken log permanent.
		warnings = append(warnings, Warn(envelope.CodeValidationFailed,
			"ignoring the run log: %v", runsErr))
	}
	env := &taskEnv{st: st, stuck: state.StuckTasks(runs)}

	data, subWarnings, runErr := sub.run(c, env, &taskFlagsState, args)
	warnings = append(warnings, subWarnings...)
	if runErr != nil || c.Options.DryRun {
		return data, warnings, runErr
	}

	if err := state.Save(layout, env.st); err != nil {
		return data, warnings, writeFailure("write state.json", err)
	}
	// The generated view is refreshed with the state. Leaving it behind would
	// make every `task add` leave a stale WORKFLOW_STATE.md, so `doctor` would
	// report staleness as the normal outcome of doing work — and a check that
	// fires on success trains people to ignore it. `ocaw report --write` stays
	// meaningful for the cases a mutation cannot cover: a hand-edited file, a
	// file that was deleted, one written out of band.
	if _, err := report.Write(layout, env.st); err != nil {
		return data, warnings, writeFailure("write "+layout.Rel(layout.StateMDPath()), err)
	}
	return data, warnings, nil
}

// ---- subcommands ----

func runTaskAdd(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	data := newTaskData("add", c.Options.DryRun)
	if len(args) > 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task add --id <id> --title <s>",
			"unexpected argument %q; the id goes in --id", args[0],
		)
	}
	if strings.TrimSpace(t.id) == "" {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task add --id <id> --title <s>",
			"--id is required",
		)
	}

	// An omitted --status means pending. Parsing the empty string instead would
	// report a usage error for the most ordinary command in the set.
	var status state.Status
	if t.status != "" {
		parsed, serr := state.ParseStatus(t.status)
		if serr != nil {
			return fail(data, serr)
		}
		status = parsed
	}
	gates, gerr := parseGates(t.gates)
	if gerr != nil {
		return data, nil, asEnvelopeError(gerr)
	}

	st := env.st
	_, existed := st.Find(t.id)
	data.Created = !existed

	task := state.Task{
		ID:     t.id,
		Title:  t.title,
		Agent:  t.agent,
		Tier:   t.tier,
		Deps:   t.deps,
		Status: status,
		Notes:  t.note,
		Gates:  gates,
	}
	if serr := st.AddTask(task, time.Now().UTC()); serr != nil {
		return fail(data, serr)
	}
	return withTask(data, env, t.id)
}

func runTaskSet(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	data := newTaskData("set", c.Options.DryRun)
	id, uerr := oneID("set", args)
	if uerr != nil {
		return data, nil, uerr
	}
	changed := c.Options.DryRun

	st := env.st

	// A status change goes through SetStatus, which owns the transition rules.
	// Field updates go through the task directly and are validated once at the
	// end, so a refused change leaves nothing behind.
	if t.status != "" {
		status, serr := state.ParseStatus(t.status)
		if serr != nil {
			return fail(data, serr)
		}
		if serr := st.SetStatus(id, status, c.Options.Yes, time.Now().UTC()); serr != nil {
			return fail(data, serr)
		}
		changed = true
	}

	task, found := st.Find(id)
	if !found {
		return data, nil, envelope.Errorf(
			envelope.CodeTaskNotFound,
			"ocaw task list",
			"no task with id %q", id,
		)
	}
	if t.title != "" {
		task.Title = t.title
		changed = true
	}
	if t.agent != "" {
		task.Agent = t.agent
		changed = true
	}
	if t.tier != "" {
		task.Tier = t.tier
		changed = true
	}
	if t.note != "" {
		task.Notes = t.note
		changed = true
	}
	if len(t.gates) > 0 {
		gates, gerr := parseGates(t.gates)
		if gerr != nil {
			return data, nil, asEnvelopeError(gerr)
		}
		for _, g := range gates {
			task.SetGate(g)
		}
		changed = true
	}
	if !changed {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task set --help",
			"nothing to change: pass at least one field or --status",
		)
	}
	if serr := st.Validate(); serr != nil {
		return fail(data, serr)
	}
	return withTask(data, env, id)
}

func runTaskDep(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	data := newTaskData("dep", c.Options.DryRun)
	id, uerr := oneID("dep", args)
	if uerr != nil {
		return data, nil, uerr
	}
	if len(t.addDeps) == 0 && len(t.rmDeps) == 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task dep --add <id>",
			"pass --add or --rm",
		)
	}

	st := env.st
	if _, found := st.Find(id); !found {
		return data, nil, envelope.Errorf(
			envelope.CodeTaskNotFound,
			"ocaw task list",
			"no task with id %q", id,
		)
	}
	// Add first, then remove, so `dep 1 --add 3 --rm 2` reads the way it is
	// written. A dep that is both added and removed ends up removed, which is
	// what a reader of the flags would expect.
	//
	// Each is guarded on its own flag. AddDeps refuses an empty list, so
	// calling it for a --rm-only invocation would report "no dep ids given"
	// about the --add that was never given.
	if len(t.addDeps) > 0 {
		if serr := st.AddDeps(id, t.addDeps, time.Now().UTC()); serr != nil {
			return fail(data, serr)
		}
	}
	if len(t.rmDeps) > 0 {
		if serr := st.RemoveDeps(id, t.rmDeps, time.Now().UTC()); serr != nil {
			return fail(data, serr)
		}
	}
	return withTask(data, env, id)
}

func runTaskRemove(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	data := newTaskData("rm", c.Options.DryRun)
	if len(args) == 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task rm <id>...",
			"pass at least one task id",
		)
	}
	st := env.st
	// An id that is not there is not an error: removing a task that is already
	// gone is the state the caller asked for, and refusing would make `rm` of a
	// batch fail on the one that was deleted last time. The ids are echoed back
	// either way, so a caller can tell what it asked for.
	//
	// RemoveTask itself stays strict about unknown ids, because a caller that
	// passes a typo should hear about it. Idempotence is a decision about what a
	// human typed, so it belongs here rather than in the rule set.
	present := make([]string, 0, len(args))
	for _, id := range args {
		if _, found := st.Find(id); found {
			present = append(present, id)
		}
	}
	if len(present) > 0 {
		if serr := st.RemoveTask(present); serr != nil {
			return fail(data, serr)
		}
	}
	data.Removed = args
	data.Counts = st.Counts()
	data.Blocked = st.Blocked()
	return data, nil, nil
}

func runTaskShow(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	data := newTaskData("show", c.Options.DryRun)
	id, uerr := oneID("show", args)
	if uerr != nil {
		return data, nil, uerr
	}
	st := env.st
	if _, found := st.Find(id); !found {
		return data, nil, envelope.Errorf(
			envelope.CodeTaskNotFound,
			"ocaw task list",
			"no task with id %q", id,
		)
	}
	return withTask(data, env, id)
}

func runTaskList(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	data := newTaskData("list", c.Options.DryRun)
	if len(args) > 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task list --status <s>",
			"unexpected argument %q", args[0],
		)
	}
	if t.ready && t.blocked {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task list --ready",
			"--ready and --blocked are opposites; pass one",
		)
	}
	var filter state.Status
	if t.status != "" {
		parsed, serr := state.ParseStatus(t.status)
		if serr != nil {
			return fail(data, serr)
		}
		filter = parsed
	}

	st := env.st
	stuck := env.stuck

	ready := map[string]bool{}
	for _, task := range st.Ready() {
		ready[task.ID] = true
	}
	blocked := st.Blocked()

	views := st.Views(stuck)
	data.Tasks = make([]state.TaskView, 0, len(views))
	for _, view := range views {
		switch {
		case t.ready && !ready[view.ID]:
			continue
		case t.blocked && len(blocked[view.ID]) == 0:
			continue
		case filter != "" && view.Status != filter:
			continue
		}
		data.Tasks = append(data.Tasks, view)
	}
	// The queue is reported unfiltered, so a caller asking for the blocked list
	// still learns what is runnable.
	for _, task := range st.Ready() {
		data.Ready = append(data.Ready, task.ID)
	}
	data.Blocked = blocked
	data.Counts = st.Counts()
	return data, nil, nil
}

func runTaskNext(c *Context, env *taskEnv, t *taskFlags, args []string) (taskData, []envelope.Warning, *envelope.Error) {
	data := newTaskData("next", c.Options.DryRun)
	if len(args) > 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task next",
			"unexpected argument %q", args[0],
		)
	}
	st := env.st
	stuck := env.stuck

	// An empty queue is a valid state, not an error: the answer is a null task
	// with ok true, and a caller that treats "nothing to do" as a failure will
	// not use this to drive work.
	next := st.Next()
	if next == nil {
		data.Counts = st.Counts()
		data.Blocked = st.Blocked()
		for _, task := range st.Ready() {
			data.Ready = append(data.Ready, task.ID)
		}
		return data, nil, nil
	}
	view := state.TaskView{Task: *next, Stuck: stuck[next.ID]}
	data.Task = &view
	data.Counts = st.Counts()
	data.Blocked = st.Blocked()
	for _, task := range st.Ready() {
		data.Ready = append(data.Ready, task.ID)
	}
	return data, nil, nil
}

// ---- shared helpers ----

// withTask fills in the single-task view plus the summaries every subcommand
// reports, so a caller that switched subcommands still finds the same keys.
func withTask(data taskData, env *taskEnv, id string) (taskData, []envelope.Warning, *envelope.Error) {
	st := env.st
	if task, found := st.Find(id); found {
		view := state.TaskView{Task: *task, Stuck: env.stuck[id]}
		data.Task = &view
	}
	data.Counts = st.Counts()
	data.Blocked = st.Blocked()
	for _, task := range st.Ready() {
		data.Ready = append(data.Ready, task.ID)
	}
	return data, nil, nil
}

// fail turns a state refusal into a result that carries both halves: the
// envelope error, and the structured detail in data. It returns the whole triple
// rather than just the error, because assigning through a pointer and returning
// the same value in one expression would copy before the mutation ran.
func fail(data taskData, serr *state.Error) (taskData, []envelope.Warning, *envelope.Error) {
	if serr == nil {
		return data, nil, nil
	}
	data.Detail = serr.DetailMap()
	return data, nil, serr.Envelope()
}

func oneID(sub string, args []string) (string, *envelope.Error) {
	switch {
	case len(args) == 0:
		return "", envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task "+sub+" --help",
			"this subcommand takes exactly one task id",
		)
	case len(args) > 1:
		return "", envelope.Errorf(
			envelope.CodeUsage,
			"ocaw task "+sub+" --help",
			"expected one task id, got %d", len(args),
		)
	}
	return args[0], nil
}

// parseGates turns name=command pairs into Gate definitions.
//
// Gates are not in the spec's usage lines for `task`, but something has to
// define them: `state.SetGate` exists to install a definition, and `ocaw verify`
// needs something to run. The syntax is the same as a dependency edge, a name,
// an explicit `=`, and a value, so it reads the way the rest of the command
// does.
func parseGates(raw []string) ([]state.Gate, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	gates := make([]state.Gate, 0, len(raw))
	for _, item := range raw {
		name, cmd, found := strings.Cut(item, "=")
		name = strings.TrimSpace(name)
		if !found || name == "" || strings.TrimSpace(cmd) == "" {
			return nil, envelope.Errorf(
				envelope.CodeUsage,
				"ocaw task set --gate name=command",
				"--gate %q is not in the form name=command", item,
			)
		}
		gates = append(gates, state.Gate{Name: name, Cmd: strings.TrimSpace(cmd)})
	}
	return gates, nil
}

func sortedSubNames() []string {
	out := make([]string, 0, len(taskSubs))
	for _, sub := range taskSubs {
		out = append(out, sub.name)
	}
	sort.Strings(out)
	return out
}

// ---- human renderers ----

func humanTaskWrite(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(taskData)
	if !ok {
		return fmt.Errorf("task: unexpected data type %T", env.Data)
	}
	switch data.Subcommand {
	case "rm":
		if len(data.Removed) == 0 {
			_, err := fmt.Fprintf(w, "removed nothing\n")
			return err
		}
		_, err := fmt.Fprintf(w, "removed %s\n", strings.Join(data.Removed, ", "))
		return err
	}
	if data.Task == nil {
		_, err := fmt.Fprintf(w, "%s: nothing to report\n", data.Subcommand)
		return err
	}
	verb := "updated"
	if data.Created {
		verb = "added"
	}
	if data.DryRun {
		verb = "would " + strings.TrimPrefix(verb, "")
	}
	if _, err := fmt.Fprintf(w, "%s %s  %s\n", verb, data.Task.ID, cellOrDash(data.Task.Title)); err != nil {
		return err
	}
	if data.DryRun {
		return nil
	}
	return writeTaskDetail(w, *data.Task, data.Blocked)
}

func writeTaskDetail(w io.Writer, view state.TaskView, blocked map[string][]string) error {
	if _, err := fmt.Fprintf(w, "  status %s", view.Status); err != nil {
		return err
	}
	if view.Stuck {
		if _, err := fmt.Fprintf(w, "  STUCK"); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "\n"); err != nil {
		return err
	}
	if len(view.Deps) > 0 {
		if _, err := fmt.Fprintf(w, "  deps   %s\n", strings.Join(view.Deps, ", ")); err != nil {
			return err
		}
	}
	if unmet := blocked[view.ID]; len(unmet) > 0 {
		if _, err := fmt.Fprintf(w, "  unmet  %s\n", strings.Join(unmet, ", ")); err != nil {
			return err
		}
	}
	for _, g := range view.Gates {
		if _, err := fmt.Fprintf(w, "  gate   %s: %s (%s, %d attempt(s))\n", g.Name, g.Cmd, g.Status, g.Attempts); err != nil {
			return err
		}
	}
	if view.Notes != "" {
		if _, err := fmt.Fprintf(w, "  note   %s\n", view.Notes); err != nil {
			return err
		}
	}
	return nil
}

func humanTaskList(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(taskData)
	if !ok {
		return fmt.Errorf("task: unexpected data type %T", env.Data)
	}
	if len(data.Tasks) == 0 {
		_, err := fmt.Fprintf(w, "no tasks\n")
		return err
	}
	if _, err := fmt.Fprintf(w, "%-8s %-12s %-6s %-10s %s\n", "ID", "STATUS", "AGENT", "DEPS", "TITLE"); err != nil {
		return err
	}
	for _, task := range data.Tasks {
		stuck := ""
		if task.Stuck {
			stuck = " STUCK"
		}
		_, err := fmt.Fprintf(w, "%-8s %-12s %-6s %-10s %s%s\n",
			task.ID, task.Status, cellOrDash(task.Agent), cellOrDash(strings.Join(task.Deps, ",")), cellOrDash(task.Title), stuck)
		if err != nil {
			return err
		}
	}
	return writeCounts(w, data.Counts)
}

func humanTaskNext(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(taskData)
	if !ok {
		return fmt.Errorf("task: unexpected data type %T", env.Data)
	}
	if data.Task == nil {
		// An empty queue is a valid answer, so say so rather than printing
		// nothing and leaving the reader to guess whether it worked.
		_, err := fmt.Fprintf(w, "nothing ready")
		if err == nil {
			_, err = fmt.Fprintf(w, " (%s)", countsInline(data.Counts))
		}
		return err
	}
	if _, err := fmt.Fprintf(w, "next: %s  %s\n", data.Task.ID, cellOrDash(data.Task.Title)); err != nil {
		return err
	}
	return writeTaskDetail(w, *data.Task, data.Blocked)
}

// humanTask routes to the subcommand's own renderer.
//
// emit() knows about one renderer per command, and `task` is one command with
// seven subcommands, so the dispatch happens here. The payload's `subcommand`
// field is the route: it is already set on every path, including the ones that
// fail before a subcommand runs, and a caller reading JSON gets it for free.
func humanTask(c *Context, env envelope.Envelope, w io.Writer) error {
	if data, ok := env.Data.(taskData); ok {
		if sub := taskSubByName(data.Subcommand); sub != nil && sub.human != nil {
			return sub.human(c, env, w)
		}
	}
	return humanTaskUnknown(c, env, w)
}

// humanTaskUnknown covers the paths where no subcommand ran at all: no
// subcommand given, or one that does not exist. The payload is a map there, so
// the subcommand list is the only thing worth showing.
func humanTaskUnknown(c *Context, env envelope.Envelope, w io.Writer) error {
	names, _ := env.Data.(map[string]any)
	if subs, ok := names["available_subcommands"].([]string); ok {
		sort.Strings(subs)
		var b strings.Builder
		b.WriteString("Subcommands:\n")
		for _, sub := range subs {
			for _, s := range taskSubs {
				if s.name == sub {
					fmt.Fprintf(&b, "  %-6s %s\n", s.name, s.summary)
				}
			}
		}
		_, err := io.WriteString(w, b.String())
		return err
	}
	return nil
}

func writeCounts(w io.Writer, counts map[string]int) error {
	_, err := fmt.Fprintf(w, "  %s\n", countsInline(counts))
	return err
}

func countsInline(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
	}
	return strings.Join(parts, ", ")
}

func cellOrDash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}
