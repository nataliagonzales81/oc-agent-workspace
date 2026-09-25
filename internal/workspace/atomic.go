package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

const (
	// TempPrefix marks a partially written file. Anything matching it left
	// behind is the debris of a crashed write and is never a real file.
	TempPrefix = ".ocaw-tmp-"
	// FileMode is the mode of every file ocaw writes: readable by the owner
	// and by git, never world-writable.
	FileMode = 0o644
	// DirMode is the mode of every directory ocaw creates.
	DirMode = 0o755
	// MaxReadBytes bounds a single file read (§9.6). A workspace file that
	// grows past this is a signal to stop, not to allocate.
	MaxReadBytes = 1 << 20
)

// WriteFileAtomic writes data to path so that a reader sees either the previous
// contents or the new contents, never a prefix of either (§4.4).
//
// The sequence is temp file in the destination directory, write, fsync, close,
// rename. A same-directory temp keeps the rename on one filesystem, where it is
// atomic; fsync before the rename is what makes the new contents durable rather
// than merely visible. Every failure path removes the temp file, so an induced
// error leaves the destination exactly as it was.
func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, TempPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	discard := func() {
		tmp.Close()
		os.Remove(name)
	}

	if _, err := tmp.Write(data); err != nil {
		discard()
		return err
	}
	if err := tmp.Sync(); err != nil {
		discard()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	// CreateTemp uses 0600; a workspace file that git or another user cannot
	// read is a file that will look modified later for no reason.
	if err := os.Chmod(name, FileMode); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	// The rename is durable once the directory entry is flushed. Some
	// filesystems refuse to sync a directory handle at all, and turning that
	// into a hard failure would make ocaw unusable there, so it is best effort.
	syncDir(dir)
	return nil
}

// syncDir best-effort flushes a directory entry.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

// AppendLine appends one line to a file, creating it if needed. One Write call
// on an O_APPEND descriptor keeps the line contiguous even with a concurrent
// appender; a line longer than the pipe atomicity limit is still written whole
// because a regular file append is not split against other writers.
func AppendLine(path string, line string) error {
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	if err := EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, FileMode)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// EnsureDir creates dir and its parents.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, DirMode); err != nil {
		return envelope.Errorf(envelope.CodeWriteFailed, "check permissions on "+dir, "cannot create %q: %v", dir, err)
	}
	return nil
}

// EnsureDirs creates every directory in the list, in order.
func EnsureDirs(dirs []string) error {
	for _, dir := range dirs {
		if err := EnsureDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// forbidden lists the paths §9.3 refuses to read, matched on the base name.
var forbiddenNames = []string{".env", ".netrc", "id_rsa", "id_dsa", "id_ecdsa", "id_ed25519"}

// RefuseRead reports why a path may not be read, or nil when it may.
//
// §9.3 is not a style rule: ocaw walks a repository it did not create, and a
// diagnosis has no reason to read a private key or a populated .env. The
// refusal is by base name plus the *.pem pattern, and it covers the user's
// ~/.ssh whether or not it is inside the workspace.
func RefuseRead(path string) error {
	base := filepath.Base(path)
	// .env and every .env.<suffix> variant, but not an unrelated dotfile that
	// merely contains the letters.
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		return refusal(path, "environment files are never read")
	}
	for _, name := range forbiddenNames {
		if base == name {
			return refusal(path, "credential files are never read")
		}
	}
	if strings.HasSuffix(strings.ToLower(base), ".pem") {
		return refusal(path, "private key material is never read")
	}
	if strings.HasPrefix(base, "id_") {
		return refusal(path, "ssh keys are never read")
	}
	clean := filepath.Clean(path)
	home, err := os.UserHomeDir()
	if err == nil {
		sshDir := filepath.Join(home, ".ssh")
		if clean == sshDir || strings.HasPrefix(clean, sshDir+string(filepath.Separator)) {
			return refusal(path, "anything under ~/.ssh is never read")
		}
	}
	return nil
}

func refusal(path, why string) error {
	return envelope.Errorf(
		envelope.CodeInvalidPath,
		"SKILL.md SECURITY.md — secrets and credentials boundaries apply",
		"refusing to read %q: %s", path, why,
	)
}

// ReadFile reads a workspace file, refusing the paths §9.3 names and bounding
// the size so a huge file cannot be pulled into memory.
func ReadFile(path string) ([]byte, error) {
	if err := RefuseRead(path); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, envelope.Errorf(envelope.CodeInvalidPath, "pass a file", "%q is a directory", path)
	}
	if info.Size() > MaxReadBytes {
		return nil, envelope.Errorf(
			envelope.CodeValidationFailed,
			fmt.Sprintf("the file is %d bytes; ocaw reads at most %d", info.Size(), MaxReadBytes),
			"not reading %q", path,
		)
	}
	return os.ReadFile(path)
}
