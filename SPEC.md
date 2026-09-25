# SPEC — `oc-agent-workspace` (binary `ocaw`)

A single-binary Go CLI that lets AI agents operate a project workspace: initialise it,
check its health, track multi-agent task state, run verification, and report.

Status: draft v1. This document is the contract. Code follows it, not the reverse.

---

## 1. Problem

Agents operating a repo today improvise workspace state — ad-hoc `TODO.md` files, shell
loops that lose the DAG between invocations, verification that gets re-derived (and
re-guessed wrong) on every run, and handoff state that only exists in a chat transcript.

The failure modes are all *state* failures, not capability failures:

| Failure | Consequence |
|---|---|
| Task DAG lives in a transcript | A restarted agent cannot resume; work is silently re-done or skipped |
| No machine-readable state | Every agent re-parses markdown prose and guesses field meanings |
| Verification command re-derived per run | A test suite runs, or doesn't, at the agent's discretion |
| No health check before operating | Agents mutate a workspace that is already broken |
| Non-idempotent commands | Agent retries double-apply changes |

`ocaw` fixes the state layer so an agent can operate a workspace deterministically and
resume after context loss.

---

## 2. Goals / non-goals

### Goals (v1)

1. `init` a workspace in any Go / Node / Python / Rust / JVM / Make repo, non-destructively.
2. `doctor` — validate workspace health before mutating anything.
3. Task DAG state with dependencies, status, and eval gates — machine-readable, resumable.
4. `verify` — detect the project's verification command once, cache it, run it, record attempts.
5. `report` — render state as markdown or JSON for humans and handoffs.
6. A **stable JSON contract** on every command, with meaningful exit codes.

### Non-goals (v1)

- **Not** an orchestrator. It does not spawn agents, dispatch to LLMs, or run a fleet.
  It is the substrate a fleet/agent harness calls.
- **Not** a knowledge store, memory system, or search index (see §11 handoff).
- **Not** a git wrapper. It reads git status for `doctor`; it never commits, pushes, or branches.
- **Not** networked. No fetches, no telemetry, no update checks. Air-gap safe.
- **Not** a secret reader. It must never read `.env`, keychains, or process environments
  beyond its own, and must never print a value it read from a credential path.

---

## 3. Conventions this must honour

Derived from the existing environment so the format is familiar rather than novel.

- **Task state is a DAG table.** Existing `WORKFLOW_STATE.md` uses
  `| ID | Task | Agent | Tier | Deps | Status |`. `ocaw` uses the same six columns
  and the same status vocabulary (`pending`, `in_progress`, `done`, `blocked`, `cancelled`).
- **Markdown is the human artifact, JSON is the machine artifact.** `state.json` is the
  single source of truth; `WORKFLOW_STATE.md` is *generated* from it. Agents read
  `state.json`; humans read `WORKFLOW_STATE.md`. The CLI never has to parse prose.
- **Verification is discovered by project marker files** (`go.mod`, `package.json`,
  `pyproject.toml`, `Cargo.toml`, `pom.xml`, `Makefile`, …), and the choice is recorded so
  it is computed once and reused.
- **Every run records an attempt** — command, exit code, changed files, timestamp — even on
  failure, because "the test suite failed three times with identical output" is the signal
  that an agent is stuck and must escalate rather than retry.

---

## 4. CLI contract (agent-first)

The JSON contract *is* the interface. Human output is a rendering of it.

### 4.1 Output selection

| Condition | Default rendering |
|---|---|
| stdout is not a TTY | JSON |
| stdout is a TTY | Human table / prose |
| `--json` passed | JSON (overrides TTY detection) |
| `--quiet` | Nothing on success; errors on stderr |

Precedence: `--json` > `--quiet` > TTY detection.

JSON is a single object on one line (no pretty printing) so an agent can read it without a
JSON parser. Pretty-printing only under `--pretty`. Machine consumers that need a file get
`--output <path>`; when `--output` is set, nothing is written to stdout.

### 4.2 Envelope

Every command emits the same envelope shape, so an agent can write one parser.

```json
{
  "ok": true,
  "command": "task.list",
  "data": { },
  "error": null,
  "warnings": [],
  "schema": "ocaw/task.list@1"
}
```

- `ok` — `true` iff the command achieved its intent. A `doctor` that finds errors returns
  `ok: false` with `data.findings` populated.
- `data` — command-specific. Never at the top level.
- `error` — `null`, or `{"code": "...", "message": "...", "hint": "..."}`. `hint` is a
  single actionable next command, not prose.
- `warnings` — array of `{"code","message"}`; present but possibly empty.
- `schema` — stable identifier, `"<command>@<major>"`. Bumped only on breaking change.

### 4.3 Errors

```
$ ocaw task show 99
{"ok":false,"command":"task.show","data":null,
 "error":{"code":"task_not_found","message":"no task with id 99",
          "hint":"ocaw task list"},
 "warnings":[],"schema":"ocaw/task.list@1"}
```

Error codes are a closed set (see §7). An agent is expected to branch on `error.code`,
never on `message` text.

### 4.4 Idempotency and safety

- Every mutating command is idempotent. Re-running `ocaw task add` with the same
  `--id` updates in place rather than erroring or duplicating.
- Every mutating command supports `--dry-run`, which performs all computation, emits the
  `data` it *would* write, and writes nothing.
- **No interactive prompts.** A command that would prompt (e.g. overwrite a non-empty
  file) fails with `needs_confirmation` unless `--yes` is passed. Agents never block.
- All writes are atomic (write temp file in the same directory, `fsync`, `rename`).
- Concurrent agents are safe: a single-writer lock at `.agent/state/lock`, acquired with
  `O_EXCL`, containing pid + host + timestamp. Stale locks (> 15 min, dead pid) are
  broken with a warning. The CLI never waits on a lock — it fails with `lock_held`.
  Age comes from the recorded timestamp, falling back to the file's mtime when the
  contents cannot be parsed, so the window between the exclusive create and the first
  write reads as *held* rather than as *stale*.
- `WORKFLOW_STATE.md` is regenerated on every mutation. `state.json` and
  `WORKFLOW_STATE.md` must never diverge; `doctor` checks this.

---

## 5. Workspace layout

`ocaw init` creates, relative to `--dir` (default: cwd, or the nearest git root):

```
.agent/
  agent.yaml              # workspace manifest (§5.1)
  RULES.md                # agent behaviour rules, free-form prose
  MEMORY.md               # session-local durable notes, free-form prose
  KNOWLEDGE.md            # project-level knowledge index, free-form prose
  knowledge/
    index.yaml            # structured knowledge entries (§5.2)
  skills/
    <name>/SKILL.md       # on-demand capability docs, frontmatter per §5.3
  hooks/
  state/
    state.json            # machine source of truth (§6.1)
    runs.jsonl            # append-only verification attempt log
    lock
WORKFLOW_STATE.md         # generated human view (§6.2)
.workflow/
  <slug>/
    plan.md
    packets/
    results/
    recipes/
```

`init` never overwrites an existing non-empty file. Conflicts are reported as
`warnings` with `code: "file_exists"`, and the file is left untouched. `--force`
overwrites only files `ocaw` itself previously wrote (tracked by a `ocaw: true` marker
line in `agent.yaml`).

### 5.1 `agent.yaml`

```yaml
ocaw: true
schema: ocaw/agent@1
name: oc-agent-workspace
created: 2026-09-25T00:00:00Z
project:
  type: go            # go|node|python|rust|jvm|make|unknown
  root: /abs/path
  verify_cmd: go test ./...
  verify_detected: 2026-09-25T00:00:00Z
  lint_cmd: go vet ./...
  test_cmd: go test ./...
```

`verify_cmd` is written once at `init` (or by `ocaw verify detect`) and thereafter only
changed by explicit request. A stale auto-detected command that no longer works is a
`doctor` **warning**, not an error — the human may have moved to Bazel or `just`, and the
CLI must not second-guess it silently.

### 5.2 `knowledge/index.yaml`

```yaml
schema: ocaw/knowledge@1
entries:
  - id: k-0001
    title: Verification command is go test
    kind: fact        # fact|decision|gotcha
    anchor: Makefile  # repo-relative, outside .agent/
    created: 2026-09-25T00:00:00Z
    supersedes: []
```

`anchor` must be repo-relative and must not point inside `.agent/`. `doctor` validates both.

### 5.3 `skills/<name>/SKILL.md` frontmatter

```yaml
---
name: verify-first
version: 1.0.0
description: "Run verification before claiming done. Triggers on: verify, check, test."
allowed-tools: Bash, Read, Grep
---
```

Required: `name` (matching the directory name). Optional: `version`, `description`,
`allowed-tools`, `hidden`. `doctor` rejects a skill whose `name` mismatches its directory
or whose `description` is missing, because description matching is how an agent decides
to load a skill.

#### Frontmatter subset boundary

Per §9.1 the reader accepts only the shapes above, and a construct outside them is an
error naming the construct rather than a silent drop. The practical limits, measured
against 90 third-party `SKILL.md` files on the development machine (39 parse, 51 are
correctly refused):

| Construct | Verdict | Consequence |
|---|---|---|
| Folded/literal block scalars (`>-`, `\|`) | refused | a long `description` must be one quoted line |
| Flow collections (`[a, b]`, `{a: b}`) | refused, except `[]` | `allowed-tools` must be the comma string, not a list |
| Any key outside the five above | refused | a third-party skill with `author:` or `tags:` is a `doctor` error |
| `yes`/`no`/`on`/`off` for a boolean | refused | only `true`/`false` |

This is a deliberate reading of §9.1, not an oversight: the alternative is a reader that
guesses, and a guess here silently rewrites a config. `ocaw` writes skills itself, so
the shape it emits is the shape it reads. Widening the subset is a v1.1 decision —
`doctor` (§7) is the component that has to be told about this, so that a hand-written
third-party skill surfaces as a clear error rather than a confusing one.

---

## 6. State model

### 6.1 `.agent/state/state.json` — source of truth

```json
{
  "schema": "ocaw/state@1",
  "workflow": {
    "request": "",
    "scope": "",
    "constraints": [],
    "acceptance": [
      {"id": "a1", "text": "", "done": false}
    ]
  },
  "tasks": [
    {
      "id": "1",
      "title": "",
      "agent": "",
      "tier": "",
      "deps": ["2"],
      "status": "pending",
      "gates": [
        {"name": "lint", "cmd": "go vet ./...", "status": "pending",
         "last_exit": null, "attempts": 0, "last_run": null}
      ],
      "notes": "",
      "updated": "2026-09-25T00:00:00Z"
    }
  ],
  "budget": {
    "max_tokens": 0,
    "spent_tokens": 0
  }
}
```

Invariants, all checked by `doctor`:

- Task `id` values are unique. If ids are non-numeric they may be arbitrary slugs; the CLI
  does not require integers (agents should not be forced to renumber).
- `deps` entries must resolve to existing task ids (dangling deps are an **error**).
- The dependency graph must be acyclic (a cycle is an **error**, reported with the
  cycle path).
- A task may be `in_progress` only if every dep is `done` or `cancelled` (an **error**
  otherwise — this is the rule that stops an agent from racing ahead of its DAG).
- Every `gates[].cmd` must be non-empty and must not contain an unescaped `&&` chain
  longer than one stage (see §8, "no shell injection by convenience").

Two consequences of the fourth rule are enforced with it, because each is a way of
reaching a state the fourth rule would then refuse to load:

- A task may not be `pending` or `blocked` while a dependent is `in_progress`. The
  dependent started because this task was done, cancelled, or under way; parking it
  withdraws that. `ocaw task set` refuses the move with the dependent named.
- `task rm` refuses to delete a task that others depend on. A task named in the same
  call does not count, so a chain can be removed from the leaves in one command.

A `gates[].cmd` is tokenised by the same parser `ocaw verify` runs it with
(`internal/verify.Tokenize`), so a command accepted on write is one ocaw can
actually run. An **unquoted** `&&`, `||`, `|`, `;`, `<`, `>`, `(`, `)`, `$`,
backtick, or backslash is refused: ocaw calls `exec` directly and never a shell
(§9.4), so one of those is someone writing a command for a shell that is not
there, and refusing it says so instead of quietly running the first half.

Inside quotes the same characters are inert literal text and are allowed, because
there is no shell here to give them meaning. `-run 'TestA && TestB'` and
`-run 'TestSub|^other$'` are ordinary test selections, and a rule that refused
them would make real commands inexpressible. There are no escapes either: a
backslash is refused rather than passed through, so a command's meaning never
depends on which of the two quoting styles the author reached for. A command
spanning a line break is refused too — a one-line config field holding two lines
is a paste accident, not a command.

`state.json` is decoded with unknown fields refused. A field this build does not know
is a field the next save would delete, and the loss would be silent — the same rule §9.1
states for YAML.

### 6.2 `WORKFLOW_STATE.md` — generated view

Rendered from `state.json` with these sections, in this order:

```markdown
# Workflow State

## Request
## Clarified Scope
## Constraints
## Acceptance Criteria
- [x] a1  …

## Workflow DAG
| ID | Task | Agent | Tier | Deps | Status |

## Subtask Registry
| ID | Task | Agent | Tier | Deps | Status |

## Verification
| Task | Gate | Status | Exit | Attempts | Last run |

## Budget
tokens spent / max
```

The `## Request` … `## Acceptance Criteria` sections are authored by the agent through
`ocaw workflow set` and are preserved verbatim on regeneration. Everything below
`## Workflow DAG` is fully derived. A hand-edit to a derived section is detected by
`doctor` (hash mismatch against the stored `rendered_sha256`) and reported as an error
with hint `ocaw report --write`.

The two halves are separated by a marker line carrying the hash of the derived block:

```markdown
<!-- ocaw:rendered_sha256=<hex> -->
```

The marker sits between the halves so the digest covers the derived block without
covering itself. Three states are distinguished, all reported as `render_drift` with
the same hint, because the two hash comparisons fall out in order and an agent
debugging a colleague's commit needs to know which one happened:

| State | Meaning |
|---|---|
| `current` | The derived block is exactly what the state renders to |
| `stale` | The file is internally consistent, but the state has moved on |
| `edited` | The stored hash does not match the derived block on disk |
| `foreign` | The file has no marker, so ocaw did not generate it |
| `missing` | There is no file yet — a first render, not drift |

The authored half is outside the hash and is never compared, so prose above the marker
cannot be drift. It is still emitted from state.json byte for byte — not trimmed, not
reflowed, not bullet-converted — which is what "preserved verbatim" means here:
`state.json` is the source of truth, so `report --write` restores text from it.

`stuck` is not rendered into the markdown. §7 surfaces it through `ocaw task next` and
`ocaw status`, which are the commands an agent reads; the file is for humans, and its
columns are pinned.

Table cells are escaped: `\` becomes `\\`, `|` becomes `\|`, and an embedded newline is
folded to a space. A title containing a pipe would otherwise silently add a column and
one containing a newline a row, corrupting the file for every reader while leaving
`state.json` perfectly correct. Bullet items in `## Constraints` get the same escaping.

`runs.jsonl` is one JSON object per line, appended and never rewritten. Each record
carries the task, the gate, the argv that ran, the status, the exit code, the
timestamp, the output, and the SHA-256 of the **full** output. Stored output is
capped at 8 KiB with a truncation marker, while the hash covers the whole thing — so
two failures that share a truncated prefix still compare honestly. A malformed line
is an error naming the line number, never a skipped record: dropping it would erase an
attempt from the only record of what was tried.

Gate status is its own closed vocabulary, `pending|pass|fail|timeout`, separate from
task status. `timeout` is distinct from `fail` so `ocaw status` can tell a slow gate
from a broken one.

---

## 7. Commands (v1)

Common flags on every command: `--json`, `--pretty`, `--quiet`, `--dir <path>`,
`--dry-run`, `--yes`. `ocaw <cmd> --help` documents all.

### `ocaw init`

```
ocaw init [--dir <path>] [--name <string>] [--project-type <auto|go|node|python|rust|jvm|make|unknown>]
          [--force]
```

- Creates the layout in §5. Detect `project.type` and `verify_cmd` from marker files.
- Idempotent: on an existing workspace, updates `agent.yaml` metadata, leaves content
  alone, exits 0.
- Exit `0` created or already valid · `3` path is not a project (no git root, no marker
  file) and `--project-type unknown` was not forced · `4` write failed.

A directory inside a git repository counts as a project even with no marker file — the
repo is the evidence — and gets `type: unknown` with an empty `verify_cmd`. The refusal
in the second bullet is only for a directory that is neither.

Marker priority is fixed, so two machines reading the same repo agree:

| Marker | Type | `verify_cmd` | `lint_cmd` |
|---|---|---|---|
| `go.mod` | `go` | `go test ./...` | `go vet ./...` |
| `Cargo.toml` | `rust` | `cargo test` | — |
| `pyproject.toml` | `python` | `pytest` | — |
| `package.json` | `node` | `npm test` | — |
| `pom.xml` | `jvm` | `mvn test` | — |
| `build.gradle` | `jvm` | `gradle test` | — |
| `Makefile` | `make` | `make test` | — |

A `Makefile` is last because every C project has one, and a Go project with a `Makefile`
is still a Go project. `lint_cmd` is recorded only for Go: `go vet` ships with the
toolchain, while `npm run lint` and `ruff check .` ship with nothing. An empty field is
discoverable; a wrong one is a bug.

`agent.yaml` is metadata, not content, and is rewritten on every run. Everything else is
content and is written only when absent or empty. `WORKFLOW_STATE.md` and `state.json`
follow the same rule: the DAG is work, so `init` never replaces it with a template, and
`--force` does not reach either.

What makes a second `init` byte-identical (AC2) is that nothing carries a fresh
timestamp: the prose templates hold no date, and `created` and `verify_detected` are
read back from the existing `agent.yaml` rather than regenerated.

A `file_exists` warning fires only when the file's content is *not* the template ocaw
would have written. A re-init that finds its own output is silent, because five
warnings for "everything is already fine" is how an agent learns to ignore warnings; a
file a human wrote or edited is reported, because `init` kept it and only the human
knows.

`--dry-run` writes nothing at all, including the lock: `Acquire` creates the directory
it lives in, so locking in a dry run would touch the filesystem the flag exists to
leave alone.

### `ocaw doctor`

```
ocaw doctor [--strict] [--fix-safe]
```

Runs, in order: layout presence · `agent.yaml` schema · state schema + invariants (§6.1) ·
`WORKFLOW_STATE.md` render hash · skill frontmatter · knowledge anchors · verify command
still parses. Emits every finding, not just the first.

```json
{"ok":false,"command":"doctor","data":{
  "findings":[
    {"severity":"error","code":"dep_cycle","message":"tasks 2 -> 4 -> 2",
     "location":".agent/state/state.json","hint":"ocaw task dep 2 --rm 4"}
  ],
  "counts":{"error":1,"warning":0}
}, ...}
```

- `severity` is `error` | `warning` | `info`.
- `--strict` promotes warnings to errors (for CI). It changes the exit code and nothing
  else: the same findings are found either way, so a report is comparable across runs.
- `--fix-safe` applies only reversible fixes: create missing empty directories, regenerate
  a stale `WORKFLOW_STATE.md`, drop knowledge entries whose `anchor` no longer exists
  (reported, never silent). It does **not** touch task data, skills, or `agent.yaml`.
- Exit `0` no errors · `4` errors found.

Each finding also carries `check` (one of `layout`, `agent_yaml`, `state`, `render`,
`skills`, `knowledge`, `verify_cmd`), `location` (repo-relative), and a `hint` naming the
command that fixes it. A caller pins behaviour to `check` and branches on `code`; both
are stable, and neither is the message text.

Three things about the exit code:

- **It describes what the pass found, not what it left behind.** A run that reports drift
  and then repairs it under `--fix-safe` still exits 4, because the `findings` it carries
  and its exit code describe the same moment. Re-checking after every fix would mean a
  caller reading `findings` and a caller reading the exit code are looking at different
  states. The hint says what happened and what to run next; the confirming run is a
  second `ocaw doctor`.
- **Findings travel in `data`, not in `error`.** Several unrelated things can be wrong and
  there is no one error to report, so `error` carries only a summary — enough that
  `ok:false` and a non-zero exit can never appear without an explanation.
- **Nothing is executed.** A health check that runs the test suite is one nobody calls in
  a loop. The recorded `verify_cmd` is tokenised, and its program looked up on `PATH`; a
  program that is not installed is a `verify_cmd_stale` *warning*, because §5.1 says so —
  the project may have moved to another runner and ocaw must not second-guess the human
  into re-running detection.

`doctor` is the one command that must work on a workspace that is already broken, so it
does not use `Require()`: a missing `.agent/` is a finding, not a reason to stop. It takes
no lock, and `--fix-safe` takes one only when it has something to write — a read-only
check that fails with `lock_held` reports the wrong problem.

What `--fix-safe` deliberately does not do, and why:

| Not fixed | Why |
|---|---|
| task data | a broken DAG is work in progress; the fix is a person deciding what it should be |
| skills | a skill is a human's document; refusing to parse one is a finding to report, not a file to correct |
| `agent.yaml` | a project that moved to another test runner has a *correct* `verify_cmd` that ocaw would overwrite with its own guess |

In human mode doctor prints its findings to stdout even though the envelope is not ok,
because for this command a non-zero exit is a verdict about the workspace rather than a
failure of the command, and the findings are the entire point of running it. Every other
command keeps stdout empty on failure.

### `ocaw status`

```
ocaw status [--task <id>]
```

One-shot workspace summary: project type, counts by status, ready-queue (tasks whose deps
are all satisfied), blocked tasks, the last verification result, and budget spend. `ocaw
status` is the command an agent runs first to orient, and must be cheap — no subprocesses,
no network.

`data` carries `root`, `project_type`, `verify_cmd`, `counts`, `total`, `ready`, `blocked`,
`next`, `stuck`, `stuck_tasks`, `last_verification`, `budget`, `acceptance`, `render`,
`history_truncated`, `task`, and `notes`. `next` and `task` are both always present: `next`
is null when the queue is empty, `task` is null unless `--task` named one. Two nullable
keys that mean different things, rather than one overloaded key.

`stuck` leads both renderings. It is the single most actionable fact in the file, and a
summary that buries it under a table has failed at the only job it has. `stuck` carries the
full `state.Stuck` detail — task, gate, when the run began, how many identical attempts —
not just a flag, because "task 2 is stuck" without "on which gate, since when" is not
actionable.

`render` reports whether `WORKFLOW_STATE.md` still matches `state.json` (`current`, `stale`,
`edited`, `foreign`, `missing`). A human or an agent reads that file and believes it, so
knowing it has drifted is part of orienting.

**No subprocesses, and no lock.** It reads `state.json`, `runs.jsonl`, `agent.yaml`, and
the generated markdown, and nothing else. Two tests hold that: a runtime one that puts a
poisoning `git`/`go`/`sh` on `PATH` and asserts no marker appears, and a source-level one
asserting that `internal/{state,report,workspace,yaml,envelope,cli}` cannot reach `os/exec`
at all and that `internal/verify/run.go` is the only file in ocaw that builds a command.
The lock is not taken, because an agent orienting while another one works would otherwise
be told the workspace is busy when the only thing it wanted to know was where to start.

**Bounded.** `runs.jsonl` is append-only and its size is not under ocaw's control, so
`state.LoadRunsTail` reads at most `state.MaxHistoryBytes` (1 MiB) from the end, starting
at the first line boundary in the window so a partial record is never parsed. The payload
carries `history_truncated` and a note says so, because presenting a tail as the whole log
is the failure mode. The trailing run of identical hashes is at the end of the file, which
is exactly what a tail read keeps, so a truncated history reaches the same stuck verdict as
a complete one — provided the window holds `StuckThreshold` records. It does: a record is
at most `MaxOutputBytes` plus a line of fields, so the default holds hundreds.

`--task <id>` adds `unmet_deps`, `dependents`, per-gate outcome and attempt count, the last
ten attempts for that task, and whether it is runnable — enough to decide what to do about
one task without reading the whole DAG.

### `ocaw task`

```
ocaw task add --id <id> --title <s> [--agent <s>] [--tier <s>] [--status <s>] [--note <s>]
              [--dep <id>...] [--gate <name>=<command>]...
ocaw task set <id> [--title <s>] [--agent <s>] [--tier <s>] [--status <s>] [--note <s>]
                  [--gate <name>=<command>]...
ocaw task dep <id> --add <id>... | --rm <id>...
ocaw task rm <id>...
ocaw task show <id>
ocaw task list [--status <s>] [--ready] [--blocked]
ocaw task next          # highest-priority ready task, or null
```

Flags may appear before, between, or after the task id. Go's `flag` stops at the first
positional, so `ocaw task set 1 --status done` would otherwise read `--status` as a
positional, ignore it, and report that nothing was changed — a silent no-op on the most
important flag in the command.

- `add` is upsert by `--id` (§4.4); fields it was not given are preserved, and `data.created`
  says which happened.
- `--status in_progress` on a task with unmet deps is rejected: error `deps_unmet`, exit 5,
  and `data.detail.blocking` names the deps. The refusal covers `add` as well as `set`, and
  the whole `add` is refused — a task is not created and then left in a state the rules
  forbid.
- `status` accepts only `pending|in_progress|done|blocked|cancelled`. An omitted `--status`
  on `add` means pending.
- Transitions are validated: `done → in_progress` requires `--yes` (it invalidates gates);
  `cancelled → done` is rejected outright, `--yes` or not; a task with a dependent in
  progress may not go back to `pending` or `blocked`.
- `next` returns `data.task: null` and `ok: true` when nothing is ready — an empty queue is
  a valid, non-error state.
- `rm` is idempotent: an id that is not there is echoed in `data.removed` and ignored, so
  removing a batch fails on nothing. `RemoveTask` itself stays strict, because a caller that
  passes a typo should hear about it; idempotence is a decision about what a human typed.
- `dep` applies `--add` then `--rm`, so a dep that is both ends up removed, which is what the
  flags read as.
- `list` filters by `--status`, `--ready`, or `--blocked` — never two at once — and always
  reports the unfiltered ready queue, so a caller asking for the blocked list still learns
  what is runnable.
- Exit `0` · `2` usage · `3` no workspace · `4` a gate command that is not a runnable argv ·
  `5` transition or invariant violation · `6` lock held · `7` task not found.

`--gate <name>=<command>` is not in the original usage lines, but something has to define
gates: the state model stores them, `ocaw verify` needs something to run, and nothing else
in v1 could. The syntax is a name, an explicit `=`, and a command — the same shape as a
dependency edge, so it reads like the rest of the command. The command is checked with
`verify.Tokenize` at the moment it is written, not at verify time: a gate ocaw cannot run
should be refused while the author is still thinking about it.

A mutating subcommand takes the single-writer lock, **including under `--dry-run`**. A dry
run reporting a verdict about a state another agent is halfway through changing is
reporting fiction, so it waits for nothing and fails with `lock_held` instead. Read-only
subcommands (`show`, `list`, `next`) take no lock at all.

Every mutation refreshes `WORKFLOW_STATE.md` from the state it just wrote. Leaving it behind
would make `doctor` report staleness as the normal outcome of doing work, and a check that
fires on success trains people to ignore it. `ocaw report --write` stays meaningful for
what a mutation cannot cover: a hand-edited file, a deleted one, one written out of band.

`data` is the same shape for all seven subcommands — `subcommand`, `task`, `tasks`,
`removed`, `counts`, `ready`, `blocked`, `created`, `dry_run`, `detail` — with every key
always present and every list an array. A caller should not have to know which subcommand it
ran before it can check whether `task` is null, and a field that appears in one subcommand
and not another is a field guarded twice. `detail` carries the structured half of a
refusal: the blocking dep ids, the cycle path, the dependent that would be stranded. An
agent that has to read the message to find out what is in its way is back to parsing prose,
which is the one thing the JSON contract exists to prevent.

### `ocaw verify`

```
ocaw verify detect [--force] [--save]      # re-detect and print the command, store only with --save
ocaw verify run [--task <id>] [--gate <name>] [--force] [--timeout <d>] [--verbose] [-- <cmd>...]
ocaw verify history [--task <id>] [--limit <n>]
```

- `run` with no `--task` verifies the whole workspace once (all gates of the named task, or
  all tasks when neither is given) and appends one record per gate to `runs.jsonl`.
- Timeout default 10m, `--timeout <duration>` to override. A bare number is read as
  **seconds**: reading `--timeout 10` as ten minutes is a plausible misreading of the
  default, in the direction that hides a hang. Exceeding the deadline is a failed attempt
  with `error.code: "verify_timeout"`, not a hang.
- The deadline kills the **process group**, not just the gate. Killing only the direct child
  leaves any grandchild holding the output pipe, and `Wait` then blocks on the copy, so a
  200ms deadline behind a `sleep 30` takes 30 seconds. A `WaitDelay` bounds the drain as
  well, so even a survivor cannot turn the deadline into a hang.
- The lock is held across every gate in a run, not released between them. A run is one
  decision about the workspace, and interleaving another agent's `task set` would leave the
  recorded attempt describing a state that never existed.
- A command given with `-- <cmd>` is recorded against the named `--task` and `--gate`, and
  the gate is **defined on the task first** so the task and the log cannot disagree about
  which gates exist. Without that, stuck detection flags a gate with no definition and
  `doctor` reports it as a broken gate command. `--task` is required: a record with no task
  is a record the history cannot serve.
- `--shell` is parseable and always refused. A flag that is accepted and then errors reads
  as a bug in `ocaw` rather than as the rule it is, so the message names §9.4 and says what
  to use instead.
- A gate with `status: pass` and `last_exit: 0` is not re-run unless `--force`. It is not
  recorded as a skip either: a gate that is silently absent from a run looks like a gate that
  was forgotten.
- **Stuck detection.** Three consecutive attempts with byte-identical output sets
  `tasks[].stuck: true`. `ocaw task next`, `ocaw task show`, `ocaw task list` and
  `ocaw status` surface it. The CLI does not auto-retry and never retries a gate on the
  agent's behalf: escalating is the agent's decision, and hiding the attempt history would
  destroy the only evidence it has. Output that *changes* between failures is not stuck —
  the signal is the same thing recurring, not a gate that keeps failing.
- A command that cannot be started is a `fail` attempt with no exit code, not an error. It
  is evidence, and a caller that treats it as an error tends to drop it.
- A malformed `runs.jsonl` is an error for `history` and a warning for `ocaw task`. The log
  is the only record of what was tried, and a skipped line would erase an attempt; but
  refusing to record a *task* because an old log line is broken would make it permanent.

`detect` never overwrites a `verify_cmd` the project already chose without `--save`: §5.1 is
explicit that the recorded command changes only by explicit request, and a project that has
moved to Bazel or `just` has a correct answer that detection would replace with a guess. A
project type with no conventional command is exit 3, not a guess.

The gate-status vocabulary is defined once, in `internal/verify`, and aliased by
`state.GateStatus`. `state` already depends on `verify` for `Tokenize`, so the dependency
runs one way; defining "pass" in both places would leave two definitions to keep in step,
and the step people forget is the one that makes a recorded run unreadable.

### `ocaw workflow`

```
ocaw workflow set --request <s> [--scope <s>] [--constraint <s>...] [--accept <s>...]
ocaw workflow accept <id> [--done|--not-done]
ocaw workflow show
```

Manages the authored (non-derived) top section of `WORKFLOW_STATE.md`.

**A flag that was not given does not touch its field.** `ocaw workflow set --request X`
leaves the scope, the constraints, and the acceptance criteria exactly as they were, and
`data.changed` says which fields moved. The alternative — treating the whole section as
one replaceable value — turns the most ordinary call into silent data loss.

A flag that *was* given, with an empty value, means "clear it". A string flag cannot tell
these two cases apart on its own, so the command asks the FlagSet which flags appeared
rather than what they hold.

`--accept` takes `id=text` or bare text. Bare text gets the next `a<N>`, continuing from
the highest numeric id already in use, and the counter advances *within* one call as well
as across calls: a command carrying both `--accept a1=first` and `--accept second` must not
hand the second one `a1` too, because `SetAcceptance` merges by id and a collision is not
an error — it is one criterion silently replacing another.

A left side is read as an explicit id only when it matches `a<digits>` or is already an id
in the workspace. A looser rule cannot distinguish `--accept "a1=ship it"` from
`--accept "a=1 and b=2"`, and the second is prose somebody wrote. The cost is that a
custom id scheme cannot be introduced in one call — `--accept "AC-1=…"` is filed as text
and gets an assigned id the author can then see and use. That is the conservative
direction: it never splits a sentence.

`accept` defaults to ticking, since the command is named after the act and the negative
case is spelled out when wanted. Neither flag is a silent no-op.

`data` is `{subcommand, workflow{request,scope,constraints}, acceptance, render, accepted,
accepted_set, changed, written, dry_run, notes, detail}` — every key present, every list an
array, on the same terms as `ocaw task`.

### `ocaw report`

```
ocaw report [--write] [--format <md|json>] [--output <path>]
```

- Without `--write`, reports the drift state and the rendered document, and writes nothing.
- With `--write`, regenerates `WORKFLOW_STATE.md`. This is the repair for the divergence
  error in §6.2, and it takes the lock because it is a mutation. A read-only `report` takes
  no lock, and failing with `lock_held` because someone else is working would be a strange
  answer to "what does the file say".
- `--format md` emits the rendered markdown to stdout, and is the only way to get anything
  other than the envelope there. It is honoured in a non-TTY, because
  `ocaw report --format md > file.md` is the documented way to get a copy and making that
  need a terminal would be a strange rule. Every other combination still follows §4.1,
  including `--quiet` and the global `--output`.
- `data.previous_status` is what the file was *before* the command touched it, and `notes`
  says when a hand-edited file was replaced. `ocaw report --write` on an edited file is
  the one operation that discards somebody's hand edits, so it says so rather than doing it
  quietly.

**Prose sections are not table cells.** The constraints and acceptance criteria are emitted
byte for byte, pipes and all. `cell()` exists so a value cannot add a column to a markdown
table, and applying it to a bullet list rewrites the author's sentence — `a | pipe` came
back as `a \| pipe`. That is the data loss the authored half exists to prevent, so the
escaping stays in the tables where the threat is.

### `ocaw schema`

```
ocaw schema <command> [--output <path>]
```

Emits the JSON Schema of a command's `data` object. Lets an agent generate a validator
instead of hardcoding field names. Schemas ship embedded in the binary; this command needs
no files and no network, and resolves no workspace — a schema query has to work on a
broken workspace, which is exactly when it is most needed.

`ocaw schema <command>` writes **the document** to stdout, not the envelope, so

```sh
ocaw schema task.list > task.json
```

produces a loadable schema file. This is the only command whose output is a document rather
than a description of one. `--json` gives the envelope instead, for a caller that wants the
metadata alongside; `--output` writes the document to a path and leaves stdout empty;
`--quiet` writes nothing. A bare `ocaw schema` lists every key, and `--help` on it lists
them as `task.list`, `verify.run`, `workflow.show`, …

Keys are dotted for subcommands. A payload shared by several subcommands is one document
under several keys — `ocaw schema task`, `ocaw schema task.list` and `ocaw schema task.next`
return **byte-identical** documents, because the document describes the envelope's `data`
rather than the subcommand that produced it. Two keys that answer to different names with
different bytes would be a distinction with no meaning behind it.

**The documents are generated from the payload types, not written beside them.** A
hand-maintained schema is a second statement of the same contract, and two statements of one
contract disagree — silently, in whichever direction the tests happen to check. `internal/
schema` derives each document from the same struct the encoder writes, by reflection, so a
field that is added, renamed, retyped, or made optional appears in the schema without anyone
editing it. The generated bytes are committed under `internal/cli/schemas/` and embedded with
`go:embed`, because a generated-at-init schema would be a second, silent path and a
`go:embed` document is what the issue asks for; `TestEmbeddedSchemasAreCurrent` regenerates
every one and compares the bytes, which is what makes embedding safe rather than a snapshot
that quietly rots. Regenerate with:

```sh
go test ./internal/cli/ -run TestEmbeddedSchemasAreCurrent -update
```

The documents are `additionalProperties: true` — open. A payload that gained an optional
field is a newer ocaw, and a consumer holding an older schema should not reject it; the
`@major` is what says whether the change was breaking.

The closed vocabularies *are* constrained, because the runtime enforces them: task status,
gate status, finding severity, and render state all carry an `enum`. `error.code` is
deliberately **not** constrained: the runtime maps an undeclared code to `internal` rather
than failing, so a schema that rejected one would be stricter than the tool — wrong in the
direction that breaks users.

### `ocaw version`

`ok`, `version`, `go_version`, `commit`, `dirty`, `schema_max`. One line in human mode.

`schema_max` is **computed** from the embedded documents at run time, not a constant. §4.2
defines it as the highest `@major` present, and computing it means a document at major 2
cannot ship while `version` still claims 1.

---

## 8. Exit codes

Closed set. Stable for v1.

| Code | Name | Meaning |
|---|---|---|
| 0 | `ok` | Intent achieved |
| 1 | `internal` | Bug. Message is safe to report |
| 2 | `usage` | Bad flags or arguments |
| 3 | `precondition` | Not a project, or workspace not initialised |
| 4 | `validation` | `doctor` found errors, or a write failed validation |
| 5 | `invariant` | DAG/transition/state rule violated |
| 6 | `lock_held` | Another agent holds the state lock |
| 7 | `not_found` | Task, gate, or entry does not exist |
| 8 | `needs_confirmation` | Would overwrite; pass `--yes` |

Exit `5` is deliberately distinct from `4`: `4` means "the workspace is unhealthy", `5`
means "your request contradicts the state", and an agent recovers from them differently.

---

## 9. Non-negotiable implementation rules

1. **Stdlib only.** No third-party modules. `go.mod` declares no `require` block. Rationale:
   single-binary, air-gapped, no supply-chain surface, `go install` never fails on a
   transitive dep. YAML is read/written by a ~200-line internal subset parser supporting
   only the shapes in §5; an unsupported construct is an error, never a silent drop.
2. **No network.** No `net/http` import outside tests.
3. **No secret reads.** Explicitly refuse to read `.env`, `.netrc`, `*.pem`, `id_*`,
   or anything under `~/.ssh`. `SECURITY.md` boundaries apply to this tool's own operation.
4. **No shell string interpolation.** Verification commands are `argv` slices
   (`["go","test","./..."]`), executed without a shell. `-- <cmd>` accepts a shell string
   only under `--shell`, which is never used by built-in gates.
5. **Deterministic output.** Same state in, byte-identical JSON out. No timestamps in
   output that are not part of the state, no map-iteration order leaking, no durations
   unless `--verbose`.
6. **Bounded.** Every walk, read, and subprocess has a limit. `doctor` on a repo with
   50k files must not hang; default traversal caps at 10k entries and reports
   `code: "traversal_truncated"` rather than silently reading less.

---

## 10. Acceptance criteria for v1

1. `ocaw init` on an empty dir with a `go.mod` produces the full §5 layout, sets
   `project.type: go` and `verify_cmd: go test ./...`, and exits 0.
2. `ocaw init` twice leaves the second run at exit 0 with no file content changed
   (verified by `git status --porcelain` being empty).
3. `ocaw init` on a dir with a hand-written `RULES.md` exits 0 and leaves that file
   byte-identical, with a `file_exists` warning.
4. Adding tasks 1←2←3 then `ocaw task next` returns 1; after `task set 1 --status done`
   it returns 2.
5. `task set 3 --status in_progress` while dep 2 is pending exits 5 with
   `error.code: "deps_unmet"`.
6. Creating a dep cycle exits 5 with `error.code: "dep_cycle"` and names the cycle path.
7. Hand-editing the `## Workflow DAG` table in `WORKFLOW_STATE.md` then `ocaw doctor`
   exits 4 with `error.code: "render_drift"`; `ocaw report --write` then `ocaw doctor`
   exits 0.
8. `ocaw verify run` against a failing `go test` exits non-zero — `4`, with
   `error.code: "verify_failed"` — appends a record to `runs.jsonl` with the real exit code,
   and leaves the gate `fail`. It does not "fix" or retry.

   The record is written *before* the exit code is decided, so a failure is fully
   recorded whether or not the command is happy about it. And the exit code is the only way
   `ocaw verify run && ocaw task set 1 --status done` refuses to mark a task done whose gate
   failed; without it, the only defence is reading `data.attempts`, which is the parsing this
   contract exists to remove. `verify_timeout` is the distinct code for a gate that never
   finished, also exit 4, so a slow gate is never confused with a broken one. A **skipped**
   gate is not a failure and does not change the exit code.
9. Three identical failing runs set `stuck: true`, visible in `ocaw status --json`.
10. Every command with a non-TTY stdout emits exactly one line of valid JSON that
    round-trips through `encoding/json` into the documented envelope.
11. `--dry-run` on every mutating command produces the correct `data` and leaves
    `git status --porcelain` empty.
12. A skill with `name:` mismatching its directory is a `doctor` error.
13. `go test ./...` passes; `go vet ./...` is clean; `CGO_ENABLED=0 go build ./cmd/ocaw`
    produces a static binary, asserted from the recorded `CGO_ENABLED=0` build setting rather
    than from the bytes — a pure-Go `darwin` binary still references `libSystem` for syscalls,
    so looking for a dynamic loader in the file proves nothing. `go list -m all` reports one
    module. Nothing ocaw ships imports `net/http`, `net/url`, `net/rpc` or `net/smtp`.

    The static-build and dependency checks live in `acceptance/`, not in `internal/`, because
    they shell out to the Go toolchain and `internal/` is held to a stricter rule: the shipped
    tool reaches `os/exec` in exactly two places, `internal/verify` (which runs a gate) and
    `internal/doctor` (which resolves one on `PATH` and starts nothing). Weakening either rule
    so one suite could hold both would lose the narrower one.
14. A goldens harness records the exact bytes of every command's envelope under
    `testdata/goldens/`, compared line for line. Two things are substituted before
    the comparison, and nothing else: the workspace's own path, and RFC3339
    timestamps. Whitespace, key order and spelling are all left alone, because
    detecting those is the point — a pretty-printer change is a diff.

    The goldens are **stricter than the schemas**, on purpose. `additionalProperties:
    true` is what a *consumer* of a released payload wants: a newer ocaw that added
    an optional field should not be rejected by an older schema. The goldens are the
    producer's side — a new field is a decision someone makes, and regenerating the
    goldens is how that decision gets reviewed.

    ```sh
    go test ./internal/cli/ -run TestGoldens -update
    ```

    Measured on the harness by breaking things on purpose: renaming a field's json
    tag fails 4 goldens, reordering the envelope's keys fails 32, dropping a key fails
    6, adding an optional key fails 8, and changing a heading in the generated markdown
    fails 2. A breaking change fails CI.

---

## 10.1 The acceptance suite

`acceptance/` is §10 as an executable list: one `TestAC<n>…` per criterion, plus the
toolchain checks and the README's examples run as written. A test named after a criterion is
the *definition* of that criterion, and the map from criterion to test is checked against the
source rather than asserted in a comment — a test that was renamed or deleted leaves the map
pointing at nothing.

The tests read the contract the way a consumer does: assertions go through the decoded
envelope's map keys, not against `internal` types. A payload that stopped being the documented
shape fails here rather than in somebody's agent.

---

## 11. Handoff to the existing toolchain

`ocaw` is a substrate, not a replacement. On this machine it must coexist with, and defer
to:

| Existing | Relationship |
|---|---|
| `state-layout` (`~/.agents/bin/`) | Owns the dotfiles-backed `~/.agents` state tree. `ocaw` owns per-project `.agent/`. No overlap; `ocaw doctor` does not inspect `~/.agents` |
| `agent-status` | Remains the top-level triage entry point per `~/.agents/system/MANIFEST.md` |
| `agent-state`, `agent-scheduler`, `fleet*` | Orchestrators. They call `ocaw`, are never invoked by it |
| `codedb`, `qmd`, `memory` | Knowledge/search. `ocaw` stores a `knowledge/index.yaml` pointer set only; it does not index or query |
| `oc-health`, `agents-lint.sh` | Health of the *toolchain*. `ocaw doctor` is health of the *project workspace*. Different subjects, both named `doctor`-adjacent — documented here to prevent confusion |

**v2 candidates**, in priority order: (1) `ocaw hook` — a `pre-commit`-shaped entry point
so gates run before commit; (2) `ocaw serve` — a read-only JSON-over-stdio mode so an agent
runtime can query state without paying process startup per call; (3) `ocaw workflow
import/export` for cross-repo handoff; (4) `ocaw task split` to turn one task into a
sub-DAG, replacing hand-edited DAG tables.

---

## 12. Repo layout

```
cmd/ocaw/main.go          # thin: parse, dispatch, render envelope
internal/cli/             # command wiring, flag defs, help text
internal/envelope/        # the §4.2 envelope, error codes, exit mapping
internal/workspace/       # layout, paths, init, atomic writes, lock
internal/state/           # state.json + runs.jsonl models, invariants, DAG
internal/report/          # WORKFLOW_STATE.md render + hash
internal/verify/          # marker detection, argv execution, stuck detection
internal/yaml/            # stdlib-only YAML subset
internal/doctor/          # the check pipeline
testdata/goldens/         # expected JSON per command
```

Package rule: `state` and `report` must not import `cli`. The DAG and rendering logic is
unit-testable without a process.
