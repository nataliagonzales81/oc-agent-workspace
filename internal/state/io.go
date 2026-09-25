package state

import (
	"bytes"
	"encoding/json"
	"errors"
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

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var s State
	if err := dec.Decode(&s); err != nil {
		return nil, envelope.Errorf(
			envelope.CodeValidationFailed,
			"repair state.json by hand, or re-run ocaw init",
			"cannot read state.json: %v", err,
		)
	}
	s.normalise()
	if verr := s.Validate(); verr != nil {
		return nil, verr
	}
	return &s, nil
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
