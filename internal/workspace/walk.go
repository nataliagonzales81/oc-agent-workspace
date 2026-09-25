package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
)

const (
	// DefaultMaxEntries is the §9.6 traversal cap. A repository with 50k files
	// must produce a truncated report, never a hang and never a silent partial
	// read.
	DefaultMaxEntries = 10000
	// maxDirDepth bounds a walk that meets a symlink cycle or a pathological
	// tree. It is a second line of defence behind the entry cap.
	maxDirDepth = 64
)

// WalkOptions configures a bounded walk.
type WalkOptions struct {
	// MaxEntries overrides DefaultMaxEntries. Zero means the default.
	MaxEntries int
	// MaxDepth bounds directory recursion, counting the root as depth 0.
	// Zero means maxDirDepth.
	MaxDepth int
	// SkipDirs are directory base names never descended into. It is applied at
	// every level, and .git is skipped unless the caller says otherwise.
	SkipDirs []string
	// FollowSymlinks is off by default: a symlink out of the tree can point at
	// an entire second repository, and a bounded walk cannot honestly bound that.
	FollowSymlinks bool
}

// WalkResult is what a bounded walk found. Truncated is the field that matters:
// it says the answer is partial, so a caller can report traversal_truncated
// instead of drawing a conclusion from half a repository.
type WalkResult struct {
	// Files are project-relative slash-separated paths, sorted, so two walks of
	// the same tree produce byte-identical output (§9.5).
	Files []string
	// Dirs counts directories visited.
	Dirs int
	// SkippedSymlinks counts entries that were not followed.
	SkippedSymlinks int
	// Truncated is true when MaxEntries was reached.
	Truncated bool
	// MaxEntries is the cap that applied.
	MaxEntries int
}

// WalkFiles walks root, collecting files with a hard entry cap.
func WalkFiles(root string, opts WalkOptions) (WalkResult, error) {
	maxEntries := opts.MaxEntries
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = maxDirDepth
	}
	skip := make(map[string]struct{}, len(opts.SkipDirs)+1)
	skip[".git"] = struct{}{}
	for _, name := range opts.SkipDirs {
		skip[name] = struct{}{}
	}

	res := WalkResult{MaxEntries: maxEntries}
	if _, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return res, nil
		}
		return res, envelope.Errorf(envelope.CodeInvalidPath, "check permissions on "+root, "cannot walk %q: %v", root, err)
	}

	rel := func(path string) string {
		r, err := filepath.Rel(root, path)
		if err != nil {
			return path
		}
		return filepath.ToSlash(r)
	}

	var walk func(dir string, depth int) error
	walk = func(dir string, depth int) error {
		if depth > maxDepth {
			return errDepthExceeded(rel(dir))
		}
		res.Dirs++
		entries, err := os.ReadDir(dir)
		if err != nil {
			return envelope.Errorf(envelope.CodeInvalidPath, "check permissions on "+dir, "cannot read directory %q: %v", rel(dir), err)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

		for _, entry := range entries {
			if len(res.Files) >= maxEntries {
				res.Truncated = true
				return nil
			}
			path := filepath.Join(dir, entry.Name())

			if entry.Type()&fs.ModeSymlink != 0 {
				res.SkippedSymlinks++
				continue
			}
			if entry.IsDir() {
				if _, skipIt := skip[entry.Name()]; skipIt {
					continue
				}
				if err := walk(path, depth+1); err != nil {
					return err
				}
				continue
			}
			if !entry.Type().IsRegular() {
				continue
			}
			res.Files = append(res.Files, rel(path))
		}
		return nil
	}

	if err := walk(root, 0); err != nil {
		return res, err
	}
	sort.Strings(res.Files)
	return res, nil
}

func errDepthExceeded(dir string) error {
	return envelope.Errorf(
		envelope.CodeValidationFailed,
		"raise MaxDepth, or remove the directory that nests this deep",
		"traversal of %q exceeded the maximum depth of %d", dir, maxDirDepth,
	)
}

// TraversalWarning returns the §9.6 warning for a truncated walk, or nil.
func (r WalkResult) TraversalWarning() *envelope.Warning {
	if !r.Truncated {
		return nil
	}
	return &envelope.Warning{
		Code: envelope.WarnTraversalTruncated,
		Message: fmt.Sprintf(
			"stopped after %d entries; the result is partial, so findings below this cap are not exhaustive",
			r.MaxEntries,
		),
	}
}

// RelFiles returns the walked files as absolute paths.
func (r WalkResult) RelFiles(root string) []string {
	out := make([]string, 0, len(r.Files))
	for _, rel := range r.Files {
		out = append(out, filepath.Join(root, filepath.FromSlash(rel)))
	}
	return out
}

// Contains reports whether the walk saw rel, which must be project-relative and
// slash-separated. Callers use it to answer "is this path inside the tree" with
// one comparison instead of a filepath.Rel and a ".." check.
func (r WalkResult) Contains(rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	for _, seen := range r.Files {
		if seen == rel {
			return true
		}
	}
	return false
}
