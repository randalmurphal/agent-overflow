# internal/aocli/

Command routing and presentation for the CLI. Keep provider, persistence, and
app lifecycle concerns out of this package. Command functions accept arguments
and writers directly so tests do not spawn subprocesses.

**There is no separate CLI binary (D30).** The CLI *is* the app binary,
dispatched by verb: `agent-overflow run start <id>`, `agent-overflow workflow
list`. `main.go` matches `os.Args[1]` against `Commands()` — this package's
dispatch table, exported so no second copy of the verb set can rot — and hands
the whole argv to `Run`. Agent sessions find the binary because boot publishes
a canonical-name symlink at `<configDir>/bin/agent-overflow` and
`sessionProcessEnv` prepends that directory to every session's PATH. Every
user-facing string here therefore says `agent-overflow`, never `ao`; the AO_*
environment variable names are an internal contract and keep their prefix.

The CLI has two halves:

- **Offline** (`agent-overflow workflow …`) — scope discovery, definition
  validation, listing, scaffolding, and the embedded authoring schema. Needs no
  running app and no credential.
- **Execution** (`agent-overflow run …`, `agent-overflow notes …`,
  `agent-overflow schedule …`) — one HTTP POST per invocation against a running
  Agent Overflow, authenticated with the scoped credential the app injected
  into the calling agent's session (spec §5, D15).

Workflow definition behavior belongs to `internal/workflow/def`; this package
discovers scopes, loads project profiles for binding checks, copies embedded
starter sources, calls the workflow APIs, and renders CLI results. Starter
content and embedding belong to `internal/workflow/starters`.

**`workflow new` namespaces EVERY sibling to the id being created, not only the
prompts** (`scaffoldFiles`). A scope is one flat directory shared by every
workflow in it, so an unscoped sibling — a helper script beside the prompts —
would collide with itself the second time the same starter is scaffolded there,
and the second scaffold would be refused naming a file the user never chose.
Only `.md` names are rewritten INSIDE the YAML, because `prompt:` is the one
field that references a sibling; anything else is bound by the project profile's
command map, which names an absolute path the user controls.

**`workflow validate <path>` and `workflow validate --id <id>` are the same
question, and both resolve a scope.** Scope is not only how a definition is
FOUND: it is what a `call:` edge resolves against and where the project
profile's bindings come from, so the path form reading no scope at all reported
`call.target` findings for a definition the id form validated clean — the same
file, two answers, and the phantom one is what an author sees when they validate
what they just edited. The path form DERIVES its scope from where the file sits
(`workflowScopeForPath`), because that is a fact about the file rather than
something a caller should have to restate: a definition under
`<root>/projects/<slug>/workflows` is project-scoped and names that project even
when the session works in a different one, and one under `<root>/workflows` is
shared-scoped and keeps whatever project the caller or `AO_PROJECT` supplied —
a shared workflow is visible to every project and is checked against the one
asking, which is exactly what `--id` does with it. A file outside the config
root derives nothing; `--project` is how its author names the project it is
meant to run under, which used to be refused outright with a path.
`TestValidateByIDAndByPathAgreeOverEveryStarter` pins the round trip in both
scopes over every shipped starter.

## The AO_* session contract

`session.go` owns these names — the reader of the contract declares them so the
writer cannot drift from it. The writer is `mintAOCredential`
(`app_ao_session.go`), which builds the map; `sessionProcessEnv`
(`app_session.go`) is the one place it is merged into a provider subprocess's
environment.

| Variable | Set for | Meaning |
|---|---|---|
| `AO_ENDPOINT` | every app-started session | the app's loopback base URL, no token, no path |
| `AO_TOKEN` | every app-started session | scoped credential, valid exactly as long as the session |
| `AO_THREAD_ID` | every app-started session | the thread the session belongs to |
| `AO_PROJECT` | every app-started session with a project | the project's **slug** — what `--project` takes |
| `AO_RUN_ID` | workflow phase / unit sessions | the run this session is a phase of |
| `AO_PHASE_ID` | workflow phase / unit sessions | the phase (a unit carries its phase's id) |

This is the *session* contract, not every AO_* name the app sets: a project's
worktree setup hooks get `AO_PROJECT_ROOT` / `AO_WORKTREE_PATH`
(`internal/workflow/profile/AGENTS.md`) and no credential at all, because they
are argv the user authored, not a session that may call back into the app.

Rules the package enforces:

- Both `AO_ENDPOINT` and `AO_TOKEN` absent means "not inside a session" — a
  distinct, typed message, because that is fixed by running the command
  somewhere else rather than by changing permissions.
- Exactly one of them present is a broken injection, reported as such rather
  than left to surface as an authorization failure.
- A 401 from the route means the credential was revoked, which for a scoped
  token means the session that owned it ended. The CLI says that in those
  words; there is no retry and no re-mint.
- A session with no credential (no transport server, no project, an
  unresolvable phase) gets no AO_* at all. Partial authority is never injected.
- `AO_PROJECT` is the **offline** half's only input. The execution commands scope
  themselves from the token, but `workflow list` / `workflow validate` resolve
  project definitions from a directory named by slug and have no app to ask — so
  `workflowProjectScope` defaults their scope from it, and an explicit
  `--project` always wins. An empty answer therefore means neither input was
  present, which is what lets an empty listing say why. A malformed value is
  reported as `AO_PROJECT: invalid project slug …`, because the fix is not
  something the caller typed.
- `workflow new` deliberately does **not** inherit it. `--project` is that
  command's write destination, not a filter: inheriting it would silently
  redirect where files land and leave no way to scaffold into the shared scope
  from inside a session. Shared scope is always included by the read commands, so
  a shared scaffold is still listed and validated by the next command run.

## Wire

One `POST {AO_ENDPOINT}/rpc` per invocation, `Authorization: Bearer $AO_TOKEN`,
body and response are `transport.ClientFrame` / `transport.ServerFrame` — this
package imports those types rather than restating them, so the CLI and the
server cannot disagree about the wire. Method NAME only: numeric method ids
exist for generated bindings, and a CLI has none.

`run start --wait` polls (`waitForRun`): 500 ms, then x1.5, capped at 5 s,
`--timeout` default 30 m. A process that makes one call and exits has no
business owning a WebSocket with a replay ring behind it.

`run watch` is the same one-POST wire with the BLOCKING on the far side: each
call hands the app a cursor and is answered when the run tree moves, when the
caller's stated budget runs out, or at once if the run has already stopped. The
CLI never sleeps — between two calls it is printing — and it is deliberately not
a subscription, because giving scoped tokens a WebSocket would mean a replay
ring, a channel filter, and a second wire for one verb. The app's hold
(`maxWorkflowWatchHold`, 25 s) sits under `rpcTimeout` (30 s) so the client is
always the one still waiting when the server answers; the same bound is the
worst case for noticing a revoked credential, which arrives as the route's 401
and ends the watch with the message rather than a hang.

`--json` prints the app's own result document verbatim. Human rendering decodes
only the fields it prints into narrow local structs, so the CLI never becomes a
second definition of what a run status looks like.

## Exit codes

Binary-wide, offline and execution alike:

| Code | Meaning |
|---|---|
| 0 | success, including a surface-and-skip replay (the effect the caller asked for exists) |
| 1 | the answer was "no": validation findings, or a `--wait` whose run rested somewhere other than `done` |
| 2 | usage error, or an operational failure (no session, refused, backend unreachable) |
| 3 | `run watch` only: `--timeout` expired and the run is still going |
| 4 | `run watch` only: the app stopped answering, so the watch ended with no verdict |

3 and 4 exist because a supervisor branching on a watch's exit has to tell
"the run rested" from "I stopped looking" from "I was cut off", and collapsing
either into 1 or 2 makes the second and third read as the run's own outcome —
the silent-monitor failure the verb was built to end.

## Command tree

```
agent-overflow <command>
  workflow new|validate|list|schema            offline (each takes --config-root; put it after
                                               the subcommand — aocli's root FlagSet would accept
                                               a leading one, but the binary's entry router only
                                               recognises a leading verb, so a flag-first
                                               invocation never reaches these commands)
  run start <workflow-id> [--scope|--goal|--seed k=v|--base-branch|--step|--wait|--timeout|--json]
  run status|wait|output <run-id> [--json] [--timeout]
  run watch <run-id> [--tree] [--timeout <dur>] [--json]
  run inspect <run-id> [--phase <id> [--attempt <n>]] [--json]
  run narrative <run-id> --phase <id> [--attempt <n>] [--unit <id>] [--json]
  run list [--active] [--json]
  run pause|cancel <run-id> [--json]
  run soft-stop <run-id> [--clear] [--json]
  run resume <run-id> [--phase <id>] [--refresh-def] [--at <when>] [--json]
  run rerun <run-id> [--guidance <text>] [--refresh-def] [--json]
  run retry-unit <run-id> <unit-id> [--note <text>] [--json]
  run retry-failed-units <run-id> [--note <text>] [--json]
  run resolve <run-id> --approve|--reject [--note <text>] [--json]
  run answer <run-id> <text> [--json]
  run amend <run-id> --seed k=v [--seed k2=v2]... [--json]
  run guide <run-id> "<text>" [--json]
  memory add --kind <kind> "<note>" [--file <path>]... [--run <id>] [--json]
  memory list [--kind <kind>] [--run <id>] [--json]
  notes get|set <automation-id> [--file <path>] [--json]
  schedule <workflow-id> --cron <expr> [--name|--scope|--seed k=v|--json]
```

Every usage string lives in `usage.go` so adding a subcommand without
documenting it is an obvious omission. `run soft-stop`'s help states the one
thing its exit code cannot: a workflow with no call edge has no boundary to
stop at, so the request is accepted and simply never fires (D36). A verb that
succeeds and then does nothing has to say so where the caller is already
looking.

`--refresh-def` (on `run resume` and `run rerun`) is the repair for "I edited
the prompt of a parked phase". A run freezes its whole resolved definition —
prompt file contents inlined — at start and renders that on every attempt, which
is invisible until it bites: the operator edits the file, resumes, and watches
the run park again on the prompt it already had. The flag re-reads the workflow
and its prompt files from disk for that entry and re-freezes what it read, so
the edited version is what runs from there on. It applies where a phase is
entered FRESH — the engine refuses it on a bare resume of a continuable park,
naming `--phase <id>` as the way to say "discard that attempt" — and it is
needed only for repair: between campaign waves the call edge already resolves
its target from disk on every invocation. The usage strings and the composer
repair-map footer both say so, because the freeze is the kind of default a
caller only learns by being bitten.

`run resume --at <when>` schedules that same bare resume instead of taking it
now, on any park a bare resume continues. It is strictly operator-authored: a
typed provider usage refusal parks `provider-usage-limited` without retries or
an automatic schedule, and a normal resume remains free to make a real attempt
at any time. `<when>` is RFC 3339 or a `+`-prefixed duration, resolved against
the APP's clock because that is the clock the timer runs on, and the command
prints the moment it armed rather than a run state that has deliberately not changed.
It refuses `--phase` and `--refresh-def`: both name a fresh entry, and what is
being scheduled is the continuation. Anything that repairs the run in the
meantime disarms the schedule.

`run watch` is the verb that replaces the sleep loop. `run wait` polls for one
run's final state; this one streams every transition as it happens and ends when
the watched run rests. One line per transition — UTC timestamp, run id, phase and
attempt, `old->new`, the typed reason, and the park cause quoted through
`internal/untrustedtext` and bounded exactly as `run status` bounds it — then a
final line carrying the resting state and the app's own repair sentence (the one
`wake.RepairSentence` composes, printed verbatim, because a CLI that reworded it
would be a second answer to "which verb settles this"). `--tree` widens the watch
to the runs the watched run called, which for a campaign is where every
transition happens; the set is re-resolved on every wake, so a wave started while
the call was blocked is watched from its birth transition. `--json` is NDJSON —
one line per transition, forwarding the app's own objects, the run document last
— because a stream cannot be one document. Its own events (gap, timeout,
disconnect) are objects on the same stream rather than prose, so every line
parses.

Three things it will not do. It will not print a backlog: a first call is
answered with where the run is and the cursor to continue from, because the
caller asked what happens NEXT and replaying a campaign's retained history into
an agent's context is the opposite of that. It will not skip silently: a cursor
the app's ring cannot honour comes back as a `gap`, which is printed, since a
monitor that quietly dropped transitions is a monitor lying about the run. And it
will not die quietly: a transport failure is retried once and immediately (a
torn localhost hop — the Windows↔WSL2 relay's clean FIN — is not a dead app), and
a second failure prints the cause and exits 4.

`run amend --seed k=v` is `--refresh-def`'s counterpart for INPUTS: the same
"I got one value wrong and the run is parked" repair, for a seed instead of a
prompt. It changes only the keys it names, is refused on a running run (an
attempt reads its seeds when it starts) and on a terminal one (nothing is left to
read them), and validates each value through `def.ValidateInput` — the per-key
half of the very validator a start seed passes through, so a value accepted at
start and one accepted later cannot be judged by different rules. An undeclared
key is refused naming the declared ones. The output states WHEN the run reads the
change, because the answer is not the same for every park: an ordinary park's
next attempt renders it, while a fan-out or call park is repaired IN PLACE by a
bare resume and runs on the variables its attempt froze, so its note names `run
resume <id> --phase <id>` as what enters a phase fresh now. A CALLED run may be
amended — its seeds are its own row and its remaining phases read it — and the
result then also says that the next run its caller starts re-evaluates the
caller's `args:` and will not carry the change, naming the root to amend instead.

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
readable from the CLI. `run status` alone additionally renders one
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
(`app_workflow_narrative.go` → `workflowNarrativeLookup` / its unit twin), never
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
                          units: [{unitId, kind, status, unitAttempt,
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
none) and `session` (`"continued"`, absent for a cold attempt) on both verbs, and gains `outputs` / `outputOverflow` — populated by `run
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
for a run this phase started) and are `LocalOnly`: a wider view of a run the
caller may already see is not a wider set of runs, so a read grant of their own
would have been ceremony.

`workflow list` follows the same rule and adds the one cause a blank answer has:
with no rows it prints `No workflows are configured here.`, plus — when no
project scope was resolved from either input — `Project workflows need --project
<slug>, or a session that sets AO_PROJECT.` A hint that cannot apply is worse
than none, so the second line appears only when the scope really is missing.

Both `workflow list` and a failed `workflow validate --id` also report
**skipped directories** (`def.SkippedDirs`): discovery is flat, so a
hand-authored `<id>/workflow.yaml` resolves to nothing at all, and "not found"
for an id whose file is visibly on disk is otherwise unexplainable. The note is
`note: <path> is a directory and was skipped — a workflow is a flat <id>.yaml
beside its <id>-*.md prompts`. It goes to **stderr** in both modes, including
`--json`: it describes the state of the directory rather than a row of the
requested result, so the JSON document stays exactly the list and a machine
reader still cannot lose the information. `validate --id` carries the same notes
inside the not-found error, where the caller is already looking.

`--seed k=v` parses the value as JSON when it parses, and treats it as a string
otherwise: `--seed count=3` seeds a number, `--seed name=alice` seeds a string,
with no shell quote round trip. A repeated key is an error, not a silent
overwrite.

Flags may appear before, after, or between positionals (`parsePermuted`): Go's
`flag` package stops at the first non-flag token, which would make
`agent-overflow run start flow --wait` read `--wait` as a second workflow id. A
literal `--` still ends flag parsing.

## `agent-overflow memory`, and why it is not `notes`

Campaign memory is a run TREE's accumulated lessons: append-only, many-authored,
keyed by the root run, injected into every element's prompt. `notes` is one
automation's continuity message to its own next occurrence: one mutable string,
replaced wholesale. Two shapes, two lifetimes, two verbs — do not overload
either.

- `memory add --kind <pattern|warning|learning|handoff> "<note>" [--file <path>]...`
  records one note. `--run <id>` is only needed outside a phase session (a
  phase's own run is the default); a phase may name its own run or one it
  started, and nothing else in the project.
- `memory list [--kind <k>]` prints them **oldest first** — the order the log
  holds, and the order a reader scrolling a terminal wants. That is deliberately
  the opposite of the injected digest, which is newest-first because it is
  bounded and the reader may never reach the end. Unreadable lines (a note torn
  by a crash) are counted in the header rather than hidden: a reader deciding
  whether the memory is complete has to know one was lost.
- The kind is validated **before** the round trip, on **both** verbs. A typo is a
  usage error the caller fixes on the spot and the usage string already answered;
  a wire refusal for it would be a round trip that taught nothing new, and one
  verb answering differently from the other would make the rule unlearnable. An
  empty `--kind` on `list` is the exception that is not one: it names no kind at
  all, it is the unfiltered read, and it stays legal.
- Neither verb takes a grant (`transport.GrantNotRequired`). Recording what the
  work learned is part of doing the work, exactly as returning an envelope is,
  and a `grants:` line between an element and its own campaign's memory would
  mean every workflow that forgot one silently relearns everything each wave.
  The authority that applies is row-level and enforced app-side.
- Note prose and cited paths are quoted through `internal/untrustedtext` on the
  human lines, like `run inspect`'s output values: they came out of a model and
  are usually being read by one.

## Grants and refusals

The CLI does not evaluate grants — `internal/transport` authorizes the method
against the token's scope, and the bound methods enforce which rows that scope
may touch. What this package owns is that both refusals reach the caller
intact: the typed `grant_required` message naming the missing grant, and the
row-level refusal naming the phase that may act only on the runs it started.

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
`workflowComposerBlock` (`app_workflow_composer.go`); the split exists so the
block format is unit-testable without a database. That resolver is NOT a bound
method — the block never reaches the frontend. The composer holds only the
literal word `/workflow`, and the send path appends the block to the
provider-bound payload (D31, `app_composer_commands.go`).

The block also carries the **reason→verb repair map** (`composerRepair`, D38):
every park reason a CLI verb settles, the verb, and what taking it does — the
last part is load-bearing where adjacent reasons have different answers:
`provider-retries-exhausted` continues the parked attempt on the session its
turn died in, while `loop-limit-exhausted` needs `run resume --phase <id>` at an
earlier phase because that fresh entry is the only one that refills the loop
budget. Legacy `retries-exhausted` names both possibilities without guessing.
`stuck` also needs the effect spelled out (`run resume` re-enters the parked phase once the blocker the
phase named is cleared; `--refresh-def` re-reads the definition when the fix was
an edit to it). A reason
absent from the map is one whose own cause is the instruction, and the line
under the table says so, because an unexplained omission is what makes an agent
invent a verb. The same line names who may decide a gate or a question: the
`resolve` grant in a phase, implicitly in an interactive session. Both tables
render through `writeComposerRows`, which pads the left column in runes at
render time, so editing a row cannot leave the block ragged.

Under the table sits the one sentence the whole run-verb set turns on: **`run
resume` continues and preserves; `run resume --phase <id>` starts that phase
over**, re-running everything in it including the runs it called. It is stated
once, outside any row, because it is what an agent has to know before it picks a
row at all — a `unit-failed` park is repaired by `run retry-failed-units` (the
join included) or by a bare resume, and by `--phase` only when re-doing the wave
is the actual intent.

## Files

| File | Responsibility |
|---|---|
| `run.go` | Root dispatch table (`Commands` / `IsCommand` / `Run`), offline workflow commands, exit codes, output helpers. |
| `workflow_scopes.go` | Scope discovery and resolution: config-root, source directories, `ResolveConfigured`, call resolvers, listing. |
| `usage.go` | Every usage string. |
| `session.go` | The AO_* contract, session resolution, and the scoped HTTP RPC client. |
| `exec.go` | Execution-command skeleton: seed parsing, permuted flag parsing, JSON/human rendering. |
| `exec_run.go` | `agent-overflow run …`. |
| `exec_run_view.go` | What a run, its budget, and its phase attempts look like on a human line. |
| `exec_run_inspect.go` | `run inspect` and `run narrative`: the two whole-run reads and their blocks. |
| `exec_run_watch.go` | `run watch`: the long-poll loop, its transition and summary lines, and the timeout / gap / disconnect events that are the CLI's own. |
| `exec_run_amend.go` | `run amend`: the seed change and the block stating when the run reads it. |
| `exec_run_guide.go` | `run guide`: the steer and the block stating when the run reads it. |
| `exec_memory.go` | `agent-overflow memory add` / `memory list` and the log's human block. |
| `exec_automation.go` | `agent-overflow notes …` and `agent-overflow schedule`. |
| `composer.go` | The `/workflow` block renderer. |
| `workflow_files.go`, `workflow_new.go`, `profile.go` | Definition discovery, scaffolding, profile binding checks. |
