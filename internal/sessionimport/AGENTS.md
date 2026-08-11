# internal/sessionimport/

The store side of session import. Two halves:

- the **writer** — `[]importir.Event` in, one `store.ImportBatch` out —
  which is what makes an imported provider session indistinguishable from
  one AO ran itself;
- the **orchestrator** (`Scan` / `ImportOne` / `Cursor`) — which sessions
  are importable, which thread rows they become, and where a refresh
  resumes.

Provider-specific reading stays in `internal/provider/claude/sessionimport`
and `internal/provider/codex/rollout`; those two never see `store` or
`triage`, and this package never parses a session file.

It deliberately does **not** drive `triage.Router`. The Router has
live-only side effects (session-ref updates, thread-activity bumps,
`now()`-stamped usage, async settle goroutines) and would persist an
imported prompt as an "Injected provider context" notification, because
no pending-send entry ever registered it. Instead every row shape comes
from triage's exported, Router-free shaping helpers — see
[`internal/triage/AGENTS.md` §Exported shape surface](../triage/AGENTS.md).
`parity_test.go` is what keeps that honest.

## Layout

- `writer.go` — `Writer` / `NewWriter` / `Build`, the per-event dispatch,
  the shared row plumbing (item-index allocation, provenance stamping,
  the writer's control meta keys, meta helpers), and the invariant-23
  force-close of tools a turn boundary left unresolved.
- `items.go` — the per-kind row builders for the standalone rows:
  prompts, subagent prompts, proposed plans, compaction boundaries,
  errors, notifications, command results.
- `blocks.go` — the streaming-block family: `assistant_text` and
  `thinking` rows, the open-block bookkeeping that lets consecutive
  deltas accumulate into one row, and the content-block-stop settle.
- `tools.go` — the tool-call lifecycle: launch, in-place completion,
  Codex's split `tool_completion` sibling, the backgrounded-Task
  terminal, file-change result payloads, diff / command-output payloads.
- `turns.go` — `turnState`: turn allocation, adoption of a provider's
  own turn id, settle, and the seal every unfinished turn gets.
- `usage.go` — `usage_ledger` row projection for a settled turn.
- `orchestrator.go` — `Scan`: what is importable. Provider availability,
  the three dedup subtractions, the origin marker, per-row warnings.
- `projectindex.go` — `projectIndex`: which project each row is grouped
  under, and the per-cwd memo that makes answering it once per distinct
  cwd rather than once per row. See "Which project a row lands under".
- `import_one.go` — `ImportOne`: one scanned row → thread rows, history,
  and cursors. `resolveProject` prefers the `ProjectID` the scan already
  stamped and falls back to `project.EnsureForWorkspace` — so the project
  a row was LISTED under is the project it imports into, including for a
  dead worktree the index could only place from a registration. Claude
  branches are converted and applied ONE AT A TIME
  (`LoadedSession.ConvertBranch`), so a multi-branch transcript never
  holds every branch's events at once.
- `import_apply.go` — `sessionImporter`: the per-session accumulator
  `ImportOne` drives — thread creation, warnings, whole-session
  rollback, and `settleBranches` (the sole-survivor retitle and the
  session-ref stamp).
- `cursor.go` — `Cursor` + `Diverged` + `Advance`: where an import
  stopped, in the thread and in the source file.
- `refresh.go` — `PlanUpdate` / `ApplyUpdate`: what a re-read of the
  source file would add to a thread that was already imported.

`Scan`, `ImportOne` and `ApplyUpdate` are the only things here that
WRITE. The writer half (everything above them) reads the thread row and
writes nothing —
`store.ApplyImportBatch` is what commits, and `ImportOne` is its caller.

## Contract

```go
w := sessionimport.NewWriter(store, thread)
batch, warnings, err := w.Build(events)   // store-pure
err = store.ApplyImportBatch(thread.ID, batch)   // caller commits
```

`Build` reads the thread's existing history (to know which turn index a
refresh continues from) and otherwise touches nothing. The caller owns
the commit, so a whole session lands in one transaction.

Errors vs warnings:

- **Error** — structurally broken input, and the whole import is
  refused: a tool completion with no launch AND no `import_unavailable`
  marker, an item-producing event with no timestamp or no source
  coordinate, a turn_complete with no typed payload, two rows claiming
  one item id, a turn id the thread already holds. These mean the reader
  handed us something it should not have, and a half-shaped thread is
  worse than a refused one.
- **Warning** — recoverable, and the rest of the rows still import: an
  event kind with no row mapping, a payload naming no tool call, a
  background terminal with no launch, a subagent prompt with no provider
  item id, a tool-meta shaping hiccup.

## Writer input contract

What the writer needs from a provider reader, beyond a well-formed
`provider.ProviderEvent`. Everything here is "be liberal where it is
cheap": an absent optional key degrades one field, never the import.

| Field | Required | Meaning |
|---|---|---|
| `Event.Timestamp` | **yes**, on every item-producing event | The row's clock. Imported rows carry the provider's own times end to end; there is no now() fallback, because a thread whose history claims it happened at import time is worse than no thread. |
| `Event.SourceUUID` | **yes**, on every item-producing event | Provenance, for BOTH providers. Claude readers set the transcript row's `uuid`; a Codex rollout record has no id of its own, so its reader mints `line:<byte offset of the line's first byte>`. Absent is refused — the refresh cursor is the one reader that depends on this. |
| `Event.SourceOffset` | Codex only | An OPTIONAL resume position (one byte past the line's newline), not a second spelling of the provenance stamp. Claude leaves it zero. |
| `Event.ItemID` | yes for tool events | The provider tool_use id. It is the row id and the only thing correlating a completion with its launch. |
| `Event.ParentToolUseID` | when nested | Becomes `Item.ParentID`, nesting subagent rows under their launching tool_call. A launch may also carry it as `meta.parent_tool_use_id`, which the writer falls back to (triage's metaUpdateOnly path reads the same key). |
| `Event.TurnID` | Codex only | Adopted as the `turns` row primary key and `provider_turn_id`. Claude readers leave it empty and the writer synthesizes `<threadID>:<turnIndex>`, exactly as triage does. |
| `Event.TurnComplete` | on `EventTurnComplete` | One of the typed `provider.*TurnCompleteMeta` payloads. Never a JSON blob in `Meta` — this is the same rule triage enforces. `ModelUsage` is what becomes usage-ledger rows. |

Meta keys the writer reads by name. The first two are CONTROL keys —
the writer routes on them and `providerMeta` strips both, so neither
ever reaches `items.meta` in its wire spelling:

| Key | On | Meaning |
|---|---|---|
| `import_unavailable` | any item-producing event | The session file no longer holds this item's payload. The value is the REASON (`tool-output-gc`, `exec-detail`); the writer strips the key from the stored provider meta and re-stamps it through `itemmeta.MarkImportUnavailable` so there is exactly one persisted spelling. Set it on the event that ADDRESSES the row — a completion whose output is gone marks its launch row. It is ALSO what makes an orphan legal: a `tool_completion` carrying it with no launch in the batch synthesizes a placeholder `running` launch row and marks that, because the reader emitted the completion precisely to say "the launch is not in this file". Without the marker the same event is still a hard error. |
| `blockType` | `EventContentBlockStop` | Which settled block this is (`text` / `thinking`). Pure framing: the live path reads it to pick a settle and never persists it, so the writer strips it too. |
| `provider_item_id` | user text | The wire id of the prompt. Top-level prompts store it as a correlation key; a subagent prompt is KEYED on it (`user:wire:<id>`) and is dropped with a warning when absent, matching triage. |
| `parent_uuid` | top-level user text | Claude's transcript parent, stored as `provider_parent_uuid`. Optional. |
| `summary` | `EventCompactBoundary` | The committed compaction summary. Lifted into an on-demand `compaction` payload and removed from the row meta, so `items.meta` stays a cheap `{trigger}` blob. |
| `api_error_enum` | `EventError` | Promotes the row from `error` to `api_error` kind and keeps the meta, so the frontend can render the actionable branch. |
| `tool_use_result` / `item.changes` | tool events | The provider's structured file-change result. Claude and Codex shapes both route through triage's own extractors — do not pre-normalize one into the other. |
| `toolName`, `input`, `is_background`, `exit_code`, `is_error`, `item_status`, `task_id` | tool events | Decoded by `triage.DecodeToolStartMeta` / `DecodeToolCompleteMeta`. Same keys the live parsers emit; a reader that renames one silently loses the field. |

## Event kind → row

| Event kind | Row |
|---|---|
| `EventTurnStart` | Opens a `turns` row. Adopts an implicitly-opened turn when one is already open and empty. |
| `EventTurnComplete` | Settles the open turn (`TurnCompletion` + `usage_ledger` rows) and force-closes its unresolved tools. |
| `EventUserText`, top-level | `user_text` row `user:<turn>`, role `user`, and it OPENS the turn (Claude transcripts have no turn-start row). Meta is the two correlation keys only. |
| `EventUserText`, parented | `user_text` row `user:wire:<provider_item_id>` nested under the launching tool_call, meta `{provider_item_id, wire_only:true}`. |
| `EventTextDelta` | `assistant_text` row `text:<turn>[:<scope>]:<seg>` (0-based per turn+scope) + an `assistant_text` payload. Consecutive events for one provider item id accumulate into ONE row, exactly as live deltas do. |
| `EventThinking` | `thinking` row `think:<turn>[:<scope>]:<block>` + a `thinking` payload; summary is the bounded tail preview. |
| `EventToolStart` | `tool_call` row keyed on the tool_use id, status `running`. Heavy input (Edit/Write/MultiEdit/NotebookEdit) promotes to a `tool_call_input` payload. A second start for the same id ANNOTATES the row (Claude's `task_started` mapping, Codex child-thread labels) — it never creates a second row. |
| `EventToolComplete` | Settles that SAME row in place: status, summary, merged meta, result payload. Three carve-outs: a backgrounded launch stays running (its terminal writes the sibling), Codex `wait_agent`/`resume_agent` get a `tool_completion` sibling, and a file-change result becomes a `tool_result` payload BEFORE the summary derivation so the row reads "Edited x.go". |
| `EventBackgroundTaskTerminal` | Settles the backgrounded launch and appends its `tool_completion` sibling (`complete:<launchID>`). |
| `EventDiff`, `EventCommandOutput` | Replace the addressed tool_call's payload. Imported events carry whole blobs, so there is no delta-append branch. |
| `EventCompactBoundary` | `compaction` divider row + on-demand summary payload. |
| `EventError` | `error` or `api_error` row `error:<turn>[:<scope>]:<seq>` (0-based). |
| `EventNotification` | `notification` row. |
| `EventCommandResult` | `command_result` row, with the full text in a payload above the inline bound. |
| `EventContentBlockStop` | The SETTLED whole block: an `assistant_text` / `thinking` row selected by the `blockType` meta, exactly as `EventTextDelta` / `EventThinking` build one. This is not framing — the Codex reader speaks the live Codex adapter's vocabulary, where an `agentMessage` / `reasoning` item completing arrives ONLY as a content-block stop carrying the whole text (triage: `blockTypeForStop` → `settleStreaming*Async` → `persistOrUpdateCompleted*Item`). A stop against an OPEN block replaces its content rather than appending, and closes it either way, so two consecutive agent messages are two rows. A stop with no `blockType` warns and writes nothing: live resolves it from the block it has open, and no import reader emits one. |
| `EventProposedPlan` | `tool_call` row named `plan`, status completed, plan markdown in a `proposed_plan` payload — the shape `handleProposedPlan` writes, including its fallback to the tool name when the plan has no title. `items.kind` has no plan kind, which is why a plan rides the timeline as a tool call. |
| `EventTokenUsage`, `EventInit`, `EventSessionStatus`, `EventRateLimits`, `EventContentBlockStart` | Nothing. Live-only signals: the context meter, the session handshake, and the block OPENING an imported event already carries whole. |
| anything else | Nothing, plus an `import.unmapped-event` warning. A provider that grows a kind must not fail an import. |

## Scan, import, cursor

```go
d := sessionimport.Deps{Store: st,
    ClaudeProjectsDir: claudeHome + "/projects", CodexHome: codexHome}

result, err := sessionimport.Scan(ctx, d, sessionimport.Filter{})
outcome, err := sessionimport.ImportOne(ctx, d, row)
```

Both provider homes are INJECTED. Nothing here calls `os.UserHomeDir` —
which home is in play is an app-level decision (credential-home override,
WSL relocation) and a library guess would list sessions the app cannot
resume.

`Deps` carries **no git handle**. Resolving a cwd to a project is pure
filesystem reads (`internal/gitroot`), which is the only reason it can run
per row: a `git rev-parse` per distinct cwd would be a subprocess storm on
a real home, and its `--show-toplevel` answer is the wrong one anyway.

`Scan` subtracts three things, and all three are what makes "Import All"
safe to press twice:

1. **Sessions AO already knows** — `ListImportedSessionRefs`, the union of
   `session_ref`, `pending_fork_session_ref`, and an earlier import's
   `source_session_id`. A failure to read it fails the whole scan: offering
   an already-imported session again would duplicate the user's history.
2. **Fork ancestors** — a fork's file contains its parent's history, so
   importing both imports that history twice, as two threads. Claude reads
   the provenance out of the head buffer at list time; Codex needs one
   BOUNDED head read (`ReadSessionMeta`) per surviving candidate. Computed
   over the SURVIVING candidates only: a fork that is itself already
   imported says nothing about its ancestor.
3. **Subagent / spawned-child sessions** — already excluded inside each
   provider's lister, which is where the provider-specific "is this a
   child" test belongs.

### Which project a row lands under

`projectIndex` (`projectindex.go`) answers it, and a session that ran in a
WORKTREE has to land in the worktree's repository — not in a project of its
own named after a branch.

The index is built once per scan and holds ONE path-keyed map. Into it go
every known project's own root, and then every worktree path those projects
have REGISTERED (`gitroot.RegisteredWorktrees`, once per project), mapped to
the project that registered it. Order matters: registrations are folded in
only after every real project row is placed, and first writer wins — a project
row sitting on a worktree path is one the user has been working in, and
another repository's registration of that path must not displace it. Among
registrations first-wins too: a worktree belongs to exactly one repository, so
two claims mean one project row is stale.

Then, per DISTINCT cwd (a real home has ~1600 rows over ~120 cwds), the whole
answer — project, label, and whether the cwd still exists — is resolved once
and memoized as a unit:

1. One `os.Stat` of the cwd. It decides the row's missing-workspace warning,
   and whether walking is worth attempting at all: `MainRoot` refuses a path
   that does not exist, deliberately.
2. `gitroot.MainRoot(cwd)` — git's `--git-common-dir` semantics, no
   subprocess. See [`internal/gitroot/AGENTS.md`](../gitroot/AGENTS.md).
3. Probe, most specific first: a project AT the cwd, then the project covering
   the resolved repository root, then the project covering the cwd itself.

That order is the load-bearing part. **A project row on the cwd wins
outright**, even when the cwd resolves to a repository that also has one:
those sessions have been living in that project, and resolving to the
repository would move them. Probing the resolved root first makes the more
specific row unreachable in exactly the case it exists for. The third probe is
what places a directory INSIDE a deleted worktree, whose registration is
already in the map — a registration outlives the directory, and it is the only
thing that can still place a dead worktree's sessions.

A cwd under no known project gets only a `ProjectLabel`: `filepath.Base` of
the resolved root, so the label names the repository rather than the branch,
and it is what the import will name the project it creates. (No registration
can apply there — registrations exist only for projects AO already has.)

A registry that exists but cannot be read is logged and skipped for that
project — it degrades to exactly the pre-existing behavior (ungrouped, with
the existing missing-workspace warning), and one project on a stale network
path must not fail the listing.

`Row.ProjectPath` is the session's own cwd and STAYS that: where the provider
ran, and where a resume has to run — `import_one.go` hands it straight to the
thread's `workspace_path`. Resolution decides only which project row the
session is stamped with (`ProjectID` / `ProjectLabel` / `KnownProject`); it
never rewrites the path.

### The origin marker

`Row.Origin` is the provider's own raw marker (`""` when the file has none)
and `Row.RanInAgentOverflow` is the derived boolean. Both come free — Claude's
`entrypoint` is scanned out of the head buffer the lister already read, and
Codex's `session_meta.payload.originator` off the `ReadSessionMeta` the fork
-ancestor pass already performs.

The two spellings differ (`agent-overflow` vs `agent_overflow`) and that is
exactly why the boolean is derived HERE — neither spelling reaches the wire,
and no consumer gets to re-derive it and pick one. Nothing else counts as
ours: `forge_desktop` and every other originator is somebody else's tool.

Both spellings are `internal/provider`'s (`ClaudeEntrypointOrigin` /
`CodexClientOrigin`), the same constants the two spawn paths WRITE. One owner
per side is the point: split across writer and reader, renaming a writer would
leave every test green while making `RanInAgentOverflow` permanently false for
every session written afterwards.

The marker is **provider-recorded text, and a hint — never a trust decision.**
A project-scoped settings env block or an originator override can spell
anything into it, and this package neither writes nor validates it (it is
capped at extraction, since it is cached, wired per row, and rendered). All it
decides is which rows sit behind a frontend toggle by default.

Scan does NOT filter these rows out. The user can still want a session AO
already ran; hiding it is a frontend default with a toggle, and a backend
filter would make that toggle impossible.

`Row.BranchCount` is **0 for Claude, meaning not determined**. Enumerating
a transcript's leaves needs a full read of the file, and a real home is
several GB across a thousand-plus transcripts — the list would take
minutes for a hint. Codex is always 1 (a rollout is one linear
conversation). The true Claude count is known at import, and `ImportOne`
reports it as the number of threads it created.

`ImportOne` is all-or-nothing per session and isolated from every other
session. If any branch fails after its thread row exists, every thread the
call created is deleted: the dedup set keys on the source session id, so a
half-imported session would hide its missing branches from the next scan
forever.

At most ONE thread of a multi-branch Claude transcript carries
`session_ref` — the thread of the file's ACTIVE branch, which is the last
one (`BuildBranches` orders by leaf file position). `claude --resume <id>`
reopens the active branch, so handing the ref to every branch would make
resuming an abandoned one silently continue a different conversation,
with two AO threads appending to one file. The others keep their
provenance in `thread_import_state`, so a refresh still finds them.

"At most" is load-bearing: a branch that produced zero events gets no
thread, and when THAT is the active branch, no thread gets the ref. The
ref belongs to a position in the file, not to a rank among the survivors
— giving it to the last SURVIVING thread would hand it to a thread whose
own leaf is not where a resume lands, which is the exact silent
wrong-conversation failure the rule exists to prevent. Ref-less threads
are not stranded: `materializeImportedClaudeBranch` cuts them their own
session file from `thread_import_state.leaf_uuid` the first time they
start a session (see the last "easy to get wrong" rule below).

The **cursor** is two positions, and both are needed:

- `(LastTurnIndex, LastItemIndex)` is a position in the THREAD. It is a
  pair because `items.item_index` restarts at 0 in every turn — item 2 of
  turn 1 and item 2 of turn 9 share an index. `Diverged` asks the only
  exact question (`store.HasItemsAfterCursor`): does any row sort
  lexicographically after the pair? True means the user resumed the thread
  in AO after importing it, and a refresh must refuse rather than
  interleave a second copy of history.
- `(LastSourceUUID, LastSourceOffset)` is a position in the SOURCE FILE.
  `LastSourceUUID` is written by both providers — it is the last consumed
  event's provenance stamp, a transcript uuid for Claude and `line:<n>` for
  Codex — but only Claude ANCHORS on it, walking the branch for events after
  it. `LastSourceOffset` is Codex's own anchor and stays 0 for Claude; the
  recorded value is the parse's `EndOffset`, not the last event's, because
  trailing lines that produced no event are still consumed.

## Refresh

```go
update, err := sessionimport.PlanUpdate(ctx, d, threadID)
if update.Appliable() {
    items, turns, err := sessionimport.ApplyUpdate(d, update)
}
```

`PlanUpdate` is BOTH the check and the plan, and that is deliberate: it
builds the rows an apply would write (the writer is store-pure) and
writes nothing, so the counts a check reports are exact rather than
estimated, and a tail that cannot be converted is refused before any of
it lands. `ApplyUpdate` refuses anything but an appliable plan rather
than returning a silent no-op — every other status is something the user
has to be shown.

The six statuses:

| Status | Meaning |
|---|---|
| `not-imported` | No `thread_import_state` row. The thread is AO's own. |
| `diverged-local` | `Diverged` says the thread grew past its cursor — the user resumed it inside AO, so the timeline and the file are two different futures. |
| `source-missing` | The recorded `source_path` is gone. |
| `source-diverged` | The file is there but no longer contains the position this thread stopped at. |
| `up-to-date` | The tail produced no rows. |
| `updates-available` | Rows are built and waiting. |

These six strings are declared HERE and passed through the app layer verbatim,
even though the wire DTOs beside them (`ImportScanResult`, `ImportableSession`,
`ImportUpdateStatus`) are re-declared in `app_session_import.go` — the binding
generator only sees `main`, so a shape the frontend renders has to live there,
while a status the frontend only compares against is a value this package
decides and nothing is served by giving it a second spelling to drift from.

Finding the tail is provider-shaped, same split as the cursor:

- **Claude** re-reads the transcript and rebuilds the branch DAG. "Did it
  grow" is a question about ONE branch, so the candidates are the
  branches whose chain still contains this thread's recorded
  `leaf_uuid`, and the answer is the file-order-last of those (which is
  what a resume would call the active branch). Inside that branch, the
  new events are the ones after `last_source_uuid`. A leaf on no branch,
  or a cursor uuid on no event, means the file was rewritten — that is
  `source-diverged`, not an error. An apply records the branch's NEW
  leaf, so the next refresh anchors on the branch by its own identity
  rather than on an ancestor it happens to share.
- **Codex** tails from `last_source_offset`. `rollout.ErrSourceShrank` is
  `source-diverged`: a rollout is append-only, so a smaller file is a
  replaced one.

`Cursor.Advance` is what keeps a refresh from moving the cursor
BACKWARDS. A tail that wrote no rows leaves the row position at
`EmptyCursor`, a Codex tail carries no transcript uuid, and a Claude tail
carries no byte offset — each field takes the later of the two.

## Rules that are easy to get wrong

- **`item_index` is allocated per (turn, index-from-0)**, because that
  is what store's live `nextItemIndexTx` does and every ordering read
  sorts by `(turn_index, item_index)`.
- **Turn indices start at 1.** Index 0 is what a thread's own first live
  send allocates; leaving it free keeps an imported thread's turns from
  colliding if the user resumes the session in AO. A refresh seeds from
  `store.LastTurnIndex`, which already unions items ∪ turns — so a batch
  of turns with no items still moves the seed, and there is no
  "does the thread have items" branch to get wrong.
- **A turn id the thread already holds fails the BATCH, not the INSERT.**
  `turns.turn_id` is a primary key and `ApplyImportBatch` INSERTs, so a
  re-opened id is a transaction failure — and `PlanUpdate` has already
  told the user the refresh would apply by then. `newBuilder` loads
  `store.TurnIDsForThread` and `turnState.open` refuses a taken id, which
  surfaces from `Build` and lands on `PlanUpdate`'s ordinary refusal
  path. Both providers can produce one: a Codex rollout imported
  mid-turn re-opens its wire `turn_id` on the next read, and a synthetic
  `import-turn-<n>` is minted from a counter that restarts in every
  `rollout.Parse`.
- **A provider's own turn id is what continues a turn.** A user message
  arriving inside an open turn that NAMES that turn is a steer (Codex),
  not a new turn — `turnState` tracks the open turn's provider id so the
  row stays where the provider put it. Closing and re-opening would write
  a second `turns` row under one wire id, which is the same primary-key
  failure from the other direction.
- **No turn may reach SQLite with a NULL `completed_at`.** The boot
  sweep (`RecoverCrashedTurns`) would flip it to "interrupted" and
  rewrite imported history as a crash, so `batch()` seals whatever the
  file left open with the turn's last activity timestamp and an empty
  stop reason.
- **Usage rows are `CostSource: "none"`, dated by the turn.** No session
  file carries a wire-reported cost — Claude's `total_cost_usd` lives on
  the stream-json result envelope, which transcripts do not contain, and
  Codex has no cost on its wire — so "none" is what makes
  `GetUsageStats` price them from `internal/usagecost` at query time
  instead of reading a zero as a real total.
- **A tool_call is ONE row across its whole life.** Sibling
  `tool_completion` rows exist only where triage produces them.
- **Only the active Claude branch gets `session_ref`, and the others get
  theirs LATER.** `settleBranches` (`import_apply.go`) explains the first
  half — including the case where the active branch produced no thread
  and NOBODY gets the ref. The second half lives in the app layer,
  because it writes a file:
  `materializeImportedClaudeBranch` (`app_session_import_branch.go`) cuts
  an abandoned branch out of the source transcript into a session file of
  its own — through `sessionfork.WriteForkFileThroughUUID`, anchored on
  the `leaf_uuid` this package recorded — the first time that thread
  starts a session. Doing it here instead would write one transcript copy
  per abandoned branch across a whole "Import All", and every one of them
  would show up in the user's own `claude --resume` picker.

  It cuts into the slug dir of the thread's CURRENT `workspace_path`, not
  beside the source file. A workspace changed before the thread's first
  send relocates nothing (there is no `session_ref` yet to relocate), so
  writing beside the source would leave the fork under the OLD cwd's slug
  while `claude --resume` looks under the new one — "No conversation
  found", on a thread that never ran. Falling back to the source directory
  is correct only where a destination slug cannot exist (an over-length
  path, a workspace that is gone).

## Intentional differences from a live session

These are the only places an imported row is knowingly not identical to
the live row for the same wire event. Everything else is a bug and
`parity_test.go` should catch it.

- `items.meta.import_source_uuid` — provenance a live row cannot have.
- `items.meta.import_unavailable` — a live session always has its
  payloads; only a re-read file can have lost one.
- A top-level `user_text` row carries no `usermessage` composer blob
  (attachments, draft source, revision provenance). An imported prompt
  has no composer to have authored one.
- `threads.updated_at` and thread activity are not bumped. An import
  replays history that already happened; floating every imported thread
  to the top of the sidebar contradicts the timestamps it just wrote.
  (`ApplyImportBatch` owns this.)
- A launch whose parent is only in `meta.parent_tool_use_id` still
  nests. Live only reads the event field on a fresh launch (its
  metaUpdateOnly path reads the meta key), and a reader that has the
  parent should not have to drop it.

## Testing

- `parity_test.go` is the gate: a synthetic wire sequence driven through
  the real `triage.Router` into store A and through `Writer.Build` +
  `ApplyImportBatch` into store B, asserted row for row. There is one
  sequence PER PROVIDER, because the two reach the writer through
  different vocabularies and a Claude-only fixture leaves every
  Codex-only branch — wire turn ids, `EventContentBlockStop` instead of
  deltas, the split `wait_agent` completion, proposed plans, the
  no-wire-item-id user echo — unguarded. Its header documents exactly
  what normalization it allows and why. When it fails, the fix is almost
  always to make the writer match triage — not to widen the
  normalization. `parity_rows_test.go` holds the read-back half.
  A new provider, or a new event kind either reader can emit, needs a
  case here.
- `golden_test.go` + `testdata/*.json` pin the shapes parity cannot
  reach (multi-turn, subagent nesting, Codex splits, background
  terminals, the unavailable marker) as reviewable text. Regenerate with
  `go test ./internal/sessionimport -run TestGolden -update` and READ
  the diff — a golden that changed without a reason is the finding.
- `writer_test.go` covers the writer's own rules: provenance, clocks,
  turn sealing, refresh append, force-close, and the fail-loud boundary.
- `integration_test.go` is the reader↔writer gate: hand-written Claude
  transcripts and Codex rollouts (plus a fixture `state_5.sqlite`) driven
  through `Scan` → `ImportOne` → SQLite, asserting the rows, the original
  timestamps end to end, the cursor, subagent nesting, multi-leaf
  fan-out, and whole-session rollback. `importFixtureRow` fails on an
  `import.unmapped-event` warning, and
  `TestWriterHandlesEveryKindTheReadersEmit` lists every kind the two
  readers can emit — that list is hand-maintained because a provider
  package may never import this one (layering), and its doc comment
  carries the grep that refreshes it.
- Stores are files under `t.TempDir()`. Nothing in this package reads a
  provider home or spawns a provider binary, and nothing may start to
  (root `AGENTS.md` §Permanent invariants).

## Anti-patterns

- Do NOT drive `triage.Router` from here. See the top of this file.
- Do NOT re-implement a summary, status, or id rule that triage already
  owns. Export the pure helper from triage instead; the parity test
  exists precisely so one definition serves both.
- Do NOT branch inside a shared triage helper "for import". If an
  imported row genuinely differs, the branch belongs here and the
  difference belongs in the list above.
- Do NOT stamp `time.Now()` anywhere. Every timestamp comes off an
  event.
- Do NOT invent an `items.kind`. The column's CHECK enum is closed.
