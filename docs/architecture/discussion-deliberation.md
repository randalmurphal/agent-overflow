# Discussion Deliberation

Multi-agent discussions are the one piece of Go code in this project
that looks like orchestration. Per the core principles it is
*coordination* between multiple provider processes and the frontend,
not orchestration — the individual participants remain the source of
truth for their own turns, and the `discussion` package only decides
whose turn is next. Implementation lives in `internal/discussion/` and
`app_discussion_*.go`.

## The Shape

A discussion starts when `App.StartDiscussion(threadID, name)` is
called on a "parent" thread. The binding (in `app_discussion_start.go`)
resolves a `DiscussionDefinition` (global or per-project), spawns one
child thread per participant role, creates a `channels` row of type
`deliberation`, and wires each participant's `DiscussionID` to that
channel id. All four step-failures share a cleanup helper that tears
down whatever partial state was built (`cleanupDiscussionSetup` in
the same file).

Child thread shape (see `buildDiscussionParticipantPlans` in
`app_discussion_start.go`):

| Column | Value |
|---|---|
| `mode` | `"discussion"` — reserved for this path; `CreateThread` rejects it. |
| `parent_thread_id` | the parent thread's ID. |
| `discussion_id` | the deliberation channel ID (set after channel creation). |
| `workspace_path` / `worktree_path` / `branch` | inherited from the parent. |
| `provider` / `model` | from the definition, falling back to the parent. |

Each participant gets a role-specific system prompt injected via
`setThreadSystemPrompt` before its session starts.

## The FSM

`discussion.Deliberation` is a small state machine keyed by channel ID
(`internal/discussion/deliberation.go`). It tracks:

- `participants` — ordered list of child thread IDs.
- `TurnCount` / `MaxTurns` — circuit breaker (default 8).
- `CurrentSpeaker` — whose turn is next.
- `ConclusionProposals` — a map of thread-ID → summary; when every
  participant has posted one, the channel is marked concluded.

`RecordPost(threadID)` rotates to the next speaker, or returns
`shouldConclude = true` when `TurnCount` hits `MaxTurns`.

## The Channel Projection

`internal/discussion/channel.go` owns the `channels` and
`channel_messages` tables. Messages are appended with
`InsertChannelMessageAtomic` so `sequence` is assigned inside the
transaction. When a participant finishes a turn, `syncDiscussionTurn`
(in `app_discussion_runtime.go`) reads the latest assistant text from
that child thread's timeline, posts it as a `channel_messages` row
with `from_type="agent"`, and calls `RecordPost` on the deliberation.
On conclusion the channel status moves from `open` to `concluded` via
`UpdateChannelStatus`.

Human interventions arrive through `App.PostChannelMessage` (in
`app_discussion.go`) as `from_type="human"` and do not advance the
deliberation counter.

## Why This Lives in Go

A single provider process can't know about its sibling participants —
Claude and Codex don't share wire formats, never mind a shared session.
The deliberation FSM is the minimum glue needed to drive "whose turn is
next" between two independent subprocesses while still letting each one
own its own transcript. Nothing here tries to reconstruct per-turn
provider state — that stays in the provider session files and in the
per-participant `items` rows.
