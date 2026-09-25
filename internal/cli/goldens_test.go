package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/schema"
)

// The goldens are the contract. Everything else in this repository describes
// what ocaw is meant to do; these files describe what it actually emits, byte
// for byte, so a field renamed or a key reordered is a test failure rather than a
// surprise for whatever is parsing the output.
//
// AC14 is the whole point. Once this lands, a payload that gains a field is
// still fine, and a payload that loses one, renames one, changes a type, or
// moves a key is a breaking change that has to come with a schema major bump.

// updateGoldens is the same flag as updateSchemas. One flag for both, so
// `go test -update` means "accept every generated artefact in this package" and
// nobody has to remember which is which.
var updateGoldens = updateSchemas

const goldenDir = "../../testdata/goldens"

// goldenCase is one recorded command invocation. A case owns everything that
// makes its output reproducible: the workspace state, the argv, and the exit
// code. A case that needed the surrounding test to arrange something first
// would be a case whose golden breaks the moment the setup is refactored.
type goldenCase struct {
	// name is the file stem under testdata/goldens.
	name string
	// setup builds the workspace. It runs after the directory exists and before
	// the command, and is not recorded.
	setup func(t *testing.T, dir string)
	// argv is the full argument vector, `--dir` included.
	argv []string
	// wantExit is the exit code the golden asserts, so a case cannot record a
	// success and then be re-read as a failure without noticing.
	wantExit int
}

// timestamp matches an RFC3339 instant, with or without fractional seconds and
// with either a Z or a numeric offset. It is deliberately a value-level
// substitution on the rendered JSON rather than a per-field rewrite, so a
// timestamp in a field added tomorrow is covered without a second edit.
var timestamp = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})`)

// normalise replaces the two things that legitimately differ between runs and
// nothing else.
//
// The root is the workspace's own path, which changes on every run because the
// harness runs in a fresh temporary directory. Replacing the exact string is
// safe: no other value in an envelope can contain it. Whitespace, key order and
// spelling are all left alone, because detecting those is the point — a
// pretty-printer change is a diff here.
func normalise(raw []byte, dir string) string {
	s := string(raw)
	if dir != "" {
		s = strings.ReplaceAll(s, dir, "<ROOT>")
	}
	return timestamp.ReplaceAllString(s, "<TS>")
}

func goldenPath(name string) string {
	return filepath.Join(goldenDir, name+".golden")
}

// freshDir is a project ocaw will accept: a marker file is enough, and no git
// repository is required. The goldens do not depend on git because git is not
// part of what they are locking.
func freshDir(t *testing.T, dir string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/golden\n\ngo 1.22\n")
}

func initialised(t *testing.T, dir string) {
	t.Helper()
	freshDir(t, dir)
	mustRunExit(t, dir, initCommandName, 0)
}

func goldenCases(t *testing.T) []goldenCase {
	t.Helper()
	// chain is 1 <- 2 <- 3 with 1 done, so `next` answers 2 and 3 is blocked.
	chain := func(t *testing.T, dir string) {
		initialised(t, dir)
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "1", "--title", "scaffold", "--agent", "coder")
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "2", "--title", "invariants", "--agent", "coder", "--dep", "1")
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "3", "--title", "render", "--agent", "coder", "--dep", "2")
		mustRunExit(t, dir, taskCommandName, 0, "set", "1", "--status", "done")
	}
	// stuck has one task whose gate has failed three times identically, which is
	// what sets `stuck` — the most important thing the payload can say.
	stuck := func(t *testing.T, dir string) {
		initialised(t, dir)
		gate := writeExecutable(t, dir, "gate.sh", "#!/bin/sh\necho 'FAIL: two things are wrong'\nexit 1\n")
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "1", "--title", "broken", "--gate", "test="+gate)
		mustRunExit(t, dir, taskCommandName, 0, "set", "1", "--status", "in_progress")
		// A failing gate exits 4 (AC8), so the setup that manufactures three of
		// them asserts 4. Asserting 0 here would have been asserting the bug.
		for range stuckAfter {
			mustRunExit(t, dir, verifyCommandName, 4, "run", "--force")
		}
	}
	// described is a workspace with prose in every authored section, which is
	// what makes `workflow` and `report` goldens worth having.
	described := func(t *testing.T, dir string) {
		initialised(t, dir)
		mustRunExit(t, dir, workflowCommandName, 0,
			"set",
			"--request", "ship the DAG surface",
			"--scope", "add, set, dep, rm, show, list, next",
			"--constraint", "stdlib only",
			"--constraint", "no network",
			"--accept", "a1=next follows the chain",
			"--accept", "a2=drift is detected",
		)
	}
	// broken deletes state.json behind ocaw's back, so doctor and task both have
	// something real to report.
	broken := func(t *testing.T, dir string) {
		initialised(t, dir)
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "1", "--title", "scaffold")
		if err := os.Remove(filepath.Join(dir, ".agent", "state", "state.json")); err != nil {
			t.Fatal(err)
		}
	}

	return []goldenCase{
		{name: "version", argv: []string{versionCommandName}, wantExit: 0},
		// --json because the default stdout for `schema <key>` is the document,
		// not the envelope. The document is covered by TestEmbeddedSchemasAreCurrent.
		{name: "schema", argv: []string{schemaCommandName, "--json"}, wantExit: 0},
		{name: "schema-task", argv: []string{schemaCommandName, "task", "--json"}, wantExit: 0},

		{name: "init", setup: freshDir, argv: []string{initCommandName}, wantExit: 0},
		{name: "init-rerun", setup: initialised, argv: []string{initCommandName}, wantExit: 0},
		{name: "init-not-a-project", argv: []string{initCommandName}, wantExit: 3},

		{name: "doctor-clean", setup: initialised, argv: []string{doctorCommandName}, wantExit: 0},
		{name: "doctor-findings", setup: broken, argv: []string{doctorCommandName}, wantExit: 4},
		{name: "doctor-uninitialised", setup: freshDir, argv: []string{doctorCommandName}, wantExit: 4},

		{name: "status-empty", setup: initialised, argv: []string{statusCommandName}, wantExit: 0},
		{name: "status-populated", setup: chain, argv: []string{statusCommandName}, wantExit: 0},
		{name: "status-stuck", setup: stuck, argv: []string{statusCommandName}, wantExit: 0},
		{name: "status-task", setup: chain, argv: []string{statusCommandName, "--task", "3"}, wantExit: 0},

		{name: "task-add", setup: initialised, argv: []string{taskCommandName, "add", "--id", "1", "--title", "scaffold", "--agent", "coder"}, wantExit: 0},
		{name: "task-list", setup: chain, argv: []string{taskCommandName, "list"}, wantExit: 0},
		{name: "task-next", setup: chain, argv: []string{taskCommandName, "next"}, wantExit: 0},
		{name: "task-show", setup: chain, argv: []string{taskCommandName, "show", "3"}, wantExit: 0},
		{name: "task-not-found", setup: chain, argv: []string{taskCommandName, "show", "nope"}, wantExit: 7},
		{name: "task-deps-unmet", setup: chain, argv: []string{taskCommandName, "set", "3", "--status", "in_progress"}, wantExit: 5},
		{name: "task-dry-run", setup: chain, argv: []string{taskCommandName, "add", "--id", "4", "--title", "fourth", "--dry-run"}, wantExit: 0},

		{name: "verify-run-pass", setup: passingGate(t), argv: []string{verifyCommandName, "run"}, wantExit: 0},
		{name: "verify-run-fail", setup: failingGate(t), argv: []string{verifyCommandName, "run"}, wantExit: 4},
		{name: "verify-run-timeout", setup: timingOutGate(t), argv: []string{verifyCommandName, "run", "--timeout", "1"}, wantExit: 4},
		{name: "verify-history", setup: twoFailedRuns(t), argv: []string{verifyCommandName, "history"}, wantExit: 0},

		{name: "workflow-set", setup: initialised, argv: []string{workflowCommandName, "set", "--request", "ship it", "--constraint", "stdlib only", "--accept", "the DAG is validated"}, wantExit: 0},
		{name: "workflow-accept", setup: described, argv: []string{workflowCommandName, "accept", "a1"}, wantExit: 0},
		{name: "workflow-show", setup: described, argv: []string{workflowCommandName, "show"}, wantExit: 0},

		{name: "report", setup: described, argv: []string{reportCommandName}, wantExit: 0},
		{name: "report-write", setup: described, argv: []string{reportCommandName, "--write"}, wantExit: 0},

		{name: "usage-unknown-command", setup: initialised, argv: []string{"bogus"}, wantExit: 2},
		{name: "usage-no-command", setup: initialised, argv: nil, wantExit: 2},
		{name: "task-uninitialised", setup: freshDir, argv: []string{taskCommandName, "list"}, wantExit: 3},
	}
}

// stuckAfter is the attempt count that makes a gate stuck. state.StuckThreshold
// is 3, and a golden that depends on that number is a golden that breaks when it
// changes — which is correct, since the number is part of the contract.
const stuckAfter = 3

func passingGate(t *testing.T) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		initialised(t, dir)
		gate := writeExecutable(t, dir, "gate.sh", "#!/bin/sh\necho 'ok'\n")
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "1", "--title", "scaffold", "--gate", "test="+gate)
	}
}

func failingGate(t *testing.T) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		initialised(t, dir)
		gate := writeExecutable(t, dir, "gate.sh", "#!/bin/sh\necho 'FAIL: two things are wrong'\nexit 1\n")
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "1", "--title", "scaffold", "--gate", "test="+gate)
	}
}

func timingOutGate(t *testing.T) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		initialised(t, dir)
		gate := writeExecutable(t, dir, "gate.sh", "#!/bin/sh\nsleep 30\n")
		mustRunExit(t, dir, taskCommandName, 0, "add", "--id", "1", "--title", "slow", "--gate", "test="+gate)
	}
}

func twoFailedRuns(t *testing.T) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		failingGate(t)(t, dir)
		mustRunExit(t, dir, verifyCommandName, 4, "run")
		mustRunExit(t, dir, verifyCommandName, 4, "run", "--force")
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeExecutable puts a gate script inside the workspace rather than in a
// separate temporary directory, so the path the gate records in the run log
// normalises through the same <ROOT> substitution as everything else. A script
// outside the workspace would put an unnormalised path into two goldens.
func writeExecutable(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRunExit(t *testing.T, dir, command string, wantExit int, rest ...string) {
	t.Helper()
	var out, errOut bytes.Buffer
	argv := append([]string{"--dir", dir, command}, rest...)
	if code := Main(argv, &out, &errOut, false); code != wantExit {
		t.Fatalf("%s %v: exit %d, want %d\n%s", command, rest, code, wantExit, out.String())
	}
}

// runGolden executes a case and returns the normalised stdout.
func runGolden(t *testing.T, c goldenCase) (string, int) {
	t.Helper()
	dir := t.TempDir()
	if c.setup != nil {
		c.setup(t, dir)
	}
	var out, errOut bytes.Buffer
	argv := append(append([]string{"--dir", dir}, "--json"), c.argv...)
	// --json is explicit: a golden is a JSON contract, and a golden that changed
	// shape because stdout happened to be a terminal would be a golden nobody can
	// rely on.
	code := Main(argv, &out, &errOut, false)
	raw := out.Bytes()

	// The one-line rule, checked here rather than in a separate test, because it
	// is a property of the same bytes: anything that breaks it breaks a golden
	// too, but only a test that says so names the reason.
	if c.wantExit >= 0 {
		trimmed := bytes.TrimRight(raw, "\n")
		if n := bytes.Count(trimmed, []byte("\n")); n != 0 {
			t.Errorf("%s: stdout is %d lines, want exactly 1:\n%s", c.name, n+1, trimmed)
		}
		var env envelope.Envelope
		if err := json.Unmarshal(trimmed, &env); err != nil {
			t.Errorf("%s: stdout is not a JSON envelope: %v\n%s", c.name, err, trimmed)
		}
	}
	return normalise(raw, dir), code
}

func TestGoldens(t *testing.T) {
	cases := goldenCases(t)
	if *updateGoldens {
		if err := os.MkdirAll(goldenDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			// A case whose setup is missing would be recorded as an empty file
			// and then pass, so a nil setup is an error rather than a default.
			if c.argv == nil && c.name != "usage-no-command" {
				t.Errorf("%s has no argv", c.name)
			}
			got, code := runGolden(t, c)
			if code != c.wantExit {
				t.Errorf("%s: exit %d, want %d\n%s", c.name, code, c.wantExit, got)
			}
			if err := os.WriteFile(goldenPath(c.name), []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("wrote %d goldens to %s", len(cases), goldenDir)
		return
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want, err := os.ReadFile(goldenPath(c.name))
			if err != nil {
				t.Fatalf("no golden for %q: %v\nregenerate with: go test ./internal/cli/ -run TestGoldens -update", c.name, err)
			}
			got, code := runGolden(t, c)
			if code != c.wantExit {
				t.Errorf("exit %d, want %d\n%s", code, c.wantExit, got)
			}
			if got != string(want) {
				t.Errorf("output does not match %s.\n\n got: %s\nwant: %s\n\nif this change is intended:\n  go test ./internal/cli/ -run TestGoldens -update",
					goldenPath(c.name), got, want)
			}
		})
	}
}

// Every command in §7 has a golden. A new command ships without one only if
// someone adds it to this list, which is the point: the list is the review.
func TestEveryCommandHasAGolden(t *testing.T) {
	covered := make(map[string]bool)
	for _, c := range goldenCases(t) {
		if len(c.argv) == 0 {
			covered["ocaw"] = true
			continue
		}
		covered[c.argv[0]] = true
	}
	for _, name := range commandNames() {
		if !covered[name] {
			t.Errorf("no golden covers %q; add a case to goldenCases", name)
		}
	}
}

// Each golden validates against the embedded schema for its own command, so the
// schema in task #12 and the bytes here cannot drift apart. A schema that
// describes a different shape than the tool emits is the failure this catches.
func TestGoldensValidateAgainstTheirSchemas(t *testing.T) {
	for _, c := range goldenCases(t) {
		if len(c.argv) == 0 {
			continue
		}
		if commandByName(c.argv[0]) == nil {
			// A case for a command that does not exist, such as the unknown-command
			// refusal. There is nothing to validate against, and inventing a
			// schema for it would be the wrong answer.
			continue
		}
		doc, found := embeddedSchema(c.argv[0])
		if !found {
			t.Errorf("%s: no embedded schema for command %q", c.name, c.argv[0])
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			got, _ := runGolden(t, c)
			var env envelope.Envelope
			if err := json.Unmarshal([]byte(got), &env); err != nil {
				t.Fatalf("golden is not an envelope: %v", err)
			}
			data, err := json.Marshal(env.Data)
			if err != nil {
				t.Fatal(err)
			}
			ok, skipped, err := schema.Validate(doc, data)
			if err != nil {
				t.Fatalf("validation failed: %v", err)
			}
			if !ok {
				t.Errorf("data does not validate against %s", c.argv[0])
			}
			if len(skipped) > 0 {
				t.Logf("keywords not interpreted: %v", skipped)
			}
		})
	}
}

// Regeneration has to be deterministic, or the harness is a source of noise
// rather than a source of truth. Two -update runs must produce identical bytes.
func TestGoldenRegenerationIsDeterministic(t *testing.T) {
	cases := goldenCases(t)
	first := make(map[string]string, len(cases))
	for _, c := range cases {
		got, _ := runGolden(t, c)
		first[c.name] = got
	}
	for _, c := range cases {
		got, code := runGolden(t, c)
		if code != c.wantExit {
			t.Errorf("%s: exit %d on the second run, want %d", c.name, code, c.wantExit)
		}
		if got != first[c.name] {
			t.Errorf("%s is not deterministic across runs.\nfirst: %s\nsecond: %s", c.name, first[c.name], got)
		}
	}
}

// The goldens are stricter than the schemas, on purpose.
//
// A JSON Schema with `additionalProperties: true` is what a *consumer* of a
// released payload wants: a newer ocaw that added an optional field should not
// be rejected by an older schema. The goldens are the opposite, because the
// goldens are the *producer*'s side. A new field is a decision someone makes, and
// regenerating the goldens is how that decision gets reviewed — so adding a
// field is caught here, deliberately, and the fix is to run -update and read the
// diff.
//
// Measured on this harness, by breaking things on purpose and counting the
// goldens that failed:
//
//	rename a field's json tag          detected  (4)
//	reorder the envelope's keys        detected  (32)
//	drop a key (make it omitempty)     detected  (6)
//	add an optional key                detected  (8)  — deliberate, see above
//	change a heading in the markdown   detected  (2)
//
// Two of those took a second attempt, and both of the failures were in the
// probe rather than in the harness: a change that does not compile produces a
// build error, which looks exactly like "nothing was detected" if the probe only
// greps for golden mismatches. Worth recording, because a probe that reports
// "not detected" for a build failure is worse than no probe.
func TestGoldensCatchBreakingChanges(t *testing.T) {
	// This test cannot mutate the source it is testing, so what it can assert is
	// the property that makes mutation detectable in the first place: the
	// comparison is on the whole rendered line, not on a subset of it.
	got, _ := runGolden(t, goldenCase{
		name:     "probe",
		setup:    initialised,
		argv:     []string{statusCommandName},
		wantExit: 0,
	})
	// A single space difference anywhere must change the result, because a
	// comparison that tolerates whitespace is a comparison that tolerates a
	// pretty-printer rewrite.
	altered := strings.Replace(got, `"total":`, `"total": `, 1)
	if altered == got {
		t.Fatal("adding a space changed nothing, so the comparison cannot be byte-for-byte")
	}
}
