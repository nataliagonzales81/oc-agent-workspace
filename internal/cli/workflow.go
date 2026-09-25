package cli

import (
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

const workflowCommandName = "workflow"

// workflowData is the payload for all three subcommands, on the same terms as
// `ocaw task`: every key present, every list an array. A caller that ran `show`
// and then `set` reads the same shape both times.
type workflowData struct {
	Subcommand  string             `json:"subcommand"`
	Workflow    workflowSections   `json:"workflow"`
	Acceptance  []state.Acceptance `json:"acceptance"`
	Render      string             `json:"render"`
	Accepted    string             `json:"accepted"`
	AcceptedSet bool               `json:"accepted_set"`
	Changed     []string           `json:"changed"`
	Written     string             `json:"written"`
	DryRun      bool               `json:"dry_run"`
	Notes       []string           `json:"notes,omitempty"`
	// Detail carries the structured half of a refusal, so an agent can read the
	// id that was not found without parsing the message.
	Detail map[string]any `json:"detail"`
}

// workflowSections mirrors state.Workflow. It is duplicated rather than aliased
// so the JSON key names are the contract rather than a field rename away.
type workflowSections struct {
	Request     string   `json:"request"`
	Scope       string   `json:"scope"`
	Constraints []string `json:"constraints"`
}

type workflowSub struct {
	name    string
	summary string
	mutate  bool
	setup   func(fs *flag.FlagSet, w *workflowFlags)
	run     func(c *Context, env *workflowEnv, w *workflowFlags, args []string) (workflowData, []envelope.Warning, *envelope.Error)
	human   func(c *Context, env envelope.Envelope, w io.Writer) error
}

// workflowFlags holds every subcommand's flags, reset before each parse.
type workflowFlags struct {
	request     string
	scope       string
	constraints stringList
	accept      stringList
	done        bool
	notDone     bool
}

var workflowFlagsState workflowFlags

type workflowEnv struct {
	st     *state.State
	render report.Status
	// flags is the subcommand's own FlagSet, kept so `set` can ask which flags
	// the caller actually typed. A string flag cannot tell an absent flag from
	// `--request ""`, and those mean opposite things.
	flags *flag.FlagSet
}

var workflowSubs = []*workflowSub{
	{
		name:    "set",
		summary: "set the authored request, scope, constraints, and acceptance",
		mutate:  true,
		setup: func(fs *flag.FlagSet, w *workflowFlags) {
			fs.StringVar(&w.request, "request", "", "the request, as the person who asked for it stated it")
			fs.StringVar(&w.scope, "scope", "", "the clarified scope")
			fs.Var(&w.constraints, "constraint", "a constraint; repeatable, and replaces the list when given")
			fs.Var(&w.accept, "accept", "an acceptance item as id=text, or text alone for the next id; repeatable")
		},
		run:   runWorkflowSet,
		human: humanWorkflowSet,
	},
	{
		name:    "accept",
		summary: "tick or untick one acceptance item",
		mutate:  true,
		setup: func(fs *flag.FlagSet, w *workflowFlags) {
			fs.BoolVar(&w.done, "done", false, "tick the item; the default when neither flag is given")
			fs.BoolVar(&w.notDone, "not-done", false, "untick the item")
		},
		run:   runWorkflowAccept,
		human: humanWorkflowAccept,
	},
	{
		name:    "show",
		summary: "print the authored sections",
		run:     runWorkflowShow,
		human:   humanWorkflowShow,
	},
}

var workflowCommand = &command{
	name:      workflowCommandName,
	summary:   "manage the authored sections of the workflow",
	usage:     "ocaw workflow <set|accept|show> [flags]",
	takesArgs: true,
	run:       runWorkflow,
	human:     humanWorkflow,
}

func runWorkflow(c *Context, args []string) envelope.Result {
	res := envelope.Result{Command: workflowCommandName}

	if len(args) == 0 {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow --help",
			"workflow needs a subcommand: accept|set|show",
		)
		res.Data = map[string]any{"available_subcommands": []string{"accept", "set", "show"}}
		printWorkflowHelp(c)
		return res
	}
	sub := workflowSubByName(args[0])
	if sub == nil {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow --help",
			"unknown subcommand %q; expected accept|set|show", args[0],
		)
		res.Data = map[string]any{"available_subcommands": []string{"accept", "set", "show"}}
		printWorkflowHelp(c)
		return res
	}

	fs := newFlagSet("ocaw workflow " + sub.name)
	registerGlobals(fs, &c.Options)
	workflowFlagsState = workflowFlags{}
	if sub.setup != nil {
		sub.setup(fs, &workflowFlagsState)
	}
	workflowEnvState := &workflowEnv{flags: fs}
	rest, parseErr := parseInterspersed(fs, args[1:])
	if parseErr != nil {
		res.Err = envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow "+sub.name+" --help",
			"%s", parseErr.Error(),
		)
		return res
	}

	data, warnings, err := dispatchWorkflow(c, sub, rest, workflowEnvState)
	if data.Subcommand == "" {
		data = newWorkflowData(sub.name, c.Options.DryRun)
	}
	res.Data = data
	res.Warnings = warnings
	if err != nil {
		res.Err = err
	}
	return res
}

func dispatchWorkflow(c *Context, sub *workflowSub, args []string, env *workflowEnv) (workflowData, []envelope.Warning, *envelope.Error) {
	layout, err := workspace.Resolve(c.Options.Dir)
	if err != nil {
		return workflowData{}, nil, asEnvelopeError(err)
	}
	if missing := layout.Require(); missing != nil {
		return workflowData{}, nil, missing
	}

	var warnings []envelope.Warning
	release := func() {}
	if sub.mutate {
		lock, lockWarnings, lockErr := workspace.Acquire(layout)
		if lockErr != nil {
			return workflowData{}, nil, asEnvelopeError(lockErr)
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
		return workflowData{}, warnings, asEnvelopeError(loadErr)
	}
	env.st = st
	// Read the drift state before the subcommand runs, so `workflow show` can
	// report it, and so a `set` can tell the user the file it is about to
	// regenerate replaces something they had edited by hand.
	if doc, found, derr := report.Load(layout); derr == nil && found {
		status, _ := report.Check(doc, st)
		env.render = status
	} else {
		env.render = report.StatusMissing
	}

	data, subWarnings, runErr := sub.run(c, env, &workflowFlagsState, args)
	warnings = append(warnings, subWarnings...)
	data.Render = string(env.render)
	if runErr != nil || c.Options.DryRun {
		return data, warnings, runErr
	}

	if err := state.Save(layout, env.st); err != nil {
		return data, warnings, writeFailure("write state.json", err)
	}
	if _, err := report.Write(layout, env.st); err != nil {
		return data, warnings, writeFailure("write "+layout.Rel(layout.StateMDPath()), err)
	}
	// The file on disk is now exactly what this state renders to.
	data.Render = string(report.StatusCurrent)
	return data, warnings, nil
}

// ---- subcommands ----

// acceptanceIDPattern decides whether the left of an `=` in `--accept` is an id
// or the start of the text. A bare `=` in real prose is common enough that
// treating every `=` as a separator would mangle it, so the left side has to
// look like an id before it is treated as one.
// The only way to name an explicit id in `--accept` is the shape ocaw assigns
// one, or an id already in the workspace.
//
// A looser rule ("anything before an `=` that looks like a word") cannot
// distinguish `--accept "a1=ship it"` from `--accept "a=1 and b=2"`, and the
// second is prose a person wrote. `a` is not an id ocaw would ever have created,
// so text containing one keeps its `=`. The cost is that a custom id scheme
// cannot be introduced in one call: `--accept "AC-1=..."` is filed as text and
// gets an assigned id, which the author can then see and use. That is the
// conservative direction — it never silently splits a sentence.
var acceptanceIDPattern = regexp.MustCompile(`^a[0-9]+$`)

func runWorkflowSet(c *Context, env *workflowEnv, w *workflowFlags, args []string) (workflowData, []envelope.Warning, *envelope.Error) {
	data := newWorkflowData("set", c.Options.DryRun)
	if len(args) > 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow set --request <s>",
			"unexpected argument %q; everything is a flag", args[0],
		)
	}

	// Which flags did the caller actually type? A flag that was not given must
	// not touch its field, or `ocaw workflow set --request X` would quietly wipe
	// the constraints and the acceptance criteria. `fs.Visit` reports exactly
	// the flags that appeared, which a string flag cannot tell on its own: an
	// absent flag and `--request ""` both leave the variable empty, and one of
	// those means "leave it alone" and the other means "clear it".
	given := map[string]bool{}
	env.flags.Visit(func(f *flag.Flag) { given[f.Name] = true })

	var changed []string
	if given["request"] {
		env.st.Workflow.Request = w.request
		changed = append(changed, "request")
	}
	if given["scope"] {
		env.st.Workflow.Scope = w.scope
		changed = append(changed, "scope")
	}
	if given["constraint"] {
		env.st.Workflow.Constraints = append([]string{}, w.constraints...)
		changed = append(changed, "constraints")
	}
	if given["accept"] {
		items, ierr := buildAcceptance(env.st, w.accept)
		if ierr != nil {
			return data, nil, asEnvelopeError(ierr)
		}
		if serr := env.st.SetAcceptance(items); serr != nil {
			return workflowFail(data, serr)
		}
		changed = append(changed, "acceptance")
	}
	if len(changed) == 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow set --request <s>",
			"nothing to change: pass at least one of --request, --scope, --constraint, --accept",
		)
	}

	if verr := env.st.Validate(); verr != nil {
		return workflowFail(data, verr)
	}
	data.Changed = changed
	// An edited file is replaced here, and only here, so the warning is
	// reported before it happens rather than being discovered by `doctor` later.
	if env.render == report.StatusEdited || env.render == report.StatusForeign {
		data.Notes = append(data.Notes,
			"regenerated WORKFLOW_STATE.md from state.json; the previous file had been edited by hand, and those edits are gone")
	}
	fillWorkflow(&data, env.st)
	return data, nil, nil
}

// buildAcceptance turns the `--accept` values into items, assigning an id to a
// bare string. The next id continues from the highest numeric one already in use,
// so an agent that adds items one at a time gets a1, a2, a3 rather than a
// collision. A left side is read as an explicit id only when it matches
// acceptanceIDPattern or is already in the list.
func buildAcceptance(st *state.State, raw []string) ([]state.Acceptance, error) {
	items := make([]state.Acceptance, 0, len(raw))
	// The counter has to advance *within* one call as well as across calls. A
	// command carrying both `--accept a1=first` and a bare `--accept second` must
	// not hand the bare one a1 too: the two would collide, and SetAcceptance
	// merges by id, so the second would silently overwrite the first.
	next := nextAcceptanceID(st)
	taken := map[string]bool{}
	for _, a := range st.Workflow.Acceptance {
		taken[a.ID] = true
	}
	for _, item := range raw {
		text := strings.TrimSpace(item)
		if text == "" {
			return nil, envelope.Errorf(
				envelope.CodeUsage,
				"ocaw workflow set --accept <id>=<text>",
				"--accept was given an empty value",
			)
		}
		if id, rest, found := strings.Cut(text, "="); found &&
			(acceptanceIDPattern.MatchString(id) || hasAcceptanceID(st, id)) {
			if strings.TrimSpace(rest) == "" {
				return nil, envelope.Errorf(
					envelope.CodeUsage,
					"ocaw workflow set --accept <id>=<text>",
					"--accept %q has an id and no text", item,
				)
			}
			items = append(items, state.Acceptance{ID: id, Text: rest})
			taken[id] = true
			continue
		}
		for taken[fmt.Sprintf("a%d", next)] {
			next++
		}
		id := fmt.Sprintf("a%d", next)
		items = append(items, state.Acceptance{ID: id, Text: text})
		taken[id] = true
		next++
	}
	return items, nil
}

func hasAcceptanceID(st *state.State, id string) bool {
	for _, a := range st.Workflow.Acceptance {
		if a.ID == id {
			return true
		}
	}
	return false
}

func nextAcceptanceID(st *state.State) int {
	highest := 0
	for _, a := range st.Workflow.Acceptance {
		var n int
		if _, err := fmt.Sscanf(a.ID, "a%d", &n); err == nil && n > highest {
			highest = n
		}
	}
	return highest + 1
}

func runWorkflowAccept(c *Context, env *workflowEnv, w *workflowFlags, args []string) (workflowData, []envelope.Warning, *envelope.Error) {
	data := newWorkflowData("accept", c.Options.DryRun)
	if len(args) != 1 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow accept <id> --done",
			"expected exactly one acceptance id",
		)
	}
	if w.done && w.notDone {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow accept <id> --done",
			"--done and --not-done are opposites; pass one",
		)
	}
	// Neither flag means "show me the current value" would be a silent no-op, so
	// default to ticking: the command is named after the act, and the negative
	// case is spelled out when it is wanted.
	done := true
	if w.notDone {
		done = false
	}
	if serr := env.st.SetAcceptanceDone(args[0], done); serr != nil {
		return workflowFail(data, serr)
	}
	if verr := env.st.Validate(); verr != nil {
		return workflowFail(data, verr)
	}
	data.Accepted = args[0]
	data.AcceptedSet = true
	fillWorkflow(&data, env.st)
	return data, nil, nil
}

func runWorkflowShow(c *Context, env *workflowEnv, w *workflowFlags, args []string) (workflowData, []envelope.Warning, *envelope.Error) {
	data := newWorkflowData("show", c.Options.DryRun)
	if len(args) > 0 {
		return data, nil, envelope.Errorf(
			envelope.CodeUsage,
			"ocaw workflow show",
			"unexpected argument %q", args[0],
		)
	}
	fillWorkflow(&data, env.st)
	return data, nil, nil
}

// ---- shared helpers ----

func newWorkflowData(sub string, dryRun bool) workflowData {
	return workflowData{
		Subcommand: sub,
		Workflow: workflowSections{
			Constraints: []string{},
		},
		Acceptance: []state.Acceptance{},
		Render:     string(report.StatusMissing),
		Changed:    []string{},
		DryRun:     dryRun,
		Detail:     map[string]any{},
	}
}

func fillWorkflow(data *workflowData, st *state.State) {
	// Constraints and acceptance come out of a decoder that may have left them
	// nil, and a nil slice marshals to null. One null list in a payload teaches
	// a caller to guard two shapes.
	constraints := st.Workflow.Constraints
	if constraints == nil {
		constraints = []string{}
	}
	acceptance := st.Workflow.Acceptance
	if acceptance == nil {
		acceptance = []state.Acceptance{}
	}
	data.Workflow = workflowSections{
		Request:     st.Workflow.Request,
		Scope:       st.Workflow.Scope,
		Constraints: constraints,
	}
	data.Acceptance = acceptance
	data.Written = "WORKFLOW_STATE.md"
}

// workflowFail turns a state refusal into a result carrying both halves: the
// envelope error, and the structured detail in data.
//
// It returns the whole triple rather than just the error, for the same reason
// `task.fail` does: assigning through a pointer and returning the same value in
// one expression copies before the mutation runs, and the detail is silently
// dropped — which is the bug this shape was introduced to prevent.
func workflowFail(data workflowData, serr *state.Error) (workflowData, []envelope.Warning, *envelope.Error) {
	data.Detail = serr.DetailMap()
	return data, nil, serr.Envelope()
}

func workflowSubByName(name string) *workflowSub {
	for _, sub := range workflowSubs {
		if sub.name == name {
			return sub
		}
	}
	return nil
}

const workflowSubcommandHelp = "Subcommands:\n" +
	"  set      set the authored request, scope, constraints, and acceptance\n" +
	"  accept   tick or untick one acceptance item\n" +
	"  show     print the authored sections\n\n"

// printWorkflowHelp answers a caller mistake on a terminal with the list of what
// was available. In JSON mode the envelope already carries
// available_subcommands, so this is a no-op there.
func printWorkflowHelp(c *Context) {
	if c.Mode != ModeHuman {
		return
	}
	_, _ = io.WriteString(c.Stdout, workflowSubcommandHelp)
}

// ---- human renderers ----

func humanWorkflow(c *Context, env envelope.Envelope, w io.Writer) error {
	if data, ok := env.Data.(workflowData); ok {
		if sub := workflowSubByName(data.Subcommand); sub != nil && sub.human != nil {
			return sub.human(c, env, w)
		}
	}
	names, _ := env.Data.(map[string]any)
	if _, ok := names["available_subcommands"]; !ok {
		return nil
	}
	_, err := io.WriteString(w, workflowSubcommandHelp)
	return err
}

func humanWorkflowSet(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(workflowData)
	if !ok {
		return fmt.Errorf("workflow: unexpected data type %T", env.Data)
	}
	verb := "set"
	if data.DryRun {
		verb = "would set"
	}
	if _, err := fmt.Fprintf(w, "%s %s\n", verb, strings.Join(data.Changed, ", ")); err != nil {
		return err
	}
	for _, note := range data.Notes {
		if _, err := fmt.Fprintf(w, "  note    %s\n", note); err != nil {
			return err
		}
	}
	return writeWorkflowSections(w, data)
}

func humanWorkflowAccept(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(workflowData)
	if !ok {
		return fmt.Errorf("workflow: unexpected data type %T", env.Data)
	}
	verb := "accepted"
	if data.DryRun {
		verb = "would accept"
	}
	if _, err := fmt.Fprintf(w, "%s %s\n", verb, data.Accepted); err != nil {
		return err
	}
	return writeAcceptanceList(w, data.Acceptance)
}

func humanWorkflowShow(c *Context, env envelope.Envelope, w io.Writer) error {
	data, ok := env.Data.(workflowData)
	if !ok {
		return fmt.Errorf("workflow: unexpected data type %T", env.Data)
	}
	if err := writeWorkflowSections(w, data); err != nil {
		return err
	}
	return writeAcceptanceList(w, data.Acceptance)
}

func writeWorkflowSections(w io.Writer, data workflowData) error {
	if data.Workflow.Request != "" {
		if _, err := fmt.Fprintf(w, "  request     %s\n", data.Workflow.Request); err != nil {
			return err
		}
	}
	if data.Workflow.Scope != "" {
		if _, err := fmt.Fprintf(w, "  scope       %s\n", data.Workflow.Scope); err != nil {
			return err
		}
	}
	if len(data.Workflow.Constraints) > 0 {
		if _, err := fmt.Fprintf(w, "  constraints %d\n", len(data.Workflow.Constraints)); err != nil {
			return err
		}
		for _, c := range data.Workflow.Constraints {
			if _, err := fmt.Fprintf(w, "    - %s\n", c); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeAcceptanceList(w io.Writer, items []state.Acceptance) error {
	if len(items) == 0 {
		_, err := fmt.Fprintf(w, "  no acceptance criteria\n")
		return err
	}
	done := 0
	for _, a := range items {
		if a.Done {
			done++
		}
	}
	if _, err := fmt.Fprintf(w, "  acceptance  %d of %d\n", done, len(items)); err != nil {
		return err
	}
	for _, a := range items {
		mark := " "
		if a.Done {
			mark = "x"
		}
		if _, err := fmt.Fprintf(w, "    [%s] %-8s %s\n", mark, a.ID, a.Text); err != nil {
			return err
		}
	}
	return nil
}
