package yaml

import "strings"

// AgentSchema is the schema id written into every agent.yaml.
const AgentSchema = "ocaw/agent@1"

// Field names that other packages read without repeating string literals.
const (
	KeyOcaw   = "ocaw"
	KeySchema = "schema"
)

// Agent is the .agent/agent.yaml manifest (SPEC §5.1).
type Agent struct {
	// Ocaw is the `ocaw: true` marker that records ocaw as the file's writer, so
	// `init --force` knows which files it may overwrite (§5).
	Ocaw    bool
	Schema  string
	Name    string
	Created string
	Project Project
}

// Project records how the project is built and verified (SPEC §5.1). VerifyCmd is
// written once at init or by `ocaw verify detect` and only changed on request; a
// stale auto-detected command is a doctor warning, never a silent rewrite.
type Project struct {
	Type           string
	Root           string
	VerifyCmd      string
	VerifyDetected string
	LintCmd        string
	TestCmd        string
}

// ProjectTypes is the closed set of values project.type may take.
var ProjectTypes = []string{"go", "node", "python", "rust", "jvm", "make", "unknown"}

// Named members of ProjectTypes, so a caller that builds an agent.yaml has
// something to write other than a bare string literal.
const (
	ProjectTypeGo      = "go"
	ProjectTypeNode    = "node"
	ProjectTypePython  = "python"
	ProjectTypeRust    = "rust"
	ProjectTypeJvm     = "jvm"
	ProjectTypeMake    = "make"
	ProjectTypeUnknown = "unknown"
)

// ParseAgent reads an agent.yaml document.
func ParseAgent(src string) (Agent, error) {
	v, err := Parse(src, 1)
	if err != nil {
		return Agent{}, err
	}
	m, err := mustMap(v, "agent.yaml")
	if err != nil {
		return Agent{}, err
	}
	if err := checkKeys(m, "agent.yaml", KeyOcaw, KeySchema, "name", "created", "project"); err != nil {
		return Agent{}, err
	}

	var a Agent
	ocaw, err := boolean(m, KeyOcaw, "agent.yaml")
	if err != nil {
		return Agent{}, err
	}
	a.Ocaw = ocaw

	schema, err := optStr(m, KeySchema, "agent.yaml")
	if err != nil {
		return Agent{}, err
	}
	if schema != "" && schema != AgentSchema {
		return Agent{}, errf(m.Line, "agent.yaml: unsupported schema %q (this build writes %s)", schema, AgentSchema)
	}
	a.Schema = AgentSchema

	if a.Name, err = str(m, "name", "agent.yaml"); err != nil {
		return Agent{}, err
	}
	if a.Created, err = optStr(m, "created", "agent.yaml"); err != nil {
		return Agent{}, err
	}

	pv, ok := m.Get("project")
	if !ok || pv == nil {
		return Agent{}, errf(m.Line, "agent.yaml: missing required field %q", "project")
	}
	pm, err := mustMap(pv, "agent.yaml.project")
	if err != nil {
		return Agent{}, err
	}
	if err := checkKeys(pm, "agent.yaml.project",
		"type", "root", "verify_cmd", "verify_detected", "lint_cmd", "test_cmd"); err != nil {
		return Agent{}, err
	}
	if a.Project.Type, err = enum(pm, "type", "agent.yaml.project", ProjectTypes...); err != nil {
		return Agent{}, err
	}
	for _, f := range []struct {
		key string
		dst *string
	}{
		{"root", &a.Project.Root},
		{"verify_cmd", &a.Project.VerifyCmd},
		{"verify_detected", &a.Project.VerifyDetected},
		{"lint_cmd", &a.Project.LintCmd},
		{"test_cmd", &a.Project.TestCmd},
	} {
		if *f.dst, err = optStr(pm, f.key, "agent.yaml.project"); err != nil {
			return Agent{}, err
		}
	}
	if err := a.Validate(); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// Validate enforces the invariants the writer depends on, so Encode can never
// produce a file that ParseAgent would reject.
func (a Agent) Validate() error {
	if !a.Ocaw {
		return errf(0, "agent.yaml: %q must be true; it marks the file as written by ocaw", KeyOcaw)
	}
	if strings.TrimSpace(a.Name) == "" {
		return errf(0, "agent.yaml: %q must not be empty", "name")
	}
	if !contains(ProjectTypes, a.Project.Type) {
		return errf(0, "agent.yaml.project: %q must be one of: %s (got %q)",
			"type", strings.Join(ProjectTypes, "|"), a.Project.Type)
	}
	return nil
}

// Encode renders the manifest in the §5.1 field order, so a rewritten file keeps
// the same shape a human read once.
func (a Agent) Encode() string {
	var b strings.Builder
	b.WriteString(kvb(0, KeyOcaw, true))
	b.WriteString(kv(0, KeySchema, AgentSchema))
	b.WriteString(kv(0, "name", a.Name))
	b.WriteString(kv(0, "created", a.Created))
	b.WriteString("project:\n")
	b.WriteString(kv(2, "type", a.Project.Type))
	b.WriteString(kv(2, "root", a.Project.Root))
	b.WriteString(kv(2, "verify_cmd", a.Project.VerifyCmd))
	b.WriteString(kv(2, "verify_detected", a.Project.VerifyDetected))
	b.WriteString(kv(2, "lint_cmd", a.Project.LintCmd))
	b.WriteString(kv(2, "test_cmd", a.Project.TestCmd))
	return b.String()
}
