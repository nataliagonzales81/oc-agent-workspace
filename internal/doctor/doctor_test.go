package doctor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/doctor"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

func run(f *fixture, opts doctor.Options) *doctor.Report {
	f.t.Helper()
	return doctor.Run(f.l, opts)
}

// The baseline. Every later test breaks exactly one thing, so a failure names
// the check responsible rather than "the workspace is unhealthy".
func TestHealthyWorkspaceHasNoFindings(t *testing.T) {
	f := newFixture(t)
	f.initialise()

	rep := run(f, doctor.Options{})
	if len(rep.Findings) != 0 {
		t.Errorf("a healthy workspace reported %d findings:\n%s", len(rep.Findings), messages(rep))
	}
	if rep.HasErrors(false) || rep.HasErrors(true) {
		t.Error("a healthy workspace reports errors")
	}
	c := counts(rep)
	for _, sev := range doctor.Severities {
		if c[string(sev)] != 0 {
			t.Errorf("counts[%s] = %d, want 0", sev, c[string(sev)])
		}
	}
	if rep.VerifyCmd != "go test ./..." {
		t.Errorf("verify_cmd = %q, want it carried into the report", rep.VerifyCmd)
	}
}

// Counts must always carry every key. A caller branching on counts must not
// have to test for absence, which is how half the callers get it wrong.
func TestCountsAlwaysCarryEverySeverity(t *testing.T) {
	f := newFixture(t)
	rep := run(f, doctor.Options{})
	c := counts(rep)
	for _, sev := range doctor.Severities {
		if _, ok := c[string(sev)]; !ok {
			t.Errorf("counts is missing the %q key: %v", sev, c)
		}
	}
}

// doctor exists to report a workspace's whole health in one pass. Reporting one
// problem per run is a health check an agent has to run seven times.
func TestEveryFindingIsReportedNotJustTheFirst(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	// Four unrelated problems, in four different checks.
	if err := os.RemoveAll(f.l.SkillsDir()); err != nil {
		t.Fatal(err)
	}
	f.write(".agent/agent.yaml", "ocaw: true\nschema: ocaw/agent@1\nname: \n")
	f.write(".agent/knowledge/index.yaml",
		"schema: ocaw/knowledge@1\nentries:\n  - id: k1\n    title: gone\n    kind: fact\n    anchor: no/such/file.go\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n")
	f.write(".agent/skills/broken/SKILL.md", "---\nname: not-broken\n---\nbody\n")
	// Break the render hash.
	doc := f.read("WORKFLOW_STATE.md")
	f.write("WORKFLOW_STATE.md", strings.Replace(doc, "## Budget", "## Budget (hand written)", 1))

	rep := run(f, doctor.Options{})
	checks := map[doctor.Check]bool{}
	for _, f := range rep.Findings {
		checks[f.Check] = true
	}
	for _, want := range []doctor.Check{doctor.CheckAgentYAML, doctor.CheckRender, doctor.CheckSkills, doctor.CheckKnowledge} {
		if !checks[want] {
			t.Errorf("no finding from the %q check; got findings from %v", want, keys(checks))
		}
	}
	if len(rep.Findings) < 4 {
		t.Errorf("got %d findings, want at least 4:\n%s", len(rep.Findings), messages(rep))
	}
}

func keys(m map[doctor.Check]bool) []doctor.Check {
	out := make([]doctor.Check, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// AC7, the first half: a hand-edit to the generated view is render_drift.
func TestAC7HandEditIsRenderDrift(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	doc := f.read("WORKFLOW_STATE.md")
	edited := strings.Replace(doc, "| 1 | scaffold |", "| 1 | scaffold (by hand) |", 1)
	if edited == doc {
		// The state is empty, so edit a row that is certainly there.
		edited = strings.Replace(doc, "## Budget", "## Budget (by hand)", 1)
	}
	f.write("WORKFLOW_STATE.md", edited)

	rep := run(f, doctor.Options{})
	if !hasCode(rep, envelope.CodeRenderDrift) {
		t.Errorf("want %s, got:\n%s", envelope.CodeRenderDrift, messages(rep))
	}
	found := false
	for _, finding := range rep.Findings {
		if finding.Code == envelope.CodeRenderDrift {
			found = true
			if finding.Check != doctor.CheckRender {
				t.Errorf("check = %q, want %q", finding.Check, doctor.CheckRender)
			}
			if finding.Hint != "ocaw report --write" {
				t.Errorf("hint = %q, want %q", finding.Hint, "ocaw report --write")
			}
			if finding.Location != "WORKFLOW_STATE.md" {
				t.Errorf("location = %q, want WORKFLOW_STATE.md", finding.Location)
			}
		}
	}
	if !found {
		t.Fatal("no render_drift finding")
	}
}

// AC7, the second half: --fix-safe repairs the drift, and a clean workspace then
// has nothing to report. This is the whole reason --fix-safe exists.
func TestAC7FixSafeRepairsTheDrift(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write("WORKFLOW_STATE.md", strings.Replace(f.read("WORKFLOW_STATE.md"), "## Budget", "## Budget x", 1))

	if rep := run(f, doctor.Options{}); !rep.HasErrors(false) {
		t.Fatalf("the hand-edit was not detected:\n%s", messages(rep))
	}
	// The report describes the workspace as it was found, so the proof that the
	// fix worked is a fresh pass, not this one.
	rep := run(f, doctor.Options{FixSafe: true})
	if len(rep.Fixed) == 0 {
		t.Error("--fix-safe applied nothing and said nothing")
	}
	for _, fixed := range rep.Fixed {
		if !strings.Contains(fixed, "regenerate") {
			t.Errorf("fixed = %q, want it to name the regenerated view", fixed)
		}
	}
	if after := run(f, doctor.Options{}); len(after.Findings) != 0 {
		t.Errorf("a second pass is still reporting:\n%s", messages(after))
	}
}

// AC12: a skill whose declared name does not match its directory is an error,
// because the directory name is the address an agent loads it by.
func TestAC12SkillNameMismatchIsAnError(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write(".agent/skills/verify-first/SKILL.md",
		"---\nname: something-else\ndescription: \"Run the tests.\"\n---\n\nbody\n")

	rep := run(f, doctor.Options{})
	if !hasCode(rep, envelope.CodeValidationFailed) || !hasCheck(rep, doctor.CheckSkills) {
		t.Fatalf("want a validation failure from the skills check:\n%s", messages(rep))
	}
	found := false
	for _, finding := range rep.Findings {
		if finding.Check == doctor.CheckSkills && strings.Contains(finding.Message, "verify-first") {
			found = true
		}
	}
	if !found {
		t.Errorf("the finding does not name the directory:\n%s", messages(rep))
	}
}

// A skill with no description is unreachable in practice: description matching
// is how an agent decides to load one.
func TestSkillWithoutADescriptionIsAnError(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write(".agent/skills/no-desc/SKILL.md", "---\nname: no-desc\nversion: 1.0.0\n---\n\nbody\n")

	rep := run(f, doctor.Options{})
	if !strings.Contains(messages(rep), "description") {
		t.Errorf("a skill with no description is not reported:\n%s", messages(rep))
	}
}

// The subset boundary from §5.3, measured against 90 real third-party files: a
// construct outside the subset is a clear doctor error, not a confusing one.
func TestFrontmatterOutsideTheSubsetIsAClearError(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"folded scalar", "---\nname: folded\ndescription: >-\n  a long\n  description\n---\n", "block scalar"},
		{"flow list", "---\nname: flows\ndescription: x\nallowed-tools: [Bash, Read]\n---\n", ""},
		{"unknown key", "---\nname: extra\ndescription: x\nauthor: someone\n---\n", "author"},
		{"bad boolean", "---\nname: boolish\ndescription: x\nhidden: yes\n---\n", "hidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.initialise()
			f.write(".agent/skills/thing/SKILL.md", tc.body)

			rep := run(f, doctor.Options{})
			if !hasCheck(rep, doctor.CheckSkills) {
				t.Fatalf("no skills finding:\n%s", messages(rep))
			}
			if tc.want != "" && !strings.Contains(messages(rep), tc.want) {
				t.Errorf("the error does not name %q:\n%s", tc.want, messages(rep))
			}
			if !strings.Contains(hints(rep), "§5.3") {
				t.Errorf("the hint does not point at the subset boundary:\n%s", hints(rep))
			}
		})
	}
}

// A well-formed third-party skill is accepted, so the boundary does not read as
// "ocaw refuses skills".
func TestWellFormedSkillIsAccepted(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write(".agent/skills/verify-first/SKILL.md",
		"---\nname: verify-first\nversion: 1.0.0\ndescription: \"Run verification first. Triggers on: verify, check.\"\nallowed-tools: Bash, Read, Grep\n---\n\n# Verify first\n\nAlways run the gate.\n\n---\n\nA horizontal rule in the body must not be parsed.\n")

	rep := run(f, doctor.Options{})
	if hasCheck(rep, doctor.CheckSkills) {
		t.Errorf("a well-formed skill was reported:\n%s", messages(rep))
	}
}

func TestKnowledgeAnchorThatNoLongerExists(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write("docs/design.md", "# design\n")
	f.write(".agent/knowledge/index.yaml",
		"schema: ocaw/knowledge@1\nentries:\n  - id: k1\n    title: kept\n    kind: fact\n    anchor: docs/design.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n  - id: k2\n    title: gone\n    kind: fact\n    anchor: docs/deleted.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n")

	rep := run(f, doctor.Options{})
	if !hasCode(rep, envelope.WarnKnowledgeAnchorGone) {
		t.Fatalf("a dangling anchor was not reported:\n%s", messages(rep))
	}
	// A warning, not an error: the entry may describe something that moved.
	if rep.HasErrors(false) {
		t.Errorf("a dangling anchor must not fail the run:\n%s", messages(rep))
	}
	if !strings.Contains(messages(rep), "k2") {
		t.Errorf("the finding does not name the entry:\n%s", messages(rep))
	}
	if strings.Contains(messages(rep), "k1") {
		t.Errorf("an entry with a live anchor was reported:\n%s", messages(rep))
	}
}

// --fix-safe drops a dead entry, reports it, and leaves the rest of the index
// byte-intact.
func TestFixSafeDropsDeadKnowledgeEntriesAndSaysSo(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write("docs/design.md", "# design\n")
	f.write(".agent/knowledge/index.yaml",
		"schema: ocaw/knowledge@1\nentries:\n  - id: k1\n    title: kept\n    kind: fact\n    anchor: docs/design.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n  - id: k2\n    title: gone\n    kind: fact\n    anchor: docs/deleted.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n")

	rep := run(f, doctor.Options{FixSafe: true})
	if len(rep.Fixed) == 0 {
		t.Fatal("--fix-safe dropped nothing and said nothing")
	}
	after := f.read(".agent/knowledge/index.yaml")
	if strings.Contains(after, "k2") {
		t.Errorf("the dead entry survived:\n%s", after)
	}
	if !strings.Contains(after, "k1") || !strings.Contains(after, "docs/design.md") {
		t.Errorf("the live entry was damaged:\n%s", after)
	}
	// Reported, never silent.
	if !strings.Contains(strings.Join(rep.Fixed, " "), "knowledge") {
		t.Errorf("fixed = %v, want it to name the knowledge fix", rep.Fixed)
	}
	// And the rewritten file must be one ocaw can read back unchanged.
	index, err := yaml.ParseKnowledgeIndex(after)
	if err != nil {
		t.Fatalf("the rewritten index does not parse: %v", err)
	}
	if len(index.Entries) != 1 || index.Entries[0].ID != "k1" {
		t.Errorf("index = %+v, want just k1", index.Entries)
	}
}

// The three things --fix-safe must never touch, checked by content.
func TestFixSafeNeverTouchesTaskDataSkillsOrAgentYAML(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	// A deliberately invalid DAG, written straight to disk because state.Save
	// refuses exactly what this test needs: --fix-safe must leave a broken DAG
	// exactly as broken as it found it.
	f.write(".agent/state/state.json", `{
  "schema": "ocaw/state@1",
  "workflow": {"request": "the request must survive", "scope": "", "constraints": [], "acceptance": []},
  "tasks": [
    {"id": "1", "title": "do a thing", "agent": "", "tier": "", "deps": [], "status": "done", "gates": [], "notes": "", "updated": ""},
    {"id": "2", "title": "running with unmet deps", "agent": "", "tier": "", "deps": ["3"], "status": "in_progress", "gates": [], "notes": "", "updated": ""}
  ],
  "budget": {"max_tokens": 0, "spent_tokens": 0}
}`)
	f.write("WORKFLOW_STATE.md", "# Workflow State\n\nhand written, not ocaw's\n")
	f.write(".agent/skills/keep-me/SKILL.md", "---\nname: wrong-name\n---\nbody\n")

	before := map[string]string{
		"state.json":      f.read(".agent/state/state.json"),
		"agent.yaml":      f.read(".agent/agent.yaml"),
		"SKILL.md":        f.read(".agent/skills/keep-me/SKILL.md"),
		"knowledge index": f.read(".agent/knowledge/index.yaml"),
	}
	rep := run(f, doctor.Options{FixSafe: true})
	if len(rep.Fixed) == 0 {
		t.Fatal("--fix-safe had nothing to report, so this test proves nothing")
	}
	for rel, want := range before {
		if got := f.read(map[string]string{
			"state.json":      ".agent/state/state.json",
			"agent.yaml":      ".agent/agent.yaml",
			"SKILL.md":        ".agent/skills/keep-me/SKILL.md",
			"knowledge index": ".agent/knowledge/index.yaml",
		}[rel]); got != want {
			t.Errorf("--fix-safe modified %s", rel)
		}
	}
}

// The invariant failures come through as findings, each naming the check that
// caught it, and the cycle path is intact.
func TestStateInvariantFailuresBecomeFindings(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write(".agent/state/state.json", `{
  "schema": "ocaw/state@1",
  "workflow": {"request": "", "scope": "", "constraints": [], "acceptance": []},
  "tasks": [
    {"id": "2", "title": "b", "agent": "", "tier": "", "deps": ["4"], "status": "in_progress", "gates": [], "notes": "", "updated": ""},
    {"id": "4", "title": "d", "agent": "", "tier": "", "deps": ["2"], "status": "pending", "gates": [], "notes": "", "updated": ""},
    {"id": "6", "title": "f", "agent": "", "tier": "", "deps": ["99"], "status": "pending", "gates": [], "notes": "", "updated": ""}
  ],
  "budget": {"max_tokens": 0, "spent_tokens": 0}
}`)

	rep := run(f, doctor.Options{})
	if !hasCode(rep, envelope.CodeDepCycle) {
		t.Errorf("the cycle was not reported:\n%s", messages(rep))
	}
	if !hasCode(rep, envelope.CodeDepDangling) {
		t.Errorf("the dangling dep was not reported:\n%s", messages(rep))
	}
	if !hasCode(rep, envelope.CodeDepsUnmet) {
		t.Errorf("in_progress with unmet deps was not reported:\n%s", messages(rep))
	}
	if !strings.Contains(messages(rep), "2 -> 4 -> 2") {
		t.Errorf("the cycle path is missing or reordered:\n%s", messages(rep))
	}
	// Every finding names the check and a location, so a reader can go look.
	for _, finding := range rep.Findings {
		if finding.Check == "" || finding.Location == "" {
			t.Errorf("finding %+v has no check or no location", finding)
		}
		if finding.Severity == doctor.SeverityError && finding.Hint == "" {
			t.Errorf("error finding %+v offers no way to fix it", finding)
		}
	}
}

// --strict changes the exit code without changing what was found.
func TestStrictPromotesWarnings(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write(".agent/knowledge/index.yaml",
		"schema: ocaw/knowledge@1\nentries:\n  - id: k1\n    title: gone\n    kind: fact\n    anchor: nope.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n")

	lenient := run(f, doctor.Options{})
	strict := run(f, doctor.Options{Strict: true})

	if lenient.HasErrors(false) {
		t.Fatalf("a warning must not fail the run without --strict:\n%s", messages(lenient))
	}
	if !strict.HasErrors(true) {
		t.Error("--strict did not promote the warning")
	}
	if len(strict.Findings) != len(lenient.Findings) {
		t.Errorf("--strict changed what was found: %d vs %d findings", len(strict.Findings), len(lenient.Findings))
	}
	if strict.FailingCount(true) != 1 {
		t.Errorf("FailingCount(strict) = %d, want 1", strict.FailingCount(true))
	}
}

// doctor is the command that has to work on a broken workspace.
func TestMissingWorkspaceIsAFindingNotACrash(t *testing.T) {
	f := newFixture(t)
	rep := run(f, doctor.Options{})
	if !hasCheck(rep, doctor.CheckLayout) {
		t.Fatalf("a missing workspace produced no layout finding:\n%s", messages(rep))
	}
	if !hasCode(rep, envelope.CodeWorkspaceMissing) {
		t.Errorf("want %s:\n%s", envelope.CodeWorkspaceMissing, messages(rep))
	}
	// A missing agent.yaml means there is no verify_cmd to check, and the
	// verify check must stay quiet rather than invent a problem.
	if hasCheck(rep, doctor.CheckVerifyCmd) {
		t.Errorf("the verify check invented a finding with no agent.yaml:\n%s", messages(rep))
	}
}

func TestMissingDirectoriesAreReportedIndividually(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	for _, d := range []string{f.l.SkillsDir(), f.l.HooksDir(), f.l.WorkflowRoot()} {
		if err := os.RemoveAll(d); err != nil {
			t.Fatal(err)
		}
	}
	rep := run(f, doctor.Options{})
	layout := rep.Of(doctor.CheckLayout)
	if len(layout) < 3 {
		t.Errorf("got %d layout findings, want at least 3 for three missing directories:\n%s", len(layout), messages(rep))
	}
	for _, finding := range layout {
		if finding.Hint == "" {
			t.Errorf("finding %+v offers no way to fix it", finding)
		}
	}
}

// --fix-safe recreates exactly what was missing and nothing else.
func TestFixSafeCreatesMissingDirectories(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	hooks := f.l.HooksDir()
	if err := os.RemoveAll(hooks); err != nil {
		t.Fatal(err)
	}
	rep := run(f, doctor.Options{FixSafe: true})
	if len(rep.Fixed) == 0 {
		t.Fatal("--fix-safe reported nothing")
	}
	info, err := os.Stat(hooks)
	if err != nil || !info.IsDir() {
		t.Errorf("--fix-safe did not recreate %s: %v", hooks, err)
	}
	after := run(f, doctor.Options{})
	if hasCheck(after, doctor.CheckLayout) {
		t.Errorf("the layout is still reported broken:\n%s", messages(after))
	}
}

// Nothing runs. doctor has to be cheap and safe to call in a loop, and a health
// check that runs the test suite is one nobody calls in a loop.
func TestDoctorNeverRunsTheVerifyCommand(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	marker := filepath.Join(f.dir, "ran.txt")
	f.write("go.mod", "module x\n")
	// A command that would leave evidence if it were ever executed.
	agent := yaml.Agent{
		Ocaw: true, Schema: yaml.AgentSchema, Name: "fixture", Created: "2026-01-01T00:00:00Z",
		Project: yaml.Project{Type: yaml.ProjectTypeGo, Root: f.dir, VerifyCmd: "touch " + marker},
	}
	f.write(".agent/agent.yaml", agent.Encode())

	run(f, doctor.Options{})
	run(f, doctor.Options{Strict: true, FixSafe: true})
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("doctor executed the verify command")
	}
}

// A recorded command whose program is not installed is a warning, not an error:
// §5.1 says so, because the project may have moved to another runner and ocaw
// must not second-guess the human into re-running detection.
func TestStaleVerifyCommandIsAWarning(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	agent := yaml.Agent{
		Ocaw: true, Schema: yaml.AgentSchema, Name: "fixture", Created: "2026-01-01T00:00:00Z",
		Project: yaml.Project{Type: yaml.ProjectTypeMake, Root: f.dir, VerifyCmd: "ocaw-no-such-runner test"},
	}
	f.write(".agent/agent.yaml", agent.Encode())

	rep := run(f, doctor.Options{})
	if !hasCode(rep, envelope.WarnVerifyCmdStale) {
		t.Errorf("a missing program was not reported:\n%s", messages(rep))
	}
	if rep.HasErrors(false) {
		t.Errorf("a stale verify command must not fail the run:\n%s", messages(rep))
	}
	if !strings.Contains(messages(rep), "will not re-detect") {
		t.Errorf("the finding does not explain that ocaw leaves the choice alone:\n%s", messages(rep))
	}
}

// A gate command and a verify_cmd are the same kind of string and are parsed by
// the same parser, so a command one refuses cannot be accepted by the other.
func TestVerifyCmdThatIsNotAnArgvIsAnError(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	agent := yaml.Agent{
		Ocaw: true, Schema: yaml.AgentSchema, Name: "fixture", Created: "2026-01-01T00:00:00Z",
		Project: yaml.Project{Type: yaml.ProjectTypeMake, Root: f.dir, VerifyCmd: "make test && make lint"},
	}
	f.write(".agent/agent.yaml", agent.Encode())

	rep := run(f, doctor.Options{})
	if !hasCode(rep, envelope.CodeValidationFailed) {
		t.Errorf("a shell pipeline in verify_cmd was not an error:\n%s", messages(rep))
	}
	if !strings.Contains(messages(rep), "argv") {
		t.Errorf("the message does not explain the rule:\n%s", messages(rep))
	}
}

// An anchor that climbs out of the project is refused, even though the YAML
// parser's rules do not catch it: `../..` is neither absolute nor inside
// .agent/.
func TestAnchorEscapingTheProjectIsRefused(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write(".agent/knowledge/index.yaml",
		"schema: ocaw/knowledge@1\nentries:\n  - id: k1\n    title: escape\n    kind: fact\n    anchor: ../../etc/hosts\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n")

	rep := run(f, doctor.Options{})
	if !hasCode(rep, envelope.WarnKnowledgeAnchorGone) {
		t.Errorf("an anchor leaving the project was not refused:\n%s", messages(rep))
	}
}

// Findings come out in a fixed order, so two runs of the same broken workspace
// produce byte-identical output and a diff means something changed.
func TestFindingsAreDeterministic(t *testing.T) {
	f := newFixture(t)
	f.initialise()
	f.write(".agent/knowledge/index.yaml",
		"schema: ocaw/knowledge@1\nentries:\n  - id: k2\n    title: b\n    kind: fact\n    anchor: b.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n  - id: k1\n    title: a\n    kind: fact\n    anchor: a.md\n    created: 2026-01-01T00:00:00Z\n    supersedes: []\n")
	f.write(".agent/skills/z/SKILL.md", "---\nname: z\n---\nno description\n")
	f.write(".agent/skills/a/SKILL.md", "---\nname: a\n---\nno description\n")

	first := messages(run(f, doctor.Options{}))
	for range 4 {
		if got := messages(run(f, doctor.Options{})); got != first {
			t.Fatal("two runs of the same broken workspace reported differently")
		}
	}
}

func TestDoctorRunsChecksInSpecOrder(t *testing.T) {
	if len(doctor.Checks) != 7 {
		t.Errorf("Checks has %d entries, want the seven §7 lists", len(doctor.Checks))
	}
	want := []doctor.Check{
		doctor.CheckLayout, doctor.CheckAgentYAML, doctor.CheckState, doctor.CheckRender,
		doctor.CheckSkills, doctor.CheckKnowledge, doctor.CheckVerifyCmd,
	}
	for i, w := range want {
		if doctor.Checks[i] != w {
			t.Errorf("Checks[%d] = %q, want %q", i, doctor.Checks[i], w)
		}
	}
}
