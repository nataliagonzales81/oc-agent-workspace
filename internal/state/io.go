package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
)

// Load reads state.json.
//
// Unknown fields are refused. The alternative — decoding what we recognise and
// dropping the rest — means the next save deletes whatever a newer ocaw (or a
// human) put there, and the loss is silent until something it described goes
// missing. Refusing to read a file we cannot fully represent is the same rule
// the YAML subset follows (§9.1).
//
// A missing state.json in an initialised workspace is an error rather than an
// empty state: the workspace claims to exist, so losing its state is damage, not
// a first run.
func Load(l *workspace.Layout) (*State, error) {
	raw, err := workspace.ReadFile(l.StateJSON())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, envelope.Errorf(
				envelope.CodeValidationFailed,
				"ocaw init --dir "+l.Root,
				"no state.json in the initialised workspace at %s", l.StateJSON(),
			)
		}
		return nil, err
	}

	st, derr := Decode(raw)
	if derr != nil {
		return nil, derr
	}
	if verr := st.Validate(); verr != nil {
		return nil, verr
	}
	return st, nil
}

// Save writes state.json atomically and refuses to write a state that would fail
// to load.
//
// The second half matters more than it looks: ocaw holds the single-writer lock
// precisely so two agents cannot interleave, and a write that produced an
// unloadable file would leave the next run with no way to recover except a hand
// edit. Refusing at the door is the whole point of the check.
//
// The state directory is created if missing. WriteFileAtomic deliberately does
// not make parents — a mistyped path should fail loudly rather than appear as a
// tree nobody asked for — so the one directory state.json must live in is
// ensured here, where the layout already says what it is.
func Save(l *workspace.Layout, s *State) error {
	if verr := s.Validate(); verr != nil {
		return verr
	}
	raw, err := s.Marshal()
	if err != nil {
		return envelope.Errorf(
			envelope.CodeInternal,
			"this is an ocaw bug; please report it",
			"cannot encode state.json: %v", err,
		)
	}
	if err := workspace.EnsureDir(l.StateDir()); err != nil {
		return err
	}
	return workspace.WriteFileAtomic(l.StateJSON(), raw)
}

// SaveTo is Save against an explicit path, for a command that writes somewhere
// other than the workspace root.
func SaveTo(path string, s *State) error {
	if verr := s.Validate(); verr != nil {
		return verr
	}
	raw, err := s.Marshal()
	if err != nil {
		return envelope.Errorf(
			envelope.CodeInternal,
			"this is an ocaw bug; please report it",
			"cannot encode state.json: %v", err,
		)
	}
	if err := workspace.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	return workspace.WriteFileAtomic(path, raw)
}

// LoadRuns reads runs.jsonl. A workspace that has never been verified has no
// log, which is empty rather than an error.
func LoadRuns(l *workspace.Layout) ([]Run, error) {
	raw, err := workspace.ReadFile(l.RunsJSONL())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return ParseRuns(raw)
}

// AppendRun adds one record to the log. Appending rather than rewriting is what
// makes the log evidence: an attempt is never edited away, so the file always
// shows what was actually tried.
func AppendRun(l *workspace.Layout, run Run) error {
	line, err := run.Marshal()
	if err != nil {
		return envelope.Errorf(
			envelope.CodeInternal,
			"this is an ocaw bug; please report it",
			"cannot encode a run record: %v", err,
		)
	}
	if err := workspace.EnsureDir(l.StateDir()); err != nil {
		return err
	}
	return workspace.AppendLine(l.RunsJSONL(), string(line))
}

// Decode reads a state document without validating its invariants.
//
// It is separate from Load because the two callers want opposite behaviour. A
// command that is about to write needs the first violation and nothing else —
// it is going to refuse, and a list would be noise. doctor needs all of them,
// because a health check that reports one problem per run is a health check an
// agent has to run seven times.
//
// Unknown fields are refused here, for the same reason Load refuses them.
func Decode(raw []byte) (*State, *Error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s State
	if err := dec.Decode(&s); err != nil {
		return nil, newError(
			envelope.CodeValidationFailed,
			"repair state.json by hand, or re-run ocaw init",
			Detail{},
			"cannot read state.json: %v", err,
		)
	}
	s.normalise()
	return &s, nil
}

// MaxHistoryBytes bounds how much of runs.jsonl a reader will load.
//
// The log is append-only and never rewritten, so it is the one file in the
// workspace whose size is not under ocaw's control: a machine that runs gates
// in a loop for a month accumulates tens of thousands of records. Loading all
// of them to answer "is anything stuck" is work that grows forever for an answer
// that only ever depends on the tail — stuck detection compares consecutive
// trailing attempts, and "the last verification result" is by definition the last
// record.
const MaxHistoryBytes = 1 << 20

// LoadRunsTail reads at most maxBytes from the end of the run log.
//
// A zero maxBytes uses MaxHistoryBytes. The second return value reports whether
// records were dropped, so a caller can say so rather than presenting a truncated
// history as a complete one.
//
// The read starts at the first newline inside the window, so the first parsed
// record is always whole. A record whose first bytes were cut off would otherwise
// parse into a Run with empty task and gate names, and that phantom would be
// indistinguishable from a real record in the stuck comparison.
func LoadRunsTail(l *workspace.Layout, maxBytes int) (runs []Run, truncated bool, err error) {
	if maxBytes <= 0 {
		maxBytes = MaxHistoryBytes
	}
	f, err := os.Open(l.RunsJSONL())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	size := info.Size()
	if size > int64(maxBytes) {
		truncated = true
		if _, err := f.Seek(size-int64(maxBytes), io.SeekStart); err != nil {
			return nil, false, err
		}
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, false, err
	}
	if truncated {
		// Drop the leading partial line, and the newline that ended it.
		if nl := bytes.IndexByte(raw, '\n'); nl >= 0 {
			raw = raw[nl+1:]
		} else {
			// No line boundary in the window at all: one record longer than the
			// budget. Report nothing rather than a fragment.
			return nil, true, nil
		}
	}
	parsed, err := ParseRuns(raw)
	if err != nil {
		return nil, truncated, err
	}
	return parsed, truncated, nil
}
