# internal/provider/codex/rollout/

Read-only reader for Codex's own on-disk session state: the `state_5.sqlite`
thread index and the rollout JSONL files it points at. It lists importable
sessions (`List`) and converts a rollout into the neutral `internal/importir`
stream the session-import writer consumes (`Parse`). Nothing here spawns a
process, writes live Codex state, or resolves the Codex home. Transfer copies
write only a NEW caller-owned operation scratch directory.

**The on-disk format is documented elsewhere.**
[`docs/references/codex.md` §Rollout files on disk](../../../../docs/references/codex.md#rollout-files-on-disk)
is authoritative for the history-mode record sets, `TurnItem` shape facts,
item-versus-`response_item` precedence, `history_base`, and the ledger JSON.
This guide carries the AO-side contracts a change can break.

## Path containment

`rollout_path` is a value out of Codex's SQLite, not a path AO chose, so
`PathInHome` (`home.go`, `ErrOutsideCodexHome`) gates every use: `List` before
its `os.Stat` (a failure is the per-row `WarnRolloutOutside` skip, never a
failed listing), and `sessionimport` before it reopens a recorded
`source_path`. The check is lexical on cleaned absolute paths so it runs
BEFORE the file is touched, because without it a tampered or stale row leaks
an arbitrary file's existence, size and contents into a thread. A MOVED home
lands on the same answer, which is why callers degrade rather than error.
`ListOptions.CodexHome` is likewise **required**, and this package never calls
`os.UserHomeDir()`: a test that resolved the developer's real `~/.codex` would
be reading live session state.

## Listing

`listQuery` (`list.go`) applies four exclusions, each with a reason that is
not interchangeable with the others:

- `archived = 0`. The user hid it.
- `preview <> ''`. The thread has content. **This is upstream's own emptiness
  test, not `has_user_event`**, which nothing has written since migration 0007,
  so filtering on that column returns the empty set on a real home
  (`TestListDoesNotFilterOnHasUserEvent`).
- `thread_source NOT IN ('subagent', 'guardian_review')` and
  `source NOT LIKE '{"subagent"%'`. Spawned and Guardian-review children, in
  two columns because both representations exist on disk.
- `id NOT IN (SELECT child_thread_id FROM thread_spawn_edges)`. The structural
  backstop for a child whose own row does not admit it.

The DSN is `file:<home>/state_5.sqlite?mode=ro&immutable=1`. `immutable=1` is
load-bearing: SQLite then skips `-wal` and `-shm` entirely, so AO creates no
lock files in the Codex home and can never block a running Codex. The cost is
a snapshot only as fresh as the last checkpoint, which is why refresh exists.

An absent, locked, or schema-moved database is a hard error, and there is
deliberately **no directory-walk fallback** over `sessions/`: walking loses
every exclusion above, which means importing sub-agent threads as user
sessions. A missing rollout FILE is only a `codex-rollout-missing` warning.

## Converting a rollout

`items.go` maps every `TurnItem` variant onto the SAME emit helpers the legacy
branch uses, so a paginated and a legacy import of one conversation produce
the same rows. Four rules hold that together.

- **The gate is the declared mode, not a running flag.** `converter.paginated`
  comes off the header, so a TAIL REFRESH decides as a full parse does.
- **The discriminators are read alone, before the item.** `applyItemCompleted`
  decodes `{type, kind}` and returns on the four kinds the `response_item`
  mirror owns without touching the item body. Those four are the highest-volume
  records in a paginated rollout and all are dropped, so decoding their content
  first was pure allocation on the hottest path. One consequence is deliberate:
  a MALFORMED item of a dropped kind is not counted as corrupt.
- **A tool row is emitted once either way.** An item standing alone records its
  call id in `converter.itemRows`, so the `response_item` call arriving one line
  later is skipped rather than opening a second row.
- **Recognised-and-dropped records are DECODED, never blanket-cased.** A
  `world_state` or `security_risk_score` payload that no longer matches its
  documented shape falls through to `UnknownTypes` under a
  `<type> (unexpected shape)` key and warns, so real drift is still reported.
  `dropRecognised` decodes for SHAPE and never content, allocating nothing.

**AO does not follow a `history_base` chain.** `Parse` emits a
`codex-history-base` warning saying the earlier conversation is not imported,
because presenting a truncated thread as complete would be worse.
`SessionMeta.HistoryBase` carries the TODO and the three things a follower
must solve: chains nest so a cycle guard is needed, the resume cursor becomes
two coordinates, and `PathInHome` must still gate the prefix path.

## Codex's external import ledger

`ReadExternalImportLedger` (`import_ledger.go`) reads the file Codex appends
to when it migrates a Claude Code or Cursor session into a Codex thread. It is
the ONLY place that provenance survives: without it the import picker offers a
Claude Code conversation as a Codex session and cannot say AO already has it.

- **`Agent` is DERIVED from the source path's shape**, because no record names
  its agent: Cursor's `agent-transcripts` directory or a `.cursor` segment,
  then a `.claude` segment, then Claude's `projects/<slug>/<uuid>.jsonl`
  layout, which recognises a RELOCATED Claude home. An unrecognised shape is
  `""`, never a guess.
- **The recovered `SourceSessionID` uses the Claude lister's own admission
  rule** (a canonical UUID stem), because it is compared against AO's Claude
  import state, so a stem that lister could not produce must not match one.
- An absent file is the common case and is SILENT. Unreadable, over the 8 MiB
  bound, not JSON, or not this shape costs the labels, raises exactly one
  `codex-import-ledger-unreadable`, and never an error. `imported_at` converts
  to epoch ms here, so nothing downstream carries upstream's unit split.

## Parsing

Envelope: `{"timestamp", "type", "payload"}`. Ten types are recognised
(`wire.go`) and anything else is counted in `UnknownTypes` and skipped.
**Skip-unknown is mandatory, not defensive**: the installed CLI runs ahead of
any checkout of codex-rs, so the reference enum is never closed. A corrupt
line, a line holding a NUL byte, and a line over `MaxLineBytes` (32 MiB
default) are each skipped and counted in `CorruptLines`. `scan.go` is
offset-exact and deliberately not `bufio.Scanner`.

`Parse` runs a PRE-SCAN always from offset 0, even on a tail refresh, because
a refresh still needs the session header and the same dedup decision the first
parse made. It answers three questions (a matching `session_meta`, does the
file carry `event_msg` messages, and `event_msg` reasoning) and exits once all
three settle. `compacted` is deliberately NOT one of them, because proving
that negative costs a full read of every file that never compacted, so the
converter runs a cheaper running flag and the pre-scan owes it a SEED: on a
tail refresh it reads on until it has seen a `compacted` record or reached
`FromOffset`. Without the seed, a cursor landing between a `compacted` record
and its `context_compacted` twin duplicates a divider
(`TestParseTailRefreshDoesNotDuplicateADividerAcrossTheCursor`). The second
pass seeks to `FromOffset` and emits.

Only the `session_meta` whose `payload.id` equals `ParseOptions.SessionID` is
accepted, because a fork embeds its source's meta line and accepting the first
one seen would attribute the fork to its parent's cwd, git branch and creation
time. (`session_id` is the id only when `id` is absent, a pre-0.100 shape.)
`SessionMeta.Originator` is the raw `payload.originator` naming which TOOL
started the thread, and `internal/sessionimport` decides which value is AO's.

`ParseResult.Profile` is the newest model, effort and context window the
converted region recorded, taken from `turn_context`, `task_started` and
`token_count` and never inferred from usage. Released Codex versions have
written `turn_context` both BEFORE and AFTER `task_started`, and the late form
patches the already-emitted turn-start meta as well as the profile, so an
aborted turn with no token count still keeps its model. `ReadLatestProfile`
(`profile.go`) is the constant-memory full-file variant, used ONLY when a
refresh meets an older imported thread whose model is empty. The converter's
clock is separately seeded from `session_meta.created_at`, which keeps one
unparseable timestamp from costing the whole session: the import writer
refuses an event with no timestamp rather than restamp it with `now()`.

### Resume offsets and source coordinates

Every event carries `SourceUUID = "line:<byte offset of the line's first
byte>"` and `SourceOffset = byte offset one past the terminating newline`. A
rollout is append-only, so a line's start offset is a stable identity, used as
`items.meta.import_source_uuid` for re-import dedup. `SourceOffset` is the END
of the line because a refresh cursor resumes from it, and the last event's
value equals `ParseResult.EndOffset`.

`EndOffset` always lands on a record boundary, because the scanner never
returns a trailing line with no terminating newline and rewinds past it
(`TestParseTailResumeRoundTripsExactly`). A resume offset is then validated
STRUCTURALLY: `FromOffset > size` gives `ErrSourceShrank`, and so does
`FromOffset > 0` with the byte at `FromOffset-1` not `'\n'`. The size check
alone is not enough, because a file truncated to mid-record then grown back
PAST the old cursor would pass it and resume from mid-record, making the
refresh report events that were never in the file. `ErrSourceShrank` is
`source-diverged`.

## Meta keys this package introduces

Everything the converter stamps into `provider.ProviderEvent.Meta` is read by
the import writer. Most keys are shapes `internal/triage` already decodes
(`toolName`, `input`, `command`, `cwd`, `exit_code`, `is_error`, `item_status`,
`call_id`, `parent_tool_use_id`, `blockType`, `windowId`, `source`, `files`,
`mcpServer`, `mcpTool`, `query`, `codexErrorInfo`, `status`, `title`, `model`,
`effort`, `contextWindow`). Six carry a rule you cannot read off the name.
Warning codes are the `Warn*` constants in `wire.go`.

| Key | On | Meaning |
|---|---|---|
| `codexToolName` | tool start/complete | The RAW wire tool name when it differs from the normalized one (`exec` becomes `Bash`). |
| `kind` | notification | `subagent_activity` \| `agent_message` \| `thread_goal` \| `thread_rolled_back` \| `sleep`. Review boundaries become a `codex_review` agent launch and sourced result, never status notifications. |
| `activityKind`, `agentPath`, `agentThreadId`, `recipient`, `agentNickname`, `agentRole` | notification / collab completion | Collab-agent identity. |
| `agent_path`, `canonical_path`, `activity_call_id` | subagent status | A typed 0.150 completed activity settles the existing launch without creating a notification row. |
| **`import_synthetic_turn`** | turn start | This turn has no `task_started` line; the parser opened it so content outside a turn is not dropped. |
| **`import_unresolved`**, `import_unavailable: "exec-detail"` | tool complete | The call never settled, or an end record named a call outside the imported range. Both exist so a gap is visible rather than fabricated. No status is invented. |

## Turn ids

A synthetic turn's id (`import-turn:<sessionID>:<n>`) combines the
authoritative rollout id with a counter scoped to one `Parse` call, so
different rollouts cannot collide in the store and a tail parse re-mints the
same identity for the same inferred turn. Do NOT seed that counter from store
state, which would make the reader depend on persistence it must not read. A
wire `turn_id` can likewise be re-opened when a rollout was imported mid-turn.
The writer owns both same-thread collisions, loading the thread's existing
scoped turn ids and refusing the batch
([`sessionimport`](../../../sessionimport/AGENTS.md) §"A turn id the thread
already holds").

WITHIN one `Parse`, reuse is knowable and is the converter's job.
`usedTurnIDs` (`turns.go`) tracks every id a turn opened under, so trailing
records written after a settle mint a synthetic id instead of re-claiming the
wire one, a `task_complete` racing an abort is dropped rather than re-opened,
and a `task_started` whose id `ensureTurn` already opened adopts that turn.
Before this, 122 of 1297 real rollouts hard-failed first import
(`turns_reopen_test.go`, 2026-08-08).

## Tool correlation

**Everything correlates by `call_id`, and only by `call_id`.** The call record
(`tools.go`), its `*_output` record, and all four end-event families
(`exec_command_end`, `patch_apply_end`, `mcp_tool_call_end`, `web_search_end`,
in `tool_ends.go`) each declare it on the wire, `exec_command_end` included
(`codex-rs/protocol/src/protocol.rs` declares `pub call_id: String`, confirmed
on 154 older rollouts on disk). A weaker key produces wrong pairings that look
right, which is worse than an honest gap.

End records normally arrive BEFORE the output line, so an end record enriches
the open call and the output line settles it. A known call merges into the
completion. An unknown call with a self-contained record becomes its own tool
row: a patch applied from inside an `exec` script is stamped with a synthetic
`exec-<uuid>` call id that appears nowhere else in the file, and dropping
those turned ~5,600 real diffs into placeholders in corpus testing. An unknown
call with a contentless record becomes an `import_unavailable: "exec-detail"`
row plus a `codex-unmatched-tool-end` warning. `web_search_call` is
`selfCompleting`, having no `*_output` response item and its own terminal
status, without which ~99% of searches settle as unresolved.

## Anti-patterns

- Do NOT resolve the Codex home here, drop `immutable=1`, open the database
  read-write, or add a directory-walk fallback for listing.
- Do NOT make an unrecognised envelope type fatal, derive the recognised set
  from a checkout of codex-rs, or silence a type without decoding it. A bare
  `case X: return` turns future drift in X into silent data loss.
- Do NOT correlate tools by anything but `call_id`, infer `history_mode` from
  the records seen so far, or invent a completion status the file never gave.

## Testing and references

### Conversation transfer files

`transfer.go` collects explicit `thread/read` paths and their `history_base`
closure. This targeted export does not change session listing or its immutable
index reader. A reverted rollout's filename ID can differ from `session_meta.id`;
prefix lookup uses the filename while root validation uses the native session ID.
Only a prefix dependency triggers the bounded filename index over sessions and
archived_sessions. Plain files take precedence over compressed siblings; zstd
metadata reads have a bounded decoder. Unknown history modes, missing/ambiguous
prefixes, cycles and excessive depth are refusals. Callers provide all child
references and release provider writers before snapshotting. Archive bytes remain
opaque; there is no path replacement in arbitrary historical text.


Fixtures are hand-written into `t.TempDir()`: a `state_5.sqlite` built from a
schema subset in `list_test.go`, and rollout JSONL written line by line. This
package must never read the developer's real `~/.codex`, which holds live
session state and a live login.
[`docs/references/codex.md`](../../../../docs/references/codex.md) covers the
on-disk format and how to read upstream. In `/home/rmurphy/repos/codex`,
`codex-rs/rollout/src/policy.rs` is the authority on what is persisted,
`codex-rs/protocol/src/protocol.rs` on payload shapes, and
`codex-rs/state/migrations/` on the thread index schema. The checkout lags the
installed CLI, so cross-check against real files. `internal/importir/` is the
neutral vocabulary this package emits, and `internal/provider/events.go` holds
the `ProviderEvent` kinds.

## Native transfers

`TransferGraph` follows structured collaboration references as well as
`history_base` prefixes. Native `thread/fork` copies only the root: its saved
child IDs still name the originals. `CopyTransferFiles` therefore assigns
operation-stable independent IDs to root, children and prefix files; it remaps
only understood identity fields, preserving prose and unrelated tool payloads.
Prefix files precede dependents. Re-encoding recomputes byte offsets at exact
record boundaries and preserves ordinal coordinates. Incomplete records fail
transfer rather than silently truncating the continuation.

The source app calls `FlattenTransferFiles` before copying or moving a paginated
prefix chain. It streams only retained records into a standalone native rollout,
with the current header and contiguous ordinals. Both byte and ordinal cuts must
agree; historical turn/item IDs and content payloads remain unchanged. Remap
`subagent_history_start_ordinal` over removed metadata headers so inherited
context stays outside the child's projected turns. A boundary beyond retained
history is a refusal; `forked_from_ordinal_exclusive` names the logical parent
and is not a coordinate on the child's rewritten file. The provider then
rebuilds its own SQLite history index on resume. Copying a prefix chain alone
preserves model context but leaves that index empty in a fresh home, breaking
historical reverts (CLI 0.153.4 probe). `TransferGraph` likewise visits only retained
prefix records: discarded future collaboration calls cannot pull unrelated
sessions into a transfer. The session importer still reports `history_base` gaps;
this transfer-specific materialization does not change its parsing contract.

Current reverted filenames carry `<thread-id>_<rollout-id>`. `SessionIDFromPath`
selects the first identity for native metadata; `rolloutFileIDs` supplies the
second for prefix discovery and byte coordinates. Copy rewrites BOTH filename
identities. A trailing UUID parser would confuse ownership with a history segment
and reject current revert files (verified with CLI 0.153.4, 2026-09-05).
Collaboration output can be a string or content-item array; decode structured
results only in `input_text`, preserving image/audio/encrypted items. Nested JSON
uses `UseNumber` too: UUID remapping must not round unrelated integer content.

A fork can put its new header BEFORE an inherited source header. Prefer the
explicit session ID, otherwise the filename's ID and finally the first header;
never take the last metadata record. Current paths come from metadata-only
app-server reads, not stale descendant indexes or guessed filenames.
An injected home may use a filesystem alias the provider canonicalizes. Transfer
resolves only that trusted home and repeats lexical containment against it; never
stat an arbitrary rejected target to decide whether it was safe to read.

`TransferMinimumVersion` reads every native member's declared history mode.
Paginated history requires Codex 0.148.0; legacy uses AO's ordinary CLI floor,
independent of the source's current binary version. Isolated Go-produced Move
and Copy exports from 0.153.4 resumed, continued and reverted on 0.148.0.
Upstream's [0.148 metadata contract](https://github.com/openai/codex/blob/rust-v0.148.0/codex-rs/protocol/src/protocol.rs)
also carries child projection boundaries. Unknown modes remain refusals.
