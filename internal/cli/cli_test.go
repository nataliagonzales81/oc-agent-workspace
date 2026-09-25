package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

type runResult struct {
	Code   int
	Stdout string
	Stderr string
}

func exec(t *testing.T, stdoutTTY bool, argv ...string) runResult {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Main(argv, &out, &errOut, stdoutTTY)
	return runResult{Code: code, Stdout: out.String(), Stderr: errOut.String()}
}

func decode(t *testing.T, raw string) envelope.Envelope {
	t.Helper()
	var env envelope.Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("stdout is not a valid envelope: %v\n%s", err, raw)
	}
	return env
}

func TestVersionOnNonTTYIsOneLineOfJSON(t *testing.T) {
	got := exec(t, false, "version")
	if got.Code != 0 {
		t.Fatalf("exit %d, stderr %s", got.Code, got.Stderr)
	}
	if n := strings.Count(got.Stdout, "\n"); n != 1 {
		t.Fatalf("expected exactly one newline, got %d in %q", n, got.Stdout)
	}
	if !strings.HasSuffix(got.Stdout, "\n") {
		t.Fatalf("output must end in a newline: %q", got.Stdout)
	}
	if got.Stderr != "" {
		t.Fatalf("success must not write to stderr: %q", got.Stderr)
	}

	env := decode(t, got.Stdout)
	if !env.OK {
		t.Fatalf("ok=false: %+v", env.Err)
	}
	if env.Command != "version" {
		t.Errorf("command: got %q", env.Command)
	}
	if env.Schema != "ocaw/version@1" {
		t.Errorf("schema: got %q", env.Schema)
	}
	if env.Err != nil {
		t.Errorf("error should be null: %+v", env.Err)
	}
	if env.Warnings == nil {
		t.Error("warnings must be an array, not null")
	}

	var data struct {
		Version   string `json:"version"`
		GoVersion string `json:"go_version"`
		Commit    string `json:"commit"`
		Dirty     bool   `json:"dirty"`
		SchemaMax int    `json:"schema_max"`
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatalf("data payload: %v", err)
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("data payload: %v", err)
	}
	if data.Version == "" || data.GoVersion == "" || data.Commit == "" {
		t.Errorf("build fields must be populated: %+v", data)
	}
	if data.GoVersion != runtime.Version() {
		t.Errorf("go_version: got %q want %q", data.GoVersion, runtime.Version())
	}
	if data.SchemaMax != envelope.SchemaMajor {
		t.Errorf("schema_max: got %d want %d", data.SchemaMax, envelope.SchemaMajor)
	}
}

func TestVersionOnTTYIsHuman(t *testing.T) {
	got := exec(t, true, "version")
	if got.Code != 0 {
		t.Fatalf("exit %d, stderr %s", got.Code, got.Stderr)
	}
	if strings.HasPrefix(strings.TrimSpace(got.Stdout), "{") {
		t.Fatalf("TTY stdout should not be JSON: %q", got.Stdout)
	}
	if n := strings.Count(got.Stdout, "\n"); n != 1 {
		t.Fatalf("human version is one line, got %d newlines: %q", n, got.Stdout)
	}
	for _, want := range []string{"ocaw ", runtime.Version(), "schema@1"} {
		if !strings.Contains(got.Stdout, want) {
			t.Errorf("human line missing %q: %q", want, got.Stdout)
		}
	}
}

func TestJSONFlagOverridesTTY(t *testing.T) {
	got := exec(t, true, "version", "--json")
	if !strings.HasPrefix(got.Stdout, `{"ok":true`) {
		t.Fatalf("--json must override TTY detection: %q", got.Stdout)
	}
}

func TestQuietFlagOverridesTTY(t *testing.T) {
	got := exec(t, true, "version", "--quiet")
	if got.Stdout != "" {
		t.Fatalf("--quiet must silence stdout on success: %q", got.Stdout)
	}
	if got.Stderr != "" {
		t.Fatalf("--quiet must stay silent on success: %q", got.Stderr)
	}
	if got.Code != 0 {
		t.Fatalf("exit %d", got.Code)
	}
}

func TestJSONBeatsQuiet(t *testing.T) {
	// Precedence is --json > --quiet > TTY (SPEC.md §4.1).
	got := exec(t, false, "version", "--quiet", "--json")
	if !strings.HasPrefix(got.Stdout, `{"ok":true`) {
		t.Fatalf("--json must beat --quiet: %q", got.Stdout)
	}
	if got.Code != 0 {
		t.Fatalf("exit %d", got.Code)
	}
}

func TestQuietBeatsTTY(t *testing.T) {
	got := exec(t, true, "version", "--quiet")
	if got.Stdout != "" {
		t.Fatalf("--quiet must beat TTY detection: %q", got.Stdout)
	}
}

func TestGlobalFlagsAcceptedBeforeAndAfterTheCommand(t *testing.T) {
	before := exec(t, false, "--json", "version")
	after := exec(t, false, "version", "--json")
	if before.Stdout != after.Stdout {
		t.Fatalf("flag position changed output:\n%s\n%s", before.Stdout, after.Stdout)
	}
	if before.Code != after.Code {
		t.Fatalf("flag position changed exit code: %d vs %d", before.Code, after.Code)
	}
}

func TestValueFlagBeforeCommandIsConsumed(t *testing.T) {
	// --dir takes a value, so "." is its argument and "version" is the command.
	got := exec(t, false, "--dir", ".", "version")
	if got.Code != 0 {
		t.Fatalf("exit %d, stderr %s", got.Code, got.Stderr)
	}
	if decode(t, got.Stdout).Command != "version" {
		t.Fatalf("command not dispatched: %q", got.Stdout)
	}
}

func TestValueFlagSwallowsTheCommandName(t *testing.T) {
	// "--dir version" sets the directory to "version"; nothing is left to run.
	got := exec(t, false, "--dir", "version")
	if got.Code != 2 {
		t.Fatalf("exit %d want 2", got.Code)
	}
	env := decode(t, got.Stdout)
	if env.Err == nil || env.Err.Code != envelope.CodeUsage {
		t.Fatalf("error: %+v", env.Err)
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	got := exec(t, false, "bogus")
	if got.Code != envelope.CodeUnknownCommand.Exit() {
		t.Fatalf("exit %d want %d", got.Code, envelope.CodeUnknownCommand.Exit())
	}
	env := decode(t, got.Stdout)
	if env.Err == nil || env.Err.Code != envelope.CodeUnknownCommand {
		t.Fatalf("error: %+v", env.Err)
	}
	if env.Err.Hint == "" {
		t.Error("an error must carry a hint")
	}
	data, _ := env.Data.(map[string]any)
	if _, ok := data["available_commands"]; !ok {
		t.Errorf("unknown command should list what is available: %+v", env.Data)
	}
}

func TestUnknownCommandRespectsQuiet(t *testing.T) {
	got := exec(t, false, "bogus", "--quiet")
	if got.Stdout != "" {
		t.Fatalf("--quiet must silence stdout: %q", got.Stdout)
	}
	if !strings.Contains(got.Stderr, "unknown_command") {
		t.Fatalf("error must still reach stderr: %q", got.Stderr)
	}
	if got.Code != 2 {
		t.Fatalf("exit %d", got.Code)
	}
}

func TestNoCommandIsUsage(t *testing.T) {
	got := exec(t, false)
	if got.Code != 2 {
		t.Fatalf("exit %d want 2", got.Code)
	}
	env := decode(t, got.Stdout)
	if env.Err == nil || env.Err.Code != envelope.CodeUsage {
		t.Fatalf("error: %+v", env.Err)
	}
	if env.Err.Hint != "ocaw --help" {
		t.Errorf("hint: got %q", env.Err.Hint)
	}
}

func TestBadFlagIsUsage(t *testing.T) {
	got := exec(t, false, "version", "--nope")
	if got.Code != 2 {
		t.Fatalf("exit %d want 2", got.Code)
	}
	env := decode(t, got.Stdout)
	if env.Err == nil || env.Err.Code != envelope.CodeUsage {
		t.Fatalf("error: %+v", env.Err)
	}
}

func TestUnexpectedArgumentIsUsage(t *testing.T) {
	got := exec(t, false, "version", "extra")
	if got.Code != 2 {
		t.Fatalf("exit %d want 2", got.Code)
	}
	env := decode(t, got.Stdout)
	if env.Err == nil || env.Err.Code != envelope.CodeUsage {
		t.Fatalf("error: %+v", env.Err)
	}
}

func TestHelpExitsZero(t *testing.T) {
	for _, argv := range [][]string{{"--help"}, {"help"}, {"-h"}} {
		got := exec(t, true, argv...)
		if got.Code != 0 {
			t.Errorf("%v: exit %d", argv, got.Code)
		}
		if !strings.Contains(got.Stdout, "Usage:") {
			t.Errorf("%v: help text missing: %q", argv, got.Stdout)
		}
		if !strings.Contains(got.Stdout, "Exit codes:") {
			t.Errorf("%v: exit code table missing from help", argv)
		}
	}
}

func TestHelpUnderJSONIsStructured(t *testing.T) {
	got := exec(t, false, "help")
	if got.Code != 0 {
		t.Fatalf("exit %d", got.Code)
	}
	env := decode(t, got.Stdout)
	if env.Command != "help" {
		t.Errorf("command: got %q", env.Command)
	}
	data, _ := env.Data.(map[string]any)
	if _, ok := data["available_commands"]; !ok {
		t.Errorf("data: %+v", data)
	}
	if _, ok := data["exit_codes"]; !ok {
		t.Errorf("exit_codes missing: %+v", data)
	}
}

func TestHelpTopicForKnownCommand(t *testing.T) {
	got := exec(t, true, "help", "version")
	if got.Code != 0 {
		t.Fatalf("exit %d", got.Code)
	}
	if !strings.Contains(got.Stdout, "ocaw version") {
		t.Fatalf("topic help missing usage: %q", got.Stdout)
	}
}

func TestQuietSuppressesHelp(t *testing.T) {
	got := exec(t, true, "--quiet", "help")
	if got.Stdout != "" {
		t.Fatalf("--quiet help must be silent: %q", got.Stdout)
	}
	if got.Code != 0 {
		t.Fatalf("exit %d", got.Code)
	}
}

func TestOutputFileLeavesStdoutEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "version.json")
	got := exec(t, false, "version", "--output", path)
	if got.Code != 0 {
		t.Fatalf("exit %d, stderr %s", got.Code, got.Stderr)
	}
	if got.Stdout != "" {
		t.Fatalf("--output must leave stdout empty: %q", got.Stdout)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("output file is not valid JSON: %s", raw)
	}
	if decode(t, string(raw)).Command != "version" {
		t.Fatalf("unexpected payload: %s", raw)
	}
}

func TestOutputFileWriteFailureIsReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "out.json")
	got := exec(t, false, "version", "--output", path)
	if got.Code != envelope.CodeWriteFailed.Exit() {
		t.Fatalf("exit %d want %d", got.Code, envelope.CodeWriteFailed.Exit())
	}
	if !strings.Contains(got.Stderr, "write_failed") {
		t.Fatalf("stderr: %q", got.Stderr)
	}
}

func TestOutputFileIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.json")
	if err := os.WriteFile(path, []byte("PREVIOUS"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := exec(t, false, "version", "--output", path)
	if got.Code != 0 {
		t.Fatalf("exit %d, stderr %s", got.Code, got.Stderr)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("temp file left behind: %v", names)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) == "PREVIOUS" {
		t.Fatal("file was not replaced")
	}
}

func TestPrettyIsIndented(t *testing.T) {
	got := exec(t, false, "version", "--pretty")
	if !strings.Contains(got.Stdout, "\n  \"ok\"") {
		t.Fatalf("not indented: %q", got.Stdout)
	}
	if !json.Valid([]byte(got.Stdout)) {
		t.Fatal("pretty output must remain valid JSON")
	}
}

func TestEveryCommandHasAHumanRenderer(t *testing.T) {
	for _, cmd := range commands {
		if cmd.human == nil {
			t.Errorf("command %q has no human renderer; human mode would fall through to a bare error", cmd.name)
		}
		if cmd.run == nil {
			t.Errorf("command %q has no implementation", cmd.name)
		}
		if cmd.summary == "" {
			t.Errorf("command %q has no summary for --help", cmd.name)
		}
		if cmd.usage == "" {
			t.Errorf("command %q has no usage line", cmd.name)
		}
	}
}

func TestCommandNamesAreSorted(t *testing.T) {
	names := commandNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Fatalf("command names not sorted: %v", names)
		}
	}
}

func TestSuggestCommand(t *testing.T) {
	if got := suggestCommand("vers"); got != "ocaw version" {
		t.Errorf("prefix match: got %q", got)
	}
	if got := suggestCommand("zzzz"); got != "ocaw --help" {
		t.Errorf("no match: got %q", got)
	}
}

func TestResolveMode(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		tty  bool
		want Mode
	}{
		{"pipe defaults to json", Options{}, false, ModeJSON},
		{"tty defaults to human", Options{}, true, ModeHuman},
		{"json beats tty", Options{JSON: true}, true, ModeJSON},
		{"json beats quiet", Options{JSON: true, Quiet: true}, true, ModeJSON},
		{"quiet beats tty", Options{Quiet: true}, true, ModeQuiet},
		{"quiet on a pipe", Options{Quiet: true}, false, ModeQuiet},
	}
	for _, tc := range cases {
		if got := ResolveMode(tc.opts, tc.tty); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestModeString(t *testing.T) {
	for mode, want := range map[Mode]string{ModeHuman: "human", ModeJSON: "json", ModeQuiet: "quiet"} {
		if got := mode.String(); got != want {
			t.Errorf("%d: got %q want %q", mode, got, want)
		}
	}
}

func TestDiagnosticsGoToStderrInHumanMode(t *testing.T) {
	got := exec(t, true, "bogus")
	if got.Stdout == "" {
		t.Fatal("human mode should print help to stdout for a caller mistake")
	}
	if !strings.Contains(got.Stderr, "ocaw: error:") {
		t.Fatalf("stderr: %q", got.Stderr)
	}
	if !strings.Contains(got.Stderr, "ocaw: hint:") {
		t.Fatalf("stderr: %q", got.Stderr)
	}
}

func TestIsTerminalOnRegularFile(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "ocaw-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if IsTerminal(f) {
		t.Fatal("a regular file is not a terminal")
	}
	if IsTerminal(nil) {
		t.Fatal("nil is not a terminal")
	}
}

func TestBuildInfoIsAlwaysPopulated(t *testing.T) {
	version, commit, _ := buildInfo()
	if version == "" {
		t.Error("version must never be empty")
	}
	if commit == "" {
		t.Error("commit must never be empty")
	}
	if len(commit) > 12 {
		t.Errorf("commit should be shortened, got %q", commit)
	}
}

// TestGoModHasNoRequireBlock and TestNoNetHTTPImport enforce SPEC.md §9.1 and
// §9.2 at the test level, so a careless `go get` fails CI instead of quietly
// adding a supply-chain surface to an air-gapped binary.
func TestGoModHasNoRequireBlock(t *testing.T) {
	raw, err := os.ReadFile(moduleFile(t, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "require") {
			t.Errorf("go.mod declares a dependency: %q", trimmed)
		}
	}
}

func TestNoNetHTTPImport(t *testing.T) {
	root := moduleFile(t, "internal")
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		parsed, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", rel, parseErr)
		}
		for _, spec := range parsed.Imports {
			if imported, _ := strconv.Unquote(spec.Path.Value); imported == "net/http" {
				t.Errorf("internal/%s imports net/http; ocaw must stay air-gap safe (SPEC.md §9.2)", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal: %v", err)
	}
}

func moduleFile(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot determine caller")
	}
	// The test lives in <module>/internal/cli, so the module root is two up.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", rel)
}
