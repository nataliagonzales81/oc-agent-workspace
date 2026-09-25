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

```sh
ocaw init
ocaw status
ocaw task add --id 1 --title "Scaffold cmd/ocaw" --dep ""
ocaw verify run
ocaw doctor --json
```

Full command surface, state schema, exit codes, and v1 acceptance criteria:
**[SPEC.md](SPEC.md)**.

## Status

Spec written, implementation not started. Binary name `ocaw` is provisional.

## License

MIT
