# internal/aocli/

Command routing and presentation for the CLI. Keep provider, persistence, and
app lifecycle concerns out of this package. Command functions accept arguments
and writers directly so tests do not spawn subprocesses.

**There is no separate CLI binary (D30).** The CLI IS the app binary, dispatched
by verb. `main.go` matches `os.Args[1]` against `Commands()`, this package's
dispatch table, exported so no second copy of the verb set can rot, then hands
the whole argv to `Run`. Agent sessions find the binary because boot publishes a
canonical-name symlink at `<configDir>/bin/agent-overflow` and
`sessionProcessEnv` prepends that directory to every session's PATH. Every
user-facing string here therefore says `agent-overflow`, never `ao`; the AO_*
environment variable names are an internal contract and keep their prefix.

**One verb the root usage names is not a row in that table: `serve`.** It boots
the windowless backend, and a boot needs the embedded asset filesystem and the
whole transport/App graph, both of which live in package `main` by construction.
`decideEntry` matches it BEFORE the `IsCommand` check and routes it to
`runServe`; this package only documents it, because `usage.go` is the one help
text a person reads. The argument is at `serveVerb` in `main_entry.go`, and the
seam is pinned by `TestServeVerbIsNotACLICommand` — adding a `serve` row here
would create a verb that means two different things depending on which file you
read. The operator-facing walkthrough is
[docs/architecture/serve-mode.md](../../docs/architecture/serve-mode.md).

The CLI has three halves:

- **Offline** (`agent-overflow workflow …`): scope discovery, definition
  validation, listing, scaffolding, and the embedded authoring schema. Needs no
  running app and no credential.
- **Execution** (`agent-overflow run …`, `memory …`, `notes …`, `schedule …`):
  one HTTP POST per invocation against a running Agent Overflow, authenticated
  with the scoped credential the app injected into the calling agent's session
  (spec §5, D15).
- **Host** (`agent-overflow service …`): manages the machine, not a backend. It
  talks to no app and holds no credential — it writes a unit file and drives
  the platform's service manager through `internal/serviceinstall`. `serve` is
  the fourth host verb and the one that is not a row here.

`service.go` resolves every host fact ONCE, into a `serviceEnv` it passes down
(`hostServiceEnv` is the production reader). That is why its tests describe a
machine rather than running on one, and why there is no package-level seam a
test must remember to reset — a `service` test that reached the real host would
enable a service on the developer's own login. `--help` works on a host where
`os.Executable()` does not, which is why `serviceEnv` carries its two possible
failures rather than resolving them eagerly. The unit-file rules, the injected
`Runner`, and what install deliberately does not do live in
[internal/serviceinstall](../serviceinstall/AGENTS.md).

Workflow definition behavior belongs to `internal/workflow/def`, and starter
content to `internal/workflow/starters`. This package discovers scopes, loads
project profiles for binding checks, calls the workflow APIs, and renders CLI
results.

**Command tree, flags, and the `--json` document shapes**:
[docs/references/ao-cli.md](../../docs/references/ao-cli.md), plus
`agent-overflow help` and `agent-overflow <group> --help`. Every usage string
lives in `usage.go`, so adding a subcommand without documenting it is an obvious
omission.

## The offline half

**`workflow new` namespaces EVERY sibling to the id being created, not only the
prompts** (`scaffoldFiles`). A scope is one flat directory shared by every
workflow in it, so an unscoped sibling (a helper script beside the prompts)
would collide with itself the second time the same starter is scaffolded there.
Only `.md` names are rewritten INSIDE the YAML, because `prompt:` is the one
field that references a sibling; anything else is bound by the project profile's
command map, which names an absolute path the user controls.

**`workflow validate <path>` and `workflow validate --id <id>` are the same
question, and both resolve a scope.** Scope is not only how a definition is
FOUND: it is what a `call:` edge resolves against and where the project
profile's bindings come from, so a path form reading no scope reports
`call.target` findings for a definition the id form validates clean. The path
form DERIVES its scope from where the file sits (`workflowScopeForPath`), which
is a fact about the file rather than something a caller should restate. A
definition under `<root>/projects/<slug>/workflows` names that project even when
the session works in a different one; one under `<root>/workflows` keeps
whatever project the caller or `AO_PROJECT` supplied, because a shared workflow
is checked against the project asking. A file outside the config root derives
nothing, and `--project` is how its author names the project it runs under.
`TestValidateByIDAndByPathAgreeOverEveryStarter` pins the round trip in both
scopes over every shipped starter.

`workflow list` with no rows prints `No workflows are configured here.`, plus,
when no project scope was resolved from either input, `Project workflows need
--project <slug>, or a session that sets AO_PROJECT.` A hint that cannot apply
is worse than none, so the second line appears only when the scope is missing.

`workflow list` and a failed `workflow validate --id` also report **skipped
directories** (`def.SkippedDirs`). Discovery is flat, so a hand-authored
`<id>/workflow.yaml` resolves to nothing at all, and "not found" for an id whose
file is visibly on disk is otherwise unexplainable. The note goes to **stderr**
in both modes, including `--json`, because it describes the directory rather
than a row of the requested result.

## The AO_* session contract

`session.go` owns these names — the reader of the contract declares them so the
writer cannot drift from it. The writer is `mintAOCredential`
(`app_session_runtime.go`), which builds the map; `sessionProcessEnv`
(`app_session.go`) is the one place it is merged into a provider subprocess's
environment.

| Variable | Set for | Meaning |
|---|---|---|
| `AO_ENDPOINT` | every app-started session | the app's loopback base URL, no token, no path |
| `AO_TOKEN` | every app-started session | scoped credential, valid exactly as long as the session |
| `AO_THREAD_ID` | every app-started session | the thread the session belongs to |
| `AO_PROJECT` | every app-started session with a project | the project's **slug**, what `--project` takes |
| `AO_RUN_ID` | workflow phase / unit sessions | the run this session is a phase of |
| `AO_PHASE_ID` | workflow phase / unit sessions | the phase (a unit carries its phase's id) |

This is the SESSION contract, not every AO_* name the app sets. A project's
worktree setup hooks get `AO_PROJECT_ROOT` / `AO_WORKTREE_PATH`
(`internal/workflow/profile/AGENTS.md`) and no credential at all, because they
are argv the user authored, not a session that may call back into the app.

Rules the package enforces:

- Both `AO_ENDPOINT` and `AO_TOKEN` absent means "not inside a session", a
  distinct typed message, because that is fixed by running the command
  somewhere else rather than by changing permissions.
- Exactly one of them present is a broken injection, reported as such rather
  than left to surface as an authorization failure.
- A 401 from the route means the credential was revoked, which for a scoped
  token means the session that owned it ended. The CLI says that in those words.
  There is no retry and no re-mint.
- A session with no credential (no transport server, no project, an
  unresolvable phase) gets no AO_* at all. Partial authority is never injected.
- `AO_PROJECT` is the OFFLINE half's only input. Execution commands scope
  themselves from the token, but `workflow list` / `workflow validate` resolve
  project definitions from a directory named by slug and have no app to ask, so
  `workflowProjectScope` defaults their scope from it and an explicit
  `--project` always wins. An empty answer therefore means neither input was
  present, which is what lets an empty listing say why.
- `workflow new` deliberately does NOT inherit it. `--project` is that command's
  write destination, not a filter: inheriting it would silently redirect where
  files land and leave no way to scaffold into the shared scope from inside a
  session.

## Wire

One `POST {AO_ENDPOINT}/rpc` per invocation, `Authorization: Bearer $AO_TOKEN`,
body and response are `transport.ClientFrame` / `transport.ServerFrame`. This
package imports those types rather than restating them, so the CLI and the
server cannot disagree about the wire. Method NAME only: numeric method ids
exist for generated bindings, and a CLI has none.

`run start --wait` polls (`waitForRun`): 500 ms, then x1.5, capped at 5 s,
`--timeout` default 30 m. A process that makes one call and exits has no
business owning a WebSocket with a replay ring behind it.

`run watch` is the same one-POST wire with the BLOCKING on the far side: each
call hands the app a cursor and is answered when the run tree moves, when the
caller's budget runs out, or at once if the run has stopped. It is deliberately
not a subscription, because giving scoped tokens a WebSocket would mean a replay
ring, a channel filter, and a second wire for one verb.

**Every backend hold sits under `rpcTimeout` (30 s), and that ordering is a
contract rather than a coincidence.** The watch hold is 25 s
(`maxWorkflowWatchHold`); the workflow engine's reply budget, how long a run
verb's answer waits for the runner start it dispatched, is 20 s
(`runnerStartReplyBudget`, `internal/workflow/engine/reply_budget.go`). A
backend that answers second turns a verb it has ALREADY COMMITTED into `context
deadline exceeded` here, and the operator's retry then meets an FSM refusal for
the state their first call produced (incident 2026-08-15). Lowering
`rpcTimeout`, or raising either hold, breaks it. Change them together.

`--json` prints the app's own result document verbatim, and human rendering
decodes only the fields it prints into narrow local structs, so the CLI never
becomes a second definition of what a run status looks like.

## Exit codes

| Code | Meaning (binary-wide, offline and execution alike) |
|---|---|
| 0 | success, including a surface-and-skip replay (the effect the caller asked for exists) |
| 1 | the answer was "no": validation findings, or a `--wait` whose run rested somewhere other than `done` |
| 2 | usage error, or an operational failure (no session, refused, backend unreachable) |
| 3 | `run watch` only: `--timeout` expired and the run is still going |
| 4 | `run watch` only: the app stopped answering, so the watch ended with no verdict |

3 and 4 exist because a supervisor branching on a watch's exit has to tell "the
run rested" from "I stopped looking" from "I was cut off". Collapsing either
into 1 or 2 makes the last two read as the run's own outcome.

## Run verb rules

- **`--refresh-def` (on `run resume` and `run rerun`) is the repair for "I
  edited the prompt of a parked phase".** A run freezes its whole resolved
  definition, prompt file contents inlined, at start, and renders that on every
  attempt. The flag re-reads the workflow and its prompts from disk and
  re-freezes what it read. It applies where a phase is entered FRESH: the engine
  refuses it on a bare resume of a continuable park, naming `--phase <id>` as
  the way to say "discard that attempt".
- **`run resume --at <when>` schedules that same bare resume**, on any park a
  bare resume continues, and it is strictly operator-authored: a typed provider
  usage refusal parks `provider-usage-limited` with no automatic schedule.
  `<when>` resolves against the APP's clock, because that is the clock the timer
  runs on. It refuses `--phase` and `--refresh-def`, both of which name a fresh
  entry when what is scheduled is the continuation.
- **`run watch` replaces the sleep loop.** It streams every transition and ends
  when the watched run rests, printing the app's own repair sentence
  (`wake.RepairSentence`) verbatim, because a CLI that reworded it would be a
  second answer to "which verb settles this". `--tree` widens it to the runs the
  watched run called, re-resolved on every wake. Three things it will not do:
  print a backlog (a first call answers where the run is, plus a cursor), skip
  silently (a cursor the ring cannot honour prints as a `gap`), or die quietly
  (one immediate retry, then the cause and exit 4).
- **`run amend --seed k=v` is `--refresh-def`'s counterpart for INPUTS.** It
  changes only the keys it names, is refused on a running run (an attempt reads
  its seeds when it starts) and on a terminal one, and validates each value
  through `def.ValidateInput`, the per-key half of the validator a start seed
  passes, so start and amend cannot judge one value by different rules.
- **`run guide <run-id> "<text>"` never interrupts.** There is no mid-turn
  injection at all: the text is delivered at the run's next FRESH phase entry,
  rendered into that attempt's prompt as a labelled quoted block, then cleared.
  The author is stamped app-side from the calling credential, never from the
  text, and both bounds on waiting entries are the app's.
- **Both state WHEN the run reads the change**, because the answer differs by
  park: an ordinary park's next attempt renders it, while a fan-out or call park
  is repaired IN PLACE by a bare resume and runs on the variables its attempt
  froze. A CALLED run may be amended or guided and reaches its own remaining
  phases only; amending one also says the caller's next start re-evaluates
  `args:` and will not carry the change.
- **`run resolve` settles `human:` routes only.** `--approve` and `--reject`
  name the routes the gate itself declared, so neither can be a default and
  supplying both is a usage error rather than a precedence rule. A `park:` route
  rests under the same `gate` reason and declares no approve/reject, so the
  engine refuses it naming `run resume`; the usage string, the composer repair
  map, and the wake all key that distinction off the `decision=` field `run
  status` renders per attempt (D41 amendment). `run answer` takes its text as a
  positional and `requireArgs` refuses a blank one, since an empty answer is a
  question still unanswered.
- **`run soft-stop`'s help states the one thing its exit code cannot**: a
  workflow with no call edge has no boundary to stop at, so the request is
  accepted and never fires (D36).

## What the human lines carry

Sized so the next verb is readable from the output (D38).

- `runView.line()` renders `parent=<run-id>`, so a campaign's flat `run list`
  shows its tree, and `failed-units=<ids>` on `run status`, so `run
  retry-unit`'s second argument is readable from the CLI.
- Both single-run reads print one `failed-unit=<id> note=<quoted>` line per
  failed unit that carries a note (`runView.writeFailedUnitLines`, shared by
  `run status` and `run inspect`). **A pause tears its in-flight units down
  `failed` with an interrupted note**: there is no interrupted unit status, so a
  reader given only ids and statuses reads their own pause as agent failures.
- `run status` alone renders one line per phase attempt
  (`runView.statusBlock`), because a gate consumed exactly one attempt's outputs
  and a reader deciding between `run resolve`, `run resume --phase`, and `run
  rerun` needs to know which one and what it ran with. `session=continued` marks
  an attempt that ran on a session an EARLIER attempt of the same phase started
  (a loop route's `session: continue`, an answered question, or a finalized
  takeover); the three are deliberately not distinguished, because they mean the
  same thing to a reader. `cause=` is the ENGINE's own park diagnosis (`store`
  v51's `park_cause`), absent for an attempt that rested on its own envelope.
- Both single-run reads print one **budget line** under the run line
  (`runView.writeBudgetLine`, shared so the two cannot spell a run's spend two
  ways). The app resolves every number through the same call the engine ENFORCES
  with and computes the percent itself; recomputing the share here would be a
  second answer to "how much is left". Units come from the ceiling's kind, so a
  reader never has to guess what 25 is. **A run with no ceiling prints no budget
  line at all**, because most runs are that run and a `budget=none` on each of
  them is a field a reader learns to skip.
- Untrusted values (park causes, unit notes, envelope outputs, memory prose and
  its cited paths) are quoted through `internal/untrustedtext` and bounded
  (`maxCauseRunes`, `maxUnitNoteRunes`). There is no leading data notice: a
  command result the caller asked for is not an injected message. Narrative
  CONTENT is printed verbatim, because it is the point of the command.
- Failed units and phase attempts are resolved on the single-run read only (a
  list would pay an extra query per row), and the acting verbs re-read through
  `reportRunState`, which prints the run line alone. A `run list` with no rows
  prints `No runs in this project.`; `--json` still prints the app's own `[]`.

## The read verbs

`run status` answers where a run is; `run inspect` and `run narrative` answer
what it IS, out of data that was already persisted. Neither adds state, and both
stay narrow for the reason `run status` does: an agent's context window pays per
byte. On `run inspect`, the per-phase output digest and the `--phase` drill-down
are exclusive on purpose, because the digest is what stands in for outputs
nobody named.

`run narrative` resolves its path through the same builders the wake reference
resolver uses (`app_workflow_narrative.go`), never one the CLI assembles.
`--unit <id>` reads on the try the unit ROW carries: the try is the one path
component a caller cannot see.

`run guide <run-id> "<text>"` is the other direction of a `notify:` gate: that
tells the watching thread what the run decided, this tells the run what the
watcher wants next. The run keeps working — the turn in flight is never
interrupted, and there is no mid-turn injection at all — and the text is
delivered at the run's next FRESH phase entry, rendered into that attempt's
prompt as a labelled, quoted block of operator guidance, then cleared. Before it
existed, redirecting a free-running campaign meant pausing it, editing, and
resuming, which throws away the turn in flight and, in a fan-out, the wave under
it. The output states WHEN the run reads it, because the answer differs by where
the run is resting: a `running` run reads it at the phase it advances or loops
into, while a run parked on a continuable reason is CONTINUED by a bare `run
resume` — not a phase entry — so its note names `run resume <id> --phase <id>` as
what enters one now. Both bounds (how many entries may wait, how long one may
be) are the app's and its refusal states the number, so this CLI does not
duplicate them. The author is stamped app-side from the calling credential — an
interactive session as a human operator, a phase session as that phase's run —
never from the text. Guiding a CALLED run reaches that run's own remaining
phases only, and the output says so.

`run resolve` is the one run verb outside the `runControl` family, because its
two directions are a DECISION rather than a state: `--approve` and `--reject`
name the routes the gate itself declared, so neither can be the default and
supplying both is a usage error, not a precedence rule. It settles `human:`
routes only — a `park:` route rests under the same `gate` reason but declares
no approve/reject, so the engine refuses it with a message naming `run resume`
as the repair, and the usage string, composer repair map, and wake all key the
distinction off the `decision=` field `run status` renders per attempt (D41
amendment). `run answer` takes its
text as a positional — it is the point of the command — and `requireArgs`
refuses a blank one, since an empty answer is a question still unanswered. Both
verbs need the `resolve` grant in a phase session; an interactive session holds
every listed method already.

The human lines carry what the next verb needs (D38): `runView.line()` renders
`parent=<run-id>` so a campaign's flat `run list` shows its tree, and
`failed-units=<ids>` on `run status` so `run retry-unit`'s second argument is
readable from the CLI. Both single-run reads then print one
`failed-unit=<id> note=<quoted>` line per failed unit that carries a note
(`runView.writeFailedUnitLines`, shared by `run status` and `run inspect`),
because the status is not the whole answer: **a pause tears its in-flight units
down `failed` with an interrupted note** — there is no interrupted unit status,
and `failed` is exactly what the repair verbs recover — so a reader given only
the ids and the status reads their own pause as a wave of agent failures. The
note is quoted as untrusted data (a runner's error text lands in it) and bounded
at `maxUnitNoteRunes`, like the park cause beside it; `run inspect --phase <id>`
prints it whole on the unit's own line. A unit with no note contributes no line —
the run line already named it. `run status` alone additionally renders one
`phase=<id> attempt=N status=… provider=… model=… effort=… cause=… session=… decision=<kind>->…`
line per phase attempt (`runView.statusBlock`), because a gate consumed exactly
one attempt's outputs and a reader deciding between `run resolve`, `run resume
--phase`, and `run rerun` needs to know which one and what it ran with. Kind and
target print as one field: they are one fact. `session=continued` marks an
attempt that ran on a session an EARLIER attempt of the same phase started — a
loop route's `session: continue`, an answered question, or a finalized takeover.
The three are deliberately not distinguished: they mean the same thing to a
reader (this round remembers the last one), the definition says which edge asked
for it, and no column records the mode — reusing the thread is what a
continuation is, so the two rows' shared thread id is the whole evidence. A cold
attempt carries no value at all, because that is every attempt of every run that
has never looped. `cause=` is the ENGINE's own
diagnosis of a park (`store` v51's `park_cause`) — the worktree that would not
cut, the phase missing from the snapshot, the budget that ran out — quoted as
untrusted data and bounded at `maxCauseRunes`, because a status block carries
one line per attempt. It is absent for every attempt that rested on its own
envelope, and for the reasons that name their own cause. Before it existed, an
engine-side park was diagnosable only from the filesystem.

Both single-run reads also print one **budget line** directly under the run line
(`runView.writeBudgetLine`, shared so `run status` and `run inspect` cannot
spell a run's spend two ways): `budget=<spent>/<ceiling> (<n>%)`, plus
`of-run=<id>` when the ceiling belongs to an ancestor, `estimated=true` when
part of the spend was priced from the app's rate table rather than reported by a
provider, `unpriced-rows=<n>` when some rows resolve to no rate at all (which
makes the figure a lower bound and is why the run will park at its next phase
boundary), and `exhausted=true` once it is crossed. The units come from the kind
— dollars, tokens, or a duration — so a reader never has to guess what 25 is,
and an unknown kind still prints its percent under a name rather than vanishing.
The app resolves every number through the same call the engine ENFORCES with and
computes the percent itself; a CLI that recomputed the share would be a second
answer to "how much is left". **A run with no ceiling prints no budget line at
all** — most runs are that run, and a `budget=none` on each of them is a field a
reader learns to skip on the one surface they scan for what the run needs.
Before this, a ceiling was enforced, announced once at the park, and invisible
every moment before that.

Failed units and phase attempts
are both resolved on the single-run read only — a list would pay an extra query
per row — and the acting verbs re-read status through `reportRunState`, which
prints the run line alone: "where is it now" is what they were asked, and a
spend line is not it. A `run
list` with no rows prints `No runs in this project.` rather than nothing,
because a blank answer reads as a command that did not work; `--json` still
prints the app's own `[]`.

## The read verbs, and their documented `--json` shapes

`run status` answers where a run is. `run inspect` and `run narrative` answer
what it *is*, and they exist because the alternative was measured: an agent
supervising a multi-day campaign ran 45 raw SQLite queries against the live
database and 79 hand-assembled narrative-file reads, because no verb exposed a
run's worktree, branch, seeds, called runs, or any attempt's outputs. Everything
they return was already persisted. Neither adds state; both stay narrow for the
reason `run status` does — an agent's context window pays per byte.

`run inspect <run-id>` is the one-call picture: the `run status` document
unchanged, plus the worktree/branch/base-branch the work happens on, the seeds
the run froze at start, the runs it called with the invocation that made each,
and — for the LATEST attempt of each phase — a bounded digest of that attempt's
envelope outputs. `--phase <id>` (optionally `--attempt <n>`) reads one attempt
whole instead: full envelope outputs, gate decision, the park cause printed
WHOLE on its own line (naming an attempt is how a caller says the bounded form
on the status line was not enough — the engine already capped what it stored),
and the fan-out units with each unit's status, try, branch, and worktree. The digests and the drill-down
are exclusive on purpose — the digest is what stands in for outputs nobody
named, so computing both would print the same values twice.

`run narrative <run-id> --phase <id>` prints the account one attempt wrote,
resolved through the same path builders the wake reference resolver uses
(`internal/workflowhost/narrative.go` → `NarrativeLookup` / its unit twin), never
a path the CLI assembles. `--unit <id>` reads a fan-out unit's account instead,
on the try the unit ROW carries: the try is the one path component a caller
cannot see, so asking for it would be asking them to guess.

Absence and a wrong coordinate are different answers and exit differently. An
attempt that wrote no narrative exits **1** with the path that was looked for —
it ran and left no account, which is a verdict, not a failure. A run, phase,
attempt, or unit that does not exist is refused by the app and exits **2**, and
the refusal names what the run actually has (`it has phases survey, review`), so
a typo is repaired without a second command.

The `--json` documents, which the CLI forwards verbatim as everywhere else:

```
run inspect  { run: <run status document, incl. seeds>,
               worktreePath?, branch?, baseBranch?,
               children: [{itemId, workflowId?, goal?, state, reason?,
                           parentPhaseId?, parentUnitId?, parentAttempt?}],
               guidance?: [{text, at, ageSeconds, by, byRun?}],
               phase?:   {phaseId, attempt, status, provider?, model?, effort?,
                          cause?,
                          outputs?: {<name>: <raw JSON value>},
                          decision?, decisionTarget?, exhaustedLoops?,
                          units: [{unitId, kind, status, unitAttempt, note?,
                                   branch?, worktreePath?, threadId?}]} }
run narrative { itemId, phaseId, attempt, unitId?, unitAttempt?,
                path, present, bytes?, truncated?, content? }
```

`run status --json` gains `budget` — `{kind, ceilingTokens|ceilingUsd|
ceilingMillis, spentTokens, spentUsd, elapsedMillis, percent, estimated,
unpricedRows?, exhausted, rootItemId?}`, absent entirely for a run with no ceiling, and carried
on `run inspect`'s nested `run` document the same way. It also gains
`seeds` (the run's frozen input object) and
`pendingGuidance` (how many `run guide` entries are waiting for the run's next
phase entry, absent at zero; `run inspect` carries the entries themselves under
`guidance`, bounded and aged); each entry of
`phases` carries `cause` (the engine's park diagnosis, absent when there is
none) and `session` (`"continued"`, absent for a cold attempt) on both verbs;
each entry of `failedUnits` carries `note` (what the unit rests with — how it
ended, or what a repair told its next try — absent when the row carries none),
as does each `phase.units` entry on `run inspect`; and `phases` gains `outputs` / `outputOverflow` — populated by `run
inspect` alone, absent on `run status`, whose projection stays envelope-free. Seeds print
on the human block too, one `seed <name>=<json>` line each, exactly as `run
output` prints a declared output; they stay off `runView.line()` because the
control verbs share it and "where is it now" is not a run's frozen inputs.

Envelope output values are quoted through `internal/untrustedtext` wherever they
reach a human line — digest and drill-down alike. They came out of a model and
are usually being read by one; there is no leading data notice, because a
command result the caller asked for is not an injected message, and the quoting
is what makes each value one unambiguous token. Narrative CONTENT is printed
verbatim: it is the point of the command, the header above it already says what
it is, and quoting prose into `\n` escapes would make it unreadable.

Both verbs take the same grants as `run status` (`introspect`, or `start-run`
for a run this phase started) and stay off the observe tier: a wider view of a
run the caller may already see is not a wider set of runs.

## Parsing

`--seed k=v` parses the value as JSON when it parses and treats it as a string
otherwise, so `--seed count=3` seeds a number and `--seed name=alice` seeds a
string with no shell quote round trip. A repeated key is an error, not a silent
overwrite. Flags may appear before, after, or between positionals
(`parsePermuted`), because Go's `flag` package stops at the first non-flag
token, which would make `agent-overflow run start flow --wait` read `--wait` as
a second workflow id. A literal `--` still ends flag parsing.

## `agent-overflow memory`, and why it is not `notes`

Campaign memory is a run TREE's accumulated lessons: append-only, many-authored,
keyed by the root run, injected into every element's prompt. `notes` is one
automation's continuity message to its own next occurrence: one mutable string,
replaced wholesale. Two shapes, two lifetimes, two verbs. Do not overload either.

- `memory add --kind <pattern|warning|learning|handoff>` records one note.
  `--run <id>` is only needed outside a phase session; a phase may name its own
  run or one it started, and nothing else in the project.
- `memory list` prints **oldest first**, the order the log holds and the order a
  reader scrolling a terminal wants. That is deliberately the opposite of the
  injected digest, which is newest-first because it is bounded and the reader
  may never reach the end. Unreadable lines (a note torn by a crash) are counted
  in the header rather than hidden.
- The kind is validated **before** the round trip, on **both** verbs, because
  one verb answering differently from the other would make the rule unlearnable.
  An empty `--kind` on `list` stays legal: it names no kind at all, and it is
  the unfiltered read.
- Neither verb takes a grant (`transport.GrantNotRequired`). Recording what the
  work learned is part of doing the work, exactly as returning an envelope is,
  and a `grants:` line between an element and its own campaign's memory would
  mean every workflow that forgot one silently relearns everything each wave.

## Grants and refusals

The CLI does not evaluate grants. `internal/transport` authorizes the method
against the token's scope, and the bound methods enforce which rows that scope
may touch. What this package owns is that both refusals reach the caller intact:
the typed `grant_required` message naming the missing grant, and the row-level
refusal naming the phase that may act only on the runs it started.

## `/workflow` composer context

`composer.go` is the pure renderer behind the `/workflow` composer command: it
takes a resolved `ComposerContext` (project name and **slug**, workflow source
directories, available workflows, active runs, whether the thread has a live
session, whether boot published the command on PATH) and returns the text block
injected
into the conversation. Lists are bounded (`MaxComposerWorkflows`,
`MaxComposerRuns`) and truncation is stated in the block, never silent. A
`CommandOnPath: false` block says so outright — it is the one place an agent
reads before typing the command, so a boot that could not publish the symlink
must not leave "command not found" as the first news of it. The project-scope
line names the slug `--project` takes (`--project <slug>` when there is none),
because the offline commands cannot infer it and a reader who has only the block
must still be able to write the flag. The app-side
resolver that produces the live data is
`workflowComposerBlock` (`app_workflow_host.go`); the split exists so the
block format is unit-testable without a database. That resolver is NOT a bound
method — the block never reaches the frontend. The composer holds only the
literal word `/workflow`, and the send path appends the block to the
provider-bound payload (D31, `app_chat_bar.go`).

The app-side resolver that produces the live data is `workflowComposerBlock`
(`app_workflow_composer.go`), and the split exists so the block format is
unit-testable without a database. That resolver is NOT a bound method: the block
never reaches the frontend. The composer holds only the literal word
`/workflow`, and the send path appends the block to the provider-bound payload
(D31, `app_composer_commands.go`).

The block also carries the **reason to verb repair map** (`composerRepair`,
D38): every park reason a CLI verb settles, the verb, and what taking it does.
That last part is load-bearing where adjacent reasons have different answers.
`provider-retries-exhausted` continues the parked attempt on the session its
turn died in, while `loop-limit-exhausted` needs `run resume --phase <id>` at an
earlier phase, because that fresh entry is the only one that refills the loop
budget. A reason absent from the map is one whose own cause is the instruction,
and the line under the table says so.

Under the table sits the one sentence the whole run-verb set turns on: **`run
resume` continues and preserves; `run resume --phase <id>` starts that phase
over**, re-running everything in it including the runs it called. It is stated
once, outside any row, because an agent needs it before it picks a row at all.
