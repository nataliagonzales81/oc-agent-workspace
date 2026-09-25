package yaml

import (
	"reflect"
	"strings"
	"testing"
)

// TestAgentFixtureRoundTrip is the §5.1 acceptance criterion: the spec's own
// fixture survives parse and re-encode byte for byte, and a second pass through
// the parser produces an identical value.
func TestAgentFixtureRoundTrip(t *testing.T) {
	a, err := ParseAgent(agentFixture)
	if err != nil {
		t.Fatalf("ParseAgent = %v; want success", err)
	}
	if got := a.Encode(); got != agentFixture {
		t.Fatalf("Encode:\n%s\nwant:\n%s", got, agentFixture)
	}
	again, err := ParseAgent(a.Encode())
	if err != nil {
		t.Fatalf("ParseAgent(Encode) = %v; want success", err)
	}
	if !reflect.DeepEqual(a, again) {
		t.Fatalf("round trip changed the value:\n%+v\n%+v", a, again)
	}
}

func TestAgentFields(t *testing.T) {
	a, err := ParseAgent(agentFixture)
	if err != nil {
		t.Fatalf("ParseAgent = %v", err)
	}
	if !a.Ocaw {
		t.Error("Ocaw = false; the spec fixture marks the file as ocaw-written")
	}
	if a.Schema != AgentSchema {
		t.Errorf("Schema = %q; want %q", a.Schema, AgentSchema)
	}
	if a.Name != "oc-agent-workspace" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.Created != "2026-09-25T00:00:00Z" {
		t.Errorf("Created = %q; timestamps stay text and must survive verbatim", a.Created)
	}
	if a.Project.Type != "go" {
		t.Errorf("Project.Type = %q", a.Project.Type)
	}
	if a.Project.VerifyCmd != "go test ./..." {
		t.Errorf("Project.VerifyCmd = %q; an unquoted value with spaces must round-trip", a.Project.VerifyCmd)
	}
	if a.Project.LintCmd != "go vet ./..." || a.Project.TestCmd != "go test ./..." {
		t.Errorf("Project lint/test = %q/%q", a.Project.LintCmd, a.Project.TestCmd)
	}
}

// TestAgentEncodeIsDeterministic covers SPEC §9.5: the same value must produce
// the same bytes, or every write produces a spurious diff.
func TestAgentEncodeIsDeterministic(t *testing.T) {
	a, err := ParseAgent(agentFixture)
	if err != nil {
		t.Fatalf("ParseAgent = %v", err)
	}
	first := a.Encode()
	for i := 0; i < 5; i++ {
		if got := a.Encode(); got != first {
			t.Fatalf("Encode is not deterministic:\n%q\nthen\n%q", first, got)
		}
	}
}

// TestAgentOptionalFieldsAreAlwaysWritten matters because `init --force` rewrites
// the file. Omitting a field the struct has would make a round trip lossy in a way
// nobody notices until a config value has silently reverted to its default.
func TestAgentOptionalFieldsAreAlwaysWritten(t *testing.T) {
	minimal := `ocaw: true
schema: ocaw/agent@1
name: demo
created: ""
project:
  type: unknown
  root: ""
  verify_cmd: ""
  verify_detected: ""
  lint_cmd: ""
  test_cmd: ""
`
	a, err := ParseAgent(minimal)
	if err != nil {
		t.Fatalf("ParseAgent = %v", err)
	}
	if got := a.Encode(); got != minimal {
		t.Fatalf("Encode:\n%s\nwant:\n%s", got, minimal)
	}
	// A file missing the optional keys entirely must still read, and must be
	// brought up to the canonical shape on the next write.
	sparse := `ocaw: true
name: demo
project:
  type: go
`
	if _, err := ParseAgent(sparse); err != nil {
		t.Fatalf("ParseAgent(sparse) = %v; want success", err)
	}
	full, err := ParseAgent(sparse)
	if err != nil {
		t.Fatalf("ParseAgent = %v", err)
	}
	if !strings.Contains(full.Encode(), "schema: "+AgentSchema) {
		t.Error("Encode did not restore the schema id")
	}
	if !strings.Contains(full.Encode(), "verify_cmd: \"\"\n") {
		t.Errorf("Encode did not write an empty verify_cmd as %q:\n%s", "\"\"", full.Encode())
	}
}

// TestAgentRejects covers the failure paths harder than the happy path, per the
// task notes. Each of these is a manifest ocaw would otherwise write back having
// misread it.
func TestAgentRejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"unknown top-level field", "ocaw: true\nname: d\nproject:\n  type: go\nsurprise: 1\n", `unknown field "surprise"`},
		{"unknown project field", "ocaw: true\nname: d\nproject:\n  type: go\n  wat: 1\n", `unknown field "wat"`},
		{"missing ocaw marker", "name: d\nproject:\n  type: go\n", `missing required field "ocaw"`},
		{"ocaw false", "ocaw: false\nname: d\nproject:\n  type: go\n", `must be true`},
		{"ocaw as yes", "ocaw: yes\nname: d\nproject:\n  type: go\n", "must be true or false"},
		{"ocaw as 1", "ocaw: 1\nname: d\nproject:\n  type: go\n", "must be true or false"},
		{"missing name", "ocaw: true\nproject:\n  type: go\n", `missing required field "name"`},
		{"empty name", "ocaw: true\nname: \"  \"\nproject:\n  type: go\n", `must not be empty`},
		{"missing project", "ocaw: true\nname: d\n", `missing required field "project"`},
		{"project not a map", "ocaw: true\nname: d\nproject: go\n", "expected a mapping, got a scalar"},
		{"missing project type", "ocaw: true\nname: d\nproject:\n  root: /x\n", `missing required field "type"`},
		{"bad project type", "ocaw: true\nname: d\nproject:\n  type: cobol\n", "must be one of: go|node|python|rust|jvm|make|unknown"},
		{"project type is a map", "ocaw: true\nname: d\nproject:\n  type:\n    a: b\n", "must be a scalar, got a mapping"},
		{"verify_cmd is a list", "ocaw: true\nname: d\nproject:\n  type: go\n  verify_cmd: [go, test]\n", "flow sequences are not supported"},
		{"name is a list", "ocaw: true\nname:\n  - d\nproject:\n  type: go\n", "must be a scalar, got a list"},
		{"unsupported schema", "ocaw: true\nschema: ocaw/agent@2\nname: d\nproject:\n  type: go\n", "unsupported schema"},
		{"empty document", "", "expected a mapping"},
		{"comment only", "# nothing here\n", "expected a mapping"},
		{"nested map of maps", "ocaw: true\nname: d\nproject:\n  type: go\n  build:\n    cmd: make\n", `unknown field "build"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAgent(tc.src)
			if err == nil {
				t.Fatalf("ParseAgent succeeded; want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseAgent error = %q; want it to contain %q", err, tc.want)
			}
			var yerr *Error
			if !asError(err, &yerr) {
				t.Fatalf("error is %T; want *yaml.Error", err)
			}
		})
	}
}

// TestAgentValidateRoundTrip proves the writer cannot emit a file its own reader
// rejects. Without this, a future field added to Project but not to Encode would
// be caught by a test, not by a user.
func TestAgentValidateRoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		agent Agent
		want  string
	}{
		{"no marker", Agent{Name: "d", Project: Project{Type: "go"}}, "must be true"},
		{"blank name", Agent{Ocaw: true, Name: " ", Project: Project{Type: "go"}}, "must not be empty"},
		{"bad type", Agent{Ocaw: true, Name: "d", Project: Project{Type: "cobol"}}, "must be one of"},
		{"valid empty", Agent{Ocaw: true, Name: "d", Project: Project{Type: "unknown"}}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.agent.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v; want nil", err)
				}
				if _, perr := ParseAgent(tc.agent.Encode()); perr != nil {
					t.Fatalf("ParseAgent(Encode()) = %v; Encode emitted a file the reader rejects", perr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %v; want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestAgentProjectTypeAcceptsEveryDeclaredValue keeps the enum and the fixtures
// from drifting apart.
func TestAgentProjectTypeAcceptsEveryDeclaredValue(t *testing.T) {
	for _, pt := range ProjectTypes {
		src := "ocaw: true\nname: d\nproject:\n  type: " + pt + "\n"
		a, err := ParseAgent(src)
		if err != nil {
			t.Errorf("ParseAgent with type %q = %v; ProjectTypes and the parser disagree", pt, err)
			continue
		}
		if a.Project.Type != pt {
			t.Errorf("type = %q; want %q", a.Project.Type, pt)
		}
	}
}

// TestAgentEncodeIsAFixedPoint is the real field-loss guard. Every value here is
// awkward in a different way; if any of them failed to survive encode and re-parse
// unchanged, `init --force` would be quietly reverting a manifest field, which is
// the failure this package exists to make impossible.
func TestAgentEncodeIsAFixedPoint(t *testing.T) {
	awkward := []string{
		"plain", "go test ./...", "/abs/path", "2026-09-25T00:00:00Z",
		"1.0.0", "ocaw/agent@1", "has: colon", "ends:", "trailing ", " leading",
		"true", "false", "null", "~", "42", "3.14", "0x1f", "0b101", "1_000",
		"#hash", "a#b", "- dash", "-", "[bracket", "]close", "{brace", "}close",
		"quote\"inside", "back\\slash", "line\nbreak", "tab\there", "carriage\rreturn",
		"Bash, Read, Grep", ".agent/secret", "a: b: c", "'single'", "%percent",
		"@at", "`tick`", "*star", "&amp", "!bang", "|pipe", ">gt", "?question",
		"colon:", "a: ", "\"quoted\"", "'q'", "deep\"er\"quote", "é中文",
	}
	for _, val := range awkward {
		t.Run(strings.ReplaceAll(strings.ReplaceAll(val, "\n", "NL"), "\r", "CR"), func(t *testing.T) {
			// type must stay in the enum, so it is not part of the awkward sweep.
			a := Agent{
				Ocaw:    true,
				Name:    val,
				Created: val,
				Project: Project{Type: "go", Root: val, VerifyCmd: val, VerifyDetected: val, LintCmd: val, TestCmd: val},
			}
			once := a.Encode()
			parsed, err := ParseAgent(once)
			if err != nil {
				t.Fatalf("ParseAgent(Encode()) = %v;\nencoded:\n%s", err, once)
			}
			twice := parsed.Encode()
			if twice != once {
				t.Fatalf("encode is not a fixed point for %q:\nonce:\n%stwice:\n%s", val, once, twice)
			}
			if parsed.Name != val {
				t.Errorf("Name survived as %q; want %q", parsed.Name, val)
			}
			if parsed.Project.VerifyCmd != val {
				t.Errorf("Project.VerifyCmd survived as %q; want %q", parsed.Project.VerifyCmd, val)
			}
		})
	}
}

// TestKnowledgeEncodeIsAFixedPoint applies the same guard to the entry fields,
// where a lost field would delete a fact rather than reset a setting.
func TestKnowledgeEncodeIsAFixedPoint(t *testing.T) {
	awkward := []string{
		"k-0001", "title: with colon", "#not-a-comment", "a#b", "1.0.0",
		"true", "42", "[]", "-", "quote\"inside", "line\nbreak", "tab\there",
	}
	for _, val := range awkward {
		t.Run(strings.ReplaceAll(val, "\n", "NL"), func(t *testing.T) {
			k := KnowledgeIndex{
				Schema: KnowledgeSchema,
				Entries: []KnowledgeEntry{{
					ID: val, Title: val, Kind: KindFact, Anchor: val, Created: val,
					Supersedes: []string{val},
				}},
			}
			once := k.Encode()
			parsed, err := ParseKnowledgeIndex(once)
			if err != nil {
				t.Fatalf("ParseKnowledgeIndex(Encode()) = %v;\nencoded:\n%s", err, once)
			}
			if got := parsed.Encode(); got != once {
				t.Fatalf("encode is not a fixed point for %q:\nonce:\n%stwice:\n%s", val, once, got)
			}
			e := parsed.Entries[0]
			if e.ID != val || e.Title != val || e.Created != val {
				t.Errorf("entry fields changed: %+v", e)
			}
			if !reflect.DeepEqual(e.Supersedes, []string{val}) {
				t.Errorf("Supersedes = %v; want [%q]", e.Supersedes, val)
			}
		})
	}
}

// TestBlankRequiredFieldsAreRefused closes the gap the fixed-point sweep leaves:
// a value the validator forbids must not survive an encode, because that is the one
// case where a round trip could not be made to work.
func TestBlankRequiredFieldsAreRefused(t *testing.T) {
	for _, blank := range []string{"", " ", "\t", "\n"} {
		a := Agent{Ocaw: true, Name: blank, Project: Project{Type: "go"}}
		if err := a.Validate(); err == nil {
			t.Errorf("Validate accepted name %q; want it refused", blank)
		} else if _, perr := ParseAgent(a.Encode()); perr == nil {
			t.Errorf("ParseAgent accepted an encoded blank name %q", blank)
		}
		k := KnowledgeIndex{Schema: KnowledgeSchema, Entries: []KnowledgeEntry{{
			ID: "k-1", Title: "t", Kind: KindFact, Anchor: blank, Created: "2026-01-01T00:00:00Z",
		}}}
		if _, err := ParseKnowledgeIndex(k.Encode()); err == nil {
			t.Errorf("ParseKnowledgeIndex accepted anchor %q; want it refused", blank)
		}
	}
}

// TestTopLevelSequence: no §5 shape is a bare list, but the parser must still
// report one sanely rather than panicking or looping.
func TestTopLevelSequence(t *testing.T) {
	v, err := Parse("- a\n- b\n", 1)
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	want := Seq{"a", "b"}
	if !reflect.DeepEqual(v, want) {
		t.Fatalf("Parse = %#v; want %#v", v, want)
	}
	if _, err := ParseAgent("- a\n"); err == nil {
		t.Error("ParseAgent on a top-level list succeeded; want an error")
	}
	if _, err := ParseKnowledgeIndex("- a\n"); err == nil {
		t.Error("ParseKnowledgeIndex on a top-level list succeeded; want an error")
	}
}
