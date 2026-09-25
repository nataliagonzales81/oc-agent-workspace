package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// StaleLockAge is how old a lock may be before it is presumed abandoned
// (§4.4). A live writer that has held the lock longer than this is unusual
// enough to be worth breaking loudly rather than blocking on.
const StaleLockAge = 15 * time.Minute

// LockInfo is the lock file's entire contents: who holds it and since when.
type LockInfo struct {
	PID  int    `json:"pid"`
	Host string `json:"host"`
	Time string `json:"time"`
}

// Lock is a held single-writer lock. The zero value is not a held lock.
type Lock struct {
	path string
	info LockInfo
}

// Path returns the lock file's path.
func (l *Lock) Path() string { return l.path }

// Info returns what was written to the lock file.
func (l *Lock) Info() LockInfo { return l.info }

// Holder renders the current holder for an error message.
func (i LockInfo) Holder() string {
	if i.PID == 0 || i.Host == "" {
		return "another ocaw process"
	}
	if i.Host == hostname() {
		return fmt.Sprintf("pid %d on %s since %s", i.PID, i.Host, i.Time)
	}
	return fmt.Sprintf("pid %d on host %s since %s", i.PID, i.Host, i.Time)
}

// Acquire takes the single-writer lock, or returns lock_held immediately.
//
// It never waits. An agent that blocks on a lock cannot be interrupted, cannot
// report why, and looks like a hang; failing fast with a code the caller can
// branch on is strictly better (§4.4).
//
// The second return value holds warnings — currently only a broken stale lock.
func Acquire(l *Layout) (*Lock, []envelope.Warning, error) {
	return acquireAt(l, time.Now(), hostname())
}

func acquireAt(l *Layout, now time.Time, host string) (*Lock, []envelope.Warning, error) {
	path := l.LockPath()
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return nil, nil, err
	}

	var warnings []envelope.Warning
	previous, err := inspectLock(path)
	if err != nil {
		return nil, nil, err
	}
	if previous.present {
		reason, stale := staleReason(previous, now, host)
		if !stale {
			return nil, warnings, envelope.NewError(
				envelope.CodeLockHeld,
				"the workspace lock is held by "+previous.info.Holder(),
				"retry once the other ocaw run finishes; ocaw never waits on a lock",
			)
		}
		warnings = append(warnings, envelope.Warning{
			Code: envelope.WarnStaleLockBroken,
			Message: fmt.Sprintf(
				"removed a stale lock held by %s (%s)",
				previous.info.Holder(), reason,
			),
		})
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, warnings, envelope.Errorf(envelope.CodeWriteFailed, "check permissions on "+path, "cannot clear a stale lock: %v", err)
		}
	}

	info := LockInfo{PID: os.Getpid(), Host: host, Time: now.UTC().Format(time.RFC3339)}
	raw, err := json.Marshal(info)
	if err != nil {
		return nil, warnings, err
	}
	raw = append(raw, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, FileMode)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another writer won the race between our read and our create.
			return nil, warnings, envelope.NewError(
				envelope.CodeLockHeld,
				"the workspace lock is held by another ocaw process",
				"retry once the other ocaw run finishes; ocaw never waits on a lock",
			)
		}
		return nil, warnings, envelope.Errorf(envelope.CodeWriteFailed, "check permissions on "+path, "cannot take the workspace lock: %v", err)
	}
	written := false
	defer func() {
		if !written {
			f.Close()
			os.Remove(path)
		}
	}()
	if _, err := f.Write(raw); err != nil {
		return nil, warnings, err
	}
	if err := f.Sync(); err != nil {
		return nil, warnings, err
	}
	if err := f.Close(); err != nil {
		return nil, warnings, err
	}
	written = true
	return &Lock{path: path, info: info}, warnings, nil
}

// Release drops the lock. Removing someone else's lock is prevented by
// comparing the holder recorded on disk: a lock that was broken and retaken
// must not be deleted by the process that lost it.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	current, err := inspectLock(l.path)
	if err != nil {
		return err
	}
	if !current.present || current.info.PID != l.info.PID || current.info.Host != l.info.Host {
		return nil
	}
	if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return envelope.Errorf(envelope.CodeWriteFailed, "check permissions on "+l.path, "cannot release the workspace lock: %v", err)
	}
	return nil
}

// Held reports whether any lock file currently exists, without judging staleness.
func Held(l *Layout) (bool, LockInfo, error) {
	state, err := inspectLock(l.LockPath())
	if err != nil {
		return false, LockInfo{}, err
	}
	return state.present, state.info, nil
}

// lockState is one look at the lock file.
type lockState struct {
	present bool
	info    LockInfo
	// modTime is the file's own timestamp, used as the age reference when the
	// contents cannot supply one.
	modTime time.Time
	// parsed is true when a pid and host were actually read, which is what
	// makes a dead-pid check possible.
	parsed bool
}

// inspectLock reads the lock file. A lock whose contents cannot be parsed is
// still present, but carries no pid, so the only question left for it is age.
//
// The reason it is present-but-unparsed rather than absent is the create/write
// window: a lock taken with O_EXCL exists as an empty file for the microseconds
// before its contents land. Treating that window as "no lock" let a second
// acquirer steal the lock from a live first acquirer.
func inspectLock(path string) (lockState, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return lockState{}, nil
	}
	if err != nil {
		return lockState{}, envelope.Errorf(envelope.CodeInvalidPath, "check permissions on "+path, "cannot read the workspace lock: %v", err)
	}
	state := lockState{present: true}
	if info, err := os.Stat(path); err == nil {
		state.modTime = info.ModTime()
	}
	var info LockInfo
	if err := json.Unmarshal(raw, &info); err != nil {
		return state, nil
	}
	state.info = info
	state.parsed = info.PID > 0 && info.Host != ""
	return state, nil
}

// staleReason decides whether an existing lock may be broken, and says why in
// words that go straight into a warning.
func staleReason(state lockState, now time.Time, host string) (string, bool) {
	if !state.parsed {
		// No pid and no host: nothing can be checked but how old it is. A
		// fresh unreadable lock is a writer mid-write, so it is held.
		if age := now.Sub(state.modTime); age > StaleLockAge {
			return fmt.Sprintf("unreadable and untouched for %s, over the %s limit", age.Round(time.Second), StaleLockAge), true
		}
		return "", false
	}
	held, err := time.Parse(time.RFC3339, state.info.Time)
	if err != nil {
		held = state.modTime
	}
	if age := now.Sub(held); age > StaleLockAge {
		return fmt.Sprintf("held for %s, over the %s limit", age.Round(time.Second), StaleLockAge), true
	}
	if state.info.Host == host && !pidAlive(state.info.PID) {
		return "the holding process is gone", true
	}
	return "", false
}

// pidAlive reports whether a process exists. A permission error means it
// exists and belongs to someone else, which is alive enough to hold a lock.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	return errors.Is(err, os.ErrPermission)
}

func hostname() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		return "unknown"
	}
	return host
}
