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
| `meta.go` | `SessionMeta`, `ReadSessionMeta` (bounded head read), `SessionIDFromPath`. |
| `parse.go` | `Parse` — the two-pass orchestration and the resume-offset contract. |
| `convert.go` | Converter state, `emit`/`lineUUID`, content-row emitters, compaction. |
| `dispatch.go` | `event_msg` / `response_item` payload dispatch. |
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
`codex-session-meta-missing`.

## Anti-patterns

- Do NOT resolve the Codex home in this package. It is injected.
- Do NOT drop `immutable=1`, and do NOT open the database read-write. AO must
  not create lock files in a home a live Codex is writing.
- Do NOT add a directory-walk fallback for listing. Losing the exclusions
  means importing sub-agent threads as user sessions.
- Do NOT make an unrecognised envelope type fatal, and do NOT derive the
  recognised set from a checkout of codex-rs. The enum is open.
- Do NOT correlate tools by anything but `call_id`.
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
