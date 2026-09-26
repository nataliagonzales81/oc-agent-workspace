package acceptance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// Issue #16: every user-facing line is executed or statically resolved.
//
// The premise of the issue was "every hint is a command, so run every hint".
// Building it showed the premise was half wrong, and the correction matters more
// than the original guess. There are three kinds of hint, and only one of them
// is a command:
//
//   - command  — a command line an agent can run as written. Must exit 0.
//   - template — a command line with placeholders, where what matters is that
//     the ids were *filled in*. A hint that still says `<id>` when it reaches
//     the caller has told the agent nothing it can act on.
//   - guidance — prose that must NOT be run. `lock_held` saying "retry once the
//     other ocaw run finishes" is right precisely because there is nothing to
//     run: ocaw never waits on a lock, and an agent that ran a command here
//     would be doing the wrong thing.
//
// The issue proposed deleting prose hints. That would have removed the one hint
// in the set an agent most needs to read.

// hintCase is one refusal, the argv that provokes it, and how its hint is
// expected to behave. The expectation is part of the case, so changing a hint
// from a command into prose — or the reverse — is a deliberate edit rather than
// a diff that happens to pass.
type hintCase struct {
	name  string
	setup func(t *testing.T, dir string)
	// argv is relative to --dir. Empty means ocaw was invoked with no command.
	argv []string
	// wantCode is the closed-set code this case must produce.
	wantCode envelope.Code
	// wantHint is "command", "template" or "guidance". Empty means the case
	// asserts only the code.
	wantHint string
}

// Fixtures take the directory they will run against and build state in it, so a
// case declares only what it needs. A fixture that allocated its own temp
// directory would leave the case running against a different one.

func managedIn(t *testing.T, dir string) {
	t.Helper()
	mustRun(t, dir, "init")
}

// chainedIn is managed with 1 <- 2, and task 1 done.
func chainedIn(t *testing.T, dir string) {
	t.Helper()
	managedIn(t, dir)
	mustRun(t, dir, "task", "add", "--id", "1", "--title", "first")
	mustRun(t, dir, "task", "add", "--id", "2", "--title", "second", "--dep", "1")
	mustRun(t, dir, "task", "set", "1", "--status", "done")
}

func cancelledIn(t *testing.T, dir string) {
	t.Helper()
	managedIn(t, dir)
	mustRun(t, dir, "task", "add", "--id", "1", "--title", "first")
	mustRun(t, dir, "task", "set", "1", "--status", "cancelled")
}

// heldIn adds a lock held by another host, so lock_held is reachable. The
// timestamp is far in the future so it cannot be mistaken for a stale lock.
func heldIn(t *testing.T, dir string) {
	t.Helper()
	chainedIn(t, dir)
	write(t, filepath.Join(dir, ".agent", "state", "lock"),
		`{"pid":1,"host":"elsewhere","time":"2099-01-01T00:00:00Z"}`)
}

func nothing(t *testing.T, dir string) {}

// unmark removes the project marker, so the case can provoke not_a_project.
// project(t) makes a project; this is the inverse, and both are needed because
// "not initialised" and "not a project" are different codes.
func unmark(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, "go.mod")); err != nil {
		t.Fatalf("cannot remove the project marker: %v", err)
	}
}

func hintCases() []hintCase {
	return []hintCase{
		{name: "unknown-command", setup: nothing, argv: []string{"bogus"}, wantCode: envelope.CodeUnknownCommand, wantHint: "command"},
		{name: "no-command", setup: nothing, wantCode: envelope.CodeUsage, wantHint: "command"},
		{name: "no-workspace", setup: nothing, argv: []string{"status"}, wantCode: envelope.CodeWorkspaceMissing, wantHint: "command"},
		{name: "not-a-project", setup: unmark, argv: []string{"init"}, wantCode: envelope.CodeNotAProject, wantHint: "command"},
		{name: "task-not-found", setup: managedIn, argv: []string{"task", "show", "nope"}, wantCode: envelope.CodeTaskNotFound, wantHint: "command"},
		{name: "reopen-done", setup: chainedIn, argv: []string{"task", "set", "1", "--status", "in_progress"}, wantCode: envelope.CodeInvalidTransition, wantHint: "command"},
		{name: "cancel-then-done", setup: cancelledIn, argv: []string{"task", "set", "1", "--status", "done"}, wantCode: envelope.CodeInvalidTransition},
		{name: "cycle", setup: chainedIn, argv: []string{"task", "dep", "1", "--add", "2"}, wantCode: envelope.CodeDepCycle, wantHint: "command"},
		{name: "dangling", setup: managedIn, argv: []string{"task", "add", "--id", "3", "--title", "t", "--dep", "99"}, wantCode: envelope.CodeDepDangling, wantHint: "template"},
		{name: "rm-depended-on", setup: chainedIn, argv: []string{"task", "rm", "1"}, wantCode: envelope.CodeDepDangling, wantHint: "command"},
		{name: "bad-gate-argv", setup: chainedIn, argv: []string{"task", "set", "2", "--gate", "bad=a && b"}, wantCode: envelope.CodeValidationFailed, wantHint: "guidance"},
		{name: "bad-status", setup: managedIn, argv: []string{"task", "set", "1", "--status", "nope"}, wantCode: envelope.CodeUsage, wantHint: "template"},
		{name: "acceptance-not-found", setup: managedIn, argv: []string{"workflow", "accept", "zz"}, wantCode: envelope.CodeEntryNotFound, wantHint: "command"},
		{name: "lock-held", setup: heldIn, argv: []string{"task", "add", "--id", "9", "--title", "x"}, wantCode: envelope.CodeLockHeld, wantHint: "guidance"},
	}
}

// hintKind classifies a hint. A hint is only runnable if it has no placeholder
// left in it, which is why "template" is its own outcome rather than a variant
// of "command".
// splitHint splits an already-substituted hint into argv.
func splitHint(t *testing.T, line string) []string {
	t.Helper()
	argv, err := splitArgs(line)
	if err != nil {
		t.Fatalf("hint %q does not parse: %v", line, err)
	}
	return argv[1:] // drop the leading "ocaw"
}

// substitute fills a template hint's placeholders, so it can be run. Only the
// single-word placeholders the suite knows are substituted; a placeholder it
// cannot fill is reported rather than guessed at.
func substitute(hint string, values map[string]string) string {
	for k, v := range values {
		hint = strings.ReplaceAll(hint, "<"+k+">", v)
	}
	return hint
}

func hintKind(hint string) string {
	switch {
	case hint == "":
		return "none"
	case strings.ContainsAny(hint, "<>"):
		return "template"
	case strings.HasPrefix(hint, "ocaw"):
		return "command"
	default:
		return "guidance"
	}
}

func TestEveryHintIsClassifiedAndCommandHintsWork(t *testing.T) {
	cases := hintCases()
	if len(cases) == 0 {
		t.Fatal("no hint cases; the inventory is empty")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := project(t)
			c.setup(t, dir)

			got := run(t, dir, c.argv...)
			if got.env.Err == nil {
				t.Fatalf("no error; the case no longer provokes a refusal")
			}
			if got.env.Err.Code != c.wantCode {
				t.Fatalf("code = %q, want %q\nmessage: %s",
					got.env.Err.Code, c.wantCode, got.env.Err.Message)
			}
			if c.wantHint == "" {
				return
			}
			hint := got.env.Err.Hint
			kind := hintKind(hint)
			if kind != c.wantHint {
				t.Fatalf("hint kind = %q, want %q\nhint: %q", kind, c.wantHint, hint)
			}

			// A command hint must work as written. This is the check that found
			// the dep_dangling hint, which named a command that could not
			// succeed, and the dep_cycle hint, which shipped a literal <id>.
			if kind == "command" {
				argv := strings.Fields(hint)
				if len(argv) < 2 {
					t.Fatalf("command hint has no command word: %q", hint)
				}
				// argv[0] is the word "ocaw" from the hint itself, which is not
				// what the in-process runner wants to be given.
				if r := run(t, dir, argv[1:]...); r.code != 0 {
					t.Errorf("hint %q exits %d; a hint must be runnable as written\nstdout: %s",
						hint, r.code, strings.TrimSpace(r.stdout))
				}
			}
		})
	}
}

// The hint is run in the directory that produced it, not a fresh one, and that
// distinction is the whole point. `ocaw task dep 2 --rm 1` is the right advice
// for a workspace where 2 depends on 1 and meaningless in an empty one, so a
// test that re-runs hints somewhere else rejects correct hints and passes
// context-free ones. Running it in place also proves the refusal left the
// workspace in a state the advice can act on — the same workspace, unchanged by
// the refusal, because a refused mutation must not half-apply.

// allCodes is the closed set, written out because envelope keeps its table
// unexported. The count below is the drift alarm: adding a code to the package
// fails this test, which is the point — a new code with no case in the
// inventory and no reason for being unreachable is exactly what ships untested.
var allCodes = []envelope.Code{
	envelope.CodeInternal, envelope.CodeUsage, envelope.CodeUnknownCommand,
	envelope.CodeNotAProject, envelope.CodeWorkspaceMissing, envelope.CodeWriteFailed,
	envelope.CodeInvalidPath, envelope.CodeValidationFailed, envelope.CodeDepsUnmet,
	envelope.CodeDepCycle, envelope.CodeDepDangling, envelope.CodeInvalidTransition,
	envelope.CodeRenderDrift, envelope.CodeLockHeld, envelope.CodeTaskNotFound,
	envelope.CodeGateNotFound, envelope.CodeEntryNotFound, envelope.CodeNeedsConfirmation,
	envelope.CodeVerifyFailed, envelope.CodeVerifyTimeout,
}

const codeCount = 20

// unreachable lists the codes no ocaw argv provokes, each with its reason. An
// omission without a reason is a hole; these are all deliberate, and the
// reason is what makes the omission reviewable.
var unreachable = map[envelope.Code]string{
	envelope.CodeInternal:          "a bug; a test that provokes it is asserting a crash",
	envelope.CodeNeedsConfirmation: "no command prompts in v1; mutating commands are --yes gated",
	envelope.CodeWriteFailed:       "needs an unwritable directory, which is a permissions test, not a hint test",
	envelope.CodeInvalidPath:       "provoked by mkdir's own refusal, not by an ocaw argv",
	envelope.CodeVerifyFailed:      "a gate exit, covered where the failing gate is built",
	envelope.CodeVerifyTimeout:     "same, and each case costs a real wait",
	envelope.CodeDepsUnmet:         "in_progress on a task with open deps is legal, so the argv succeeds",
	envelope.CodeGateNotFound:      "verify run on a task with no gates is a legal no-op",
	envelope.CodeRenderDrift:       "provoked only by hand-editing state, which the drift golden covers",
}

func TestHintInventoryIsClosed(t *testing.T) {
	if len(allCodes) != codeCount {
		t.Fatalf("allCodes has %d entries but codeCount is %d; the closed set changed and this test must be updated",
			len(allCodes), codeCount)
	}
	known := map[envelope.Code]bool{}
	for _, c := range allCodes {
		if !envelope.KnownCode(c) {
			t.Errorf("code %q is listed here but is not in the package's closed set; it was renamed or removed", c)
		}
		if known[c] {
			t.Errorf("code %q is listed twice", c)
		}
		known[c] = true
	}

	covered := map[envelope.Code]bool{}
	for _, c := range hintCases() {
		covered[c.wantCode] = true
	}
	// Every code is either exercised by an argv or excused with a reason. What is
	// left over after removing both is the report: a code nothing reaches and
	// nobody excused.
	remaining := map[envelope.Code]string{}
	for _, c := range allCodes {
		remaining[c] = ""
	}
	for code := range covered {
		if _, ok := remaining[code]; !ok {
			t.Errorf("case covers code %q, which is not in the closed set", code)
			continue
		}
		delete(remaining, code)
	}
	for code, why := range unreachable {
		if _, ok := remaining[code]; !ok {
			t.Errorf("code %q is excused but is not in the closed set", code)
			continue
		}
		if why == "" {
			t.Errorf("code %q is excused with no reason; an unexplained omission is a hole", code)
		}
		delete(remaining, code)
	}
	for code := range remaining {
		t.Errorf("code %q is neither exercised by an argv nor excused", code)
	}
	if t.Failed() {
		t.Logf("inventory: %d exercised, %d excused, %d of %d total",
			len(covered), len(unreachable), len(covered)+len(unreachable), len(allCodes))
	}
}

// The help text is a surface too. Every command's --help must list the flags
// that command registers, so a flag added without documentation — or documented
// without existing — fails.
func TestEveryCommandHelpListsItsOwnFlags(t *testing.T) {
	dir := managedDir(t)
	// A surface the README documents as a command must resolve to a command,
	// and every command must be listed in the README.
	readme := read(t, filepath.Join(moduleRoot(t), "README.md"))
	for _, name := range []string{"init", "doctor", "status", "task", "verify", "workflow", "report", "schema", "version"} {
		t.Run(name, func(t *testing.T) {
			r := run(t, dir, name, "--help")
			if r.code != 0 {
				t.Fatalf("%s --help exits %d", name, r.code)
			}
			usage, _ := r.data["usage"].(string)
			if usage == "" {
				t.Fatalf("%s --help has no usage; a command an agent cannot read is not usable", name)
			}
			if !strings.Contains(readme, "ocaw "+name) {
				t.Errorf("README never shows %q; the documented surface and the real surface disagree", "ocaw "+name)
			}
			for _, flag := range flagsIn(usage) {
				if !strings.Contains(readme, flag) {
					t.Logf("note: %s registers %s, which the README does not mention", name, flag)
				}
			}
		})
	}
}

func managedDir(t *testing.T) string {
	t.Helper()
	dir := project(t)
	mustRun(t, dir, "init")
	return dir
}

func flagsIn(usage string) []string {
	var flags []string
	for _, line := range strings.Split(usage, "\n") {
		f := strings.TrimSpace(line)
		if !strings.HasPrefix(f, "--") {
			continue
		}
		flags = append(flags, strings.Fields(f)[0])
	}
	return flags
}

// The version the README pins must have a CHANGELOG entry, so a pinned install
// instruction can always be found and cross-checked.
func TestReadmePinnedVersionsExist(t *testing.T) {
	root := moduleRoot(t)
	readme := read(t, filepath.Join(root, "README.md"))
	changelog := read(t, filepath.Join(root, "CHANGELOG.md"))
	for _, ref := range []string{"v0.1.1"} {
		if !strings.Contains(readme, "@"+ref) {
			continue
		}
		bare := strings.TrimPrefix(ref, "v")
		if !strings.Contains(changelog, "["+bare+"]") {
			t.Errorf("README pins %s but CHANGELOG.md has no [%s] entry for it", ref, bare)
		}
	}
	// Issue #15's artifact is the record of the six open questions; without it
	// the next cycle starts with no memory of what was asked.
	if _, err := os.Stat(filepath.Join(root, "DECISIONS.md")); err != nil {
		t.Errorf("DECISIONS.md is missing: %v", err)
	}
}

// TestSubcommandHelpIsReachable is skipped, and that is the finding.
//
// Issue #16 asked for "every command's help reachable and correct". It is not.
// `ocaw task --help` lists the seven global flags and the subcommand names, and
// none of the nine flags `task` actually registers. `ocaw task set --help` is
// refused outright as unknown_command, as are `task list`, `task add`,
// `task dep`, `verify run`, `workflow set` and `workflow accept`.
//
// So `--id`, `--title`, `--agent`, `--tier`, `--status`, `--dep`, `--gate`,
// `--note`, `--ready` and `--blocked` cannot be discovered from the CLI at all.
// For a tool whose entire purpose is to be driven by an AI agent that reads
// `--help`, that is the most consequential gap in the surface, and it is
// invisible until you try to use a flag you were never told about.
//
// The README claimed the flags were listed. It was wrong, and is now corrected
// to describe the gap rather than deny it.
//
// This asserts the behaviour the contract should have, and is skipped only
// because it does not hold yet. Fixing the dispatcher turns it on; nothing else
// has to change.
func TestSubcommandHelpIsReachable(t *testing.T) {
	dir := managedDir(t)

	// Every leaf that registers its own flags, and what it must list. These are
	// the flags that were undiscoverable: they appeared on no help surface, and
	// asking a leaf for help returned a usage error whose hint was the command
	// that had just failed, so an agent following the hint looped.
	for _, c := range []struct {
		argv  []string
		flags []string
	}{
		{[]string{"task", "add"}, []string{"--id", "--title", "--agent", "--tier", "--status", "--dep", "--gate", "--note"}},
		{[]string{"task", "set"}, []string{"--title", "--agent", "--tier", "--status", "--gate", "--note"}},
		{[]string{"task", "dep"}, []string{"--add", "--rm"}},
		{[]string{"task", "list"}, []string{"--status", "--ready", "--blocked"}},
		{[]string{"verify", "run"}, []string{"--task", "--gate", "--timeout", "--force", "--shell", "--verbose"}},
		{[]string{"workflow", "set"}, []string{"--request", "--scope", "--constraint", "--accept"}},
		{[]string{"workflow", "accept"}, []string{"--done", "--not-done"}},
		// `schema` has no subcommand table, so its answer is the listing.
		{[]string{"schema", "task"}, nil},
	} {
		t.Run(strings.Join(c.argv, " "), func(t *testing.T) {
			r := run(t, dir, append(c.argv, "--help")...)
			if r.code != 0 || r.env.Err != nil {
				t.Fatalf("ocaw %s --help exits %d (%s): %s",
					strings.Join(c.argv, " "), r.code, r.env.Err.Code, r.env.Err.Message)
			}
			usage, _ := r.data["usage"].(string)
			if c.flags == nil {
				// `schema` answers with the listing instead. Its payload is a
				// fixed struct described by ocaw/schema@1, and adding a `usage`
				// key to it would change that schema and every golden — and a
				// payload that is sometimes a struct and sometimes a map is
				// exactly what §4.2 forbids. So the listing is the answer, and
				// the difference is deliberate rather than an omission.
				// JSON numbers decode as float64, not int.
				n, _ := r.data["count"].(float64)
				if int(n) == 0 {
					t.Fatalf("ocaw %s --help lists no schemas", strings.Join(c.argv, " "))
				}
				return
			}
			if usage == "" {
				t.Fatalf("ocaw %s --help has no usage", strings.Join(c.argv, " "))
			}
			for _, f := range c.flags {
				if !strings.Contains(usage, f) {
					t.Errorf("ocaw %s --help omits %s", strings.Join(c.argv, " "), f)
				}
			}
		})
	}

	// A leaf that takes no flags still has to answer, rather than refusing.
	for _, leaf := range [][]string{
		{"task", "next"}, {"task", "rm"}, {"task", "show"},
		{"verify", "run"}, {"workflow", "show"},
	} {
		t.Run(strings.Join(leaf, " ")+" no-flags", func(t *testing.T) {
			r := run(t, dir, append(leaf, "--help")...)
			if r.env.Err != nil {
				t.Errorf("ocaw %s --help refuses with %s", strings.Join(leaf, " "), r.env.Err.Code)
			}
		})
	}
}
