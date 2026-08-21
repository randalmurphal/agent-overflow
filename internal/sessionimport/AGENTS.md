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
- `import_one.go` — `ImportOne`: one scanned row → at most one thread, history,
  and cursors. `resolveProject` prefers the `ProjectID` the scan already
  stamped and falls back to `project.EnsureForWorkspace` — so the project
  a row was LISTED under is the project it imports into, including for a
  dead worktree the index could only place from a registration. Claude uses
  `ConvertActiveBranch`: one transcript may retain alternate DAG leaves, but
  only the coherent active ancestry that `claude --resume` continues becomes
  the selected provider session's thread.
- `import_apply.go` — `sessionImporter`: the per-session accumulator
  `ImportOne` drives — single-thread creation, warnings, cursor persistence,
  and rollback.
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
| `Event.TurnID` | Codex only | Preserved as `provider_turn_id` when it came from the wire; the durable primary key is `store.ScopedTurnID(threadID, Event.TurnID, turnIndex)` because provider ids repeat across sessions. Inferred rollout turns carry an internal event id for correlation but leave `provider_turn_id` empty. Claude readers leave it empty and get `<threadID>:<turnIndex>`, exactly as triage does. |
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

`Scan` subtracts two things, and both are what makes "Import All"
safe to press twice:

1. **Sessions AO already knows** — `ListImportedSessionRefs`, the union of
   `session_ref`, `pending_fork_session_ref`, and an earlier import's
   `source_session_id`. A failure to read it fails the whole scan: offering
   an already-imported session again would duplicate the user's history.
2. **Subagent / spawned-child sessions** — already excluded inside each
   provider's lister, which is where the provider-specific "is this a
   child" test belongs.

Explicit provider forks are deliberately retained. They share a historical
prefix, but each has a distinct provider session id and can be resumed and
continued independently. `Row.ParentSessionID` carries Claude's
`forkedFrom.sessionId` or Codex's explicit `forked_from_id`; Codex
`parent_thread_id` is spawned-child provenance and is never treated as a
user fork. Import persists that raw parent id, then globally reconciles it to
`threads.forked_from_thread_id`, so parent-first, child-first, sibling, and
multi-generation imports produce the same lineage. Missing parents stay
unlinked until a later import. Ambiguous, self, or cyclic metadata keeps all
history and omits only the unsafe edge with a surfaced warning.

Codex's `session_meta.history_mode` (`legacy` | `paginated`) is deliberately
NOT on `Row`. It had a field once, with no reader; a listing field nothing
branches on is a shape every caller has to carry and no caller can verify. The
refresh guard reads the mode where it is actually load-bearing, off
`rollout.ReadSourceIdentity`, and compares it against the recorded
`SourceHistoryMode` to catch a Codex history migration (see
"Source identity"). If a listing consumer ever needs it, it rides the bounded
head read the scan already performs for fork provenance, so re-adding it costs
nothing — but read it from the rollout header, never from `threads.history_mode`
in `state_5.sqlite`: that column arrived with a state migration a Codex home can
predate, and naming it in the listing query would turn a browse into a hard
failure on any home that has not migrated.

### Sessions Codex imported from another agent

`Row.ImportedFrom` is set when Codex's own external-import ledger
(`<codexHome>/external_agent_session_imports.json`, Codex >= 0.147) says a
Codex thread is a conversation Codex migrated from Claude Code or Cursor. It
carries the source agent, the source path, the source session id where the
layout encodes one, when Codex recorded the import, and — the second thing the
ledger buys — `DuplicateOfThreadID`.

The imported rollout is an ordinary Codex rollout whose originator says
`codex_cli`, so without the ledger the picker offers a Claude Code
conversation as a Codex session with nothing to say where it came from. The
shape, the units, and how the agent is derived live in
[`internal/provider/codex/rollout/AGENTS.md`](../provider/codex/rollout/AGENTS.md)
§"Codex's external import ledger".

Two rules here:

- **The duplicate check reuses `known`.** It is the same
  `ListImportedSessionRefs` map the scan already builds to subtract sessions
  AO has, so the check costs no extra read — it just asks a different
  question of it: the Codex-side dedup asks whether AO holds this CODEX
  THREAD, and this asks whether AO holds the CONVERSATION inside it, keyed on
  `(claude, sourceSessionID)`.
- **A duplicate row is still OFFERED.** Two provider sessions genuinely exist
  and both are independently resumable — the Codex copy resumes in Codex — so
  suppressing it would take away a real choice, the same reasoning that keeps
  explicit forks listed. Naming the duplicate lets the picker say so instead.

One bounded read of one small JSON file per SCAN, not per row. Absence is
silent; a corrupt ledger costs the labels and raises one
`codex-import-ledger-unreadable`. That warning is not a per-file skip, so
`countSkipped` does not count it — and `Scan` surfaces warnings only through
`ProviderStatus.SkippedCount`, so a corrupt ledger is visible to library
callers and tests but not, today, to the user. The user-visible effect is
simply that no origin badges appear.

Unlike the history mode above, this one IS on the app's wire shape
(`ImportableSession.importedFrom`, `app_session_import.go`), because the
import picker renders it.

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
Codex's `session_meta.payload.originator` off the same bounded
`ReadSessionMeta` that recovers explicit-fork provenance.

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

One selected provider session maps to one AO thread; an empty or metadata-only
active history can still produce none. Claude's retained alternate leaves are
not independent sessions and are not merged into the active timeline. There
is no branch-count field in the catalogue because import never fans a row out.

`ImportOne` is all-or-nothing per session and isolated from every other
session. If history fails after the thread row exists, the thread is deleted:
the dedup set keys on the source session id, so a partial import would hide the
source from the next scan. Rollback also removes the import's usage rows;
ordinary thread deletion retains usage by design.

The provider readers return `importir.ModelProfile` beside their events. That
separation is load-bearing: a model is branch/session state, not a timeline
row, and deriving it from usage made zero-usage Claude messages invisible
while deriving it from Codex turn starts lost a `turn_context` written after
`task_started`. Claude reads the newest model on the complete active ancestry.
Codex rollout values are
authoritative, with the already-read state-index columns used only when the
rollout recorded none. A genuinely empty profile stays empty and therefore
means provider default; import never fabricates a historical model.

The Claude thread carries `session_ref` for the source session and records the
active leaf in `thread_import_state`. `BuildBranches` orders leaves by file
position and `ConvertActiveBranch` owns the last-leaf selection so callers
cannot accidentally fan one catalogue row out into sibling threads. If that
active chain produces no events, no thread or dedup marker is written.

Import identity is `(provider, source_session_id)`, never the bare session id
and never content equality. The provider scope matters because unrelated
homes can mint the same string; content equality cannot identify forks because
their shared prefix is expected. Migration v63's insert/identity-change
triggers reject every new duplicate claim while allowing cursor updates and
legacy duplicate rows created by the pre-active-branch importer to remain
readable.

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

A refresh may also carry a model-profile repair with zero new timeline rows.
That is how imports written by the older event-derived reader recover without
replaying their history. The update uses a model-field compare-and-swap, so a
selection the user makes after planning always wins. Codex's EOF repair uses a
constant-memory profile scan; ordinary tail refreshes keep their offset-bounded
Parse path. Only an EMPTY stored model is repairable: once a thread has a model,
that row is the user's selection and importing newer provider history never
replaces it.

The six statuses:

| Status | Meaning |
|---|---|
| `not-imported` | No `thread_import_state` row. The thread is AO's own. |
| `diverged-local` | `Diverged` says the thread grew past its cursor — the user resumed it inside AO, so the timeline and the file are two different futures. |
| `source-missing` | The recorded `source_path` is gone. |
| `source-diverged` | The file is there but no longer contains the position this thread stopped at — it was replaced, truncated, or (Codex) rewritten in place by a history migration. |
| `up-to-date` | The tail produced no rows and no model repair. |
| `updates-available` | Rows and/or a model-profile repair are built and waiting. |

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
- **Codex** tails from `last_source_offset`, behind TWO divergence tests
  that do not subsume each other:
  - **Source identity.** `rollout.ReadSourceIdentity` fingerprints the
    file's first line (sha256) and reads its declared `history_mode`; both
    are compared against `thread_import_state.source_meta_hash` /
    `source_history_mode` (migration v67) BEFORE the offset is trusted.
    This is what catches a rollout Codex REWROTE in place: since 0.147 a
    thread can be migrated from `legacy` to `paginated` history, and the
    migration canonicalises the whole file and publishes it atomically over
    the same path. The result is usually the SAME SIZE OR LARGER — so the
    append-only test below passes — while every byte offset in it addresses
    a different record. The refusal names the migration as a cause, because
    that is a thing the user can recognise.
  - **Append-only position.** `rollout.ErrSourceShrank` is `source-diverged`:
    a rollout is append-only, so a file smaller than the cursor, or a cursor
    that no longer follows a record boundary, is a replaced one.

  A RECORDED fingerprint of `""` means UNKNOWN, not mismatched: a row written
  before v67 has none, the comparison is skipped, and the next successful apply
  BACKFILLS it. Reporting every pre-v67 thread as diverged would be a worse
  answer than the size test those threads have always had. Claude records no
  fingerprint at all — its refresh anchors on a transcript uuid, which a
  rewritten file invalidates by itself.

  A CURRENT fingerprint of `""` is the opposite case and FAILS CLOSED.
  `ReadSourceIdentity` answers a NIL ERROR with an empty hash whenever the file
  has no complete, in-window first line — a first record past the bounded head
  read, a truncated head, an empty file — so a guard phrased as "compare only
  when both sides are non-empty" skips the fingerprint exactly when the file is
  least like the one that was imported, and the recorded byte offset is then
  trusted against a header nothing read. A thread that HAS a fingerprint proved
  its first line was readable at import time; a header that stopped being
  readable is a header that changed. The same posture covers the READ ERROR
  (`codexIdentityForRefresh`): unfingerprinted is unknown, fingerprinted with a
  real resume offset is divergence.

  The two `history_mode` spellings are compared through `historyModeLabel`, so
  an ABSENT mode and an explicit `legacy` are one mode rather than a migration
  between them — the field exists only from Codex 0.147 and its enum defaults
  to Legacy. A header that genuinely changed is still caught by the
  fingerprint, whose prose is the accurate one for it.

  A Codex rollout that inherits history from ANOTHER file
  (`session_meta.history_base`) imports only its own region and carries a
  `codex-history-base` warning; AO does not follow the chain. See
  [`internal/provider/codex/rollout/AGENTS.md`](../provider/codex/rollout/AGENTS.md)
  §history_base.

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
  `import-turn:<sessionID>:<n>` is deterministically re-minted when a tail
  parse starts from the same inferred turn boundary. The session component is
  required because `turns.turn_id` is globally keyed.
- **The reader's turn correlation id is what continues a turn.** A user
  message arriving inside an open turn that names that turn is a steer
  (Codex), not a new turn. This is normally the provider id; inferred rollout
  turns use their deterministic internal id while leaving `provider_turn_id`
  empty. Closing and re-opening would split one logical turn into two rows.
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
- **One Claude session imports only its active branch.** Sibling leaves are
  alternate histories, not independent provider sessions and not timelines
  that can be merged coherently. The imported thread gets the source
  `session_ref` directly. `sessionImporter.add` also refuses a second thread,
  so future callers cannot silently restore fan-out by looping.

  `materializeImportedClaudeBranch` (`app_session_import_branch.go`) remains
  for upgrade compatibility with inactive, ref-less threads created by older
  releases. It cuts such a branch into its own session file through
  `sessionfork.WriteForkFileThroughUUID`, anchored on the stored `leaf_uuid`,
  the first time that legacy thread starts.

  It cuts into the slug dir of the thread's CURRENT `workspace_path`, not
  beside the source file. A workspace changed before the thread's first
  send relocates nothing (there is no `session_ref` yet to relocate), so
  writing beside the source would leave the fork under the OLD cwd's slug
  while `claude --resume` looks under the new one — "No conversation
  found", on a thread that never ran. Falling back to the source directory
  is correct only where a destination slug cannot be resolved at all — a
  workspace that is gone. Path length is no longer such a case: the CLI's
  truncate-and-hash slug for a >200-char path is reproduced exactly.

  `WriteForkFileThroughUUID` remints every transcript UUID. The new session
  ref and every imported user item's provider-id metadata are therefore
  committed atomically through copy-on-write; retaining the source UUIDs would
  make a later rollback or fork search the new transcript for rows it cannot
  contain.

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
  timestamps end to end, the cursor, subagent nesting, multi-leaf active-chain
  selection, and rollback. The multi-leaf case proves shared ancestry and the
  active continuation land while the inactive sibling does not, then compares
  the stored active history against an independent full active-branch build,
  including ordered items, payload bytes, turn records, and usage totals.
  `importFixtureRow` fails on an
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
