package verify

import (
	"strings"
	"testing"
)

func TestTokenizePlainCommand(t *testing.T) {
	argv, err := Tokenize("go test ./...")
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	want := []string{"go", "test", "./..."}
	if len(argv) != len(want) {
		t.Fatalf("argv = %q, want %q", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
}

// Quoting exists to let one argument contain a space. That is the whole of it:
// not interpolation, not escapes, not a shell.
//
// Note that `”` does not escape a quote the way it does in YAML, and that is
// not an oversight. A doubled quote is a YAML convention this format simply does
// not have; supporting it would mean a gate command reads one way and means
// another depending on which parser touched it.
func TestTokenizeQuoting(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`go test -run TestFoo ./...`, []string{"go", "test", "-run", "TestFoo", "./..."}},
		{`pytest -k "a b"`, []string{"pytest", "-k", "a b"}},
		{`pytest -k 'a b'`, []string{"pytest", "-k", "a b"}},
		{`echo ""`, []string{"echo", ""}},
		{`echo ''`, []string{"echo", ""}},
		// Operators inside quotes are literal text: there is no shell here to
		// interpret them, and these are ordinary test selections.
		{`go test -run 'TestA && TestB'`, []string{"go", "test", "-run", "TestA && TestB"}},
		{`go test -run 'TestSub|^other$'`, []string{"go", "test", "-run", "TestSub|^other$"}},
		{`echo "a; b"`, []string{"echo", "a; b"}},
		{`  go   test  `, []string{"go", "test"}},
		{`./script.sh --strict -- --watch=false`, []string{"./script.sh", "--strict", "--", "--watch=false"}},
		{`go test -run 'TestThing/sub$|^other$'`, []string{"go", "test", "-run", "TestThing/sub$|^other$"}},
	}
	for _, tc := range cases {
		got, err := Tokenize(tc.in)
		if err != nil {
			t.Errorf("Tokenize(%q): %v", tc.in, err)
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tc.want, "\x00") {
			t.Errorf("Tokenize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// SPEC §9.4: an argv, never a shell. Every operator is refused, and quoting is
// not an escape — a gate command is an argv whose second element may happen to
// contain a quote character.
func TestTokenizeRefusesShellOperators(t *testing.T) {
	for _, in := range []string{
		"go test && echo ok",
		"go test || echo fail",
		"go test | tee out",
		"go test; rm -rf /",
		"go test `whoami`",
		"go test $(whoami)",
		"go test > out.txt",
		"go test >> out.txt",
		"go test < in.txt",
		"go test &",
		"go test\nrm -rf /",
	} {
		if argv, err := Tokenize(in); err == nil {
			t.Errorf("Tokenize(%q) = %q, want a refusal", in, argv)
		}
	}
}

func TestTokenizeErrors(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n ", "echo 'unclosed", `echo "unclosed`} {
		if argv, err := Tokenize(in); err == nil {
			t.Errorf("Tokenize(%q) = %q, want an error", in, argv)
		}
	}
}

// A refusal has to point at the character, not make the reader find it.
func TestTokenizeErrorNamesTheColumn(t *testing.T) {
	_, err := Tokenize("go test && echo ok")
	if err == nil {
		t.Fatal("want a refusal")
	}
	if !strings.Contains(err.Error(), "column 9") {
		t.Errorf("error = %q, want it to name column 9", err)
	}
	if !strings.Contains(err.Error(), "&&") {
		t.Errorf("error = %q, want it to name the operator", err)
	}
}

// An argv with thousands of elements is a mistake, not a command.
func TestTokenizeBoundsTheArgumentCount(t *testing.T) {
	long := strings.TrimSpace(strings.Repeat("a ", maxArgvTokens+10))
	if _, err := Tokenize(long); err == nil {
		t.Error("want a refusal for an argv over the limit")
	}
}

func TestMustTokenizePanicsOnGarbage(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MustTokenize did not panic on an unparseable command")
		}
	}()
	MustTokenize("go test && echo")
}
