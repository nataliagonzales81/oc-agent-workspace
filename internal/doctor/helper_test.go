package doctor_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/doctor"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

// A workspace built by init, because doctor is a statement about a real one and
// a hand-rolled fixture would drift from what init actually produces.
type fixture struct {
	t   *testing.T
	dir string
	l   *workspace.Layout
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, dir: dir, l: &workspace.Layout{Root: dir}}
}

// initialise builds the same tree init builds, without going through the CLI, so
// these tests do not depend on task #6 staying correct.
func (f *fixture) initialise() {
	f.t.Helper()
	if err := workspace.EnsureDirs(f.l.Dirs()); err != nil {
		f.t.Fatal(err)
	}
	agent := yaml.Agent{
		Ocaw: true, Schema: yaml.AgentSchema, Name: "fixture", Created: "2026-01-01T00:00:00Z",
		Project: yaml.Project{
			Type: yaml.ProjectTypeGo, Root: f.dir, VerifyCmd: "go test ./...",
			VerifyDetected: "2026-01-01T00:00:00Z", LintCmd: "go vet ./...", TestCmd: "go test ./...",
		},
	}
	f.write(".agent/agent.yaml", agent.Encode())
	f.write(".agent/RULES.md", "rules\n")
	f.write(".agent/MEMORY.md", "memory\n")
	f.write(".agent/KNOWLEDGE.md", "knowledge\n")
	f.write(".agent/knowledge/index.yaml",
		(yaml.KnowledgeIndex{Schema: yaml.KnowledgeSchema, Entries: []yaml.KnowledgeEntry{}}).Encode())
	f.save(state.New())
}

func (f *fixture) write(rel, body string) {
	f.t.Helper()
	path := filepath.Join(f.dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, filepath.FromSlash(rel)))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(raw)
}

func (f *fixture) save(st *state.State) {
	f.t.Helper()
	if err := state.Save(f.l, st); err != nil {
		f.t.Fatal(err)
	}
	if _, err := report.Write(f.l, st); err != nil {
		f.t.Fatal(err)
	}
}

func counts(rep *doctor.Report) map[string]int { return rep.Counts() }

func errorFindings(rep *doctor.Report) []doctor.Finding {
	var out []doctor.Finding
	for _, f := range rep.Findings {
		if f.Severity == doctor.SeverityError {
			out = append(out, f)
		}
	}
	return out
}

func warningFindings(rep *doctor.Report) []doctor.Finding {
	var out []doctor.Finding
	for _, f := range rep.Findings {
		if f.Severity == doctor.SeverityWarning {
			out = append(out, f)
		}
	}
	return out
}

func hasCode(rep *doctor.Report, code envelope.Code) bool {
	for _, f := range rep.Findings {
		if f.Code == code {
			return true
		}
	}
	return false
}

func hasCheck(rep *doctor.Report, check doctor.Check) bool {
	for _, f := range rep.Findings {
		if f.Check == check {
			return true
		}
	}
	return false
}

func messages(rep *doctor.Report) string {
	var b strings.Builder
	for _, f := range rep.Findings {
		b.WriteString(f.Message)
		b.WriteString("\n")
	}
	return b.String()
}

func now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func hints(rep *doctor.Report) string {
	var b strings.Builder
	for _, f := range rep.Findings {
		b.WriteString(f.Hint)
		b.WriteString("\n")
	}
	return b.String()
}
