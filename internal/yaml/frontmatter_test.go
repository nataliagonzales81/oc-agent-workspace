package yaml

import (
	"reflect"
	"strings"
	"testing"
)

// The §5.3 fixture, transcribed from SPEC.md.
const frontmatterFixture = `---
name: verify-first
version: 1.0.0
description: "Run verification before claiming done. Triggers on: verify, check, test."
allowed-tools: Bash, Read, Grep
---
`

func TestFrontmatterFixture(t *testing.T) {
	f, err := ParseFrontmatter(frontmatterFixture)
	if err != nil {
		t.Fatalf("ParseFrontmatter = %v; want success", err)
	}
	if f.Name != "verify-first" {
		t.Errorf("Name = %q", f.Name)
	}
	// 1.0.0 is not a number, but a bare 1.0 would be; the subset keeps version as
	// text so a skill's version is never rewritten under it.
	if f.Version != "1.0.0" {
		t.Errorf("Version = %q; want it kept as text", f.Version)
	}
	if f.Description != "Run verification before claiming done. Triggers on: verify, check, test." {
		t.Errorf("Description = %q", f.Description)
	}
	if !reflect.DeepEqual(f.AllowedTools, []string{"Bash", "Read", "Grep"}) {
		t.Errorf("AllowedTools = %v; want the comma list split and trimmed", f.AllowedTools)
	}
	if f.Hidden {
		t.Error("Hidden = true; the fixture does not set it")
	}
}

func TestFrontmatterOptionalFields(t *testing.T) {
	doc := "---\nname: only-a-name\n---\n"
	f, err := ParseFrontmatter(doc)
	if err != nil {
		t.Fatalf("ParseFrontmatter = %v", err)
	}
	if f.Name != "only-a-name" {
		t.Errorf("Name = %q", f.Name)
	}
	if f.Version != "" || f.Description != "" || f.Hidden || len(f.AllowedTools) != 0 {
		t.Errorf("unset optional fields are not zero-valued: %+v", f)
	}
}

func TestFrontmatterHidden(t *testing.T) {
	doc := "---\nname: n\nhidden: true\n---\n"
	f, err := ParseFrontmatter(doc)
	if err != nil {
		t.Fatalf("ParseFrontmatter = %v", err)
	}
	if !f.Hidden {
		t.Error("Hidden = false; want true")
	}
	doc = "---\nname: n\nhidden: false\n---\n"
	f, err = ParseFrontmatter(doc)
	if err != nil {
		t.Fatalf("ParseFrontmatter = %v", err)
	}
	if f.Hidden {
		t.Error("Hidden = true; want an explicit false to be honoured")
	}
}

func TestFrontmatterAllowedToolsVariants(t *testing.T) {
	cases := []struct {
		in   string // the literal value as it appears in the file
		want []string
	}{
		{"\"Bash, Read, Grep\"", []string{"Bash", "Read", "Grep"}},
		{"Bash", []string{"Bash"}},
		{`"  Bash ,  Read  "`, []string{"Bash", "Read"}},
		{`"Bash,,Read"`, []string{"Bash", "Read"}},
		{`""`, nil},
		{`"   "`, nil},
	}
	for _, tc := range cases {
		doc := "---\nname: n\nallowed-tools: " + tc.in + "\n---\n"
		f, err := ParseFrontmatter(doc)
		if err != nil {
			t.Fatalf("ParseFrontmatter(%q) = %v", tc.in, err)
		}
		if !reflect.DeepEqual(f.AllowedTools, tc.want) {
			t.Errorf("allowed-tools %q gave %v; want %v", tc.in, f.AllowedTools, tc.want)
		}
	}
}

// TestFrontmatterReadsOnlyTheLeadingBlock is the requirement the issue calls out.
// A skill body is markdown: it contains horizontal rules, code fences with
// unparseable YAML, and prose with colons. None of it is frontmatter, and treating
// any of it as structure would reject files that are perfectly good skill docs.
func TestFrontmatterReadsOnlyTheLeadingBlock(t *testing.T) {
	doc := frontmatterFixture + `
# verify-first

Runs ` + "`ocaw verify run`" + ` and reports the result.

---
name: not-frontmatter
---

    this: is indented code
    and: {flow: mapping}
  - a bare list item
---
`
	f, err := ParseFrontmatter(doc)
	if err != nil {
		t.Fatalf("ParseFrontmatter = %v; the body after the closing fence must be ignored", err)
	}
	if f.Name != "verify-first" {
		t.Fatalf("Name = %q; want the leading block to win", f.Name)
	}
	if !reflect.DeepEqual(f.AllowedTools, []string{"Bash", "Read", "Grep"}) {
		t.Errorf("AllowedTools = %v", f.AllowedTools)
	}
}

func TestFrontmatterWithEmptyBody(t *testing.T) {
	if _, err := ParseFrontmatter(frontmatterFixture); err != nil {
		t.Fatalf("ParseFrontmatter with no body = %v; want success", err)
	}
}

func TestFrontmatterRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"no opener", "name: n\n---\n", "must begin with a"},
		{"leading blank line", "\n---\nname: n\n---\n", "must begin with a"},
		{"leading prose", "# Title\n\n---\nname: n\n---\n", "must begin with a"},
		{"unterminated", "---\nname: n\n", "unterminated frontmatter"},
		{"empty block", "---\n---\n", "frontmatter is empty"},
		{"comment-only block", "---\n# nothing\n---\n", "expected a mapping"},
		{"missing name", "---\nversion: 1.0.0\n---\n", `missing required field "name"`},
		{"blank name", "---\nname: \"  \"\n---\n", "must not be empty"},
		{"unknown field", "---\nname: n\ncolour: red\n---\n", `unknown field "colour"`},
		{"name is a list", "---\nname:\n  - n\n---\n", "must be a scalar, got a list"},
		{"hidden as yes", "---\nname: n\nhidden: yes\n---\n", "must be true or false"},
		{"allowed-tools as a list", "---\nname: n\nallowed-tools:\n  - Bash\n---\n", "must be a scalar, got a list"},
		{"anchor in block", "---\nname: &x n\n---\n", "anchors are not supported"},
		{"flow map in block", "---\nname: {a: b}\n---\n", "flow mappings are not supported"},
		{"crlf opener", "---\r\nname: n\r\n---\r\n", ""},
		{"bom", "\ufeff---\nname: n\n---\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, err := ParseFrontmatter(tc.doc)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("ParseFrontmatter = %v; want success", err)
				}
				if f.Name != "n" {
					t.Fatalf("Name = %q; want %q", f.Name, "n")
				}
				return
			}
			if err == nil {
				t.Fatalf("ParseFrontmatter succeeded (%+v); want an error naming %q", f, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseFrontmatter error = %q; want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestFrontmatterErrorLinesAreFileLines: a diagnostic that points at line 1 of a
// sub-document sends a human to the wrong place in the file.
func TestFrontmatterErrorLinesAreFileLines(t *testing.T) {
	doc := "---\nname: n\ncolour: red\n---\n"
	_, err := ParseFrontmatter(doc)
	if err == nil {
		t.Fatal("want an error")
	}
	yerr, ok := err.(*Error)
	if !ok {
		t.Fatalf("error is %T; want *yaml.Error", err)
	}
	if yerr.Line != 3 {
		t.Fatalf("line = %d; want 3 (colour is the third line of the file)", yerr.Line)
	}
}
