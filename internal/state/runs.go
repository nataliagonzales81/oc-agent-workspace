package state

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

const (
	// StuckThreshold is how many consecutive attempts with byte-identical
	// output make a gate stuck (SPEC §7). Three is the point at which another
	// identical run is evidence, not progress: something outside the command
	// is keeping it failing.
	StuckThreshold = 3

	// MaxOutputBytes bounds the stored output of one attempt. A gate that
	// prints a megabyte must not be able to grow runs.jsonl without limit
	// (§9.6). The stored prefix is truncated; the hash is of the whole output,
	// so two truncated records still compare honestly.
	MaxOutputBytes = 8 << 10
)

// TruncationMarker ends a stored output that was cut at MaxOutputBytes, so a
// reader never mistakes a prefix for the whole thing.
const TruncationMarker = "\n[ocaw: output truncated]"

// Run is one verification attempt appended to runs.jsonl (SPEC §7). The log is
// append-only: the history of attempts is the only evidence an agent has for why
// a gate keeps failing, so a record is never rewritten or removed.
type Run struct {
	TaskID string `json:"task"`
	Gate   string `json:"gate"`
	// Cmd is the argv that was executed, kept so a history reader can tell
	// "the command changed" from "the command failed differently".
	Cmd       []string   `json:"cmd"`
	Status    GateStatus `json:"status"`
	ExitCode  *int       `json:"exit"`
	At        string     `json:"at"`
	Output    string     `json:"output"`
	Truncated bool       `json:"output_truncated,omitempty"`
	// OutputSHA256 is the hash of the full, untruncated output. Stuck detection
	// compares hashes, so it is unaffected by the stored prefix.
	OutputSHA256 string `json:"output_sha256"`
}

// NewRun builds a run record, hashing and bounding the output.
//
// Output is taken as bytes because the comparison for stuck detection is
// byte-for-byte: two attempts that differ only in trailing whitespace are
// different failures, and text normalisation would hide exactly the flapping
// that makes a gate worth flagging.
func NewRun(taskID, gate string, cmd []string, status GateStatus, exit *int, output []byte, at time.Time) Run {
	prefix := output
	truncated := false
	if len(prefix) > MaxOutputBytes {
		prefix = prefix[:MaxOutputBytes]
		truncated = true
	}
	stored := string(prefix)
	if truncated {
		stored += TruncationMarker
	}
	return Run{
		TaskID:       taskID,
		Gate:         gate,
		Cmd:          cmd,
		Status:       status,
		ExitCode:     exit,
		At:           at.UTC().Format(time.RFC3339),
		Output:       stored,
		Truncated:    truncated,
		OutputSHA256: HashOutput(output),
	}
}

// HashOutput is the digest stuck detection compares. An empty output still
// hashes to a value, so "fails with no output three times" is detectable.
func HashOutput(output []byte) string {
	return fmt.Sprintf("%x", sha256.Sum256(output))
}

// ParseRuns reads runs.jsonl. Every line is one JSON object.
//
// A malformed line is an error that names the line number, never a skipped
// record. Silently dropping a line would erase an attempt from the only log of
// what was tried, and the agent would see a clean history for a gate that
// actually failed several times.
func ParseRuns(raw []byte) ([]Run, error) {
	var out []Run
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var run Run
		if err := json.Unmarshal([]byte(line), &run); err != nil {
			return nil, envelope.Errorf(
				envelope.CodeValidationFailed,
				"repair or remove the malformed line",
				"runs.jsonl line %d is not a valid record: %v", i+1, err,
			)
		}
		if run.Cmd == nil {
			run.Cmd = []string{}
		}
		out = append(out, run)
	}
	return out, nil
}

// Marshal renders one record as a single line, ready to append. The newline is
// included so AppendLine does not have to know the file's shape.
func (r Run) Marshal() ([]byte, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// Stuck is one gate that has stopped changing.
type Stuck struct {
	Task string `json:"task"`
	Gate string `json:"gate"`
	// Since is when the run of identical attempts began.
	Since string `json:"since"`
	// Attempts is how many identical attempts were seen, at least
	// StuckThreshold.
	Attempts int `json:"attempts"`
}

// StuckGates returns the gates whose last StuckThreshold attempts produced
// byte-identical output, sorted by task then gate.
//
// Only a run of consecutive attempts at the end counts. Three identical
// failures separated by a different one is a gate that changed behaviour, which
// is not stuck. The comparison is on the full-output hash, so a re-detected
// command with the same name and different bytes correctly clears the flag.
func StuckGates(runs []Run) []Stuck {
	type attempt struct {
		hash string
		at   string
	}
	history := map[string][]attempt{}
	for _, run := range runs {
		key := run.TaskID + "\x00" + run.Gate
		history[key] = append(history[key], attempt{hash: run.OutputSHA256, at: run.At})
	}

	var out []Stuck
	for key, attempts := range history {
		if len(attempts) < StuckThreshold {
			continue
		}
		tail := attempts[len(attempts)-StuckThreshold:]
		first := tail[0].hash
		same := true
		for _, a := range tail {
			if a.hash != first {
				same = false
				break
			}
		}
		if !same {
			continue
		}
		parts := strings.SplitN(key, "\x00", 2)
		// Count the whole run of identical trailing attempts, so an agent sees
		// how far past the threshold the gate has gone.
		count := StuckThreshold
		for i := len(attempts) - StuckThreshold - 1; i >= 0; i-- {
			if attempts[i].hash != first {
				break
			}
			count++
		}
		out = append(out, Stuck{Task: parts[0], Gate: parts[1], Since: tail[0].at, Attempts: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Task != out[j].Task {
			return out[i].Task < out[j].Task
		}
		return out[i].Gate < out[j].Gate
	})
	return out
}

// StuckTasks reduces StuckGates to the task ids to flag, for `ocaw status` and
// `task next`, which report at task granularity (SPEC §7).
func StuckTasks(runs []Run) map[string]bool {
	out := map[string]bool{}
	for _, s := range StuckGates(runs) {
		out[s.Task] = true
	}
	return out
}
