package verify

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tempScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAPassRecordsExitZero(t *testing.T) {
	att := Run(Request{TaskID: "1", Gate: "test", Argv: []string{"/bin/echo", "hello"}})
	if att.Status != Pass {
		t.Errorf("status = %q, want pass; output %q", att.Status, att.Output)
	}
	if att.Exit == nil || *att.Exit != 0 {
		t.Errorf("exit = %v, want 0", att.Exit)
	}
	if att.TimedOut {
		t.Error("timedOut = true on a command that exited")
	}
	if !bytes.Contains(att.Output, []byte("hello")) {
		t.Errorf("output = %q, want it to hold what the command printed", att.Output)
	}
}

func TestAFailureRecordsTheRealExitCode(t *testing.T) {
	script := tempScript(t, "echo 'some failure detail' >&2; exit 3")
	att := Run(Request{TaskID: "1", Gate: "test", Argv: []string{script}})
	if att.Status != Fail {
		t.Errorf("status = %q, want fail", att.Status)
	}
	if att.Exit == nil || *att.Exit != 3 {
		t.Errorf("exit = %v, want the command's own 3", att.Exit)
	}
	// stderr is captured, and so is stdout, in one buffer.
	if !bytes.Contains(att.Output, []byte("some failure detail")) {
		t.Errorf("output = %q, want the stderr text", att.Output)
	}
}

// A timeout is a failed attempt, not a hang, and not a success.
func TestATimeoutIsAnAttemptNotAHang(t *testing.T) {
	script := tempScript(t, "sleep 5")
	att := Run(Request{
		TaskID: "1", Gate: "test", Argv: []string{script},
		Timeout: 150 * time.Millisecond,
	})
	if att.Status != Timeout {
		t.Errorf("status = %q, want timeout", att.Status)
	}
	if !att.TimedOut {
		t.Error("timedOut = false on a timed-out attempt")
	}
	// No exit code: the process was killed, and a made-up one reads like the
	// command's own verdict when it is ocaw's.
	if att.Exit != nil {
		t.Errorf("exit = %v, want nil: nothing exited", *att.Exit)
	}
	if !bytes.Contains(att.Output, []byte("deadline")) {
		t.Errorf("output = %q, want it to say why the attempt ended", att.Output)
	}
}

// A command that does not exist is an attempt. A caller that treats it as an
// error tends to drop it, and then the history is missing the moment someone
// most wanted to look at it.
func TestAMissingProgramIsAnAttempt(t *testing.T) {
	att := Run(Request{TaskID: "1", Gate: "test", Argv: []string{"ocaw-no-such-program-anywhere"}})
	if att.Status != Fail {
		t.Errorf("status = %q, want fail", att.Status)
	}
	if att.Exit != nil {
		t.Errorf("exit = %v, want nil: nothing ran", *att.Exit)
	}
	if !bytes.Contains(att.Output, []byte("could not be started")) {
		t.Errorf("output = %q, want it to say the command never started", att.Output)
	}
}

func TestAnEmptyCommandIsAnAttempt(t *testing.T) {
	att := Run(Request{TaskID: "1", Gate: "test"})
	if att.Status != Fail {
		t.Errorf("status = %q, want fail", att.Status)
	}
	if !bytes.Contains(att.Output, []byte("no command to run")) {
		t.Errorf("output = %q", att.Output)
	}
}

// §9.4: an argv, never a shell. A glob is the cleanest proof, because a shell
// would expand it and exec would not — and the difference is visible in what the
// command printed, with no shell to spy on.
func TestNoShellIsSpawned(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "aaa.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bbb.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	att := Run(Request{TaskID: "1", Gate: "test", Argv: []string{"/bin/echo", "*"}, Dir: dir})
	if !bytes.Contains(att.Output, []byte("*")) {
		t.Errorf("output = %q, want a literal *: something expanded it", att.Output)
	}
	if bytes.Contains(att.Output, []byte("aaa.txt")) {
		t.Errorf("output = %q, want no glob expansion", att.Output)
	}
	// For the same reason, a command with a shell operator reaches the program
	// as written rather than being split by a shell.
	att = Run(Request{TaskID: "1", Gate: "test", Argv: []string{"/bin/echo", "a && b"}})
	if !bytes.Contains(att.Output, []byte("a && b")) {
		t.Errorf("output = %q, want the whole string as one argument", att.Output)
	}
}

// Output from both streams interleaves into one record, in the order a human
// would have seen it.
func TestBothStreamsLandInOneRecord(t *testing.T) {
	script := tempScript(t, "echo out1; echo err1 >&2; echo out2; echo err2 >&2")
	att := Run(Request{TaskID: "1", Gate: "test", Argv: []string{script}})
	got := string(att.Output)
	for _, want := range []string{"out1", "err1", "out2", "err2"} {
		if !strings.Contains(got, want) {
			t.Errorf("output = %q, want it to hold %q", got, want)
		}
	}
	// And stdout comes before stderr, because a single writer served both.
	if strings.Index(got, "out1") > strings.Index(got, "err1") {
		t.Errorf("output = %q, want the streams interleaved in order", got)
	}
}

// The gate inherits the environment: a build that needs PATH, HOME, or a
// toolchain variable has to be able to see it.
func TestTheGateInheritsTheEnvironment(t *testing.T) {
	t.Setenv("OCAW_VERIFY_TEST_VAR", "present")
	att := Run(Request{TaskID: "1", Gate: "test", Argv: []string{"/bin/sh", "-c", "echo $OCAW_VERIFY_TEST_VAR"}})
	if !bytes.Contains(att.Output, []byte("present")) {
		t.Errorf("output = %q, want the variable", att.Output)
	}
}

// The deadline is real: without an explicit one, the default applies, and the
// two attempts are distinguishable.
func TestDefaultTimeoutIsTenMinutes(t *testing.T) {
	if DefaultTimeout != 10*time.Minute {
		t.Errorf("DefaultTimeout = %s, want 10m", DefaultTimeout)
	}
	att := Run(Request{TaskID: "1", Gate: "test", Argv: []string{"/bin/echo", "quick"}})
	if att.TimedOut {
		t.Error("a command that finished was reported as timed out")
	}
}

func TestParseTimeout(t *testing.T) {
	cases := []struct {
		raw     string
		want    time.Duration
		wantErr bool
	}{
		{raw: "", want: DefaultTimeout},
		{raw: "90s", want: 90 * time.Second},
		{raw: "10m", want: 10 * time.Minute},
		{raw: "1h30m", want: 90 * time.Minute},
		// A bare number is seconds. Reading it as minutes would make
		// `--timeout 10` a hundred times longer than the default it was meant to
		// shorten, and in the direction that hides a hang.
		{raw: "10", want: 10 * time.Second},
		{raw: "120", want: 120 * time.Second},
		{raw: "0", wantErr: true},
		{raw: "0s", wantErr: true},
		{raw: "-5s", wantErr: true},
		{raw: "soon", wantErr: true},
		{raw: "10 m", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseTimeout(tc.raw)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseTimeout(%q) = %s, want an error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTimeout(%q): %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseTimeout(%q) = %s, want %s", tc.raw, got, tc.want)
		}
	}
}

// The live sink is a convenience, and a closed one must not fail the gate.
func TestALiveSinkNeverFailsTheGate(t *testing.T) {
	dir := t.TempDir()
	closed, err := os.Create(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	att := Run(Request{
		TaskID: "1", Gate: "test", Argv: []string{"/bin/echo", "still recorded"},
		Live: closed,
	})
	if att.Status != Pass {
		t.Errorf("status = %q, want pass: a closed sink is not the gate's problem", att.Status)
	}
	if !bytes.Contains(att.Output, []byte("still recorded")) {
		t.Errorf("output = %q, want the record to be complete", att.Output)
	}
}

// LookPath exists so a missing program can be named without running anything.
func TestLookPath(t *testing.T) {
	if _, err := LookPath("go"); err != nil {
		t.Errorf("LookPath(go): %v", err)
	}
	if _, err := LookPath("ocaw-definitely-not-installed"); !errors.Is(err, exec.ErrNotFound) && !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("LookPath error = %v, want exec.ErrNotExist", err)
	}
}

// The deadline has to survive a gate that spawns children. Killing only the
// direct child leaves the grandchild holding the output pipe, so cmd.Wait blocks
// for the child's full duration and a 200ms deadline becomes a 5s wait — the
// exact shape of "not a hang" that is a hang.
func TestATimeoutKillsGrandchildrenToo(t *testing.T) {
	script := tempScript(t, "sleep 30")
	start := time.Now()
	att := Run(Request{
		TaskID: "1", Gate: "test", Argv: []string{script},
		Timeout: 200 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if att.Status != Timeout {
		t.Errorf("status = %q, want timeout", att.Status)
	}
	// Generous, because a loaded machine is slow. The point is that it is not
	// anywhere near the child's 30 seconds.
	if elapsed > 5*time.Second {
		t.Errorf("Run took %s for a 200ms deadline; a grandchild held the pipe open", elapsed.Round(time.Millisecond))
	}
}

// goModule writes a compilable module in a fresh directory, so a gate can be a
// real `go test` rather than a script that fakes one.
func goModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/p\n\ngo 1.22\n"
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A command that exits 0 having done nothing is not a pass. `go test` exits 0
// when its -run pattern matches nothing, so these two cases are
// indistinguishable by exit code and by every line except the marker.
func TestGateThatRanNothingIsNotAPass(t *testing.T) {
	dir := goModule(t, map[string]string{
		"lib.go":      "package p\n\nfunc Hello() string { return \"hi\" }\n",
		"lib_test.go": "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
	})

	for _, c := range []struct {
		name string
		argv []string
		want Status
	}{
		// The bug: exit 0, `ok <pkg>`, and no test ran.
		{"pattern matches nothing", []string{"go", "test", "-run", "NoSuchTestName", "./..."}, Fail},
		{"tests actually ran", []string{"go", "test", "-run", "TestA", "./..."}, Pass},
	} {
		t.Run(c.name, func(t *testing.T) {
			att := Run(Request{TaskID: "1", Gate: "t", Argv: c.argv, Dir: dir, Timeout: 2 * time.Minute})
			if att.Status != c.want {
				t.Fatalf("status = %q, want %q\noutput: %s", att.Status, c.want, att.Output)
			}
			if att.Exit == nil {
				t.Error("exit is nil; the command really did exit, and that is the evidence")
			}
			if c.want == Fail && !bytes.Contains(att.Output, []byte("ran nothing")) {
				t.Errorf("a failed gate must say why\noutput: %s", att.Output)
			}
		})
	}
}

// `[no test files]` is what every `go test ./...` reports for a package that has
// no tests. Failing it would fail essentially every real Go project, so the
// marker list must not contain it. This is the negative case that keeps the
// narrow fix narrow.
func TestPackageWithoutTestsIsStillAPass(t *testing.T) {
	dir := goModule(t, map[string]string{
		"lib.go": "package p\n\nfunc Hello() string { return \"hi\" }\n",
	})

	att := Run(Request{TaskID: "1", Gate: "t", Dir: dir, Timeout: 2 * time.Minute,
		Argv: []string{"go", "test", "./..."}})
	if !bytes.Contains(att.Output, []byte("no test files")) {
		t.Skipf("this Go version does not report [no test files] here:\n%s", att.Output)
	}
	if att.Status != Pass {
		t.Fatalf("status = %q, want pass; a package with no tests is not a gap\noutput: %s", att.Status, att.Output)
	}
}

func TestNothingRanMarkerTable(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want bool
	}{
		{"ok  \tpkg\t0.5s\t[no tests to run]\n", true},
		{"ok  \tpkg\t0.5s\n", false},
		{"?   \tpkg\t[no test files]\n", false},
		{"PASS\nok  \tpkg\t1.2s\n", false},
		{"", false},
	} {
		if _, got := nothingRan([]byte(tc.out)); got != tc.want {
			t.Errorf("nothingRan(%q) = %v, want %v", tc.out, got, tc.want)
		}
	}
}

// A gate whose own output happens to contain the marker fails, even though it
// really did the work. That is a false positive, and it is accepted on purpose:
// a verification tool that fails closed on an ambiguous signal is safer than one
// that fails open, and the alternative is parsing `go test`'s line format, which
// couples the gate runner to one toolchain.
//
// The case is pinned here so the behaviour is a decision rather than an accident.
func TestMarkerInARelevantPassStillFails(t *testing.T) {
	dir := goModule(t, map[string]string{
		"lib.go":      "package p\n\nfunc Hello() string { return \"hi\" }\n",
		"lib_test.go": "package p\n\nimport \"testing\"\n\nfunc TestLogs(t *testing.T) {\n\tt.Log(\"server said [no tests to run]\")\n}\n",
	})

	att := Run(Request{TaskID: "1", Gate: "t", Dir: dir, Timeout: 2 * time.Minute,
		Argv: []string{"go", "test", "-run", "TestLogs", "-v", "./..."}})
	if att.Status != Fail {
		t.Fatalf("status = %q; a substring match is known to fail closed, and this pins that", att.Status)
	}
	if !bytes.Contains(att.Output, []byte("ran nothing")) {
		t.Errorf("output must carry the reason\noutput: %s", att.Output)
	}
}
