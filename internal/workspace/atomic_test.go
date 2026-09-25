package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

// tempDebris lists leftover partial writes in dir. Every failure path must
// leave this empty: a stray .ocaw-tmp- file is a file a later walk or a git
// status will report as untracked noise.
func tempDebris(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), TempPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestWriteFileAtomicCreatesAFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFileAtomic(path, []byte("{}\n")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "{}\n" {
		t.Errorf("contents = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != FileMode {
		t.Errorf("mode = %v, want %v", info.Mode().Perm(), os.FileMode(FileMode))
	}
	if debris := tempDebris(t, dir); len(debris) != 0 {
		t.Errorf("temp debris left behind: %v", debris)
	}
}

// A shorter replacement must fully replace: an in-place write without a
// truncate would leave the tail of the old contents behind, which is the exact
// corruption a reader would parse as valid JSON with garbage at the end.
func TestWriteFileAtomicFullyReplacesLongerContents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	writeFile(t, path, strings.Repeat("x", 4096))

	if err := WriteFileAtomic(path, []byte("{}")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "{}" {
		t.Errorf("contents = %q, want %q with no tail", got, "{}")
	}
}

// The induced failure is a rename onto a directory, which fails after the temp
// file was written and synced. The destination must be untouched and the temp
// file must not survive.
func TestWriteFileAtomicRenameFailureLeavesDestinationIntact(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "adirectory")
	if err := os.Mkdir(dest, DirMode); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := WriteFileAtomic(dest, []byte("payload")); err == nil {
		t.Fatal("WriteFileAtomic over a directory succeeded, want an error")
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("the destination is no longer a directory")
	}
	if debris := tempDebris(t, dir); len(debris) != 0 {
		t.Errorf("temp debris left behind: %v", debris)
	}
}

func TestWriteFileAtomicMissingParentFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "no", "such", "dir", "f.txt")
	if err := WriteFileAtomic(path, []byte("x")); err == nil {
		t.Fatal("WriteFileAtomic into a missing directory succeeded, want an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "no")); !os.IsNotExist(err) {
		t.Error("a failed write created directories it should not have")
	}
}

func TestEnsureDirAndEnsureDirs(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "a", "b", "c")
	if err := EnsureDir(child); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if info, err := os.Stat(child); err != nil || !info.IsDir() {
		t.Fatalf("Stat(%q) = %v, %v", child, info, err)
	}
	// Idempotent, because init runs it on every invocation.
	if err := EnsureDir(child); err != nil {
		t.Errorf("second EnsureDir: %v", err)
	}
	if err := EnsureDirs([]string{child, filepath.Join(root, "d")}); err != nil {
		t.Errorf("EnsureDirs: %v", err)
	}
}

func TestAppendLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state", "runs.jsonl")
	if err := AppendLine(path, `{"a":1}`); err != nil {
		t.Fatalf("AppendLine: %v", err)
	}
	if err := AppendLine(path, `{"a":2}`); err != nil {
		t.Fatalf("AppendLine: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	want := "{\"a\":1}\n{\"a\":2}\n"
	if string(body) != want {
		t.Errorf("runs.jsonl = %q, want %q", body, want)
	}
}

// §9.3: ocaw walks repositories it did not create, so the refusal is by name
// and pattern. The negative cases matter as much as the positive ones, or the
// rule becomes "refuse anything vaguely dotfile-shaped".
func TestRefuseRead(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	refused := []string{
		"/repo/.env",
		"/repo/.env.production",
		"/repo/.env.local",
		"/repo/.netrc",
		"/repo/server.pem",
		"/repo/cert.PEM",
		"/repo/id_rsa",
		"/repo/id_ed25519",
		filepath.Join(home, ".ssh", "config"),
		filepath.Join(home, ".ssh", "id_ed25519"),
	}
	for _, path := range refused {
		if err := RefuseRead(path); err == nil {
			t.Errorf("RefuseRead(%q) = nil, want a refusal", path)
		} else if codeOf(t, err) != envelope.CodeInvalidPath {
			t.Errorf("RefuseRead(%q) code = %s, want %s", path, codeOf(t, err), envelope.CodeInvalidPath)
		}
	}

	allowed := []string{
		"/repo/RULES.md",
		"/repo/.gitignore",
		"/repo/environment.md",
		"/repo/envelope.json",
		"/repo/.envrc.bak",
		"/repo/idempotent.go",
		"/repo/agent.yaml",
		"/repo/knowledge/index.yaml",
	}
	for _, path := range allowed {
		if err := RefuseRead(path); err != nil {
			t.Errorf("RefuseRead(%q) = %v, want nil", path, err)
		}
	}
}

func TestReadFileRefusesSecretsWithoutReturningBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	const secret = "TOKEN=not-a-real-value\n"
	writeFile(t, path, secret)

	got, err := ReadFile(path)
	if err == nil {
		t.Fatal("ReadFile(.env) succeeded, want a refusal")
	}
	if got != nil {
		t.Error("ReadFile returned bytes for a refused path")
	}
	if !strings.Contains(err.Error(), "refusing to read") {
		t.Errorf("error = %q, want it to say it is refusing", err)
	}
}

func TestReadFileRejectsADirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadFile(dir); codeOf(t, err) != envelope.CodeInvalidPath {
		t.Errorf("code = %s, want %s", codeOf(t, err), envelope.CodeInvalidPath)
	}
}

// §9.6 bounds a single read as well as a walk: a 400MB state file must produce
// an error, not an allocation.
func TestReadFileRefusesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "huge.md")
	writeFile(t, path, strings.Repeat("a", MaxReadBytes+1))

	got, err := ReadFile(path)
	if err == nil {
		t.Fatal("ReadFile of an oversized file succeeded, want an error")
	}
	if got != nil {
		t.Error("ReadFile returned bytes for an oversized file")
	}
	if codeOf(t, err) != envelope.CodeValidationFailed {
		t.Errorf("code = %s, want %s", codeOf(t, err), envelope.CodeValidationFailed)
	}
}

func TestReadFileMissing(t *testing.T) {
	_, err := ReadFile(filepath.Join(t.TempDir(), "absent"))
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want a not-exist error so callers can test for absence", err)
	}
}
