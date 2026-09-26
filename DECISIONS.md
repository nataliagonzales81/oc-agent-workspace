# Decision log

Six questions were left open by the v1 build. Each was raised by a task that had to make a
call, and each was flagged rather than decided quietly. This file is the place the answers
live, so `v1.0.0` is not a tag that implies a question nobody answered.

**Each answer needs a date and a cost.** An answer with no recorded reversal cost is a
preference, and a 1.0 consumer is entitled to know which of the two they are looking at.

Status: **all six open.** See issue #15.

Every claim about what ships is verified against the running binary, not recalled:

| | Verified |
|---|---|
| D3 | `bad=a && b` → `validation_failed`, *"column 3 holds \"&&\", which is a shell operator"*; `'TestA && TestB'` and `'TestSub\|^other$'` both stored intact |
| D3 | a refused `add` leaves **no task behind** — the whole add is refused, not half-applied |
| D3 | `&& \| > $` are refused with a column number and a SPEC §9.4 citation; the same characters inside quotes are inert. The stored `cmd` keeps its quote characters, and the recorded command has them **stripped**, so `go test -run 'TestA && TestB' ./...` is recorded as `go test -run TestA && TestB ./...` — the record no longer distinguishes the quoted form from the unquoted one |
| D4 | `--gate` registered on exactly two commands: `add` and `set` |
| D5 | `--accept "a=1 and b=2"` → one criterion `{id: a1, text: "a=1 and b=2"}` — never split |
| D6 | 9 of 9 embedded documents are `additionalProperties: true` |

---

## D1 — Is `state.json` or the markdown's authored half authoritative?

**Ships now:** `state.json`. The authored sections are rendered from it byte-for-byte and
are never read back, so `report --write` restores state's text over a hand edit above the
marker — and says so in a note.

| Option | Consequence |
|---|---|
| **A. `state.json` is authoritative** (current) | One source of truth. The CLI never parses prose, which is the property the whole design rests on. A hand edit above the marker is reverted by a rewrite. |
| B. The markdown is authoritative | `report --write` must parse the authored sections back into `state.json`. That is prose parsing, and it makes `report --write` the only command that trusts the file it generated. A reflowed bullet or a typo'd heading becomes data loss. |

**Reversal cost:** A is cheap to reverse (write the parser, ~50 lines, no schema change).
B is also mechanically cheap but changes a documented guarantee — §6.2's "the CLI never has
to parse prose" — and would need a schema `@major` bump on the `report` payload.

**Answer:** _

---

## D2 — Should `## Workflow DAG` and `## Subtask Registry` differ?

**Ships now:** identical, as §6.2 specifies. Same rows, same order, same six columns.

**New evidence since the question was raised.** The four hand-written `WORKFLOW_STATE.md`
files on this machine: only one (`73.oc-loops`) has both sections, and its Registry drops
exactly one column — `Deps` — with identical rows in identical order.

```
## Workflow DAG      | ID | Task | Agent | Tier | Deps | Status |
## Subtask Registry  | ID | Task | Agent | Tier | Status |
```

So the established convention is a **strict column-subset**, not a second view. The two
tables carry no independent information; the Registry is a projection of the DAG with the
dependency column removed.

| Option | Consequence |
|---|---|
| **A. Identical** (current) | Simpler renderer, one table shape, no ambiguity about which is which. Renders six columns twice. |
| B. Registry drops `Deps`, matching this machine | Matches the local convention, and the shorter table is easier to scan for "who is working on what". Costs a second table shape in `report.Render` and one more golden. |

**Reversal cost:** A → B is a `report` payload change, so a schema `@major` bump and a
regenerated golden. B → A likewise. Roughly equal either way; pick on which convention you
want the file to teach.

**Answer:** _

---

## D3 — Are shell operators inside quotes allowed in a gate command?

**Ships now:** allowed. Unquoted `&& || | ; < > ( ) $ \`` are refused with a column number;
the same characters inside quotes are inert and permitted, so
`go test -run 'TestA && TestB' ./...` and `-run 'TestSub|^other$'` both work.

| Option | Consequence |
|---|---|
| **A. Allowed inside quotes** (current) | Nothing is interpreted inside quotes, so refusing them protected nothing, and the strict rule made ordinary test selections inexpressible. |
| B. Refused everywhere (task #4's original) | Maximum bluntness. Also refuses `-run 'TestA && TestB'`, which is a legitimate way to select tests. |

Note this is a **loosening** from the original v1 rule, made in task #7 when `checkGates`
and the new `verify.Tokenize` were consolidated into one parser.

### What option A costs, measured

Reversing D3 is still one function. Choosing A is not free, and the cost was found
by running the gate rather than reading it.

`go test` exits **0** when its `-run` pattern matches nothing:

```
$ ocaw task add --id 1 --title f --gate "t=go test -run 'TestA && TestB' ./..."
$ ocaw verify run --task 1        ->  "status": "pass", "exit": 0

$ go test -run 'TestA && TestB' ./...
ok  	example.com/x	0.534s	[no tests to run]     exit 0
$ go test -run 'TestA' ./...
ok  	example.com/x	0.534s                      exit 0
```

Two gates, identical exit code, identical `ok` line, one of which ran nothing.
ocaw reports a pass for both, and the `[no tests to run]` marker is in the output
it already captures and discards.

Quoting is not the cause — the argv handed to `go test` is correct. Quoting is
what makes this vacuous gate expressible: `-run 'A && B'` is a valid regex
matching no test, and the strict rule would have refused it. Option A traded a
loud refusal for a silent false pass, and a false pass in a verification tool is
worse than a refusal.

**So A is only correct together with #25.** Decide D3 and #25 together: with #25
fixed, A is right, because nothing is quietly unchecked. Without it, B is right
regardless, because the failure mode is invisible. This is the one decision on the
list whose answer is not independent of an open bug.

**Reversal cost:** A → B is one function and a golden. B → A likewise. No schema impact
either way, since gate `cmd` is already a free-form string.

**Answer:** _

---

## D4 — Where should `--gate` live?

**Ships now:** `--gate <name>=<command>` on `ocaw task add` and `ocaw task set`. It is **not**
in §7's usage lines for `task`; it was added because something had to define gates —
`state.SetGate` exists to install a definition, `ocaw verify` needs something to run, and
nothing else in v1 could.

| Option | Consequence |
|---|---|
| **A. On `task add` / `task set`** (current) | A gate is a property of a task, set where the task is set. Command was already in the neighbourhood. |
| B. A separate `ocaw gate add <task> <name>=<cmd>` | A gate gets its own command and its own help. More surface, but gates stop competing with fields for the same flag set. |
| C. Only `ocaw verify detect` writes them | Gate definitions become derived from the project rather than authored, which loses the ability to have a per-task gate that is not the project default. |

**Reversal cost:** A → B is a new command, so a new schema key, a golden, and a migration
question for anyone who used A. This is the most expensive of the three to reverse, which
is an argument for answering it before 1.0.0 rather than after.

**Answer:** _

---

## D5 — Can a custom acceptance-id scheme be introduced in one call?

**Ships now:** no. `--accept` recognises `^a[0-9]+$` or an id already in the state; anything
else is treated as text and gets the next generated id. `--accept-id` was deliberately not
added.

**The reasoning:** a looser "looks like a word" rule cannot distinguish `a1=ship it` from
`a=1 and b=2`, so it would either split a sentence or require the id to be quoted. The
strict rule never splits a sentence, which is the conservative direction.

| Option | Consequence |
|---|---|
| **A. `^a[0-9]+$` only** (current) | Never misparses prose. Cannot introduce a custom scheme in one `--accept` call. |
| B. `--accept-id <id>=<text>` as an explicit second flag | Both work, and nothing is ambiguous, because the syntax *is* the disambiguator. Costs one more flag and one more golden. |
| C. A looser heuristic | Cheapest UX, and it will eventually split a sentence containing `=`. Rejected at every review so far. |

**Reversal cost:** A → B is a flag and a golden. Cheap. The reason to decide now rather than
later is that B is additive, so doing it after 1.0.0 is a minor version.

### The part that is decided, whichever option wins

There is **no way to create an acceptance item from the CLI without going through
`workflow set`**, and `workflow accept` only ticks items that already exist. On a
fresh `init` the list is empty, so:

```
$ ocaw workflow accept a1 --yes
{"error":{"code":"entry_not_found","message":"no acceptance item with id \"a1\"",
 "hint":"ocaw workflow show"}}
```

This is not a bug — refusing to accept an id that does not exist is the whole
point — but it is a two-call sequence, and the README did not say so until the
#16 inventory found an example that assumed otherwise. The working sequence is:

```
ocaw workflow set --accept "a1=go test ./... passes"
ocaw workflow show
ocaw workflow accept a1 --yes
```

So the answer to D5 as asked — *can a custom scheme arrive in one call* — is **no
under A, yes under B**, and under both the item must be created before it can be
accepted. If one call is the actual goal, B is the only option that gets there,
and the gap is the create/accept split rather than the id syntax.

**Answer:** _

---

## D6 — Should the generated schemas be `additionalProperties: true`?

**Ships now:** true (open). The goldens are stricter, so adding a field still fails CI and
the diff is the review.

**This is a consumer/producer split, and the two halves are defended by different things:**

| | Caught by |
|---|---|
| A **producer** renaming or mistyping a field | the **goldens** — byte-for-byte, `additionalProperties: true` notwithstanding |
| A **consumer** reading a field a newer ocaw added or renamed | the **schema** — but only if it is closed |

With `true`, a consumer holding an older schema cannot detect that an ocaw it is talking to
has stopped emitting a field it depends on. That is the real cost, and it is silent.

| Option | Consequence |
|---|---|
| **A. Open** (current) | A newer ocaw that added an optional field is not rejected by an older consumer's schema. Correct for a schema meant to be consumed across versions. |
| B. Closed | A consumer detects a field it was promised and did not get. Also means every additive change is a `@major` bump, which will get abandoned. |

**Reversal cost:** A → B is a schema `@major` bump and a regeneration of all nine documents.
B → A likewise. Equal, and therefore decided on which consumer behaviour you want.

**Answer:** _

---

## After these are answered

1. Fill in each **Answer**, add a date, and note whether it changes shipped behaviour.
2. Where an answer changes behaviour, open a task for the `@major` bump and the goldens
   regeneration. Do not edit a golden to match a decision — the diff is the point.
3. Close the open comments on #5, #7, #8, #11 and #12 with the answer and a link here.
4. `SCHEMA` is still `@1` and `schema_max` is still 1. If any answer above requires
   otherwise, that bump happens before `v1.0.0` is tagged.
