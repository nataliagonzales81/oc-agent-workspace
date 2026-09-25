package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

func TestWalkFilesIsRelativeSortedAndDeterministic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "b.txt"), "b")
	writeFile(t, filepath.Join(root, "a.txt"), "a")
	writeFile(t, filepath.Join(root, "sub", "c.txt"), "c")

	first, err := WalkFiles(root, WalkOptions{})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	want := []string{"a.txt", "b.txt", "sub/c.txt"}
	if strings.Join(first.Files, ",") != strings.Join(want, ",") {
		t.Errorf("Files = %v, want %v", first.Files, want)
	}
	if first.Truncated {
		t.Error("Truncated = true for a 3-file tree")
	}
	if first.MaxEntries != DefaultMaxEntries {
		t.Errorf("MaxEntries = %d, want the default %d", first.MaxEntries, DefaultMaxEntries)
	}

	second, err := WalkFiles(root, WalkOptions{})
	if err != nil {
		t.Fatalf("second WalkFiles: %v", err)
	}
	if strings.Join(first.Files, ",") != strings.Join(second.Files, ",") {
		t.Error("two walks of the same tree disagreed, so output is not deterministic")
	}
}

// §9.6: a capped walk says it was capped. Silently returning a prefix is the
// failure mode the spec exists to prevent.
func TestWalkFilesReportsTruncationInsteadOfReadingLess(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 10; i++ {
		writeFile(t, filepath.Join(root, "f"+string(rune('a'+i))+".txt"), "x")
	}

	res, err := WalkFiles(root, WalkOptions{MaxEntries: 3})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if !res.Truncated {
		t.Error("Truncated = false after hitting the cap")
	}
	if len(res.Files) != 3 {
		t.Errorf("Files = %d entries, want the cap of 3", len(res.Files))
	}
	if res.MaxEntries != 3 {
		t.Errorf("MaxEntries = %d, want 3", res.MaxEntries)
	}

	warning := res.TraversalWarning()
	if warning == nil {
		t.Fatal("TraversalWarning() = nil for a truncated walk")
	}
	if warning.Code != envelope.WarnTraversalTruncated {
		t.Errorf("warning code = %s, want %s", warning.Code, envelope.WarnTraversalTruncated)
	}
	if !strings.Contains(warning.Message, "partial") {
		t.Errorf("warning = %q, want it to say the result is partial", warning.Message)
	}
}

func TestTraversalWarningIsNilWhenComplete(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), "x")
	res, err := WalkFiles(root, WalkOptions{MaxEntries: 1})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if res.Truncated {
		t.Fatal("Truncated = true for a walk exactly at the cap")
	}
	if w := res.TraversalWarning(); w != nil {
		t.Errorf("TraversalWarning() = %v, want nil", w)
	}
}

func TestWalkFilesSkipsGitByDefault(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, ".git", "objects", "pack.idx"), "x")
	writeFile(t, filepath.Join(root, "keep.txt"), "x")

	res, err := WalkFiles(root, WalkOptions{})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if strings.Join(res.Files, ",") != "keep.txt" {
		t.Errorf("Files = %v, want only keep.txt", res.Files)
	}
}

// A symlink can leave the tree entirely. Following it would make the cap a lie,
// because the entries behind it are not counted by any honest bound.
func TestWalkFilesDoesNotFollowSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "x")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	writeFile(t, filepath.Join(root, "own.txt"), "x")

	res, err := WalkFiles(root, WalkOptions{})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if res.SkippedSymlinks != 1 {
		t.Errorf("SkippedSymlinks = %d, want 1", res.SkippedSymlinks)
	}
	if strings.Join(res.Files, ",") != "own.txt" {
		t.Errorf("Files = %v, want only own.txt", res.Files)
	}
}

func TestWalkFilesHonoursSkipDirs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "vendor", "dep.go"), "x")
	writeFile(t, filepath.Join(root, "app.go"), "x")

	res, err := WalkFiles(root, WalkOptions{SkipDirs: []string{"vendor"}})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if strings.Join(res.Files, ",") != "app.go" {
		t.Errorf("Files = %v, want only app.go", res.Files)
	}
}

func TestWalkFilesMissingRootIsEmptyNotAnError(t *testing.T) {
	res, err := WalkFiles(filepath.Join(t.TempDir(), "absent"), WalkOptions{})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if len(res.Files) != 0 || res.Truncated {
		t.Errorf("res = %+v, want an empty complete result", res)
	}
}

// A second bound behind the entry cap: a tree nested past the depth limit
// errors instead of recursing without end.
func TestWalkFilesRejectsATreeDeeperThanTheDepthLimit(t *testing.T) {
	root := t.TempDir()
	deep := root
	for i := 0; i <= maxDirDepth+2; i++ {
		deep = filepath.Join(deep, "d")
	}
	if err := os.MkdirAll(deep, DirMode); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if _, err := WalkFiles(root, WalkOptions{}); codeOf(t, err) != envelope.CodeValidationFailed {
		t.Errorf("code = %s, want %s", codeOf(t, err), envelope.CodeValidationFailed)
	}
	if _, err := WalkFiles(root, WalkOptions{MaxDepth: 4096}); err != nil {
		t.Errorf("a raised MaxDepth should succeed: %v", err)
	}
}

func TestWalkResultHelpers(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub", "a.txt"), "x")
	res, err := WalkFiles(root, WalkOptions{})
	if err != nil {
		t.Fatalf("WalkFiles: %v", err)
	}
	if res.Dirs != 2 {
		t.Errorf("Dirs = %d, want 2", res.Dirs)
	}
	if !res.Contains("sub/a.txt") {
		t.Error("Contains(sub/a.txt) = false")
	}
	if !res.Contains("./sub/a.txt") {
		t.Error("Contains should tolerate a ./ prefix")
	}
	if res.Contains("sub/missing.txt") {
		t.Error("Contains reported a file that was not walked")
	}
	abs := res.RelFiles(root)
	if len(abs) != 1 || abs[0] != filepath.Join(root, "sub", "a.txt") {
		t.Errorf("RelFiles = %v", abs)
	}
}
