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

The `gates[].cmd` rule is read as "no shell operators at all", not "at most one
`&&`". ocaw executes argv and never a shell (§9.4), so a `&&`, `|`, `;`, backtick or
redirection left in a gate is a command that could not be honoured as written. Quoting
is not an escape: ocaw tokenises the command itself.

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
- `--strict` promotes warnings to errors (for CI).
- `--fix-safe` applies only reversible fixes: create missing empty directories, regenerate
  a stale `WORKFLOW_STATE.md`, drop knowledge entries whose `anchor` no longer exists
  (reported, never silent). It does **not** touch task data, skills, or `agent.yaml`.
- Exit `0` no errors · `4` errors found.

### `ocaw status`

```
ocaw status [--task <id>] [--json]
```

One-shot workspace summary: project type, counts by status, ready-queue (tasks whose deps
are all satisfied), blocked tasks, last verification result, budget. `ocaw status` is the
command an agent runs first to orient, and must be cheap — no subprocesses, no network.

### `ocaw task`

```
ocaw task add --id <id> --title <s> [--agent <s>] [--tier <s>] [--dep <id>...]
ocaw task set <id> [--title <s>] [--agent <s>] [--tier <s>] [--status <s>] [--note <s>]
ocaw task dep <id> --add <id>... | --rm <id>...
ocaw task rm <id>...
ocaw task show <id>
ocaw task list [--status <s>] [--ready] [--blocked]
ocaw task next          # highest-priority ready task, or null
```

- `add` is upsert by `--id` (§4.4). `--status in_progress` on a task with unmet deps is
  rejected: error `deps_unmet`, `data.ready` lists what is blocking.
- `status` accepts only `pending|in_progress|done|blocked|cancelled`.
- Transitions are validated: `done → in_progress` requires `--yes` (it invalidates gates);
  `cancelled → done` is rejected outright.
- `next` returns `data.task: null` and `ok: true` when nothing is ready — an empty queue is
  a valid, non-error state.
- Exit `0` · `2` usage · `5` transition or invariant violation · `7` task not found.

### `ocaw verify`

```
ocaw verify detect [--force]        # re-detect and print the command, store only with --save
ocaw verify run [--task <id>] [--gate <name>] [-- <cmd>...]
ocaw verify history [--task <id>] [--limit <n>]
```

- `run` with no `--task` verifies the whole workspace once (all gates of the named task, or
  all tasks when neither is given) and appends one record per gate to `runs.jsonl`.
- Timeout default 10m, `--timeout <duration>` to override. Exceeding it is a failed attempt
  with `error.code: "verify_timeout"`, not a hang.
- **Stuck detection.** Three consecutive attempts with byte-identical output sets
  `tasks[].stuck: true`. `ocaw task next` and `ocaw status` surface it. The CLI does not
  auto-retry and never retries a gate on the agent's behalf — escalating is the agent's
  decision, and hiding the attempt history would destroy the only evidence it has.
- A gate with `status: pass` and `last_exit: 0` is not re-run unless `--force`.

### `ocaw workflow`

```
ocaw workflow set --request <s> [--scope <s>] [--constraint <s>...] [--accept <s>...]
ocaw workflow accept <a1> [--done|--not-done]
ocaw workflow show
```

Manages the authored (non-derived) top section of `WORKFLOW_STATE.md`.

### `ocaw report`

```
ocaw report [--write] [--format <md|json>] [--output <path>]
```

- Without `--write`, prints the rendered markdown to stdout (or JSON with `--format json`).
- With `--write`, regenerates `WORKFLOW_STATE.md`. This is the repair for the divergence
  error in §6.2.

### `ocaw schema`

```
ocaw schema <command> [--output <path>]
```

Emits the JSON Schema of a command's `data` object. Lets an agent generate a validator
instead of hardcoding field names. Schemas ship embedded in the binary; this command needs
no files and no network.

### `ocaw version`

`ok`, `version`, `go_version`, `commit`, `dirty`, `schema_max`. One line in human mode.

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
8. `ocaw verify run` against a failing `go test` exits non-zero, appends a record to
   `runs.jsonl` with the real exit code, and leaves the gate `fail` — it does not
   "fix" or retry.
9. Three identical failing runs set `stuck: true`, visible in `ocaw status --json`.
10. Every command with a non-TTY stdout emits exactly one line of valid JSON that
    round-trips through `encoding/json` into the documented envelope.
11. `--dry-run` on every mutating command produces the correct `data` and leaves
    `git status --porcelain` empty.
12. A skill with `name:` mismatching its directory is a `doctor` error.
13. `go test ./...` passes; `go vet ./...` is clean; `CGO_ENABLED=0 go build ./cmd/ocaw`
    produces a static binary.
14. A goldens test asserts the exact JSON of every command's envelope shape, so a
    breaking schema change fails CI.

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
