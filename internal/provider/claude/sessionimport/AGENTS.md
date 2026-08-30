# internal/provider/claude/sessionimport/

Reads existing Claude Code session transcripts (`~/.claude/projects/...`)
so Agent Overflow can import them as threads. Read-only on every file it
touches, and it spawns nothing.

Consumers: `internal/sessionimport` (the neutral writer) via
`internal/importir`, driven by the App's import methods.

## Hard rules

- **No home resolution in this package.** `Options.ProjectsDir` and the
  session path are always injected. Which Claude home is in play is an
  app-level decision (credential-home override, WSL relocation) and a
  library guess would silently disagree with it. Root `AGENTS.md`
  §Permanent invariants has the reason.
- **No process spawn, ever.** Import is a file read.
- **Tests never touch a real provider home.** Fixtures are hand-written
  rows under `t.TempDir()`.
- **Nothing here calls `time.Now()`.** An imported thread that restamps
  itself to today is indistinguishable from a fresh one in every sidebar
  and every sort.

## Loading a session is TWO passes, and the second decodes one branch

```go
loaded, err := sessionimport.LoadSession(path)     // pass 1
defer loaded.Close()
branch, found, err := loaded.ConvertActiveBranch() // pass 2
```

`LoadedSession` holds the OPEN FILE plus a skeleton, never any branch's
events. `Close` is mandatory: the file handle is what pass 2 reads
through.

- **Pass 1** streams the file once and keeps a skeleton row per line:
  uuid, parent uuid, logical parent uuid, type, the four boolean flags,
  timestamp, and the line's `(offset, length)`. Roughly 100 bytes a row
  regardless of row size. The DAG, leaf enumeration and branch ordering
  all run on this.
- **Pass 2** `ReadAt`s only the lines of the ONE branch it was asked for,
  decodes them, joins that branch's subagents, and converts. Nothing else
  in the file is decoded. `ConvertActiveBranch` owns the production
  selection; indexed `ConvertBranch` remains for legacy branch-thread
  refresh and explicit branch tools.

This shape is the difference between "a 220 MB transcript costs 220 MB"
and "it costs 0.5 to 0.9 GB": decoding every line into `map[string]any`
up front retains 2.2 to 4.2 times the source size. **A change that
reintroduces a whole-file decode, or converts every branch when the
caller needs one, undoes this.**

Two ceilings, both failing the ONE session rather than the import:

| Limit | Value | Behaviour |
|---|---|---|
| One line | 16 MB | Skipped, counted, reported as a `transcript-oversized-line` warning; the scan keeps reading past it. The row is absent from the DAG, which can leave its children rooting branches of their own: a hole, not an abandoned file. A scanner whose over-long token is TERMINAL does not belong here; that is what failed a whole session on one runaway `tool_result`. |
| Whole file | 1 GB | `LoadSession` refuses on the STAT, before reading a byte, with `ErrTranscriptTooLarge` and user-facing prose naming the file. `ImportOne` passes it through so one session is skipped and the rest of an "Import All" is unaffected. |

Listing is cheaper still and must stay that way: a stat plus the first
and last 64 KB of each file, with the only JSON decode being the ONE
candidate line the first-prompt title fallback needs, and only when the
cheaper title sources came up empty. The extractors scan `[]byte` rather
than `string` so the read buffer is never copied, and they scan RAW TEXT
on purpose, because a head/tail window routinely cuts a line in half and
`json.Unmarshal` answers "nothing" for a truncated line that plainly
contains the field.

## Why this builds its own branch index

`claudeBranchIndex` (`../sessionleaf_branch.go`) already walks a
transcript's parent chain, and this package deliberately does not reuse
it. That index answers a resume-safety question: which rows on the active
chain survive Claude's deserialization filters and are valid explicit
`--resume-session-at` cursors, and it returns exactly one. Import needs a
content projection, and refresh needs to locate the descendant of a
previously recorded leaf including inactive branches stored by older
releases, so `BuildBranches` keeps the full unfiltered DAG and
deterministic leaf ordering.

Bending the live path to answer both would put an import concern inside
the code that decides whether a user's resume succeeds, the
highest-consequence path in the Claude integration (invariant 28, and the
BSOD resume-filter incident). The two are allowed to disagree.

What is NOT duplicated is row admission and parent resolution. Those come
from `sessionfork`, which owns the rules and their rationale (see its
AGENTS.md §"Shared reading surface"). Do NOT reimplement
`ParseTranscript`, `ResolveParent` or `ResolveLogicalParent`, and do not
add a flag to one of them: if import ever needs different semantics it
gets its own function. Pass 1 reads skeleton rows rather than calling
`ParseTranscript`, and shares the parent walk through
`sessionfork.ResolveParentUUID`, for exactly that reason.

## DAG rules

| Rule | Why |
|---|---|
| `progress` rows are dropped from the index entirely | `ResolveParent` already treats them as transparent for children; dropping them makes that transparency consistent for LEAVES too, so a trailing progress row cannot become a phantom branch. |
| `isSidechain` rows are dropped, with a warning | Old CLIs inlined subagent transcripts into the main file. They have their own parent graph and would enumerate as phantom branches. Current subagent content is joined from `subagents/` instead. |
| Duplicate uuids: first wins, with a warning | Forks reuse uuids ACROSS files, never within one. A repeat inside one file is corruption. |
| `compact_boundary` chains through `logicalParentUuid` | Its `parentUuid` is null; without this every compaction splits the conversation into two branches. |
| An unresolvable `parentUuid` on a `tool_result` re-attaches to `sourceToolAssistantUUID` | Parallel tool_uses answer in sequence; if one result row was never written the next one dangles, and the assistant row it names is the correct attachment point. |
| A parent cycle drops the branch, with a warning | Only reachable from a corrupt file. |
| Branches are ordered by leaf file position | Deterministic, and it puts the file's active branch where a reader expects it. |

## Event vocabulary (transcript row to `provider.ProviderEvent`)

`Convert` speaks the same vocabulary a live session does, so the writer
builds the same rows triage builds. Where the transcript and the wire
differ:

| Transcript row | Event | Notes |
|---|---|---|
| `user`, string or text-block content, not meta/compact-summary/transcript-only | `EventTurnStart` + `EventUserText` | The user prompt is the only turn boundary a transcript records. |
| `user` with `tool_result` blocks | `EventToolComplete` per block, keyed by `tool_use_id` | Upserts onto the launch row. |
| `user` with `isCompactSummary` | folded into the boundary above it, or `EventCompactBoundary` standalone | One compaction is ONE divider row. |
| `assistant` text block | `EventTextDelta`, `ContentPresent: true` | Carries the WHOLE block. Import never emits a partial delta. |
| `assistant` thinking block | `EventThinking`, `Meta.signature` | |
| `assistant` tool_use block | `EventToolStart` | Meta mirrors the live parser's `marshalToolMeta`. |
| `assistant` with `error` / `isApiErrorMessage` | `EventError` with `Meta.api_error_enum` | The enum is what makes triage persist an `api_error` row rather than `error`. The copy is NOT also emitted as assistant text. |
| `assistant` with `model: "<synthetic>"` | `EventCommandResult` | CLI-executed slash command output, never an assistant bubble. |
| `assistant.message.usage` | accumulated onto the turn's `EventTurnComplete` | See Usage below. |
| `system/compact_boundary`, `system/local_command`, `system/api_error`, `system/model_refusal_*` | `EventCompactBoundary`, `EventCommandResult`, `EventError`, `EventNotification` | |
| `system/turn_duration` | nothing | Its timestamp is the turn's real end and lands on the synthesised completion. |
| unrecognised `system` subtype | nothing, one grouped warning | The subtype set is not closed. |
| `attachment` | nothing | The wire has no attachment envelope at all, so dropping them IS parity. Their bodies are unbounded, which also makes them wrong for a cheap always-shipped `items.meta`. |
| `progress`, `mode`, `queue-operation`, `last-prompt`, `custom-title`, ... | nothing | Not transcript types, or dropped by the DAG. `last-prompt` is read separately for branch titles. |
| subagent rows | same mappings with `ParentToolUseID` set | Emitted BETWEEN the Task launch and its completion. The first scoped user row carries `provider.MetaSubagentOpeningPromptKey` and its transcript uuid in `ItemID`, which the writer re-keys through `provider.SubagentOpeningPromptItemID` to match the row live triage creates from the launch input. |

Two translations worth knowing because they are invisible otherwise:

- The transcript spells the structured tool result `toolUseResult`
  (camelCase) while the wire and every downstream shape helper spell it
  `tool_use_result`. Convert emits the snake_case key so the writer never
  has to know which side produced the event, and handles both observed
  forms (an object and a bare string).
- `MetaImportUnavailableKey` (`itemmeta.ImportUnavailableKey`) is the one
  meta key this package INTRODUCES rather than mirroring from the live
  parser, and `MetaImportUnavailableToolOutputGC` (`"tool-output-gc"`) is
  its only reason today. It means the tool's real output is gone, either
  cleared in place or externalised and later garbage-collected. The
  writer lifts it onto `items.meta` via `itemmeta.MarkImportUnavailable`.

`ConvertResult.Profile.Model` is the newest non-empty, non-`<synthetic>`
`assistant.message.model` on the TOP-LEVEL branch, captured independently
of usage because valid messages can omit usage or report a zero delta.
Joined subagent messages never update it: their model is the child's
selection, not the parent thread's.

### Usage and timestamps

A transcript has no `result` envelope, so there is no per-turn usage
frame. Convert accumulates each assistant message's `message.usage` per
model onto the synthesised `EventTurnComplete` as
`WireTurnCompleteMeta.{Usage, ModelUsage}`, exactly where the wire's
delta would be. Subagent usage is included, matching the wire's
`modelUsage`. Cost is deliberately zero (`total_cost_usd` rides the
`result` envelope, which is never written to the session file), so
imported rows are priced at query time from `internal/usagecost`.

Every event carries the source row's own ISO timestamp. Rows without one
inherit the last seen, and the clock is SEEDED from the chain's earliest
timestamped row so leading rows have something to inherit: the writer
refuses an import outright rather than restamp, so one row that lost its
`timestamp` would otherwise cost the whole branch.

## Responsibility boundary

- Belongs here: reading and interpreting Claude's on-disk session layout,
  and the transcript to `provider.ProviderEvent` projection.
- Does not: SQLite, `triage` or `store` in any form. The one dependency
  edge out is `internal/importir` (stdlib plus `internal/provider` only).
  Deciding WHICH sessions to import, project creation, and dedupe against
  already-imported threads are the orchestrator's job. Home resolution is
  nobody's here.

## Anti-patterns

- Do NOT full-parse a transcript at list time. A user with a thousand
  sessions notices.
- Do NOT call `io.ReadAll` on a transcript, and do NOT decode a whole
  transcript into memory at once.
- Do NOT hold more than one branch's converted events at a time.
  `ConvertBranch` is per branch so the caller can release each one;
  returning them all is the same regression in a different shape.
- Do NOT let one malformed record fail a session.
- Do NOT put heavy content on `Meta`. `preservedSegment` is whitelisted
  OUT of the compaction meta for exactly this reason.
