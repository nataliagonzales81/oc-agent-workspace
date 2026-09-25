package yaml

import (
	"reflect"
	"strings"
	"testing"
)

// The §5 fixtures are transcribed from SPEC.md. They are the contract this
// package has to survive, so a change to one of them is a spec change, not a
// test fix.
const agentFixture = `ocaw: true
schema: ocaw/agent@1
name: oc-agent-workspace
created: 2026-09-25T00:00:00Z
project:
  type: go
  root: /abs/path
  verify_cmd: go test ./...
  verify_detected: 2026-09-25T00:00:00Z
  lint_cmd: go vet ./...
  test_cmd: go test ./...
`

const knowledgeFixture = `schema: ocaw/knowledge@1
entries:
  - id: k-0001
    title: Verification command is go test
    kind: fact
    anchor: Makefile
    created: 2026-09-25T00:00:00Z
    supersedes: []
`

// TestUnsupportedConstructsFail is the reason this package exists. Every entry
// here is valid YAML that a full parser would accept and this subset must refuse,
// because ocaw writes these files back and a silently dropped field becomes a
// silently deleted key.
func TestUnsupportedConstructsFail(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // substring the error must name
	}{
		{"anchor", "a: &x 1\nb: *x\n", "anchors are not supported"},
		{"anchor in sequence", "l:\n  - &x go\n", "anchors are not supported"},
		{"alias", "b: *x\n", "aliases are not supported"},
		{"merge key", "a: 1\n<<: b\n", "merge keys are not supported"},
		{"directive", "%YAML 1.2\na: 1\n", "directives are not supported"},
		{"multi document", "---\na: 1\n", "multi-document streams are not supported"},
		{"document end", "a: 1\n...\n", "document end markers are not supported"},
		{"flow mapping", "a: {b: c}\n", "flow mappings are not supported"},
		{"flow mapping nested", "a:\n  b: {c: d}\n", "flow mappings are not supported"},
		{"flow sequence", "a: [b, c]\n", "flow sequences are not supported"},
		{"complex key", "? a\n: b\n", "complex mapping keys are not supported"},
		{"explicit tag", "a: !!str 1\n", "explicit tags are not supported"},
		{"explicit tag in nested", "a:\n  b: !custom v\n", "explicit tags are not supported"},
		{"tab indent", "a:\n\tb: c\n", "tab in indentation is not supported"},
		{"tab indent in sequence", "l:\n  -\tb: c\n", "tab in indentation is not supported"},
		{"duplicate key", "a: 1\na: 2\n", `duplicate key "a"`},
		{"duplicate key quoted", "\"a\": 1\na: 2\n", `duplicate key "a"`},
		{"unquoted colon in value", "a: b: c\n", "unquoted value contains"},
		{"nested sequence", "l:\n  - - a\n", "nested sequences are not supported"},
		{"unterminated double quote", "a: \"oops\n", "unterminated double-quoted string"},
		{"unterminated single quote", "a: 'oops\n", "unterminated single-quoted string"},
		{"trailing after quote", "a: \"oops\" junk\n", "unexpected"},
		{"unknown escape", "a: \"b\\qc\"\n", "unsupported escape"},
		{"bare scalar as key", "just-a-scalar\n", "expected a"},
		{"ragged indent", "a:\n  b: 1\n   c: 2\n", "unexpected indentation"},
		{"bad dedent", "a:\n  b: 1\n c: 2\n", "unexpected indentation"},
		// A block scalar is a construct rather than a shape, so it is refused. It
		// must be named: left unnamed, the indented continuation lines surface
		// later as a bare indentation error pointing at the wrong line.
		{"folded block scalar", "a: >-\n  text\n", "block scalars are not supported"},
		{"folded block scalar plain", "a: >\n  text\n", "block scalars are not supported"},
		{"literal block scalar", "a: |\n  text\n", "block scalars are not supported"},
		{"literal block scalar strip", "a: |-\n  text\n", "block scalars are not supported"},
		{"indented block scalar", "a: >2\n   text\n", "block scalars are not supported"},
		{"stray sequence after a value", "a: 1\n- two\n", "unexpected content"},
		{"stray sequence in nested map", "a:\n  b: 1\n  - two\n", "unexpected indentation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src, 1)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded; want an error naming %q", tc.src, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse(%q) error = %q; want it to contain %q", tc.src, err, tc.want)
			}
			var yerr *Error
			if !asError(err, &yerr) {
				t.Fatalf("Parse(%q) returned %T; want *yaml.Error so callers can branch on it", tc.src, err)
			}
		})
	}
}

func asError(err error, target **Error) bool {
	e, ok := err.(*Error)
	if ok {
		*target = e
	}
	return ok
}

func TestSupportedShapes(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want any
	}{
		{"scalars", "s: text\ni: 42\nb: true\nn:\n", mustMapOf(t,
			"s", "text", "i", "42", "b", "true", "n", nil)},
		{"nested map", "a:\n  b:\n    c: d\n", mapAt(t, "a", mapAt(t, "b", mustMapOf(t, "c", "d")))},
		{"empty flow seq", "a: []\n", mustMapOf(t, "a", Seq{})},
		{"block seq of scalars", "l:\n  - one\n  - two\n", mustMapOf(t, "l", Seq{"one", "two"})},
		{"block seq of maps", "l:\n  - a: 1\n    b: 2\n  - a: 3\n    b: 4\n", mustMapOf(t, "l", Seq{
			mustMapOf(t, "a", "1", "b", "2"),
			mustMapOf(t, "a", "3", "b", "4"),
		})},
		{"lone dash with block", "l:\n  -\n    a: 1\n", mustMapOf(t, "l", Seq{mustMapOf(t, "a", "1")})},
		{"comments", "# lead\na: 1 # trailing\n# tail\nb: 2\n", mustMapOf(t, "a", "1", "b", "2")},
		{"comment inside quotes", "a: \"has # inside\"\n", mustMapOf(t, "a", "has # inside")},
		{"single quotes", "a: 'it''s here'\n", mustMapOf(t, "a", "it's here")},
		{"escapes", "a: \"x\\ty\\nz\"\n", mustMapOf(t, "a", "x\ty\nz")},
		{"quoted key", "\"a b\": 1\n", mustMapOf(t, "a b", "1")},
		{"quoted empty value", "a: \"\"\n", mustMapOf(t, "a", "")},
		{"crlf", "a: 1\r\nb: 2\r\n", mustMapOf(t, "a", "1", "b", "2")},
		{"leading doc comment", "# only comments\n\n", nil},
		{"empty document", "", nil},
		{"whitespace only", "   \n\t\n", nil},
		{"deeper dash indent", "l:\n    - a: 1\n      b: 2\n", mustMapOf(t, "l", Seq{mustMapOf(t, "a", "1", "b", "2")})},
		// A block sequence may sit at its key's own column. This is the ordinary
		// spelling for a list of scalars and is not an exotic construct, so the
		// subset has to accept it; refusing it broke a large share of real
		// frontmatter on this machine.
		{"sequence at key column", "l:\n- one\n- two\n", mustMapOf(t, "l", Seq{"one", "two"})},
		{"sequence of maps at key column", "l:\n- a: 1\n  b: 2\n- a: 3\n  b: 4\n", mustMapOf(t, "l", Seq{
			mustMapOf(t, "a", "1", "b", "2"),
			mustMapOf(t, "a", "3", "b", "4"),
		})},
		{"sequence at key column then key", "l:\n- one\na: 2\nm: 3\n", mustMapOf(t, "l", Seq{"one"}, "a", "2", "m", "3")},
		{"indented sequence still wins", "a: 1\nl:\n  - x\nb: 2\n", mustMapOf(t, "a", "1", "l", Seq{"x"}, "b", "2")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.src, 1)
			if err != nil {
				t.Fatalf("Parse(%q) = %v; want success", tc.src, err)
			}
			if !reflect.DeepEqual(zeroLines(got), zeroLines(tc.want)) {
				t.Fatalf("Parse(%q) = %#v; want %#v", tc.src, got, tc.want)
			}
		})
	}
}

// TestErrorLineNumbers checks the diagnostic is actionable: an agent reading the
// message has to know which line to open.
func TestErrorLineNumbers(t *testing.T) {
	cases := []struct {
		src      string
		wantLine int
		wantMsg  string
	}{
		{"a: 1\nb: 2\na: 3\n", 3, "duplicate key"},
		{"a: 1\n  b: 2\n", 2, "unexpected indentation"},
		{"a: 1\nb: \"x\n", 2, "unterminated"},
		{"a: {b: c}\n", 1, "flow mappings"},
		{"\n\n\na: [1]\n", 4, "flow sequences"},
	}
	for _, tc := range cases {
		_, err := Parse(tc.src, 1)
		if err == nil {
			t.Fatalf("Parse(%q) succeeded; want an error", tc.src)
		}
		var yerr *Error
		if !asError(err, &yerr) {
			t.Fatalf("Parse(%q) returned %T; want *yaml.Error", tc.src, err)
		}
		if yerr.Line != tc.wantLine {
			t.Errorf("Parse(%q) line = %d; want %d (%v)", tc.src, yerr.Line, tc.wantLine, err)
		}
		if !strings.Contains(yerr.Msg, tc.wantMsg) {
			t.Errorf("Parse(%q) msg = %q; want it to contain %q", tc.src, yerr.Msg, tc.wantMsg)
		}
	}
}

// TestParseFirstLineOffset proves the offset plumbing: a caller that parsed a
// sub-block still gets real file line numbers.
func TestParseFirstLineOffset(t *testing.T) {
	block := "a: 1\na: 2\n"
	_, err := Parse(block, 12)
	var yerr *Error
	if !asError(err, &yerr) {
		t.Fatalf("Parse returned %T; want *yaml.Error", err)
	}
	if yerr.Line != 13 {
		t.Fatalf("line = %d; want 13 (block line 2 at offset 12)", yerr.Line)
	}
}

// TestQuoteRoundTrip is the property the writer exists to satisfy: anything the
// writer quotes must read back as the identical string, or ocaw would corrupt a
// value on the first rewrite.
func TestQuoteRoundTrip(t *testing.T) {
	values := []string{
		"", " ", "plain", "go test ./...", "/abs/path", "2026-09-25T00:00:00Z",
		"1.0.0", "ocaw/agent@1", "has: colon", "ends:", "trailing ", " leading",
		"true", "false", "null", "yes", "on", "42", "3.14", "0x1f", "#hash",
		"a#b", "- dash", "[bracket", "{brace", "quote\"inside", "back\\slash",
		"line\nbreak", "tab\there", "Bash, Read, Grep", "k-0001", ".agent/secret",
		"a: b: c", "'single'", "%percent", "@at", "`tick`", "*star", "&amp",
	}
	for _, v := range values {
		t.Run(strings.ReplaceAll(v, "\n", "NL"), func(t *testing.T) {
			encoded := kv(0, "k", v)
			if !strings.HasSuffix(encoded, "\n") {
				t.Fatalf("kv(%q) = %q; want a terminated line", v, encoded)
			}
			got, err := Parse(strings.TrimSuffix(encoded, "\n"), 1)
			if err != nil {
				t.Fatalf("Parse(%q) = %v; want success", encoded, err)
			}
			m, ok := got.(*Map)
			if !ok {
				t.Fatalf("Parse(%q) = %T; want *Map", encoded, got)
			}
			back, _ := m.Get("k")
			if back != v {
				t.Fatalf("round trip of %q via %q gave %#v", v, encoded, back)
			}
		})
	}
}

// TestKeyOrderIsPreserved guards SPEC §9.5: a rewritten file must not reorder
// fields, or every write produces a spurious diff.
func TestKeyOrderIsPreserved(t *testing.T) {
	m, err := Parse("z: 1\nm: 2\na: 3\n", 1)
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	mm := m.(*Map)
	got := mm.Order()
	want := []string{"z", "m", "a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v; want %v", got, want)
	}
	mm.Set("a", "changed")
	mm.Set("b", "new")
	if !reflect.DeepEqual(mm.Order(), []string{"z", "m", "a", "b"}) {
		t.Fatalf("after Set, order = %v; want z,m,a,b", mm.Order())
	}
}

func mustMapOf(t *testing.T, kvs ...any) *Map {
	t.Helper()
	if len(kvs)%2 != 0 {
		t.Fatalf("mustMapOf needs an even number of args, got %d", len(kvs))
	}
	m := NewMap()
	for i := 0; i < len(kvs); i += 2 {
		m.Set(kvs[i].(string), kvs[i+1])
	}
	return m
}

func mapAt(t *testing.T, key string, val *Map) *Map {
	t.Helper()
	return mustMapOf(t, key, val)
}

// zeroLines clears the recorded line numbers so a structural comparison is not
// about diagnostics. Line accuracy has its own test.
func zeroLines(v any) any {
	switch t := v.(type) {
	case *Map:
		t.Line = 0
		for i := range t.keys {
			t.keys[i].line = 0
		}
		for _, k := range t.Order() {
			v, _ := t.Get(k)
			t.Set(k, zeroLines(v))
		}
	case Seq:
		for i := range t {
			t[i] = zeroLines(t[i])
		}
	}
	return v
}
