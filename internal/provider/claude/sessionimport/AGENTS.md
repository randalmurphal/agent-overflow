# internal/provider/claude/sessionimport/

Reads existing Claude Code session transcripts (`~/.claude/projects/…`)
so Agent Overflow can import them as threads. Read-only on every file it
touches, and it never spawns anything.

Consumers: `internal/sessionimport` (the neutral writer) via
`internal/importir`, driven by the App's import methods.

## Hard rules

- **No home resolution in this package.** `Options.ProjectsDir` and the
  session path are always injected. Which Claude home is in play is an
  app-level decision (credential-home override, WSL relocation) and a
  library guess would silently disagree with it. See the root
  `AGENTS.md` §Permanent invariants.
- **No process spawn, ever.** Import is a file read.
- **Tests never touch a real provider home.** Fixtures are hand-written
  rows under `t.TempDir()`.

## Layout

- `scan.go` — the lite-read primitives: 64 KB head/tail reads into the two
  halves of ONE caller-owned buffer (`liteFile.Head` / `.Tail` alias it,
  so a listing pass allocates one buffer for the whole directory), and the
  raw byte-scan field extractors (`extractJSONStringField` /
  `extractLastJSONStringField` / `extractFirstPromptFromHead`). Ported from
  Claude Code's `sessionStoragePortable.ts`. They scan `[]byte` rather than
  `string` so the read buffer is never copied — a real home is a thousand
  files and the window is 128 KB each. The scan is raw text on purpose: a
  head/tail window routinely cuts a line in half, and `json.Unmarshal`
  answers "nothing" for a truncated line that plainly contains the field.
  The ONE JSON decode is the single candidate line the first-prompt
  fallback needs.
- `list.go` — `List(ctx, Options)`: enumerate every importable session
  under a projects directory. Title precedence, sidechain /
  metadata-only / no-workspace skips, subagent counting, fork
  provenance, the `entrypoint` marker, dedupe + newest-first sort.
  Ported from `listSessionsImpl.ts`.

  `SessionInfo.Entrypoint` is the transcript's own `entrypoint` field,
  raw and unjudged (`agent-overflow` when AO spawned the CLI, `cli` for
  an interactive run, `sdk-cli`, `""` when the field is absent). Every
  row carries it, so the FIRST occurrence in the head buffer wins and it
  costs no extra read. Deciding what counts as "ours" is the
  orchestrator's call, not this package's — the two providers spell it
  differently and only one place may own that equality.
- `rows.go` — `Row`, the decoded-far-enough-to-walk projection of a
  transcript entry, plus the accessors that read the rest back out of
  `Raw`. `Raw` stays the authority for everything else: a transcript
  carries far more per row than the importer models, and copying it into
  a struct would make every new wire field a code change. On a SKELETON
  row (pass 1, below) `Raw` is nil and `Offset` / `Length` locate the
  line pass 2 decodes it from.
- `transcript.go` — pass 1: the offset-recording line scanner and the
  skeleton it produces. Owns the line cap (16 MB, skip-and-warn), the
  whole-file ceiling (1 GB, refuse), and the `last-prompt` rows branch
  titles come from.
- `dag.go` — `BuildBranches`: the conversation DAG and every leaf in it.
  Runs on SKELETON rows — it needs uuids, parents and types, none of
  which require the row body.
- `subagents.go` — `LoadSubagents`: joins Task/Agent calls to
  `subagents/agent-<agentId>.jsonl`. Called per branch, so each thread
  nests a shared Task launch under its OWN launch row.
- `convert.go` — `Convert`: branch chain → `[]importir.Event`. Owns the
  `converter` state, the row dispatch, `user` rows, subagent nesting,
  turn open/close, and the one `emit` every event goes through.
- `convert_assistant.go` / `convert_system.go` — the two remaining row
  types, split out because each carries its own subtype table.
- `toolresult.go` — `tool_result` decoding: content flattening, exit
  codes, background acks, garbage-collected-output detection.
- `session.go` — `LoadSession` / `LoadedSession.ConvertBranch` /
  `Close`: the two-pass load below. This is what the orchestrator calls.

## Loading a session is TWO passes, and the second one is per branch

```go
loaded, err := sessionimport.LoadSession(path)   // pass 1
defer loaded.Close()
for i := range loaded.Branches {
    branch, err := loaded.ConvertBranch(i)       // pass 2, one branch
    // … use branch.Events, then let it go
}
```

`LoadedSession` holds the OPEN FILE and the skeleton; it does not hold
any branch's events. `Close` is mandatory — the file handle is what pass
2 reads through.

- **Pass 1 (`scanTranscript`)** streams the file once and keeps only a
  skeleton row per line: uuid, parent uuid, logical parent uuid, type,
  the four boolean flags, timestamp, and the line's `(offset, length)`.
  ~100 bytes a row, independent of how big the row is. The DAG, the leaf
  enumeration and the branch ordering all run on this.
- **Pass 2 (`ConvertBranch`)** `ReadAt`s only the lines of the ONE branch
  it was asked for, decodes them, joins that branch's subagents, and
  converts. Nothing else in the file is decoded, and the caller controls
  when each branch's events are released.

This shape is the difference between "a 220 MB transcript costs 220 MB"
and "it costs 0.5–0.9 GB". Decoding every line into `map[string]any` up
front retains 2.2–4.2× the source size, and converting every branch
eagerly holds all of them at once — a multi-branch import of one real
session was enough to run a machine out of memory. **A change that
reintroduces a whole-file decode, or that converts every branch before
returning, undoes this.**

Two ceilings, both of which fail the ONE session rather than the import:

| Limit | Value | Behaviour |
|---|---|---|
| One line | 16 MB | Skipped, counted, and reported as a `transcript-oversized-line` warning; the scan keeps reading past it. The row is simply absent from the DAG, which can leave its children rooting branches of their own — a hole, not an abandoned file. (The previous `bufio.Scanner` made this TERMINAL: one runaway `tool_result` failed the whole session with a raw "token too long".) |
| Whole file | 1 GB | `LoadSession` refuses on the STAT, before reading a byte, with `ErrTranscriptTooLarge` and user-facing prose naming the file. `ImportOne` passes it through so the session is skipped and the rest of an "Import All" is unaffected. |

## Why this builds its own branch index

`claudeBranchIndex` (`../sessionleaf_branch.go`) already walks a
transcript's parent chain, and this package deliberately does not reuse
it.

`claudeBranchIndex` answers a different question — *which single chain
will `claude --resume` accept* — and its `activeChain` walk returns
exactly ONE chain by construction (it starts at the file's last
transcript row and walks up). It then runs that chain through a mirror
of the CLI's resume deserialization filters and REPAIRS the pick. Import
needs the opposite: every leaf, unfiltered, because an abandoned branch
is still a conversation worth importing, and nothing here is going to be
resumed by the CLI at the moment it is read.

Bending the live path to answer both questions would put an import
concern inside the code that decides whether a user's resume succeeds —
the highest-consequence path in the Claude integration (see
`invariants.md` §28 and the BSOD resume-filter incident). So this
package keeps its own index and its own semantics, and the two are
allowed to disagree.

What is NOT duplicated is row admission and parent resolution: those come
from `sessionfork` (`TranscriptTypes` via `ParseTranscript`,
`ResolveParent`, `ResolveLogicalParent`, `SessionIDFromPath` — see that
package's "Shared reading surface"). An importer walking raw `parentUuid`
would break its chain on every `progress` row and start a phantom branch
at every compaction.

## DAG rules

| Rule | Why |
|---|---|
| `progress` rows are dropped from the index entirely | `ResolveParent` already treats them as transparent for children; dropping them makes that transparency consistent for LEAVES too. Keeping them would let a trailing progress row become a phantom branch. |
| `isSidechain` rows are dropped (with a warning) | Old CLIs inlined subagent transcripts into the main file. They have their own parent graph and would enumerate as phantom branches. Current subagent content is joined from `subagents/` instead. |
| Duplicate uuids: first wins (with a warning) | Forks reuse uuids ACROSS files, never within one. A repeat inside one file is corruption. |
| `compact_boundary` chains through `logicalParentUuid` | Its `parentUuid` is null; without this every compaction would split the conversation into two branches. |
| An unresolvable `parentUuid` on a `tool_result` re-attaches to `sourceToolAssistantUUID` | Parallel tool_uses answer in sequence; if one result row was never written, the next one dangles. The assistant row it names is the correct attachment point. |
| A parent cycle drops the branch (with a warning) | Only reachable from a corrupt file; looping is not an option. |
| Branches are ordered by leaf file position | Deterministic, and it puts the file's active branch where a reader expects it. |

## Event vocabulary (Claude transcript → `provider.ProviderEvent`)

`Convert` speaks the same vocabulary a live session does, so the writer
builds the same rows triage builds. Where the transcript and the wire
differ, this is the mapping:

| Transcript row | Event | Notes |
|---|---|---|
| `user`, string or text-block content, not meta/compact-summary/transcript-only | `EventTurnStart` + `EventUserText` | The user prompt is the only turn boundary a transcript records. |
| `user` with `tool_result` blocks | `EventToolComplete` per block, keyed by `tool_use_id` | Upserts onto the launch row. |
| `user` with `isCompactSummary` | folded into the boundary above it, or `EventCompactBoundary` when standalone | One compaction is ONE divider row. |
| `assistant` text block | `EventTextDelta`, `ContentPresent: true` | Carries the WHOLE block. Import never emits a partial delta. |
| `assistant` thinking block | `EventThinking`, `Meta.signature` | |
| `assistant` tool_use block | `EventToolStart` | Meta mirrors the live parser's `marshalToolMeta`. |
| `assistant` with `error` / `isApiErrorMessage` | `EventError` with `Meta.api_error_enum` | The enum is what makes triage persist the `api_error` row kind rather than `error`. The error copy is NOT also emitted as assistant text. |
| `assistant` with `model: "<synthetic>"` | `EventCommandResult` | CLI-executed slash command output; never an assistant bubble. |
| `assistant.message.usage` | accumulated onto the turn's `EventTurnComplete` | See "Usage" below. |
| `system/compact_boundary` | `EventCompactBoundary` | |
| `system/local_command` | `EventCommandResult` | |
| `system/api_error` | `EventError` with `Meta.api_error_enum` | |
| `system/model_refusal_*` | `EventNotification` | |
| `system/turn_duration` | nothing | Its timestamp is the turn's real end and lands on the synthesised completion. |
| unrecognised `system` subtype | nothing, one grouped warning | The subtype set is not closed. |
| `attachment` | nothing | See below. |
| `progress`, `mode`, `queue-operation`, `last-prompt`, `custom-title`, … | nothing | Either not a transcript type, or dropped by the DAG. `last-prompt` is read separately for branch titles. |
| subagent rows | same mappings with `ParentToolUseID` set | Emitted BETWEEN the Task launch and its completion — the order a live session streams them in. The subagent's own opening prompt is skipped: it is the Task tool's input, which the launch row already carries. |

### Meta keys this package introduces

Everything else on `Meta` mirrors a key the live Claude parser already
writes. These are import-specific:

| Key | Value | Meaning |
|---|---|---|
| `import_unavailable` | `"tool-output-gc"` | The tool's real output is gone: Claude either cleared it in place (`[Old tool result content cleared]`) or externalised it to `<sessionDir>/tool-results/<id>.txt` and later garbage-collected the file. The writer lifts this onto `items.meta` via `itemmeta.MarkImportUnavailable`; the frontend renders "Not available from import". Constants: `MetaImportUnavailableKey`, `MetaImportUnavailableToolOutputGC`. |

One translation is worth calling out because it is invisible otherwise:
the transcript spells the structured tool result `toolUseResult`
(camelCase) while the wire — and therefore every downstream shape helper
— spells it `tool_use_result`. Convert emits the snake_case key, so the
writer never has to know which side produced the event. It handles both
observed forms: an object, and a bare string.

### Usage

A transcript has no `result` envelope, so there is no per-turn usage
frame. Convert accumulates each assistant message's `message.usage` per
model and puts the totals on the synthesised `EventTurnComplete` as
`WireTurnCompleteMeta.{Usage, ModelUsage}` — exactly where the wire's
per-turn delta would be. Subagent usage is included, which matches the
wire's `modelUsage` (the only subagent-inclusive source).

Cost is deliberately zero: `total_cost_usd` rides the stream-json
`result` envelope, which is never written to the session file. Imported
usage rows are priced at query time from `internal/usagecost`.

### Why `attachment` rows produce nothing

The stream-json wire has no attachment envelope at all — the live parser
handles no such type — so dropping them IS parity, not a shortcut. Their
bodies are also unbounded (file contents, tool-discovery deltas), which
makes them the wrong thing to fold into a cheap, always-shipped
`items.meta`.

## Timestamps

Every event carries the source row's own ISO timestamp. Rows with no
timestamp inherit the last one seen, and the clock is SEEDED from the
chain's earliest timestamped row so the leading rows have something to
inherit — the writer refuses an import outright rather than restamp with
now(), so one row that lost its `timestamp` would otherwise cost the whole
branch. Nothing in this package calls `time.Now()`: an imported thread
that restamps itself to today is indistinguishable from a fresh one in
every sidebar and every sort.

## Responsibility boundary

- What BELONGS here:
  - Reading and interpreting Claude's on-disk session layout.
  - The transcript → `provider.ProviderEvent` projection.
- What does NOT belong here:
  - SQLite, `triage`, or `store` in any form (layering — see
    `internal/CLAUDE.md`). The one dependency edge out is
    `internal/importir`, which is stdlib + `internal/provider` only.
  - Deciding WHICH sessions to import, project creation, dedupe against
    already-imported threads — that is the orchestrator's job.
  - Home resolution. See "Hard rules".

## Anti-patterns

- Do NOT full-parse a transcript at list time. Listing is a stat plus two
  64 KB reads per file; a user with a thousand sessions notices the
  difference.
- Do NOT call `io.ReadAll` on a transcript, and do NOT decode a whole
  transcript into memory at once. Real sessions reach hundreds of
  megabytes and decoded rows retain several times their source size —
  see "Loading a session is TWO passes" above for what pays for it.
- Do NOT hold more than one branch's converted events at a time.
  `ConvertBranch` is per branch so the caller can release each one;
  returning them all again is the same regression in a different shape.
- Do NOT let one malformed record fail a session. The line cap skips and
  warns; a scanner whose over-long token is terminal does not belong
  here.
- Do NOT reimplement `ResolveParent` / `ResolveLogicalParent` /
  `ParseTranscript`. If import ever needs different semantics it gets its
  own function — it does not get a flag on one of `sessionfork`'s. Pass 1
  reads skeleton rows rather than calling `ParseTranscript`, and shares
  the parent walk through `sessionfork.ResolveParentUUID` for exactly
  this reason: `ParseTranscript`'s behaviour is unchanged for the fork
  consumers.
- Do NOT put heavy content on `Meta`. `preservedSegment` is whitelisted
  OUT of the compaction meta for exactly this reason.
