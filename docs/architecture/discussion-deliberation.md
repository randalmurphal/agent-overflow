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
channel id. All step failures share a cleanup helper that tears down
whatever partial state was built (`cleanupDiscussionSetup` in the same
file).

Child thread shape (see `BuildParticipantPlans` in
`internal/discussion/participant.go`):

| Column | Value |
|---|---|
| `mode` | `"discussion"` — reserved for this path; `CreateThread` rejects it. |
| `parent_thread_id` | the parent thread's ID. |
| `discussion_id` | the deliberation channel ID (set after channel creation). |
| `workspace_path` / `worktree_path` / `branch` | inherited from the parent. |
| `provider` / `model` | from the definition, falling back to the parent. |
| `title` | `"<parent title> - <FormattedRole>"` — `RoleFromThreadTitle` reverses this to recover the role for channel-message labels. |

Each participant gets a role-specific system prompt injected via
`setThreadSystemPrompt` before its session starts. Starting a
discussion only spins up the participant sessions — it does **not**
prompt anyone. The FSM seeds `CurrentSpeaker` to the first participant,
but a fresh channel has no messages yet; a human posting the opening
topic via `PostChannelMessage` is what kicks off the first turn (see
"Turn Driving" below).

## The FSM

`discussion.Deliberation` (`internal/discussion/deliberation.go`) is a
small, self-locking state machine keyed by channel ID. It tracks:

- `participants` — ordered list of child thread IDs, seeded up front
  by `NewDeliberation` (round-robin order = discussion-definition
  order). `CurrentSpeaker` starts at `participants[0]`.
- `TurnCount` / `MaxTurns` — circuit breaker (default 8, `channels.max_turns`).
- `CurrentSpeaker` — whose turn is next.
- `AwaitingResponse` — true from the moment a turn prompt is claimed
  for dispatch until that turn resolves (a post, or a failed dispatch).
- `ConclusionProposals` — a map of thread-ID → summary, fed by
  `ProposeConclusionFrom`. Note: this early-exit-by-consensus path is
  **implemented but not currently driven** by any app-layer trigger —
  nothing calls it today. The circuit breaker (`MaxTurns`) is the only
  path that actually concludes a discussion in the current wiring.

Key methods:

- `RecordPost(threadID)` — called once per completed participant turn
  (`app_discussion_runtime.go`'s `recordDiscussionPost`). Any
  participant's post counts (not just the one the FSM was waiting on —
  e.g. a human manually driving a child thread's pane directly).
  Clears `AwaitingResponse`, increments `TurnCount`, and either
  concludes (`TurnCount >= MaxTurns`) or advances `CurrentSpeaker`
  round-robin.
- `TryClaimCurrentSpeaker()` — the only way `AwaitingResponse` flips to
  true. Atomically checks-and-claims in one lock acquisition: fails
  (`ok=false`) if a turn is already in flight, the deliberation
  concluded, or there's no current speaker (a single-participant
  roster never self-loops). This exists specifically to close a race:
  a naive "read state, then dispatch in a goroutine" sequence leaves a
  gap between the read and the eventual claim where a second trigger
  (e.g. a fast second human post landing while the first prompt's
  provider dispatch is still in flight) could see `AwaitingResponse ==
  false` too and double-prompt the same speaker. The app layer always
  claims synchronously, in the same goroutine that decided to prompt,
  before handing the actual dispatch to a background goroutine — see
  `claimAndPromptNextSpeaker` in `app_discussion_drive.go`.
- `ClearAwaitingResponse()` — un-claims a turn after a dispatch attempt
  failed to actually reach the provider. Without this, a failed
  dispatch would leave `AwaitingResponse` stuck forever (the
  participant never received the prompt, so nothing would ever post to
  clear it via `RecordPost`); clearing it lets the next trigger retry
  prompting the same `CurrentSpeaker`.
- `RestoreDeliberation(channelID, maxTurns, participants, turnCount,
  currentSpeaker)` — reconstructs an equivalent FSM after a restart
  (see "Restart Recovery" below). `AwaitingResponse` always comes back
  false — no provider session survives a restart, so nothing is
  actually in flight.

## Turn Driving

Two triggers advance the conversation, both funneling through
`claimAndPromptNextSpeaker` (`app_discussion_drive.go`) so the
claim-then-dispatch sequence has exactly one implementation:

1. **A human posts** (`App.PostChannelMessage`, `app_discussion.go`).
   After persisting the message and emitting `discussion:message`,
   `maybePromptNextDiscussionSpeaker` resolves the channel's
   deliberation and attempts to claim `CurrentSpeaker`. A no-op if a
   turn is already in flight or the channel isn't a live/rebuildable
   deliberation.
2. **A participant's turn completes** (`syncDiscussionTurn`, driven off
   `EventTurnComplete` in `app_provider_events.go`). Reads the latest
   `assistant_text` item from that participant's timeline. If found,
   mirrors it into the channel as a `from_type="agent"` message and
   emits `discussion:message`. Either way — found or not —
   `recordDiscussionPost` always runs `RecordPost` and then attempts to
   claim the next speaker. The "either way" part is the stall fix: a
   **tool-only turn** (the participant only ran tools, no assistant
   text) still has to advance the FSM, or the deliberation would sit
   awaiting a reply that will never arrive.

A successful claim synchronously emits `discussion:state` (so the
frontend sees "awaiting X" immediately) and then dispatches
`promptDiscussionSpeaker` on a background goroutine
(`promptDiscussionSpeakerAsync`) so the hot path (a wire RPC call, or
the provider event read loop) never blocks on a provider round-trip.
`promptDiscussionSpeaker` itself:

1. Reads the speaker's own last-posted sequence
   (`store.LastChannelMessageSeqFrom`) — the cursor for "what has this
   participant not seen yet." A participant that has never posted gets
   `-1` (not `0` — sequences are zero-based, so `0` would silently
   exclude the channel's very first message from a never-yet-posted
   participant's first-ever prompt).
2. Loads unseen messages (`ChannelService.GetMessages`) and renders
   them via `discussion.BuildTurnPrompt` (`internal/discussion/turnprompt.go`):
   each message labeled `Human:` / `Moderator:` / `<Role>:`, followed
   by a "it's your turn" instruction naming the speaker's own
   (`FormatRole`-normalized) role.
3. Dispatches via `a.sendMessage` exactly like a normal chat message.
   On failure, calls `ClearAwaitingResponse` and re-emits
   `discussion:state` so the frontend doesn't show a permanently stuck
   "waiting for X" state; the failure is also logged and surfaced on
   the parent thread via `emitWireErrorToThread`.

A dispatched turn prompt is an ordinary user message, not a system
prompt. The system prompt is installed once via `setThreadSystemPrompt`
at start: `BuildParticipantPrompt` layers
`discussion.discussionProtocolPreamble` ("messages from others arrive
as labeled user messages, respond with your contribution only") onto
the per-role preamble via `joinPrompts`, so every participant knows the
multi-party wire shape before its first turn prompt arrives.

## Conclusion

Reaching `MaxTurns` posts a system-authored message into the channel
(`concludeDiscussionChannel`, `app_discussion_runtime.go`) —
`from_type="system"`, `from_id="deliberation"`, `from_role="moderator"`,
content `"Discussion concluded: reached the N-turn limit."` — **before**
flipping `channels.status` to `"concluded"` (`ChannelService.PostMessage`
requires the channel still be `"open"`, so this ordering is load-bearing).
`PostChannelMessage` on a concluded channel returns an error.

## Restart Recovery

`a.deliberations` is in-memory only per root `CLAUDE.md` principle 3 —
nothing about the FSM's turn state is persisted as such. Instead,
`deliberationForChannel` (`app_discussion_drive.go`) rebuilds an
equivalent `Deliberation` purely from SQLite when the process restarted
since the channel was opened:

- **Roster** (`discussionRoster`) — the parent thread's child threads
  (`ListChildThreads`, ordered by `created_at, rowid` — every
  participant thread shares the same millisecond `CreatedAt`, so the
  `rowid` tiebreak is what keeps roster order deterministic) filtered
  to the ones linked to this channel.
- **TurnCount** (`store.CountChannelMessagesByType(channelID, "agent")`)
  — the FSM's turn counter tracks agent posts only, matching
  `RecordPost`'s sole caller (`syncDiscussionTurn`, invoked exactly
  once per participant turn).
- **CurrentSpeaker** (`rebuildCurrentSpeaker`) — the participant after
  the last agent poster in round-robin order (`NextSpeakerAfter`,
  exported specifically so this rebuild path uses the exact same rule
  the live FSM does), or `participants[0]` if no agent has posted yet.
- **MaxTurns** — `channels.max_turns` (see below).

`deliberationForChannel` double-checks under `a.mu` before installing
the rebuilt instance, so a race between two concurrent callers (e.g. a
human post and a participant's turn-complete landing at the same
moment right after a restart) can't fork two different `Deliberation`
objects for the same channel — whichever rebuild wins the lock is the
one both callers end up sharing.

Only `"deliberation"`-type, `"open"`-status channels are rebuildable; a
concluded/closed channel has nothing left to coordinate, so a miss
there is a real not-found rather than something to reconstruct.

### `channels.max_turns`

Migration v12 (`channel_max_turns`) added `max_turns INTEGER NOT NULL
DEFAULT 8` to `channels`. Pre-v12 rows backfill to the same default
(`discussion.DefaultMaxTurns`) the FSM itself falls back to for a
non-positive value. `ChannelService.Create` normalizes `<= 0 →
DefaultMaxTurns` before persisting — `store` stays "dumb" (no
`discussion` import) and the fallback lives in the one place that
already imports both.

## Events

Two wire events keep the frontend live-updated instead of polling
(`app_discussion_events.go`):

- **`discussion:message`** — `{channelId, threadId, message}`, where
  `threadId` is the **parent** thread ID (`channel.ThreadID`) — the
  frontend's DiscussionView is keyed by the parent thread the channel
  hangs off of, not any one participant's child thread. Emitted at
  every post site: a human `PostChannelMessage`, the agent mirror in
  `syncDiscussionTurn`, and the conclusion system message.
- **`discussion:state`** — the same `ChannelStatePayload` shape
  `GetChannelState` returns: `{channelId, threadId, status, turnCount,
  maxTurns, awaitingResponse, currentSpeakerThreadId,
  currentSpeakerRole, participants: [{threadId, role, provider,
  model}, ...]}`. Emitted whenever the FSM's turn-facing state changes:
  a successful claim, a recorded post, a conclusion, or a failed
  dispatch's un-claim. `buildChannelState` (the shared projector behind
  both the binding and this event) transparently rebuilds via
  `deliberationForChannel` when there's no live FSM, so a client
  opening a discussion after an app restart still gets a coherent
  snapshot; a channel that isn't rebuildable (concluded/closed, or not
  a deliberation channel) falls back to recomputing just the turn count
  and roster directly from SQLite.

Both are wire-safe (not `LocalOnly`) the same way `GetChannelMessages`
and `GetChannelState` are — read-only projections of state a LAN client
is already allowed to poll for.

`PostChannelMessage`, however, **is** `LocalOnly`: unlike a pure
persistence call, it can now dispatch a live prompt into a
participant's provider session (`maybePromptNextDiscussionSpeaker` →
`promptDiscussionSpeakerAsync` → `a.sendMessage`), which puts it in the
same risk class as `SendMessage` — see the category-2 comment in
`internal/transport/internalmethods.go`.

## Why This Lives in Go

A single provider process can't know about its sibling participants —
Claude and Codex don't share wire formats, never mind a shared session.
The deliberation FSM is the minimum glue needed to drive "whose turn is
next" between two independent subprocesses while still letting each one
own its own transcript. Nothing here tries to reconstruct per-turn
provider state — that stays in the provider session files and in the
per-participant `items` rows.
