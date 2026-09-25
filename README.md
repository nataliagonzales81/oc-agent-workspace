# oc-agent-workspace

A CLI tool for managing your agent workspace.

`ocaw` is a single static binary (stdlib only, no network) that gives an AI agent a
resumable, machine-readable state layer for a project: initialise the workspace, validate
its health, track a task DAG, run verification, and report.

- Every command emits one JSON envelope when stdout is not a TTY, and uses a closed set of
  exit codes — an agent can branch on `error.code` and never on message text.
- Mutating commands are idempotent and support `--dry-run`. Nothing ever prompts.
- `state.json` is the source of truth; `WORKFLOW_STATE.md` is generated from it.

## Install

```sh
go install github.com/nataliagonzales81/oc-agent-workspace@latest
```

## Usage

A session, in order. These run exactly as written — `TestReadmeExamplesRunAsWritten` in
`acceptance/` executes every line below against a real workspace.

```sh
ocaw init
ocaw status
ocaw task add --id 1 --title "Scaffold cmd/ocaw" --agent coder --gate "test=go test ./..."
ocaw task add --id 2 --title "Add the envelope" --agent coder --dep 1
ocaw task next
ocaw task set 1 --status in_progress
ocaw verify run
ocaw task set 1 --status done
ocaw task next
ocaw doctor --json
ocaw report --write
ocaw schema task
```

`ocaw --help` and `ocaw <command> --help` list the full surface, including every subcommand's
own flags. The contract, state schema, exit codes, and acceptance criteria:
**[SPEC.md](SPEC.md)**.

### JSON by default

A stdout that is not a terminal is a single line of JSON. `--pretty` indents it, `--output`
writes it to a file, `--quiet` writes nothing, and `--json` forces the envelope even on a
terminal.

```sh
ocaw status --json --pretty
ocaw schema task > task.json
```

### Exit codes

| Code | Meaning |
|---|---|
| 0 | ok |
| 1 | internal |
| 2 | usage |
| 3 | precondition — no workspace, not a project |
| 4 | validation — a write failed, a gate failed, input is invalid |
| 5 | invariant — a transition or the DAG refused it |
| 6 | lock held by another ocaw run |
| 7 | not found |
| 8 | needs confirmation |

A failing verification gate is exit 4, after the attempt has been recorded.

## Status

v1 is complete: `init`, `doctor`, `status`, `task`, `verify`, `workflow`, `report`, `schema`,
`version`. The command surface is frozen by a goldens harness; a change to a payload is a
deliberate act with a diff attached. The binary name `ocaw` is provisional.

## License

MIT
