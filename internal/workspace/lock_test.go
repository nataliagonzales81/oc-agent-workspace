package workspace

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

const testHost = "test-host"

// writeLock plants a lock file with explicit contents, which is how a test
// states "a previous run left this behind" without inventing a race.
func writeLock(t *testing.T, l *Layout, info LockInfo) {
	t.Helper()
	if err := EnsureDir(l.StateDir()); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(l.LockPath(), append(raw, '\n'), FileMode); err != nil {
		t.Fatalf("write lock: %v", err)
	}
}

func hasWarning(warnings []envelope.Warning, code envelope.Code) bool {
	for _, w := range warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// The headline invariant: a second acquire fails fast with lock_held. An agent
// that blocks on a lock cannot be interrupted and looks like a hang, so the
// elapsed time is asserted, not just the error.
func TestSecondAcquireFailsFastWithLockHeld(t *testing.T) {
	l := newLayout(t)
	first, warnings, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	t.Cleanup(func() { _ = first.Release() })
	if len(warnings) != 0 {
		t.Errorf("first Acquire warned: %v", warnings)
	}

	start := time.Now()
	second, _, err := acquireAt(l, time.Now(), testHost)
	elapsed := time.Since(start)
	if err == nil {
		second.Release()
		t.Fatal("second Acquire succeeded, want lock_held")
	}
	if code := codeOf(t, err); code != envelope.CodeLockHeld {
		t.Errorf("code = %s, want %s", code, envelope.CodeLockHeld)
	}
	if second != nil {
		t.Error("a failed Acquire returned a lock")
	}
	if elapsed > 2*time.Second {
		t.Errorf("second Acquire took %s; the lock must fail fast, never block", elapsed)
	}
	if got := envelope.CodeLockHeld.Exit(); got != 6 {
		t.Errorf("lock_held exit = %d, want 6", got)
	}
}

// Exactly one of many simultaneous acquires may win. The losers must all get
// lock_held and must leave no debris: the O_EXCL create is what makes that
// true, and a test that only takes the lock twice never exercises the race
// between the read and the create.
func TestConcurrentAcquiresProduceExactlyOneHolder(t *testing.T) {
	l := newLayout(t)
	const racers = 8

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]*Lock, racers)
	errs := make([]error, racers)
	warns := make([][]envelope.Warning, racers)

	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], warns[i], errs[i] = acquireAt(l, time.Now(), testHost)
		}(i)
	}
	close(start)
	wg.Wait()

	held := 0
	for i := 0; i < racers; i++ {
		switch {
		case errs[i] == nil:
			held++
			if results[i] == nil {
				t.Errorf("racer %d succeeded without a lock", i)
			}
		case codeOf(t, errs[i]) != envelope.CodeLockHeld:
			t.Errorf("racer %d failed with %s, want %s", i, codeOf(t, errs[i]), envelope.CodeLockHeld)
		}
	}
	if held != 1 {
		t.Fatalf("%d racers acquired the lock, want exactly 1", held)
	}
	if debris := tempDebris(t, l.StateDir()); len(debris) != 0 {
		t.Errorf("temp debris left behind: %v", debris)
	}
	for i := 0; i < racers; i++ {
		if errs[i] == nil {
			if err := results[i].Release(); err != nil {
				t.Errorf("Release: %v", err)
			}
		}
	}
	if still, _, err := Held(l); err != nil || still {
		t.Errorf("Held after the winner released = %v, %v; want false, nil", still, err)
	}
}

func TestLockHeldNamesTheHolder(t *testing.T) {
	l := newLayout(t)
	writeLock(t, l, LockInfo{PID: 4242, Host: "elsewhere", Time: time.Now().UTC().Format(time.RFC3339)})

	_, _, err := acquireAt(l, time.Now(), testHost)
	if err == nil {
		t.Fatal("Acquire succeeded against a live lock")
	}
	msg := err.Error()
	if !strings.Contains(msg, "4242") || !strings.Contains(msg, "elsewhere") {
		t.Errorf("message = %q, want it to name the holding pid and host", msg)
	}
}

func TestAcquireAfterReleaseSucceeds(t *testing.T) {
	l := newLayout(t)
	first, _, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if held, _, err := Held(l); err != nil || held {
		t.Fatalf("Held after Release = %v, %v; want false, nil", held, err)
	}
	second, _, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire after Release: %v", err)
	}
	if err := second.Release(); err != nil {
		t.Errorf("Release: %v", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	l := newLayout(t)
	lk, _, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := lk.Release(); err != nil {
		t.Errorf("second Release: %v, want nil for an already-released lock", err)
	}
	var nilLock *Lock
	if err := nilLock.Release(); err != nil {
		t.Errorf("nil Release: %v, want nil", err)
	}
}

// A lock that was broken and retaken belongs to the new holder. The loser of a
// break-and-race must not delete the winner's lock on its way out.
func TestReleaseDoesNotRemoveAnotherHoldersLock(t *testing.T) {
	l := newLayout(t)
	old, _, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Same pid, so the identity check cannot tell them apart; assert the
	// documented behaviour that matters, which is that Release only ever
	// removes a lock that still names the caller as holder.
	writeLock(t, l, LockInfo{PID: old.info.PID + 1, Host: testHost, Time: time.Now().UTC().Format(time.RFC3339)})
	if err := old.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if held, _, err := Held(l); err != nil || !held {
		t.Errorf("Held = %v, %v; want the other holder's lock to survive", held, err)
	}
}

func TestStaleLockOlderThanTheLimitIsBroken(t *testing.T) {
	l := newLayout(t)
	held := time.Now().Add(-StaleLockAge - time.Minute)
	writeLock(t, l, LockInfo{PID: 999999, Host: testHost, Time: held.UTC().Format(time.RFC3339)})

	lk, warnings, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	if !hasWarning(warnings, envelope.WarnStaleLockBroken) {
		t.Fatalf("warnings = %v, want %s", warnings, envelope.WarnStaleLockBroken)
	}
	if lk.info.PID != os.Getpid() {
		t.Errorf("lock pid = %d, want this process %d", lk.info.PID, os.Getpid())
	}
	if lk.info.Time == held.UTC().Format(time.RFC3339) {
		t.Error("the lock still records the stale timestamp")
	}
}

func TestFreshLockIsNotBrokenByAge(t *testing.T) {
	l := newLayout(t)
	writeLock(t, l, LockInfo{PID: os.Getpid(), Host: testHost, Time: time.Now().UTC().Format(time.RFC3339)})

	if _, _, err := acquireAt(l, time.Now(), testHost); codeOf(t, err) != envelope.CodeLockHeld {
		t.Errorf("code = %s, want %s for a live holder", codeOf(t, err), envelope.CodeLockHeld)
	}
}

// A pid on another host cannot be checked from here, so a young lock from
// another host is held. Breaking it would let two machines write at once.
func TestForeignHostLockIsHeldEvenWithADeadLookingPid(t *testing.T) {
	l := newLayout(t)
	writeLock(t, l, LockInfo{PID: 1, Host: "some-other-machine", Time: time.Now().UTC().Format(time.RFC3339)})

	if _, _, err := acquireAt(l, time.Now(), testHost); codeOf(t, err) != envelope.CodeLockHeld {
		t.Errorf("code = %s, want %s", codeOf(t, err), envelope.CodeLockHeld)
	}
}

func TestStaleLockWithADeadPidIsBroken(t *testing.T) {
	l := newLayout(t)
	dead := deadPID(t)
	writeLock(t, l, LockInfo{PID: dead, Host: testHost, Time: time.Now().UTC().Format(time.RFC3339)})

	lk, warnings, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	if !hasWarning(warnings, envelope.WarnStaleLockBroken) {
		t.Fatalf("warnings = %v, want %s", warnings, envelope.WarnStaleLockBroken)
	}
}

// A lock file that cannot be parsed — a truncated write, a foreign tool's file —
// is breakable once it is old enough, or the workspace is wedged with no way out
// but a manual rm. Freshness is what protects the create/write window instead.
func TestUnparsableLockIsHeldWhileFreshAndBrokenOnceOld(t *testing.T) {
	l := newLayout(t)
	if err := EnsureDir(l.StateDir()); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	writeFile(t, l.LockPath(), "")

	// Fresh and unreadable: this is what a lock looks like between O_EXCL and
	// its first write, so it must read as held.
	if _, _, err := acquireAt(l, time.Now(), testHost); codeOf(t, err) != envelope.CodeLockHeld {
		t.Errorf("a fresh unreadable lock: code = %s, want %s", codeOf(t, err), envelope.CodeLockHeld)
	}

	old := time.Now().Add(-StaleLockAge - time.Minute)
	if err := os.Chtimes(l.LockPath(), old, old); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	lk, warnings, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire on an old unreadable lock: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	if !hasWarning(warnings, envelope.WarnStaleLockBroken) {
		t.Fatalf("warnings = %v, want %s", warnings, envelope.WarnStaleLockBroken)
	}
}

func TestStaleLockWarningSaysWhy(t *testing.T) {
	l := newLayout(t)
	held := time.Now().Add(-StaleLockAge - time.Minute)
	writeLock(t, l, LockInfo{PID: 4242, Host: testHost, Time: held.UTC().Format(time.RFC3339)})

	lk, warnings, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	for _, want := range []string{"4242", "over the 15m0s limit"} {
		if !strings.Contains(warnings[0].Message, want) {
			t.Errorf("warning %q does not mention %q", warnings[0].Message, want)
		}
	}
}

func TestLockFileIsOneJSONLine(t *testing.T) {
	l := newLayout(t)
	lk, _, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })

	raw, err := os.ReadFile(lk.Path())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.HasSuffix(string(raw), "}\n") || strings.Count(string(raw), "\n") != 1 {
		t.Errorf("lock file = %q, want one JSON object and one newline", raw)
	}
	var round LockInfo
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.PID != os.Getpid() || round.Host != testHost {
		t.Errorf("round trip = %+v, want pid %d on %s", round, os.Getpid(), testHost)
	}
	if _, err := time.Parse(time.RFC3339, round.Time); err != nil {
		t.Errorf("timestamp %q is not RFC3339: %v", round.Time, err)
	}
}

// A workspace with no .agent/state directory yet must be lockable, because
// init is exactly the command that has to take the lock before it can create
// the directory it lives in.
func TestAcquireCreatesTheStateDirectory(t *testing.T) {
	l := newLayout(t)
	lk, _, err := acquireAt(l, time.Now(), testHost)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	if _, err := os.Stat(l.StateDir()); err != nil {
		t.Errorf("the state directory was not created: %v", err)
	}
}

func TestHeldOnAFreshWorkspace(t *testing.T) {
	l := newLayout(t)
	held, info, err := Held(l)
	if err != nil {
		t.Fatalf("Held: %v", err)
	}
	if held || info != (LockInfo{}) {
		t.Errorf("Held = %v, %+v; want false and a zero info", held, info)
	}
}

func TestPidAliveForThisProcess(t *testing.T) {
	if !pidAlive(os.Getpid()) {
		t.Error("pidAlive reported this very process as dead")
	}
	if pidAlive(0) || pidAlive(-1) {
		t.Error("pidAlive accepted a non-positive pid")
	}
}

func TestHolderDescribesAForeignHost(t *testing.T) {
	got := LockInfo{PID: 7, Host: "other", Time: "2026-09-25T00:00:00Z"}.Holder()
	if !strings.Contains(got, "host other") {
		t.Errorf("Holder() = %q, want it to say the pid is on another host", got)
	}
	if got := (LockInfo{}).Holder(); got != "another ocaw process" {
		t.Errorf("empty Holder() = %q", got)
	}
}

// deadPID returns the pid of a process that has been waited for, so it is
// guaranteed reaped and therefore guaranteed dead on this host.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Skipf("cannot spawn a helper process to derive a dead pid: %v", err)
	}
	pid := cmd.Process.Pid
	if pidAlive(pid) {
		t.Skipf("pid %d is somehow still alive after Wait", pid)
	}
	return pid
}

func TestLockPathsMatchTheLayout(t *testing.T) {
	l := &Layout{Root: "/repo"}
	if want := filepath.Join("/repo", ".agent", "state", "lock"); l.LockPath() != want {
		t.Errorf("LockPath() = %q, want %q", l.LockPath(), want)
	}
}
