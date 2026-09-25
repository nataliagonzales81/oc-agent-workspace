// Package cli wires ocaw's command table, global flags, and output rendering
// (SPEC.md §4.1, §4.4, §7, §8, §12).
//
// Everything a command needs arrives through Context, and the process exit
// code is derived from the envelope rather than returned by command code, so
// no command can invent an exit status that disagrees with its error code.
package cli

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

// Mode is the resolved output rendering.
type Mode int

const (
	// ModeHuman renders for a person: a table or a line of prose.
	ModeHuman Mode = iota
	// ModeJSON renders the §4.2 envelope.
	ModeJSON
	// ModeQuiet renders nothing on success; failures still go to stderr.
	ModeQuiet
)

func (m Mode) String() string {
	switch m {
	case ModeJSON:
		return "json"
	case ModeQuiet:
		return "quiet"
	default:
		return "human"
	}
}

// Options holds the flags every ocaw command accepts (SPEC.md §7).
type Options struct {
	Dir    string
	Output string
	JSON   bool
	Pretty bool
	Quiet  bool
	DryRun bool
	Yes    bool
}

// ResolveMode implements the precedence --json > --quiet > TTY detection
// (SPEC.md §4.1). A non-terminal stdout means an agent is calling, so JSON is
// the default there.
func ResolveMode(o Options, stdoutTTY bool) Mode {
	switch {
	case o.JSON:
		return ModeJSON
	case o.Quiet:
		return ModeQuiet
	case stdoutTTY:
		return ModeHuman
	default:
		return ModeJSON
	}
}

// IsTerminal reports whether f is a character device. ocaw uses it only to
// choose a default rendering; it never requires a terminal to work.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// Context is everything a command implementation is allowed to see.
type Context struct {
	Options Options
	Mode    Mode
	Dir     string
	Stdout  io.Writer
	Stderr  io.Writer
}

type humanFunc func(c *Context, env envelope.Envelope, w io.Writer) error

type command struct {
	name    string
	summary string
	usage   string
	setup   func(fs *flag.FlagSet, c *Context)
	run     func(c *Context, args []string) envelope.Result
	human   humanFunc
	// humanOnError prints the human rendering even when the envelope is not ok.
	//
	// Most commands keep stdout empty on failure so a script piping stdout gets
	// nothing rather than a half-formed report, and the error goes to stderr.
	// doctor is the exception, and deliberately so: exit 4 there means the
	// workspace is unhealthy, not that doctor broke. Its human output IS the
	// findings, and a human who runs it on a broken workspace must see them.
	// The findings still travel in data, so this changes nothing for a JSON
	// caller; it only stops the terminal from showing a one-line summary of a
	// dozen problems.
	humanOnError bool
}

var commands = []*command{
	versionCommand,
	initCommand,
	doctorCommand,
}

func commandByName(name string) *command {
	for _, cmd := range commands {
		if cmd.name == name {
			return cmd
		}
	}
	return nil
}

func commandNames() []string {
	out := make([]string, 0, len(commands))
	for _, cmd := range commands {
		out = append(out, cmd.name)
	}
	sort.Strings(out)
	return out
}

// Warn builds a warning payload.
func Warn(code envelope.Code, format string, args ...any) envelope.Warning {
	return envelope.Warning{Code: code, Message: fmt.Sprintf(format, args...)}
}

func registerGlobals(fs *flag.FlagSet, o *Options) {
	fs.StringVar(&o.Dir, "dir", o.Dir, "workspace root (default: current directory)")
	fs.StringVar(&o.Output, "output", o.Output, "write the primary output to a file and leave stdout empty")
	fs.BoolVar(&o.JSON, "json", o.JSON, "emit the JSON envelope, overriding TTY detection")
	fs.BoolVar(&o.Pretty, "pretty", o.Pretty, "indent JSON output")
	fs.BoolVar(&o.Quiet, "quiet", o.Quiet, "write nothing on success; errors still go to stderr")
	fs.BoolVar(&o.DryRun, "dry-run", o.DryRun, "compute and report, write nothing")
	fs.BoolVar(&o.Yes, "yes", o.Yes, "confirm an overwriting or invalidating action")
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// Main runs ocaw and returns the process exit code. It never panics and never
// writes to os.Stderr directly, so the whole CLI is testable in-process.
func Main(argv []string, stdout, stderr io.Writer, stdoutTTY bool) int {
	ctx := &Context{Stdout: stdout, Stderr: stderr}
	return run(ctx, argv, stdoutTTY)
}

func run(ctx *Context, argv []string, stdoutTTY bool) int {
	opts := Options{}
	fs := newFlagSet("ocaw")
	registerGlobals(fs, &opts)

	if err := fs.Parse(argv); err != nil {
		mode := ResolveMode(opts, stdoutTTY)
		if errors.Is(err, flag.ErrHelp) {
			return writeHelp(ctx, mode, "ocaw", "")
		}
		return emit(ctx, mode, envelope.Result{
			Command: "ocaw",
			Err:     envelope.NewError(envelope.CodeUsage, err.Error(), "ocaw --help"),
		}, ocawRootCommand)
	}

	args := fs.Args()
	if len(args) == 0 {
		return emitUsage(ctx, ResolveMode(opts, stdoutTTY), envelope.Result{
			Command: "ocaw",
			Data:    map[string]any{"available_commands": commandNames()},
			Err:     envelope.NewError(envelope.CodeUsage, "no command given", "ocaw --help"),
		})
	}

	ctx.Options = opts
	ctx.Dir = opts.Dir
	mode := ResolveMode(opts, stdoutTTY)
	ctx.Mode = mode

	name := args[0]
	if name == "help" {
		topic := ""
		if len(args) > 1 {
			topic = args[1]
		}
		return writeHelp(ctx, mode, "ocaw", topic)
	}

	cmd := commandByName(name)
	if cmd == nil {
		ctx.Options = absorbTrailingGlobals(ctx.Options, args[1:])
		return emitUsage(ctx, ResolveMode(ctx.Options, stdoutTTY), envelope.Result{
			Command: "ocaw",
			Data:    map[string]any{"available_commands": commandNames()},
			Err: envelope.NewError(
				envelope.CodeUnknownCommand,
				fmt.Sprintf("unknown command %q", name),
				suggestCommand(name),
			),
		})
	}

	return dispatch(ctx, cmd, args[1:], stdoutTTY)
}

// absorbTrailingGlobals re-reads the global flags that follow an unrecognised
// command name. The command's own flags are unknowable at that point, but the
// output mode still has to honour what the caller asked for.
func absorbTrailingGlobals(o Options, args []string) Options {
	fs := newFlagSet("ocaw")
	registerGlobals(fs, &o)
	_ = fs.Parse(args)
	return o
}

// ocawRootCommand is the pseudo-command behind a caller mistake: no command
// name, or one that does not exist. It has no flags and its human rendering is
// the error line.
var ocawRootCommand = &command{
	name:  "ocaw",
	human: humanAvailableCommands,
}

// emitUsage reports a caller mistake. In human mode the help is the most
// useful thing on the terminal, so it goes to stdout alongside the error on
// stderr; in JSON mode the envelope is the whole answer.
func emitUsage(ctx *Context, mode Mode, res envelope.Result) int {
	if mode == ModeHuman {
		if _, err := io.WriteString(ctx.Stdout, helpText("ocaw", "")); err != nil {
			return reportInternal(ctx, err)
		}
	}
	return emit(ctx, mode, res, ocawRootCommand)
}

func dispatch(ctx *Context, cmd *command, args []string, stdoutTTY bool) int {
	mode := ctx.Mode
	cfs := newFlagSet("ocaw " + cmd.name)
	registerGlobals(cfs, &ctx.Options)
	if cmd.setup != nil {
		cmd.setup(cfs, ctx)
	}

	if err := cfs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return writeHelp(ctx, mode, "ocaw "+cmd.name, "")
		}
		return emit(ctx, mode, envelope.Result{
			Command: cmd.name,
			Err:     envelope.NewError(envelope.CodeUsage, err.Error(), "ocaw "+cmd.name+" --help"),
		}, cmd)
	}

	if extra := cfs.Args(); len(extra) > 0 {
		return emit(ctx, mode, envelope.Result{
			Command: cmd.name,
			Err: envelope.Errorf(
				envelope.CodeUsage,
				cmd.usage,
				"unexpected argument %q",
				extra[0],
			),
		}, cmd)
	}

	ctx.Mode = ResolveMode(ctx.Options, stdoutTTY)
	return emit(ctx, ctx.Mode, cmd.run(ctx, cfs.Args()), cmd)
}

func suggestCommand(name string) string {
	var best string
	for _, cmd := range commands {
		if strings.HasPrefix(cmd.name, name) {
			return "ocaw " + cmd.name
		}
		if best == "" && strings.Contains(cmd.name, name) {
			best = cmd.name
		}
	}
	if best != "" {
		return "ocaw " + best
	}
	return "ocaw --help"
}

func writeHelp(ctx *Context, mode Mode, name, topic string) int {
	if mode == ModeQuiet {
		return 0
	}
	if mode == ModeJSON {
		env := envelope.Result{
			Command: "help",
			Data: map[string]any{
				"usage":              helpText(name, topic),
				"available_commands": commandNames(),
				"global_flags":       globalFlagNames(),
				"exit_codes":         envelope.ExitTable(),
			},
		}.Envelope()
		raw, err := env.Marshal(ctx.Options.Pretty)
		if err != nil {
			return envelope.CatInternal.Exit()
		}
		if _, err := ctx.Stdout.Write(raw); err != nil {
			return envelope.CatInternal.Exit()
		}
		return 0
	}
	if _, err := io.WriteString(ctx.Stdout, helpText(name, topic)); err != nil {
		return envelope.CatInternal.Exit()
	}
	return 0
}

func helpText(name, topic string) string {
	if topic != "" {
		if cmd := commandByName(topic); cmd != nil {
			return fmt.Sprintf("%s\n\n%s\n\n%s\n", cmd.usage, cmd.summary, cmd.flagHelp())
		}
		return fmt.Sprintf("ocaw: no help for %q\n\n%s\n", topic, usageLine())
	}
	if name != "ocaw" {
		cmd := commandByName(strings.TrimPrefix(name, "ocaw "))
		if cmd != nil {
			return fmt.Sprintf("%s\n\n%s\n\n%s\n", cmd.usage, cmd.summary, cmd.flagHelp())
		}
	}
	return fmt.Sprintf(`ocaw — operate a project agent workspace

%s

Commands:
%s

%s

Output:
  A stdout that is not a terminal defaults to JSON. Precedence is
  --json > --quiet > TTY detection. Diagnostics go to stderr.

Exit codes:
  0 ok                1 internal          2 usage             3 precondition
  4 validation        5 invariant         6 lock_held         7 not_found
  8 needs_confirmation

Exit codes come from error.code, never from message text. The contract lives
in SPEC.md at the repository root.
`, usageLine(), commandHelp(), globalFlagHelp())
}

func usageLine() string {
	return "Usage:\n  ocaw <command> [flags]\n  ocaw <command> --help"
}

func commandHelp() string {
	var b strings.Builder
	width := 0
	for _, cmd := range commands {
		if len(cmd.name) > width {
			width = len(cmd.name)
		}
	}
	for _, cmd := range commands {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, cmd.name, cmd.summary)
	}
	return b.String()
}

// flagHelp lists a command's own flags above the global ones.
//
// The command flags are discovered by running setup against a throwaway
// FlagSet rather than by a hand-maintained list. A list would drift the first
// time a flag was added without updating the help, and the person who finds out
// is the one who typed --help to discover the flag existed.
func (cmd *command) flagHelp() string {
	if cmd.setup == nil {
		return globalFlagHelp()
	}
	fs := newFlagSet("ocaw " + cmd.name)
	// A fresh Options so a command's defaults never leak into the help text.
	ctx := &Context{}
	cmd.setup(fs, ctx)
	var b strings.Builder
	fs.VisitAll(func(f *flag.Flag) {
		// Skip the globals: they are listed once, below.
		if isGlobalFlagName(f.Name) {
			return
		}
		name := "--" + f.Name
		if ph := placeholder(f); ph != "" {
			name += " " + ph
		}
		fmt.Fprintf(&b, "  %-18s %s\n", name, f.Usage)
	})
	if b.Len() == 0 {
		return globalFlagHelp()
	}
	return "Flags:\n" + b.String() + "\n" + globalFlagHelp()
}

var globalFlagSet = func() map[string]bool {
	fs := newFlagSet("ocaw")
	registerGlobals(fs, &Options{})
	out := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) { out[f.Name] = true })
	return out
}()

func isGlobalFlagName(name string) bool { return globalFlagSet[name] }

func globalFlagNames() []string {
	fs := newFlagSet("ocaw")
	registerGlobals(fs, &Options{})
	return sortedFlagNames(fs)
}

func sortedFlagNames(fs *flag.FlagSet) []string {
	var out []string
	fs.VisitAll(func(f *flag.Flag) { out = append(out, "--"+f.Name) })
	sort.Strings(out)
	return out
}

func globalFlagHelp() string {
	fs := newFlagSet("ocaw")
	var o Options
	registerGlobals(fs, &o)
	var b strings.Builder
	fmt.Fprintf(&b, "Global flags:\n")
	fs.VisitAll(func(f *flag.Flag) {
		name := "--" + f.Name
		if placeholder(f) != "" {
			name += " " + placeholder(f)
		}
		fmt.Fprintf(&b, "  %-16s %s\n", name, f.Usage)
	})
	return b.String()
}

func placeholder(f *flag.Flag) string {
	if _, ok := f.Value.(interface{ IsBoolFlag() bool }); ok {
		return ""
	}
	return "<value>"
}

func exitCodeTable() map[string]int {
	return envelope.ExitTable()
}

func humanAvailableCommands(c *Context, env envelope.Envelope, w io.Writer) error {
	if env.Err == nil {
		return nil
	}
	return writeError(w, env.Err)
}

func renderHuman(c *Context, env envelope.Envelope, human humanFunc) ([]byte, error) {
	if human == nil {
		return nil, fmt.Errorf("command %q has no human renderer", env.Command)
	}
	var buf bytes.Buffer
	if err := human(c, env, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeError(w io.Writer, e *envelope.Error) error {
	if e == nil {
		return nil
	}
	if _, err := fmt.Fprintf(w, "ocaw: error: %s: %s\n", e.Code, e.Message); err != nil {
		return err
	}
	if e.Hint == "" {
		return nil
	}
	_, err := fmt.Fprintf(w, "ocaw: hint: %s\n", e.Hint)
	return err
}

func renderDiagnostics(env envelope.Envelope) string {
	var b strings.Builder
	if env.Err != nil {
		_ = writeError(&b, env.Err)
	}
	for _, w := range env.Warnings {
		fmt.Fprintf(&b, "ocaw: warning: %s: %s\n", w.Code, w.Message)
	}
	return b.String()
}

func emit(ctx *Context, mode Mode, res envelope.Result, cmd *command) int {
	env := res.Envelope()
	var (
		human        humanFunc
		humanOnError bool
	)
	if cmd != nil {
		human, humanOnError = cmd.human, cmd.humanOnError
	}

	var payload []byte
	toStdout := true

	switch mode {
	case ModeJSON:
		raw, err := env.Marshal(ctx.Options.Pretty)
		if err != nil {
			return reportInternal(ctx, err)
		}
		payload = raw
	case ModeHuman:
		if env.OK || humanOnError {
			raw, err := renderHuman(ctx, env, human)
			if err != nil {
				return reportInternal(ctx, err)
			}
			payload = raw
		} else {
			// Nothing was rendered, so stdout stays empty and the error goes to
			// stderr alone. A script piping stdout gets nothing rather than a
			// half-formed report.
			toStdout = false
		}
	case ModeQuiet:
		toStdout = false
	}

	if mode != ModeJSON {
		if diag := renderDiagnostics(env); diag != "" {
			if _, err := io.WriteString(ctx.Stderr, diag); err != nil {
				return envelope.CatInternal.Exit()
			}
		}
	}

	if !toStdout || len(payload) == 0 {
		return env.ExitCode()
	}

	if path := ctx.Options.Output; path != "" {
		if err := workspace.WriteFileAtomic(path, payload); err != nil {
			if _, werr := fmt.Fprintf(ctx.Stderr, "ocaw: error: %s: %v\n", envelope.CodeWriteFailed, err); werr != nil {
				return envelope.CatInternal.Exit()
			}
			return envelope.CodeWriteFailed.Exit()
		}
		return env.ExitCode()
	}

	if _, err := ctx.Stdout.Write(payload); err != nil {
		return reportInternal(ctx, err)
	}
	return env.ExitCode()
}

func reportInternal(ctx *Context, err error) int {
	_, _ = fmt.Fprintf(ctx.Stderr, "ocaw: error: %s: %v\n", envelope.CodeInternal, err)
	return envelope.CodeInternal.Exit()
}
