# internal/provider/codex/rollout/

Read-only reader for Codex's own on-disk session state: the `state_5.sqlite`
thread index and the rollout JSONL files it points at. It lists importable
sessions and converts a rollout into the neutral `internal/importir` event
stream the session-import writer consumes.

Nothing here spawns a process, writes Codex state, or resolves the Codex home
itself — the home is always injected by the caller.

## Layout

| File | Role |
|---|---|
| `list.go` | `List` — the `state_5.sqlite` query, its exclusions, and rollout stat. |
| `home.go` | `PathInHome` — the containment proof every `rollout_path` passes before it is stat'd, opened, or handed to a caller. |
| `scan.go` | Offset-exact line scanner + envelope decoding. No `bufio.Scanner`. |
| `wire.go` | Rollout payload structs, envelope type constants, warning codes. |
| `meta.go` | `SessionMeta`, `ReadSessionMeta` (bounded head read), `ReadSourceIdentity` (the refresh fingerprint), `SessionIDFromPath`. |
| `import_ledger.go` | `ReadExternalImportLedger` — Codex's own record of sessions IT imported from another coding agent. |
| `parse.go` | `Parse` — the two-pass orchestration and the resume-offset contract. |
| `convert.go` | Converter state, `emit`/`lineUUID`, content-row emitters, compaction. |
| `dispatch.go` | `event_msg` / `response_item` payload dispatch. |
| `items.go` | `event_msg/item_completed` — the full `TurnItem` set a PAGINATED rollout is built from. |
| `turns.go` | Turn lifecycle, synthetic turns, model profile, cumulative→per-turn usage. |
| `profile.go` | Constant-memory latest-profile scan used only to repair older imports. |
| `tools.go` | Tool-call open/settle correlation by `call_id`. |
| `tool_ends.go` | `*_end` records: enrichment, self-contained rows, orphan markers. |
| `collab.go` | Sub-agent activity, inter-agent delivery, MultiAgentV1 `collab_*_end`. |

## Exported API

```go
func List(ctx, ListOptions{CodexHome}) ([]SessionInfo, []importir.Warning, error)
func Parse(ctx, ParseOptions{Path, SessionID, FromOffset, MaxLineBytes}) (ParseResult, error)
func ReadSessionMeta(path, sessionID string) (SessionMeta, error)
func ReadSourceIdentity(path, sessionID string) (SourceIdentity, error)
func ReadExternalImportLedger(codexHome string) (map[string]ExternalImportRecord, []importir.Warning)
func SessionIDFromPath(path string) string
func PathInHome(codexHome, rolloutPath string) (string, error)  // ErrOutsideCodexHome
```

`rollout_path` is a value out of Codex's SQLite, not a path AO chose, so
`PathInHome` is what every consumer proves it with — `List` before the
`os.Stat` (a failure is a per-row skip warning, `WarnRolloutOutside`, never a
failed listing), and `sessionimport`'s import and refresh before they open the
recorded `source_path` again. The check is lexical on cleaned absolute paths
precisely so it runs BEFORE the file is touched: without it, a tampered or
stale row leaks the existence and size of an arbitrary file through the
listing and reads its contents into a thread. A home that MOVED lands on the
same answer — not in this home, so not readable — which is why the callers
degrade rather than error.

`ListOptions.CodexHome` is **required**. This package never calls
`os.UserHomeDir()`: a test that resolved the developer's real `~/.codex`
would be reading live session state, and the caller already owns home
resolution for the provider-account machinery.

## Listing

The query lives in `list.go` as `listQuery`. Four exclusions, each with a
reason that is not interchangeable with the others:

- `archived = 0` — the user hid it.
- `preview <> ''` — the thread has content. **This is upstream's own
  emptiness test, not `has_user_event`.** The plan called for
  `has_user_event = 1`; nothing in codex-rs has written that column since
  migration 0007, so on a real home every row has `0` and the filter returns
  the empty set. `TestListDoesNotFilterOnHasUserEvent` pins that.
- `thread_source <> 'subagent'` and `source NOT LIKE '{"subagent"%'` — a
  spawned child thread. Two columns because the representation changed
  across Codex releases and both forms exist on disk.
- `id NOT IN (SELECT child_thread_id FROM thread_spawn_edges)` — the
  structural backstop for a child whose own row does not admit it.

The DSN is `file:<home>/state_5.sqlite?mode=ro&immutable=1`. `immutable=1`
is the load-bearing half: it makes SQLite skip `-wal`/`-shm` entirely, so AO
creates no lock files in the Codex home and can never block a running Codex.
The cost is that the snapshot is whatever was last checkpointed into the main
database file — a session opened seconds ago may not appear yet. That is the
right trade for a browse-and-import surface, and it is why refresh exists.

Failure posture: an absent, locked, or schema-moved database is a hard error.
There is deliberately **no directory-walk fallback** over `sessions/` —
walking would silently produce a list with none of the exclusions applied,
which means importing sub-agent threads as if they were user sessions. A
missing rollout FILE is different: that row is dropped with a per-row
`codex-rollout-missing` warning, because one deleted file is not a broken
index.

## History modes

Codex 0.147 introduced `session_meta.history_mode` (`legacy` | `paginated`;
absent means legacy, since upstream's enum defaults to it and the field is
newer than most files on disk). The mode decides which RECORD SET holds the
conversation, and the two barely overlap
(`codex-rs/rollout/src/policy.rs` `should_persist_event_msg`):

| | legacy | paginated |
|---|---|---|
| `event_msg/user_message`, `agent_message`, `agent_reasoning` | written | **not written** |
| `event_msg/patch_apply_end`, `mcp_tool_call_end`, `web_search_end`, `sub_agent_activity`, `entered_review_mode`, `exited_review_mode`, `context_compacted`, `image_generation_end` | written | **not written** |
| `event_msg/item_completed` | Plan and `clock.sleep` only | **every turn item** |
| `response_item/*` | written | written |
| `turn_context`, `compacted`, `task_started`/`task_complete`/`turn_aborted`, `token_count` | written | written |
| `world_state`, `security_risk_score` | written | written — **recognised and dropped**, see below |

### Recognised and dropped

Two envelope types are decoded, confirmed against their rust-v0.149.0 shape,
and then skipped **silently** — they are not counted in `UnknownTypes` and
raise no warning:

- `world_state` (`WorldStateItem { full, state }`,
  `codex-rs/protocol/src/protocol.rs`) is the engine's resume baseline for
  model-visible context diffing. It has no transcript projection, and Codex
  writes one per turn on every modern thread — counting it as unknown put a
  `codex-unknown-types` warning on essentially every import.
- `security_risk_score` (`SecurityRiskScore { scores, sampled_at }`,
  `codex-rs/protocol/src/security_risk.rs`) carries upstream's own
  prohibition in its doc comment: "Scores must not enter model-visible
  conversation context or user-visible thread item projections." Importing one
  would be exactly the second.

The DECODE is what makes this recognition rather than a blanket `case`: a
payload that no longer matches the shape above falls through to
`UnknownTypes` under a `<type> (unexpected shape)` key and warns again, so
real drift in either type is still reported. Adding a third entry means
proving in the Codex source that the record has nothing a transcript should
show — never that it is merely noisy.

It decodes for SHAPE, never for content: `world_state`'s `state` map is the
largest payload in the file and is typed `*struct{}` here, which accepts any
JSON object, rejects a non-object (upstream's field is a non-`Option` map, so
that is real drift), and allocates nothing. A recognition check that
materialised the map would cost more than the record it is dropping.

So a reader that only knows the legacy set imports a paginated thread with no
tool detail at all — no commands, no diffs, no MCP results, no sub-agent
activity. On one real 12k-line 0.147 rollout that was 1,852 tool rows and 479
diffs missing.

`items.go` maps every `TurnItem` variant onto the SAME emit helpers the legacy
branch uses for its counterpart, so a paginated import and a legacy import of
the same conversation produce the same rows. Three properties of that mapping
are load-bearing:

- **The variant tags are PascalCase.** `TurnItem` carries
  `#[serde(tag = "type")]` with NO `rename_all`
  (`codex-rs/protocol/src/items.rs`). The camelCase `ThreadItem` in
  `app-server-protocol/src/protocol/v2/item.rs` is a DIFFERENT type — the
  app-server's public mirror of the same data — and is not what a rollout
  holds. Matching is case-insensitive anyway, because the same variant has
  been spelled three ways across the two surfaces and pre-0.147 files.
- **The discriminators are read alone, before the item.** `applyItemCompleted`
  decodes `{type, kind}` first and returns on the four kinds the
  `response_item` mirror owns (below) without ever decoding the item body.
  Those four are the highest-volume records in a paginated rollout by a wide
  margin, and every one of them is dropped — decoding their content slices,
  changes maps and agent-state maps first was pure allocation on the
  importer's hottest path. One consequence is deliberate: a MALFORMED item of
  a dropped kind is no longer counted as corrupt, because its bytes are never
  read.
- **Extension items are flattened.** `TurnItem::Extension(ExtensionItem)`
  carries both `"type":"Extension"` and ExtensionItem's own `kind`
  (`web.search`, `clock.sleep`, `image_gen.generation`), so dispatch reads
  `type` first and `kind` second.
- **`CommandExecutionItem.id` / `FileChangeItem.id` IS the `call_id`.**
  Upstream's own `as_legacy_end_event` copies it straight into
  `ExecCommandEndEvent.call_id`, so items route through the same call_id
  correlation as everything else — including the synthetic `exec-<uuid>` ids
  a command run from inside an `exec` script gets.

### Which record wins when both exist

`response_item` lines are persisted in BOTH modes, so a paginated file carries
a `response_item` twin for every message and reasoning item, written on the
very NEXT line (2224/2224 on the reference file). The precedence rule is:

- **The `response_item` mirror owns user text, assistant text and reasoning.**
  `UserMessage`, `HookPrompt`, `AgentMessage` and `Reasoning` items are
  recognised and dropped. The mirror is the only source of user text in a
  native paginated file (no UserMessage items are written at all), a MIGRATED
  file's items carry fresh ids that no twin shares (so id-based dedup is
  impossible there), and the migration writes one Reasoning item per chunk
  each restating the whole accumulated text — emitting those would triple a
  three-chunk thought.
- **Items own everything else.** Tool calls, diffs, MCP results, web
  searches, sub-agent activity and review markers have no mirror.
- **The gate is the declared mode, not a running flag.** `converter.paginated`
  comes off the header, which the pre-scan always reads, so a TAIL REFRESH
  decides the same way a full parse does. A running flag would be clear for a
  cursor that happened to land after the first item.
- **A tool row is emitted once either way.** When an item stands alone it
  records its call id in `converter.itemRows`, and the `response_item` call
  arriving one line later is skipped rather than opening a second row.

`agentMessage.delivery` (0.149, `"async"` — a message a background agent
delivered mid-turn) and `phase` are decoded onto `turnItem` and DROPPED: the
assistant row comes from the mirror, and AO has no non-final-message notion to
carry them into. If one is ever added, this is where they are already parsed.

### history_base

`session_meta.history_base` (upstream `HistoryPosition`) marks a rollout whose
history BEGINS INSIDE ANOTHER FILE: everything before `end_ordinal_exclusive` /
`end_byte_offset` lives in the rollout named by `thread_id` (which is a ROLLOUT
id, not a thread id — a reverted thread's prefix file carries a different one).

**AO does not follow the chain.** `Parse` emits a `codex-history-base` warning
saying the earlier conversation is not imported, which is the honest answer;
presenting a truncated thread as complete is the one thing that would be
worse. `SessionMeta.HistoryBase` carries the TODO with what a follower needs —
resolve the prefix path from the same home, parse it with a stop-at-offset
bound, prepend — plus the three things that must be solved first (chains nest,
so a cycle/depth guard is required; the resume cursor becomes two coordinates;
`PathInHome` must still gate the prefix path).

## Codex's external import ledger

Codex can migrate a Claude Code or Cursor session into a Codex thread
(`codex-rs/external-agent-migration/`). When it does it appends a record to
`<codexHome>/external_agent_session_imports.json` — `SESSION_IMPORT_LEDGER_FILE`,
present since 0.147 — naming the file it read and the thread it produced.

That file is the ONLY place the provenance survives. The resulting rollout is
an ordinary Codex rollout whose `session_meta.originator` says `codex_cli`,
with nothing in it to say the conversation started somewhere else. Without the
ledger the import picker offers a Claude Code conversation as a Codex session
and cannot tell the user it already has the same conversation.

The shape, from `ImportedExternalAgentSessionRecord` (serde derives with no
`rename_all`, so the JSON keys are the Rust field names verbatim):

```json
{"records": [{
  "source_path": "/home/u/.claude/projects/-repo/<uuid>.jsonl",
  "content_sha256": "…", "imported_thread_id": "<codex thread uuid>",
  "imported_at": 1786133870, "source_modified_at": 1786133860000000000,
  "connector_names": ["linear"], "title": "Fix the parser"}],
 "detected_connector_records": [{"source_path": "…", "connector_names": ["…"]}]}
```

Four things the reader has to know:

- **`imported_at` is unix SECONDS, `source_modified_at` is unix NANOS.** The
  units genuinely differ upstream (`now_unix_seconds` vs
  `duration.as_nanos()`). Only `imported_at` is read, and it is converted to
  epoch ms at the boundary so nothing downstream carries the discrepancy.
- **No record names its agent.** Upstream keeps Claude and Cursor apart by
  TYPE (`SessionRecordFormat::Cla` / `Cur`, chosen by whichever detector
  produced the candidate) and never persists the choice, so `Agent` is DERIVED
  from the source path's shape: Cursor's `agent-transcripts` directory or a
  `.cursor` segment, a `.claude` segment, and finally Claude's
  `projects/<slug>/<uuid>.jsonl` layout — which is what recognises a
  RELOCATED Claude home whose directory is not named `.claude`. An
  unrecognised shape yields `""`, never a guess.
- **The recovered `SourceSessionID` uses the Claude lister's own admission
  rule** (a canonical UUID stem). It is compared against AO's Claude import
  state to answer "does AO already have this conversation", so a stem that
  lister would never produce must never match one.
- **`detected_connector_records` is not read.** It describes sources Codex
  NOTICED, not ones it imported, and carries no thread id to key on.

Failure posture: an absent file is the common case and is SILENT. Anything
else — unreadable, over the 8 MiB bound, not JSON, not this shape — costs the
labels, raises exactly one `codex-import-ledger-unreadable`, and never an
error. The badge is decoration on a listing that must still list.

## Parsing

Envelope: `{"timestamp", "type", "payload"}`. Nine types are recognised;
anything else is counted in `ParseResult.UnknownTypes` and skipped.
**Skip-unknown is mandatory, not defensive.** The installed CLI is routinely
ahead of any checkout of codex-rs, the rollout enum has drifted before, and
the reference source's enum must never be treated as closed. A corrupt line,
a line containing a NUL byte, and a line over `MaxLineBytes` are each skipped
and counted in `CorruptLines`.

`Parse` runs two passes over the file:

1. **Pre-scan** — always from offset 0, even on a tail refresh, because a
   refresh still needs the session header and the same dedup decision the
   first parse made. It answers three questions (is there a matching
   `session_meta`; does this file carry `event_msg` messages; does it carry
   `event_msg` reasoning) and exits as soon as all three are settled, so it
   does not usually read the whole file.

   `compacted` is deliberately NOT one of them — proving that negative costs
   a full read of every file that never compacted, and the converter runs a
   cheaper running flag instead. What the pass owes the converter is a SEED
   for that flag covering the region the converter cannot see for itself: on
   a tail refresh (`FromOffset > 0`) it keeps reading past the three
   questions until it has either seen a `compacted` record or reached
   `FromOffset`, and no further. Without it, a cursor landing between a
   `compacted` record and its `context_compacted` twin writes a second
   divider for a compaction the first import already recorded
   (`TestParseTailRefreshDoesNotDuplicateADividerAcrossTheCursor`).
2. **Convert** — seeks to `FromOffset` and emits.

`ParseResult.Profile` is session state beside that event stream: the newest
model / effort / context window the converted region recorded. It is updated
directly from `turn_context`, `task_started`, and `token_count`, never inferred
from usage. Released Codex versions have written `turn_context` both BEFORE
and AFTER `task_started`; the late form patches the already-emitted turn-start
meta as well as the profile. An aborted turn with no token count therefore
still keeps its model.

`ReadLatestProfile` is the constant-memory full-file variant used only when a
refresh encounters an older imported thread whose model is empty. Ordinary
imports get the same answer during their existing Parse pass and must not pay
for a second scan.

Only the `session_meta` whose `payload.id` equals `ParseOptions.SessionID` is
accepted. A fork embeds its source's meta line, so accepting the first one
seen would attribute the fork to its parent's cwd, git branch and creation
time. (`session_id` is accepted as the id only when `id` is absent entirely,
which is a pre-0.100 file shape.)

`SessionMeta.Originator` is `payload.originator` — which TOOL started the
thread (`codex_cli`, `agent_overflow` when AO spawned it, `forge_desktop`,
…). It is the raw wire value; this package does not judge it, and the one
place that decides what counts as AO's own is `internal/sessionimport` (the
Claude side spells the same idea `agent-overflow`, and only one owner of
that equality is correct). It rides the `ReadSessionMeta` head read the
scan already performs for fork provenance, so it costs no extra file read.

### Timestamps

Every event carries the envelope's own timestamp, and the converter's
clock is seeded from `session_meta.created_at` before the first line is
read. The seed is what keeps one unparseable timestamp from costing the
whole session: the import writer refuses an event with no timestamp rather
than restamp it with now(), and the session's own creation time is the
only honest floor the file offers.

### Resume offsets

`EndOffset` always lands on a record boundary. The scanner never returns a
trailing line that has no terminating newline, and rewinds the offset past
it — a rollout being appended to while we read it is the normal case, and
resuming mid-line would corrupt every subsequent parse. `Parse(FromOffset:
prev.EndOffset)` on a grown file yields exactly the events the first pass did
not, which `TestParseTailResumeRoundTripsExactly` pins by half-writing a line,
parsing, completing the line, and parsing again.

A resume offset is validated STRUCTURALLY, not just against the file's
length. Two checks, and the size one alone is not enough:

- `FromOffset > size` → `ErrSourceShrank`. The file cannot contain the
  position this thread stopped at.
- `FromOffset > 0` and the byte at `FromOffset-1` is not `'\n'` →
  `ErrSourceShrank`. `EndOffset` always lands one past a newline, so a
  resume point that no longer follows a record boundary means the file
  was truncated and re-grown (a replaced or rewritten rollout), not
  appended to. Without this, a file truncated to mid-record and then
  grown back PAST the old cursor passes the size check and resumes
  reading from the middle of a record — the refresh reports events that
  were never in the file. `ErrSourceShrank` surfaces as
  `source-diverged`, which is the honest answer.

`MaxLineBytes` defaults to 32 MiB.

## Source coordinates

Every emitted event carries:

- `SourceUUID = "line:<byte offset of the line's first byte>"`
- `SourceOffset = byte offset one past the line's terminating newline`

A rollout is append-only, so a line's start offset is stable for the life of
the file and unique within it. That makes it a real identity — usable as
`items.meta.import_source_uuid` for dedup on re-import, and directly
traceable back into the file by a human. The `line:` prefix exists so it can
never be mistaken for a number to do arithmetic on. Rollout records carry no
per-record id of their own, so there is nothing weaker to fall back to and
nothing stronger to prefer.

`SourceOffset` is the END of the line, not its start: it is the value a
refresh cursor resumes from, so it must mean "everything up to here is
consumed". The last event's `SourceOffset` equals `ParseResult.EndOffset`.

## Meta keys this package introduces

Everything below lands in `provider.ProviderEvent.Meta` and is read by the
import writer.

| Key | On | Meaning |
|---|---|---|
| `toolName`, `input`, `command`, `cwd`, `exit_code`, `is_error`, `item_status` | tool start/complete | The shapes `internal/triage` already decodes. |
| `call_id` | tool start | The wire's own correlation key, kept for traceability. |
| `parent_tool_use_id` | tool start, notification | Nesting under a spawning call. |
| `codexToolName` | tool start/complete | The RAW wire tool name when it differs from the normalized one (e.g. `exec` → `Bash`). Nothing is lost by normalizing. |
| `blockType` | content-block stop | `"text"` or `"thinking"` — the same shape the live Codex adapter emits. |
| `windowId` | compact boundary | Codex's compaction window id. |
| `kind` | notification | `subagent_activity` \| `agent_message` \| `thread_goal` \| `thread_rolled_back` \| `review_status` \| `sleep`. |
| `activityKind`, `agentPath`, `agentThreadId`, `recipient`, `agentNickname`, `agentRole` | notification / collab completion | Collab-agent identity. |
| `status`, `title` | notification | Thread-goal status; review-status title. |
| `source`, `files`, `mcpServer`, `mcpTool`, `query` | tool complete | End-record detail per tool family. |
| `codexErrorInfo` | error | Codex's structured error detail alongside the message. |
| `model`, `effort`, `cwd`, `contextWindow` | turn start | Seeded or backfilled from `turn_context`, regardless of its order relative to `task_started`. |
| **`import_synthetic_turn`** | turn start | This turn has no `task_started` line; the parser opened it so content outside a turn is not dropped. |
| **`import_unresolved`** | tool complete | The call never got an output line and the file gave no terminal status. No status is invented. |
| `import_unavailable: "exec-detail"` | tool complete | Contract key: an end record named a call outside the imported range. |

`import_synthetic_turn` and `import_unresolved` are additions to the plan's
key list; both exist so a gap is visible rather than fabricated.

A synthetic turn's id (`import-turn:<sessionID>:<n>`) combines the
authoritative rollout id with a counter scoped to one `Parse` call. The
session component prevents different rollouts from colliding in the store;
the counter deliberately restarts on a tail parse so the same inferred turn
re-mints the same identity. A wire `turn_id` can likewise be re-opened when a
rollout was imported mid-turn. The writer owns both same-thread collisions: it
loads the thread's existing scoped turn ids and refuses the batch (see
[`internal/sessionimport/AGENTS.md`](../../../sessionimport/AGENTS.md)
§"A turn id the thread already holds"). Do not seed the counter from store
state; that would make the reader depend on persistence it must not read.

WITHIN one `Parse`, though, reuse is knowable and is the converter's
job: `usedTurnIDs` tracks every id a turn opened under, so the trailing
records Codex writes after a settle (`token_count` /
`thread_rolled_back` behind a `turn_aborted`, while `pendingCtx` still
names the settled turn) mint a synthetic id instead of re-claiming the
wire one, a `task_complete` racing an abort is dropped rather than
re-opened, and a `task_started` whose id ensureTurn already opened
adopts that turn in place. Before this, 122 of 1297 real rollouts
hard-failed first import (corpus smoke, 2026-08-08) —
`turns_reopen_test.go` pins all three paths.

## Tool correlation

**Everything correlates by `call_id`, and only by `call_id`.** The call
record, its `*_output` record, and all four end-event families
(`exec_command_end`, `patch_apply_end`, `mcp_tool_call_end`,
`web_search_end`) each declare it on the wire. There is no FIFO matching, no
command-string matching, and no arrival-order matching anywhere in this
package: a weaker key than the one the wire hands us produces wrong pairings
that look right, which is worse than an honest gap.

(The backend plan asserted `exec_command_end` carries no `call_id`. It does —
`codex-rs/protocol/src/protocol.rs` declares `pub call_id: String` — and 154
older rollouts on disk confirm it. The plan's FIFO-with-ambiguous-degrade
fallback was therefore not built.)

End records normally arrive BEFORE the output line, so an end record enriches
the open call and the output line settles it. Three cases fall out:

- **Known call** → enrichment merges into the completion.
- **Unknown call, self-contained record** → emitted as its own tool row. This
  is not a nicety: a patch applied from inside an `exec` script is stamped
  with a synthetic `exec-<uuid>` call id that appears nowhere else in the
  file, and dropping those turned ~5,600 real diffs into placeholders in
  corpus testing.
- **Unknown call, contentless record** → an `import_unavailable:
  "exec-detail"` row plus a `codex-unmatched-tool-end` warning. A visible gap
  beats a silently missing tool call.

`web_search_call` is marked `selfCompleting`: it has no `*_output` response
item at all and carries its own terminal status. Without that, ~99% of
searches settle as unresolved.

## Warning codes

`codex-rollout-missing`, `codex-corrupt-lines`, `codex-unknown-types`,
`codex-unmatched-tool-end`, `codex-unresolved-tool-call`,
`codex-session-meta-missing`, `codex-history-base`,
`codex-import-ledger-unreadable`.

## Anti-patterns

- Do NOT resolve the Codex home in this package. It is injected.
- Do NOT drop `immutable=1`, and do NOT open the database read-write. AO must
  not create lock files in a home a live Codex is writing.
- Do NOT add a directory-walk fallback for listing. Losing the exclusions
  means importing sub-agent threads as user sessions.
- Do NOT make an unrecognised envelope type fatal, and do NOT derive the
  recognised set from a checkout of codex-rs. The enum is open.
- Do NOT correlate tools by anything but `call_id`.
- Do NOT emit BOTH an `item_completed` content item and its `response_item`
  twin. The mirror wins for message and reasoning content; see "Which record
  wins when both exist" before adding a variant.
- Do NOT silence a record type without decoding it. A bare `case X: return`
  turns future drift in X into silent data loss; `dropRecognised` is the
  pattern, and its shape check is the part that keeps the unknown-types
  warning meaningful.
- Do NOT infer `history_mode` from the records seen so far. It is a header
  field, and a tail refresh must decide it the same way a full parse does.
- Do NOT invent a completion status for a call the file never settled.
  `import_unresolved` says so instead.

## Testing

Fixtures are hand-written into `t.TempDir()` — a `state_5.sqlite` built from
a schema subset in `list_test.go`, and rollout JSONL written line by line.
This package must never read the developer's real `~/.codex`: it holds live
session state and a live login.

## References

- `/home/rmurphy/repos/codex` — `codex-rs/rollout/src/policy.rs` is the
  authority on what is persisted, `codex-rs/protocol/src/protocol.rs` on the
  payload shapes, `codex-rs/state/migrations/` on the thread index schema.
  The checkout lags the installed CLI; cross-check against real files.
- `docs/references/codex.md` — how to use those sources.
- `internal/importir/` — the neutral vocabulary this package emits.
- `internal/provider/events.go` — the `ProviderEvent` kinds it emits.
