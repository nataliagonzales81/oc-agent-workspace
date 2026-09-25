package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/envelope"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/report"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/state"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/workspace"
	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

// What --fix-safe may change, and why nothing else is on the list.
//
// Every fix here is reversible in the sense that matters to someone who did not
// ask for it: it either rebuilds a file ocaw generates from data ocaw still
// holds, or removes a record that has already stopped being true. Nothing here
// interprets, rewrites, or guesses. In particular:
//
//   - task data is never touched. A broken DAG is work in progress, and the fix
//     for a broken DAG is a person deciding what it should be.
//   - skills are never touched. A skill is a human's document; ocaw refusing to
//     parse one is a finding to report, not a file to correct.
//   - agent.yaml is never touched, even though doctor reads it. A project that
//     moved to another test runner has a correct verify_cmd that ocaw would
//     overwrite with its own guess, which is exactly the second-guessing §5.1
//     forbids.
//
// Every fix is named in report.Fixed. A tool that repairs a workspace silently
// is a tool nobody can safely run in a loop.
const (
	fixMissingDirs = "create missing directories"
	fixRender      = "regenerate the generated view"
	fixAnchors     = "drop knowledge entries whose anchor is gone"
)

// applyFixes performs the reversible repairs and records each one.
//
// It takes the single-writer lock, and only if there is something to write: a
// read-only doctor that can fail with lock_held reports the wrong problem, and a
// health check is exactly what someone runs while another agent is working.
func applyFixes(l *workspace.Layout, rep *Report, st *state.State, index yaml.KnowledgeIndex, dryRun bool) {
	var plan []string
	if missing := missingDirs(l); len(missing) > 0 {
		plan = append(plan, fixMissingDirs)
	}
	if st != nil && needsRender(l, st) {
		plan = append(plan, fixRender)
	}
	if dropped := danglingEntries(l, index); len(dropped) > 0 {
		plan = append(plan, fmt.Sprintf("%s (%d)", fixAnchors, len(dropped)))
	}
	if len(plan) == 0 {
		return
	}
	if dryRun {
		rep.Fixed = append(rep.Fixed, plan...)
		for _, p := range plan {
			rep.Add(Finding{
				Severity: SeverityInfo,
				Code:     envelope.CodeInternal,
				Check:    CheckRender,
				Message:  "dry run: would " + p,
				Location: l.Root,
			})
		}
		return
	}

	lock, _, err := workspace.Acquire(l)
	if err != nil {
		// A fix that cannot be applied safely is not applied at all. Falling
		// back to an unlocked write would trade a reported lock for a corrupted
		// state file.
		rep.warn(CheckLayout, envelope.WarnStaleLockBroken, l.Root,
			"retry once the other ocaw run finishes",
			"cannot apply fixes: %v", err)
		return
	}
	defer func() { _ = lock.Release() }()

	rep.Fixed = append(rep.Fixed, plan...)

	if missing := missingDirs(l); len(missing) > 0 {
		if err := workspace.EnsureDirs(missing); err != nil {
			rep.errorf(CheckLayout, envelope.CodeWriteFailed, l.Root, "",
				"could not create missing directories: %v", err)
		}
	}
	if st != nil && needsRender(l, st) {
		if _, err := report.Write(l, st); err != nil {
			rep.errorf(CheckRender, envelope.CodeWriteFailed, l.Rel(l.StateMDPath()), "",
				"could not regenerate WORKFLOW_STATE.md: %v", err)
		}
	}
	// Re-checked under the lock rather than reusing the planning result: another
	// writer may have restored an anchor in the window between the two, and
	// dropping an entry that is true again would be the exact data loss this
	// flag is supposed to prevent.
	if dropped := danglingEntries(l, index); len(dropped) > 0 {
		if err := writeIndexWithout(l, index, dropped); err != nil {
			rep.errorf(CheckKnowledge, envelope.CodeWriteFailed, l.Rel(l.KnowledgeIndex()), "",
				"could not rewrite the knowledge index: %v", err)
		} else {
			for _, id := range dropped {
				rep.warn(CheckKnowledge, envelope.WarnKnowledgeAnchorGone, l.Rel(l.KnowledgeIndex()), "",
					"dropped knowledge entry %q: its anchor no longer exists", id)
			}
		}
	}
}

// needsRender reports whether the generated view is missing or has drifted.
func needsRender(l *workspace.Layout, st *state.State) bool {
	doc, found, err := report.Load(l)
	if err != nil || !found {
		return true
	}
	status, _ := report.Check(doc, st)
	return status != report.StatusCurrent
}

func missingDirs(l *workspace.Layout) []string {
	var missing []string
	for _, dir := range l.Dirs() {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			missing = append(missing, dir)
		}
	}
	return missing
}

// danglingEntries lists the ids of entries whose anchor no longer resolves.
func danglingEntries(l *workspace.Layout, index yaml.KnowledgeIndex) []string {
	var out []string
	for _, e := range index.Entries {
		if !anchorResolves(l, e.Anchor) {
			out = append(out, e.ID)
		}
	}
	return out
}

// writeIndexWithout rewrites the index with the listed ids removed.
//
// The write is a full re-encode rather than a line edit. A line-based deletion
// of a YAML list item has to guess at indentation and quoting, and this is a
// file whose whole value is that ocaw can read back exactly what it wrote.
func writeIndexWithout(l *workspace.Layout, index yaml.KnowledgeIndex, drop []string) error {
	remove := make(map[string]bool, len(drop))
	for _, id := range drop {
		remove[id] = true
	}
	kept := make([]yaml.KnowledgeEntry, 0, len(index.Entries))
	for _, e := range index.Entries {
		if !remove[e.ID] {
			kept = append(kept, e)
		}
	}
	out := yaml.KnowledgeIndex{Schema: index.Schema, Entries: kept}
	return workspace.WriteFileAtomic(l.KnowledgeIndex(), []byte(out.Encode()))
}

// relInside is used only for the containment check in anchorResolves; keeping it
// here makes the rule readable at the point it is enforced.
func relInside(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return rel, false
	}
	return rel, true
}
