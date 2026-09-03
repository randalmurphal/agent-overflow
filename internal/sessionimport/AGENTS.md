# internal/sessionimport/

The store side of session import. The **writer** (`NewWriter` / `Build`) turns
`[]importir.Event` into one `store.ImportBatch`. The **orchestrator** (`Scan` /
`ImportOne` / `Cursor` / `PlanUpdate`) decides what is importable and where a
refresh resumes. Session files are parsed only in the provider packages.

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
- `scancache.go` — `ScanCache`: THE cached `Scan`, one entry rather than a
  keyed map (the scan is unfiltered by construction), with `ScanTTL`,
  single-flight `Get(ctx, force)`, id `Lookup` that honors the TTL, and a
  deep copy per caller. Failures are never cached. `Manager` owns the
  instance; the walk itself is an injected function.
- `manager.go` — app-facing coordination around the orchestrator: the scan
  cache, one active import run, weighted worker bound, cancellation/shutdown
  join, thread-locked refresh, and wire-neutral progress/status projections.
  Provider homes, lifecycle context, locks, and emission remain injected by
  the `internal/app` adapter.

`Scan`, `ImportOne` and `ApplyUpdate` are the only things here that
WRITE. The writer half (everything above them) reads the thread row and
writes nothing —
`store.ApplyImportBatch` is what commits, and `ImportOne` is its caller.

## Contract

```go
w := sessionimport.NewWriter(store, thread)
batch, warnings, err := w.Build(events)          // store-pure
err = store.ApplyImportBatch(thread.ID, batch)   // caller commits
```

`Build` reads the thread's history for the next turn index and otherwise touches
nothing. The caller owns the commit, so one session lands in one transaction.

- **Error**, and the whole import is refused, because a half-shaped thread is
  worse than a refused one: a tool completion with no launch AND no
  `import_unavailable` marker, an item-producing event missing a timestamp or
  source coordinate, a turn_complete with no typed payload, two rows claiming
  one item id, or a turn id the thread already holds.
- **Warning**, and the rest still import: an unmapped kind, a payload naming no
  tool call, a launch-less background terminal, a subagent prompt with no id.

## Writer input contract

What the writer needs from a provider reader beyond a well-formed
`provider.ProviderEvent`. An absent optional key degrades a field, not the import.

| Field | Required | Meaning |
|---|---|---|
| `Event.Timestamp` | **yes**, every item-producing event | The row's clock. Imported rows carry the provider's own times end to end. There is no now() fallback, because a thread whose history claims it happened at import time is worse than no thread. |
| `Event.SourceUUID` | **yes**, every item-producing event | Provenance, for BOTH providers. Claude readers set the transcript row's `uuid`. A Codex rollout record has no id of its own, so its reader mints `line:<byte offset of the line's first byte>`. Absent is refused, because the refresh cursor depends on it. |
| `Event.SourceOffset` | Codex only | An OPTIONAL resume position (one byte past the line's newline), not a second spelling of the provenance stamp. Claude leaves it zero. |
| `Event.ItemID` | yes for tool events | The provider tool_use id. It is the row id and the only thing correlating a completion with its launch. |
| `Event.ParentToolUseID` | when nested | Becomes `Item.ParentID`, nesting subagent rows under their launching tool_call. A launch may also carry it as `meta.parent_tool_use_id`, which the writer falls back to, as triage's metaUpdateOnly path does. |
| `Event.TurnID` | Codex only | Preserved as `provider_turn_id` when it came from the wire. The durable key is `store.ScopedTurnID(threadID, Event.TurnID, turnIndex)`, because provider ids repeat across sessions. Inferred rollout turns carry an internal correlation id and leave `provider_turn_id` empty. Claude readers leave it empty and get `<threadID>:<turnIndex>`, as triage does. |
| `Event.TurnComplete` | on `EventTurnComplete` | One of the typed `provider.*TurnCompleteMeta` payloads, never a JSON blob in `Meta`, the same rule triage enforces. `ModelUsage` is what becomes usage-ledger rows. |

Meta keys the writer reads by name. The first two are CONTROL keys: the writer
routes on them and `providerMeta` strips both, so neither reaches `items.meta`.

| Key | On | Meaning |
|---|---|---|
| `import_unavailable` | any item-producing event | The session file no longer holds this item's payload. The value is the REASON (`tool-output-gc`, `exec-detail`). The writer strips the key and re-stamps it through `itemmeta.MarkImportUnavailable`, so there is one persisted spelling. Set it on the event that ADDRESSES the row: a completion whose output is gone marks its launch row. It is ALSO what makes an orphan legal, because a `tool_completion` carrying it with no launch in the batch synthesizes a placeholder `running` launch row and marks that. Without the marker the same event is a hard error. |
| `blockType` | `EventContentBlockStop` | Which settled block this is (`text` / `thinking`). Pure framing: the live path reads it to pick a settle and never persists it, so the writer strips it too. |
| `provider_item_id` | user text | The wire id of the prompt, stored as a correlation key on top-level prompts. Parented text is dropped with a warning when absent, matching triage. |
| `subagent_opening_prompt` | parented user text | Marks the first user row in a subagent transcript, which gets launch-scoped id `user:subagent-prompt:<launchID>` to match the live launch-time row. Later user-role deliveries stay `user:wire:<provider_item_id>`. |
| `tool_use_result` / `item.changes` | tool events | The provider's structured file-change result. Claude and Codex shapes both route through triage's own extractors. Do not pre-normalize one into the other. |
| `toolName`, `input`, `subagent_launch`, `is_background`, `exit_code`, `is_error`, `item_status`, `task_id` | tool events | Decoded by `triage.DecodeToolStartMeta` / `DecodeToolCompleteMeta`. These are the keys the live parsers emit, so a reader that renames one silently loses the field. |

## Event kind to row

`writer.go`'s dispatch is the full map. These are the rules not visible from the
case body.

| Event kind | Rule |
|---|---|
| `EventTurnComplete` | Settles the open turn (`TurnCompletion` + `usage_ledger` rows) and force-closes its unresolved tools (invariant 23). A top-level `EventUserText` is what OPENED it, because Claude transcripts have no turn-start row. |
| `EventToolStart` | A SECOND start for the same id annotates the row (Claude's `task_started` mapping, Codex child-thread labels) and never creates a second row. |
| `EventToolComplete` | Settles that same row in place, with three carve-outs: a launch whose COMPLETION carries `is_background` stays running (its terminal writes the sibling; on Claude a launch flagged at start whose completion does not is settled with the flag cleared, on Codex the wire-stamped launch flag alone keeps it running), Codex `wait_agent` / `resume_agent` get a `tool_completion` sibling, and a file-change result becomes a `tool_result` payload BEFORE the summary derivation, so the row reads "Edited x.go". `EventDiff` and `EventCommandOutput` REPLACE that row's payload, because an imported event carries the whole blob and there is no delta-append branch. |
| `EventContentBlockStop` | A settled whole block, not framing: the Codex reader speaks the live adapter's vocabulary, where an `agentMessage` / `reasoning` item completing arrives ONLY as a content-block stop carrying the whole text. A stop against an OPEN block replaces its content and closes it, so two consecutive agent messages are two rows. A stop with no `blockType` warns and writes nothing. |
| `EventTokenUsage`, `EventInit`, `EventSessionStatus`, `EventRateLimits`, `EventContentBlockStart` | Nothing, being live-only signals plus the block OPENING an event an import already carries whole. Any OTHER unhandled kind also writes nothing, plus an `import.unmapped-event` warning, so a provider that grows a kind cannot fail an import. |

## Scan and import

`Scan(ctx, Deps, Filter)` lists importable sessions and `ImportOne(ctx, Deps,
row)` converts one. `Deps` carries the store and both provider homes, always
injected, because which home is in play is an app decision (credential-home
override, WSL relocation) and a guess lists sessions AO cannot resume.

- **`Scan` subtracts what would duplicate history**, which makes "Import All"
  safe to press twice: everything `store.ListImportedSessionRefs` reports (a
  read failure fails the scan), plus subagent and spawned-child sessions.
- **Explicit provider forks are RETAINED**, because each has a distinct
  provider session id and resumes independently. `Row.ParentSessionID` carries
  Claude's `forkedFrom.sessionId` or Codex's `forked_from_id`, while Codex
  `parent_thread_id` is spawned-child provenance and never a user fork.
  `ReconcileImportedForkLineage` later resolves that raw id to
  `threads.forked_from_thread_id`, so arrival order cannot change the lineage.
- **Import identity is `(provider, source_session_id)`**, never the bare id and
  never content equality: unrelated homes mint the same strings, and forks are
  expected to share content. Store v63's triggers reject every NEW duplicate
  claim and leave legacy duplicates readable. A Codex conversation imported from
  another agent (`Row.ImportedFrom`, ledger in
  [rollout](../provider/codex/rollout/AGENTS.md)) is checked as the CONVERSATION
  under `(claude, sourceSessionID)` and still OFFERED, because both resume.
- **Project resolution is `projectindex.go`**, memoized per cwd over
  `gitroot.MainRoot` plus each project's registered worktrees
  ([`internal/gitroot/AGENTS.md`](../gitroot/AGENTS.md)). Two ordering rules are
  easy to invert: a project row ON the cwd beats the project covering the
  repository that cwd resolves to, and registrations fold in only after every
  real project row, first writer winning. `Row.ProjectPath` stays the session's
  own cwd, where a resume runs.
- **`ImportOne` is all-or-nothing per session.** A history failure after the
  thread row exists deletes the thread, because the dedup set keys on the source
  session id and a partial import would hide it from the next scan. Rollback
  also removes the import's usage rows, unlike ordinary thread deletion.
- **One selected provider session maps to at most one AO thread.** Claude's
  retained alternate leaves are alternate histories, so `ConvertActiveBranch`
  owns last-leaf selection.

## Cursor and refresh

The cursor is two positions. `(LastTurnIndex, LastItemIndex)` is a position in
the THREAD, a pair because `items.item_index` restarts at 0 in every turn.
`Diverged` asks `store.HasItemsAfterCursor` whether any row sorts after it,
meaning the user resumed the thread in AO, so a refresh must refuse rather than
interleave a second copy of history. `(LastSourceUUID, LastSourceOffset)` is a
position in the SOURCE FILE, where Codex's offset records the parse's
`EndOffset` rather than the last event's, because trailing lines that produced
no event are still consumed. `Cursor.Advance` takes the later of the two per
field, so a refresh cannot move the cursor backwards.

`PlanUpdate` is BOTH the check and the plan, deliberately. It builds the rows an
apply would write and writes nothing, so a check's counts are exact and an
unconvertible tail is refused before any of it lands. `ApplyUpdate` refuses
anything but an appliable plan. The six statuses declared here reach the app
layer verbatim: `not-imported`, `diverged-local`, `source-missing`,
`source-diverged`, `up-to-date`, `updates-available`.

- **Claude** finds the tail by rebuilding the branch DAG. "Did it grow" is a
  question about ONE branch, so the candidates are the branches whose chain
  still contains this thread's recorded `leaf_uuid`, the answer is the
  file-order-last of those, and the new events follow `last_source_uuid`. An
  apply records that branch's NEW leaf. A leaf on no branch, or a cursor uuid on
  no event, is `source-diverged` rather than an error.
- **Codex** tails from `last_source_offset` behind TWO divergence tests that do
  not subsume each other. `rollout.ErrSourceShrank` catches a replaced or
  truncated file. `rollout.ReadSourceIdentityAt` fingerprints the first line and
  reads the declared `history_mode`, both compared against
  `thread_import_state.source_meta_hash` / `source_history_mode` (store v67)
  BEFORE the offset is trusted: since Codex 0.147 a thread migrated from
  `legacy` to `paginated` history is rewritten in place, usually the same size
  or larger, with every byte offset addressing a different record. A RECORDED
  fingerprint of `""` means UNKNOWN (predates v67) and is skipped, but a CURRENT
  one FAILS CLOSED, because `ReadSourceIdentityAt` answers a nil error with an
  empty hash when the file has no complete in-window first line.

## Rules that are easy to get wrong

- **Provider item ids are thread-local, never global.** Claude transcript
  branches deliberately reuse them, and every payload id the writer mints is
  derived from an item id (`thinking:<itemID>`, `tool_result:<itemID>`), so
  payload ids repeat too. `payloads` is keyed `(thread_id, id)` for exactly that
  reason (store v58), where the earlier global key turned the reuse into an
  import failure or a cross-thread overwrite.
- **`item_index` is allocated per (turn, index-from-0)**, matching store's live
  `nextItemIndexTx`, because ordering reads sort by `(turn_index, item_index)`.
  **Turn indices start at 1**, because index 0 is what a thread's own first live
  send allocates, so leaving it free keeps an imported thread's turns from
  colliding when the user resumes the session in AO.
- **A turn id the thread already holds fails the BATCH, not the INSERT.**
  `turns.turn_id` is a primary key and `ApplyImportBatch` INSERTs, so `Build`
  loads `store.TurnIDsForThread` up front and refuses a taken id onto
  `PlanUpdate`'s ordinary refusal path, before the user is promised a refresh.
  The synthetic `import-turn:<sessionID>:<n>` names the session because
  `turns.turn_id` is globally keyed.
- **The reader's turn correlation id is what continues a turn.** A user message
  inside an open turn that names that turn is a steer (Codex), not a new turn.
- **No turn may reach SQLite with a NULL `completed_at`.** The boot sweep
  (`RecoverCrashedTurns`) would flip it to "interrupted" and rewrite imported
  history as a crash, so `batch()` seals whatever the file left open with the
  turn's last activity timestamp and an empty stop reason.
- **Usage rows are `CostSource: "none"`, dated by the turn.** No session file
  carries a wire-reported cost, so "none" is what makes `GetUsageStats` price
  them from `internal/usagecost` at query time.

## Provider storage facts the design rests on

Spike- and corpus-verified; neither provider documents them.

- Codex `thread/read` and `thread/turns/list` are LOSSY (2-3 items for a
  90-tool-call thread), so rollouts are parsed directly. The rollout enum is
  not closed (`world_state` and others appear), so skip-unknown is mandatory.
  Reasoning is encrypted and unrecoverable. There are no exec begin/end
  events, but `exec_command_end` carries `call_id`, so correlation is exact.
  About 37% of rollouts are subagent threads. The `state_5` `has_user_event`
  column is dead.
- Claude keeps no index anywhere. Forks copy full history and REUSE uuids,
  so the key is `sessionId` + `uuid`. Compaction severs `parentUuid` and is
  stitched through `logicalParentUuid`. GC'd `tool-results/*.txt` are the
  one unrecoverable loss.

Locked product decisions: one AO thread per Claude leaf (an in-thread branch
switcher is a separate future feature); dedup is mandatory (session_ref,
fork-lineage ancestors, subagent files), which is what makes Import All safe;
non-active Claude branches materialize LAZILY at first send (an eager cut
was rejected: gigabytes of copies and it pollutes `claude --resume`); no
"imported" badge, original timestamps, Codex-archived sessions skipped, no
auto-sync (right-click "Check for new items"); `rememberChatModelProfile` is
deliberately not called.

## Testing

- `parity_test.go` is the gate: one synthetic wire sequence PER PROVIDER, since
  a Claude-only fixture leaves every Codex-only branch unguarded, driven through
  the real `triage.Router` into store A and through `Writer.Build` +
  `ApplyImportBatch` into store B, asserted row for row. Its header lists the
  only knowing differences from a live row, and when it fails the fix is almost
  always to make the writer match triage rather than to widen that list.
- `golden_test.go` + `testdata/*.json` pin the shapes parity cannot reach.
  Regenerate with `-run TestGolden -update` and READ the diff, because a golden
  that changed without a reason is the finding.
- **`make import-corpus-smoke` is the format-drift gate the committed tests
  cannot be**, since synthetic fixtures only know the shapes their author knew
  about. It runs both readers, `Scan`, `Build`, and `ApplyImportBatch` over a
  COPY of a real provider home. It does NOT run `ImportOne` or the refresh path,
  so project resolution, cursor persistence, and `PlanUpdate` / `ApplyUpdate`
  bugs still escape it.

## Anti-patterns

- Do NOT drive `triage.Router` from here. See the top of this file.
- Do NOT re-implement a rule triage owns, or branch inside a shared triage
  helper "for import". Export the pure helper instead. If an imported row
  genuinely differs, the branch belongs here and in `parity_test.go`'s list.
