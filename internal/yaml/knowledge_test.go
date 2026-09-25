package yaml

import (
	"reflect"
	"strings"
	"testing"
)

// TestKnowledgeFixtureRoundTrip is the §5.2 acceptance criterion. The fixture
// writes an empty list as `supersedes: []`, so the encoder has to emit that exact
// spelling rather than a bare key.
func TestKnowledgeFixtureRoundTrip(t *testing.T) {
	k, err := ParseKnowledgeIndex(knowledgeFixture)
	if err != nil {
		t.Fatalf("ParseKnowledgeIndex = %v; want success", err)
	}
	if got := k.Encode(); got != knowledgeFixture {
		t.Fatalf("Encode:\n%s\nwant:\n%s", got, knowledgeFixture)
	}
	again, err := ParseKnowledgeIndex(k.Encode())
	if err != nil {
		t.Fatalf("ParseKnowledgeIndex(Encode) = %v; want success", err)
	}
	if !reflect.DeepEqual(k, again) {
		t.Fatalf("round trip changed the value:\n%+v\n%+v", k, again)
	}
	if len(k.Entries) != 1 {
		t.Fatalf("len(Entries) = %d; want 1", len(k.Entries))
	}
	e := k.Entries[0]
	if e.ID != "k-0001" || e.Title != "Verification command is go test" || e.Kind != KindFact {
		t.Errorf("entry = %+v", e)
	}
	if e.Anchor != "Makefile" || e.Created != "2026-09-25T00:00:00Z" {
		t.Errorf("entry = %+v", e)
	}
	if e.Supersedes == nil || len(e.Supersedes) != 0 {
		t.Errorf("Supersedes = %#v; want an empty slice, not nil, so a round trip is stable", e.Supersedes)
	}
}

func TestKnowledgeMultipleEntriesAndSupersedes(t *testing.T) {
	src := `schema: ocaw/knowledge@1
entries:
  - id: k-0002
    title: Bazel replaced make
    kind: decision
    anchor: BUILD.bazel
    created: 2026-09-26T00:00:00Z
    supersedes:
      - k-0001
      - k-0000
  - id: k-0001
    title: Verification command is go test
    kind: fact
    anchor: Makefile
    created: 2026-09-25T00:00:00Z
    supersedes: []
`
	k, err := ParseKnowledgeIndex(src)
	if err != nil {
		t.Fatalf("ParseKnowledgeIndex = %v", err)
	}
	if len(k.Entries) != 2 {
		t.Fatalf("len(Entries) = %d; want 2", len(k.Entries))
	}
	if !reflect.DeepEqual(k.Entries[0].Supersedes, []string{"k-0001", "k-0000"}) {
		t.Errorf("Supersedes = %v; want the block sequence preserved in order", k.Entries[0].Supersedes)
	}
	if got := k.Encode(); got != src {
		t.Fatalf("Encode:\n%s\nwant:\n%s", got, src)
	}
}

func TestKnowledgeEmptyIndex(t *testing.T) {
	src := "schema: ocaw/knowledge@1\nentries: []\n"
	k, err := ParseKnowledgeIndex(src)
	if err != nil {
		t.Fatalf("ParseKnowledgeIndex = %v", err)
	}
	if len(k.Entries) != 0 {
		t.Fatalf("len(Entries) = %d; want 0", len(k.Entries))
	}
	if got := k.Encode(); got != src {
		t.Fatalf("Encode = %q; want %q", got, src)
	}
	// A missing entries key means no entries, not a broken file: init writes the
	// key, but a hand-trimmed index should still read.
	trimmed := "schema: ocaw/knowledge@1\nentries:\n"
	if _, err := ParseKnowledgeIndex(trimmed); err != nil {
		t.Fatalf("ParseKnowledgeIndex(trimmed) = %v; want success", err)
	}
}

func TestKnowledgeRejects(t *testing.T) {
	const head = "schema: ocaw/knowledge@1\nentries:\n"
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"unknown top-level field", "schema: ocaw/knowledge@1\nextra: 1\nentries: []\n", `unknown field "extra"`},
		{"unknown entry field", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor: M\n    colour: red\n", `unknown field "colour"`},
		{"entries not a list", "schema: ocaw/knowledge@1\nentries: none\n", "must be a list"},
		{"entries missing", "schema: ocaw/knowledge@1\n", `missing required field "entries"`},
		{"entry not a map", head + "  - k-1\n", "expected a mapping, got a scalar"},
		{"missing id", head + "  - title: t\n    kind: fact\n    anchor: M\n", `missing required field "id"`},
		{"missing title", head + "  - id: k-1\n    kind: fact\n    anchor: M\n", `missing required field "title"`},
		{"missing kind", head + "  - id: k-1\n    title: t\n    anchor: M\n", `missing required field "kind"`},
		{"missing anchor", head + "  - id: k-1\n    title: t\n    kind: fact\n", `missing required field "anchor"`},
		{"bad kind", head + "  - id: k-1\n    title: t\n    kind: rumour\n    anchor: M\n", "must be one of: fact|decision|gotcha"},
		{"kind is a list", head + "  - id: k-1\n    title: t\n    kind:\n      - fact\n    anchor: M\n", "must be a scalar, got a list"},
		{"anchor absolute", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor: /etc/passwd\n", "must be repo-relative"},
		{"anchor tilde", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor: ~/notes.md\n", "must be repo-relative"},
		{"anchor inside .agent", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor: .agent/MEMORY.md\n", "points inside .agent/"},
		{"anchor traversing into .agent", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor: docs/../.agent/RULES.md\n", "points inside .agent/"},
		{"anchor is a list", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor:\n      - M\n", "must be a scalar, got a list"},
		{"duplicate id", head +
			"  - id: k-1\n    title: a\n    kind: fact\n    anchor: M\n" +
			"  - id: k-1\n    title: b\n    kind: fact\n    anchor: N\n", `duplicate entry id "k-1"`},
		{"supersedes not a list", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor: M\n    supersedes: k-0\n", "must be a list"},
		{"supersedes item is a map", head + "  - id: k-1\n    title: t\n    kind: fact\n    anchor: M\n    supersedes:\n      - id: k-0\n", "item 0 must be a scalar"},
		{"unsupported schema", "schema: ocaw/knowledge@2\nentries: []\n", "unsupported schema"},
		{"empty document", "", "expected a mapping"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseKnowledgeIndex(tc.src)
			if err == nil {
				t.Fatalf("ParseKnowledgeIndex succeeded; want an error naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseKnowledgeIndex error = %q; want it to contain %q", err, tc.want)
			}
			var yerr *Error
			if !asError(err, &yerr) {
				t.Fatalf("error is %T; want *yaml.Error", err)
			}
		})
	}
}

// TestKnowledgeEntryPathIsInErrors: an agent reading the message has to know which
// entry is bad, not just that one of them is.
func TestKnowledgeEntryPathIsInErrors(t *testing.T) {
	src := "schema: ocaw/knowledge@1\nentries:\n" +
		"  - id: k-1\n    title: ok\n    kind: fact\n    anchor: M\n" +
		"  - id: k-2\n    title: bad\n    kind: nonsense\n    anchor: N\n"
	_, err := ParseKnowledgeIndex(src)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "entries[1]") {
		t.Fatalf("error = %q; want it to name entries[1]", err)
	}
	if err.(*Error).Line != 7 {
		t.Fatalf("line = %d; want 7", err.(*Error).Line)
	}
}
