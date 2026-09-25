package cli

import (
	"fmt"
	"strings"

	"github.com/nataliagonzales81/oc-agent-workspace/internal/yaml"
)

// The three markdown templates carry no timestamp and no hostname. A template
// that stamped the clock would make two `ocaw init` runs differ, and AC2 is
// specifically that a second init leaves the tree byte-identical. The single
// piece of interpolated state is the verify command, which comes from the marker
// already on disk and so is stable for a given project.

// rulesTemplate is the one file a human is most likely to hand-write, so it is
// written as a skeleton with the project's own commands already filled in rather
// than as instructions to fill them in. An empty file is one nobody edits.
func rulesTemplate(a yaml.Agent) string {
	var b strings.Builder
	b.WriteString("# Project rules\n\n")
	b.WriteString("Rules an agent working in this repository must follow. Keep each rule\n")
	b.WriteString("checkable — a rule nobody can verify is a rule nobody follows.\n\n")
	b.WriteString("## Verification\n\n")
	if a.Project.VerifyCmd != "" {
		fmt.Fprintf(&b, "- Run `%s` and read the result before reporting any task as done.\n", a.Project.VerifyCmd)
	} else {
		b.WriteString("- No verification command was detected. Set one with `ocaw verify detect`,\n")
		b.WriteString("  or add it to `project.verify_cmd` in `.agent/agent.yaml`.\n")
	}
	b.WriteString("- A failing gate is recorded, never retried silently. Three identical failures\n")
	b.WriteString("  mean the work is stuck: escalate rather than try again.\n")
	b.WriteString("- The verification command is discovered once and recorded. Change it by hand\n")
	b.WriteString("  when the project moves to another runner; ocaw will not second-guess it.\n\n")
	b.WriteString("## Scope\n\n")
	b.WriteString("- Do not widen the task. If it needs another change, add a task with a\n")
	b.WriteString("  dependency instead of folding it into the current one.\n")
	b.WriteString("- Leave files outside the task's scope byte-identical, including formatting.\n\n")
	b.WriteString("## State\n\n")
	b.WriteString("- Task state lives in `.agent/state/state.json`. `WORKFLOW_STATE.md` is\n")
	b.WriteString("  generated from it; edit the state through `ocaw`, never the markdown.\n")
	b.WriteString("- `ocaw status` orients, `ocaw task next` picks up the work.\n")
	return b.String()
}

// memoryTemplate is the workspace's own memory, separate from any agent-session
// memory. It records what a future session would otherwise have to rediscover.
const memoryTemplate = `# Workspace memory

What a fresh session needs to know before touching this project. Keep entries
short and factual; if an entry has stopped being true, delete it rather than
dating it.

- Conventions this repo follows that nothing enforces
- Traps: a command that looks right and is not, a test that passes for the wrong reason
- Decisions already made, so they are not relitigated
`

// knowledgeTemplate points at the machine-readable index. The prose file and
// the index are deliberately separate: the index is what `ocaw doctor` validates
// (anchors must exist and stay outside `.agent/`), and the prose is what a human
// reads.
const knowledgeTemplate = "# Knowledge\n" +
	"\n" +
	"Durable facts about this project. Each one belongs in\n" +
	"`.agent/knowledge/index.yaml` with an anchor pointing at the file that proves\n" +
	"it, because an entry whose anchor has been deleted is worse than no entry.\n" +
	"\n" +
	"- `fact` — something true about the project\n" +
	"- `decision` — a choice made, and what it traded away\n" +
	"- `gotcha` — something that will bite the next person who does not know it\n"
