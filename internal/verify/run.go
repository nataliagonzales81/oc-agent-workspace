// Package verify runs a project's verification command as an argv and records
// what happened.
//
// Three properties define this package, and each of them is a refusal rather
// than a convenience:
//
//   - An argv, never a shell (§9.4). exec.Command(argv[0], argv[1:]...) is the
//     only execution path, so a gate command containing &&, |, or $() is
//     refused when it is written rather than being handed to something that
//     would interpret it.
//   - Every attempt is recorded, including the ones that failed and the ones
//     that were killed at the deadline. The record is the only evidence of what
//     was tried, so skipping one would erase an attempt from the history.
//   - Nothing is ever retried automatically. A gate that failed three times
//     with byte-identical output is stuck, and whether to escalate or try a
//     fourth time is the agent's call, not the tool's.
//
// This package is a leaf: it imports nothing from the rest of ocaw. internal/state
// needs Tokenize and therefore depends on verify, so a runner here that reached
// back into state for its record type would close a cycle. The Status vocabulary
// lives here and state aliases it, rather than the other way round, so there is
// one definition of "pass" in the tree.
package verify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultTimeout bounds a single attempt. Ten minutes is longer than any test
// suite that is going to be run as part of a task, and a gate that takes longer
// is a hang, not a slow test.
const DefaultTimeout = 10 * time.Minute

// killGrace is how long cmd.Wait may keep draining output after the deadline
// has fired and the group has been killed.
const killGrace = 2 * time.Second

// TimeoutNever disables the deadline. It exists so a caller can prove the
// timeout exists by asking for it to be absent; there is no reason to want it
// otherwise.
const TimeoutNever time.Duration = 0

// Status is the outcome vocabulary of one attempt. `pending` is not here: it is
// the state of a gate that has not run, which belongs to the state model rather
// than to a run.
type Status string

const (
	// Pass means the command ran and exited zero.
	Pass Status = "pass"
	// Fail means the command ran and refused, or never started.
	Fail Status = "fail"
	// Timeout means the command was killed at its deadline. It is distinct from
	// Fail so `ocaw status` can tell a slow gate from a broken one.
	Timeout Status = "timeout"
)

// Request is one gate to run.
type Request struct {
	// TaskID and Gate name the record. Both matter for reading the history
	// later, so a caller that cannot supply them is a caller the log cannot
	// serve.
	TaskID string
	Gate   string
	// Argv is the command. It is a slice because it came from Tokenize, which
	// refuses anything only a shell could interpret.
	Argv []string
	// Dir is the working directory. Empty means the caller's business.
	Dir string
	// Timeout bounds the attempt. Zero means DefaultTimeout; TimeoutNever
	// disables the deadline.
	Timeout time.Duration
	// Live, when non-nil, receives a copy of the output as it arrives, for a
	// human watching a long gate. The attempt's own Output is unaffected.
	Live *os.File
}

// Attempt is the result of one run.
type Attempt struct {
	TaskID string
	Gate   string
	Argv   []string
	Status Status
	Exit   *int
	Output []byte
	At     time.Time
	// TimedOut distinguishes a gate killed at the deadline from one that exited
	// non-zero. Both are a failed attempt; only one is worth raising with
	// whoever owns the project, because a gate that times out is usually waiting
	// for input that will never arrive.
	TimedOut bool
}

// Run executes one request and always returns an Attempt.
//
// There is no error return: every outcome is an attempt, including a command
// that does not exist. An attempt whose command could not be started is
// evidence, and a caller that treats it as an error tends to drop it.
func Run(req Request) Attempt {
	at := time.Now().UTC()
	att := Attempt{TaskID: req.TaskID, Gate: req.Gate, Argv: req.Argv, At: at}

	if len(req.Argv) == 0 {
		att.Status = Fail
		att.Output = []byte("ocaw: the gate has no command to run\n")
		return att
	}

	timeout := req.Timeout
	if timeout == TimeoutNever {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Both streams are captured into one buffer. A test suite's failures are
	// often on stderr and its progress on stdout, and a record that kept only
	// one would report a green run as a silent one — or the reverse.
	var buf bytes.Buffer
	writer := &teeWriter{Buffer: &buf, Live: req.Live}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Dir
	cmd.Stdout = writer
	cmd.Stderr = writer
	// The gate inherits the environment rather than a curated one: a build that
	// needs PATH, HOME, or a toolchain variable has to be able to see it.
	cmd.Env = os.Environ()

	// Kill the whole process group at the deadline, so a gate that spawned
	// children leaves nothing behind holding the output pipe open.
	isolate(cmd)
	cmd.Cancel = func() error { return killGroup(cmd) }
	// And bound how long Wait may spend draining the output after that. Two
	// guarantees rather than one: the group kill removes the usual culprit, and
	// the delay means a survivor still cannot turn a deadline into a hang. A
	// command that overran is already a failed attempt; there is no reason to
	// keep reading its output.
	cmd.WaitDelay = killGrace

	runErr := cmd.Run()
	att.Output = buf.Bytes()

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		// A timeout is a failed attempt, not a hang and not a success. Exit is
		// left nil: there is no exit code, because the process was killed, and
		// inventing one — a conventional -1 — reads like the command's own
		// verdict when it is ocaw's.
		att.Status = Timeout
		att.TimedOut = true
		att.Output = append(att.Output, deadlineNote(timeout)...)
		return att
	}

	if runErr == nil {
		att.Status = Pass
		zero := 0
		att.Exit = &zero
		return att
	}

	att.Status = Fail
	var exitErr *exec.ExitError
	switch {
	case errors.As(runErr, &exitErr):
		// The command ran and refused. Its exit code is the answer.
		code := exitErr.ExitCode()
		att.Exit = &code
		att.Output = append(att.Output, exitNote(code)...)
	default:
		// It never ran. The message is the evidence, and there is no exit code
		// to report because nothing was executed.
		att.Output = append(att.Output, startNote(runErr)...)
	}
	return att
}

// teeWriter copies into the buffer and, when a live sink was given, to it too.
// Both streams of a command go through one of these, so the record interleaves
// them in the order a human saw rather than all of stdout followed by stderr.
type teeWriter struct {
	Buffer *bytes.Buffer
	Live   *os.File
}

func (w *teeWriter) Write(p []byte) (int, error) {
	if _, err := w.Buffer.Write(p); err != nil {
		return 0, err
	}
	if w.Live != nil {
		// A closed terminal must never fail the gate. The attempt's own record
		// is the thing that matters, and the live copy is a convenience.
		_, _ = w.Live.Write(p)
	}
	return len(p), nil
}

func deadlineNote(d time.Duration) []byte {
	return []byte("\nocaw: the gate exceeded its " + d.String() + " deadline and was killed\n")
}

func exitNote(code int) []byte {
	return []byte("\nocaw: the gate exited " + itoa(code) + "\n")
}

func startNote(err error) []byte {
	return []byte("\nocaw: the gate could not be started: " + err.Error() + "\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}

// LookPath resolves a gate's program so a missing one can be reported as a
// missing one. Run deliberately does not call it: a command that exists but
// fails to start produces a better message from the exec layer than from here.
func LookPath(program string) (string, error) { return exec.LookPath(program) }

// ParseTimeout turns the --timeout flag's value into a duration.
//
// A bare number is read as seconds, because `--timeout 10` meaning 10 *minutes*
// is a plausible misreading of the default and the wrong direction. Go's
// ParseDuration alone rejects it, which is safe but leaves the caller guessing.
func ParseTimeout(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultTimeout, nil
	}
	if allDigits(raw) {
		seconds, err := atoi(raw)
		if err != nil {
			return 0, err
		}
		if seconds == 0 {
			return 0, errors.New("timeout must be greater than zero")
		}
		return time.Duration(seconds) * time.Second, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, errors.New("timeout must be a duration like 90s or 10m, or a whole number of seconds")
	}
	if d <= 0 {
		return 0, errors.New("timeout must be greater than zero")
	}
	return d, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func atoi(s string) (int, error) {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
		if n > 1<<40 {
			return 0, errors.New("timeout is implausibly large")
		}
	}
	return n, nil
}
