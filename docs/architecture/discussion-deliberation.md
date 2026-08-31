# Discussion Deliberation

Multi-agent discussions are the one piece of Go code in this project
that looks like orchestration. Per the core principles it is
*coordination* between multiple provider processes and the frontend,
not orchestration. The individual participants remain the source of
truth for their own turns, and the `discussion` package only decides
whose turn is next. Domain logic lives in `internal/discussion/`, while
store/session/event coordination lives in `internal/discussionapp/`.
The two stable Wails façade files remain at root.

## The Shape

A discussion starts when `App.StartDiscussion(threadID, name)` is
called on a "parent" thread. The binding delegates to
`discussionapp.Service.Start`, which resolves a `DiscussionDefinition`
(global or per-project), spawns one
child thread per participant role, creates a `channels` row of type
`deliberation`, and wires each participant's `DiscussionID` to that
channel id. All step failures share a cleanup helper that tears down
whatever partial state was built (`Service.cleanupSetup`).

Child thread shape (see `BuildParticipantPlans` in
`internal/discussion/participant.go`):

| Column | Value |
|---|---|
| `mode` | `"discussion"`: reserved for this path; `CreateThread` rejects it. |
| `parent_thread_id` | the parent thread's ID. |
| `discussion_id` | the deliberation channel ID (set after channel creation). |
| `workspace_path` / `worktree_path` / `branch` | inherited from the parent. |
| `provider` / `model` | from the definition, falling back to the parent. |
| `title` | `"<parent title> - <FormattedRole>"`. `RoleFromThreadTitle` reverses this to recover the role for channel-message labels. |

Each participant gets a role-specific system prompt injected via
`setThreadSystemPrompt` before its session starts. Starting a
discussion only spins up the participant sessions. It does **not**
prompt anyone. The FSM seeds `CurrentSpeaker` to the first participant,
but a fresh channel has no messages yet; a human posting the opening
topic via `PostChannelMessage` is what kicks off the first turn (see
"Turn Driving" below).

## The FSM

`discussion.Deliberation` (`internal/discussion/deliberation.go`) is a
small, self-locking state machine keyed by channel ID. It tracks:

- `participants`: ordered list of child thread IDs, seeded up front
  by `NewDeliberation` (round-robin order = discussion-definition
  order). `CurrentSpeaker` starts at `participants[0]`.
- `TurnCount` / `MaxTurns`: circuit breaker (default 8, `channels.max_turns`).
- `CurrentSpeaker`: whose turn is next.
- `AwaitingResponse`: true from the moment a turn prompt is claimed
  for dispatch until that turn resolves (a post, or a failed dispatch).
- `ConclusionProposals`: a map of thread-ID → summary, fed by
  `ProposeConclusionFrom` / `WithdrawConclusionProposal`. Reflects each
  participant's LATEST stance only: a text post whose final line starts
  with the `CONCLUDE:` marker (see "Conclusion" below) proposes; a post
  without that marker rescinds whatever that participant proposed
  earlier. Once every roster participant (>= 2) has a live entry, the
  FSM concludes exactly like the `MaxTurns` circuit breaker. See
  "Conclusion" for the full early-exit flow.

Key methods:

- `RecordPost(threadID)` — called once per completed participant turn
  (`discussionapp.Service.recordPost`). Any
  participant's post counts (not just the one the FSM was waiting on —
  e.g. a human manually driving a child thread's pane directly).
  Clears `AwaitingResponse`, increments `TurnCount`, and either
  concludes (`TurnCount >= MaxTurns`) or advances `CurrentSpeaker`
  round-robin.
- `TryClaimCurrentSpeaker()`: the only way `AwaitingResponse` flips to
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
  `discussionapp.Service.claimAndPrompt`.
- `ClearAwaitingResponse()` — un-claims a turn after a dispatch attempt
  failed to actually reach the provider. Without this, a failed
  dispatch would leave `AwaitingResponse` stuck forever (the
  participant never received the prompt, so nothing would ever post to
  clear it via `RecordPost`); clearing it lets the next trigger retry
  prompting the same `CurrentSpeaker`.
- `RestoreDeliberation(channelID, maxTurns, participants, turnCount,
  currentSpeaker)`: reconstructs an equivalent FSM after a restart
  (see "Restart Recovery" below). `AwaitingResponse` always comes back
  false: no provider session survives a restart, so nothing is
  actually in flight.

## Turn Driving

Two triggers advance the conversation, both funneling through
`discussionapp.Service.claimAndPrompt` so the
claim-then-dispatch sequence has exactly one implementation:

1. **A human posts** (`App.PostChannelMessage`, `app_discussion.go`).
   After persisting the message and emitting `discussion:message`,
   `discussionapp.Service.maybePromptNext` resolves the channel's
   deliberation and attempts to claim `CurrentSpeaker`. A no-op if a
   turn is already in flight or the channel isn't a live/rebuildable
   deliberation.
2. **A participant's turn completes** (`discussionapp.Service.SyncTurn`, driven off
   `EventTurnComplete` in `app_provider_events.go`). Resolves the
   channel's deliberation via `deliberationForChannel` **before**
   touching the channel. The ordering is load-bearing for restart
   correctness: the rebuild path reconstructs `TurnCount` by counting
   existing agent channel messages, so mirroring the post first would
   let a cold rebuild count the triggering turn and then `RecordPost`
   increment it again (N prior turns becoming N+2 instead of N+1,
   concluding a turn early). Only then does it read the latest
   `assistant_text` item from that participant's timeline; if found,
   it mirrors it into the channel as a `from_type="agent"` message and
   emits `discussion:message`. The mirrored content keeps a `CONCLUDE:`
   line verbatim (transcript honesty: the other participants must see
   the stance in their own turn prompts). It then parses that same
   `item.Summary` with `discussion.ParseConclusionProposal`: a match
   calls `ProposeConclusionFrom(thread.ID, summary)`, no match calls
   `WithdrawConclusionProposal(thread.ID)`. See "Conclusion" below.
   Either way, found or not, `recordDiscussionPost` always runs
   `RecordPost` against the already-resolved FSM and then attempts to
   claim the next speaker. The "either way" part is the stall fix: a
   **tool-only turn** (the participant only ran tools, no assistant
   text) still has to advance the FSM, or the deliberation would sit
   awaiting a reply that will never arrive. It also leaves conclusion
   stance untouched, since there's no new text to parse.

Exactly one `discussion:state` lands per turn advance: a successful
claim synchronously emits the post-claim snapshot (so the frontend sees
"awaiting X" immediately, and this one emission carries both the recorded
post and the new speaker), and `recordDiscussionPost` emits directly
only when no claim happened (single-participant roster, or a
conclusion). The claim then dispatches `promptDiscussionSpeaker` on a
background goroutine (`promptDiscussionSpeakerAsync`) so the hot path
(a wire RPC call, or the provider event read loop) never blocks on a
provider round-trip. `promptDiscussionSpeaker` itself:

1. Reads the speaker's own last-posted sequence
   (`store.LastChannelMessageSeqFrom`), the cursor for "what has this
   participant not seen yet." A participant that has never posted gets
   `-1` (not `0`: sequences are zero-based, so `0` would silently
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
as labeled user messages, respond with your contribution only", plus
the `CONCLUDE:` marker instruction covered under "Conclusion" below) onto the
per-role preamble via `joinPrompts`, so every participant knows the
multi-party wire shape, and how to propose ending the discussion,
before its first turn prompt arrives.

## Conclusion

A discussion ends for one of three causes (`discussion.ConclusionCause`
in `conclusion.go`), and the system-authored message posted into the
channel is cause-aware:

1. **Turn limit** (`ConclusionTurnLimit`). `RecordPost` increments
   `TurnCount` past `MaxTurns` (the circuit breaker).
2. **Unanimous conclusion proposals** (`ConclusionUnanimous`). Every
   roster participant's LATEST turn ended with a `CONCLUDE:` marker
   line.
3. **Moderator stop** (`ConclusionModerator`). The human moderator ends
   an open discussion immediately via `App.ConcludeDiscussion`,
   independent of turn count or proposal state. See "Moderator stop"
   below.

### The CONCLUDE marker and latest-stance rule

`internal/discussion/conclusion.go` owns the pure parsing/rendering
logic, kept separate from `deliberation.go`'s FSM bookkeeping:

- `ParseConclusionProposal(text) (summary string, ok bool)` inspects
  only the **last non-empty line** of a participant's message for a
  case-insensitive `CONCLUDE:` prefix (leading whitespace tolerated,
  CRLF tolerated). A marker anywhere earlier in the text, including a
  `CONCLUDE:` line followed by more prose, is NOT a proposal; only the
  participant's actual last line counts. Everything after the marker on
  that line is the summary, trimmed and capped at 500 runes
  (rune-safe). An empty summary still returns `ok=true`.
- `BuildConclusionMessage(ConclusionMessageInput)` renders the
  system-authored notice for whichever cause the `ConclusionCause`
  field names. The turn-limit form is the original, byte-identical
  text: `"Discussion concluded: reached the N-turn limit."` The
  unanimous form leads with `"Discussion concluded: all participants
  proposed to conclude."`, then (if at least one participant left a
  non-empty summary) a blank line followed by one `<Role>: <summary>`
  line per roster participant in order, skipping empty summaries. The
  moderator form is a single fixed line, `"Discussion concluded: ended
  by the moderator."`, with no summaries block: the transcript already
  shows any `CONCLUDE:` lines verbatim, and this cause has no FSM
  proposal state to summarize (see "Moderator stop" below).

The FSM's own half lives in `deliberation.go`:

- `ProposeConclusionFrom(threadID, summary)` (pre-existing) records a
  proposal and flips `Concluded` once every roster participant (>= 2)
  has one.
- `WithdrawConclusionProposal(threadID)` removes a proposal. Together
  these implement **latest-stance semantics**: a participant's stance
  is whatever its most recent post said, not "has this participant ever
  proposed." A later plain-text reply (no marker) rescinds an earlier
  proposal, so a participant that flip-flops can't accidentally leave a
  stale "yes" in the unanimity count.

### Wiring: `discussionapp.Service.SyncTurn`

`discussionapp.Service.SyncTurn`, after mirroring a
participant's assistant text into the channel (see "Turn Driving"
above), parses that same text: a match calls `ProposeConclusionFrom`,
no match calls `WithdrawConclusionProposal`. The mirrored channel
content keeps the `CONCLUDE:` line verbatim: only the FSM's
bookkeeping reacts to the marker, not the transcript other participants
read in their own turn prompts. A tool-only turn (no assistant text)
never calls either: there's no new stance to record, so the
participant's prior stance carries forward unchanged.

If the proposal reaches unanimity, `ProposeConclusionFrom` flips
`Concluded` **before** `RecordPost` runs (`syncDiscussionTurn` always
calls `RecordPost` after the propose/withdraw step). `RecordPost`'s
existing terminal-state contract (`if d.state.Concluded { return "",
true }`) then fires without incrementing `TurnCount`. A unanimous
conclusion never touches the turn counter, only the turn-limit path
does.

### Posting the notice

`postDiscussionConclusion(channelID, content)` is the shared post+flip
core: it posts the given content as the message
(`from_type="system"`, `from_id="deliberation"`, `from_role="moderator"`)
**before** flipping `channels.status` to `"concluded"`
(`ChannelService.PostMessage` requires the channel still be `"open"`,
so this ordering is load-bearing), then calls
`UpdateChannelStatus(channelID, "concluded")`. `PostChannelMessage` on
a concluded channel returns an error (wrapping
`discussion.ErrChannelNotOpen`, for the reason given under "Moderator
stop" below).

`concludeDiscussionChannel(channelID, deliberation)` is the turn-limit
and unanimous causes' caller: it derives the cause structurally from
the live FSM, never threaded through as a separate flag:
`unanimous := len(participants) >= 2 &&` every participant has a live
`ConclusionProposals` entry. This means the ordinary `MaxTurns`
conclusion (where proposals are typically absent or partial) reliably
renders the turn-limit form, and a proposal-driven conclusion reliably
renders the unanimous form with each participant's summary. A caller
can never pass a cause that disagrees with what the FSM state actually
shows. Composing the unanimous message's per-role lines needs the
roster's role labels, so `concludeDiscussionChannel` does one extra
`GetChannel` + `discussionRoster` query (a cold path: conclusion
happens once per discussion); a roster resolution failure is returned,
never silently degraded to the wrong-cause message. It then hands the
rendered content to `postDiscussionConclusion`.

A **successful** conclusion also removes the FSM from the service runtime map
(`removeDeliberationByID`) — a concluded channel has nothing left to
coordinate, and retaining the entry would leak it until thread
deletion. `buildChannelState`'s SQLite fallback branch serves concluded
channels from then on, so the post-conclusion `discussion:state` and
any later `GetChannelState` still return a coherent snapshot.

If the conclusion **fails to persist** (the FSM's `Concluded` flips
before `concludeDiscussionChannel` runs, whichever cause triggered it),
the channel row stays `"open"` while the FSM refuses every claim, a
shape that would otherwise wedge the discussion forever.
`maybePromptNextDiscussionSpeaker` is the self-heal seam: on the next
human post it detects the concluded FSM against an open row,
re-attempts `concludeDiscussionChannel` (removing the FSM only on
success, so a still-failing conclusion stays resolvable for the next
retry), emits `discussion:state`, and never claims/prompts. Because
`concludeDiscussionChannel` derives cause from the FSM's own state, this
retry always posts the cause-correct message, including the
restart-shaped variant, where `deliberationForChannel`'s rebuild re-seeds
conclusion proposals from history (see "Restart Recovery" below) and can
come back already concluded-by-consensus while the channel row is still
`"open"`.

### Moderator stop: `App.ConcludeDiscussion`

`App.ConcludeDiscussion(channelID)` (`app_discussion.go`) is the human
"conclude now" affordance: a bound method the frontend calls from
`ChannelHeader`'s Conclude control (visible only while `status ===
'open'`) to end an open discussion immediately, independent of
`MaxTurns` and CONCLUDE-marker unanimity. Flow: `GetChannel` (error if
already non-open, with a clear "already concluded"-shaped message, not
a generic failure), `discussion.BuildConclusionMessage` with
`Cause: ConclusionModerator`, `postDiscussionConclusion`,
`removeDeliberationByID` (safe no-op if no FSM was ever resolved for
this channel), `emitDiscussionState`, then `buildChannelState` to
return a coherent snapshot (serving the SQLite-fallback branch since
the channel is no longer open).

Deliberately, this does **not** resolve or rebuild the deliberation
FSM. Unlike the turn-limit and unanimous causes, the moderator form
needs no proposals or roster. Rebuilding an FSM (or its restart-cold
`deliberationForChannel` reconstruction) just to immediately discard it
would only add failure modes (e.g. a roster-resolution error blocking a
stop the human explicitly asked for) for zero benefit.

`ConcludeDiscussion` does **not** interrupt an in-flight participant
turn. If a participant's provider session is already mid-turn when the
human concludes, that session finishes normally (nothing tells the
provider process to stop) and its reply is dispatched into
`syncDiscussionTurn` exactly like any other completed turn. This is the
in-flight-turn race the feature makes benign rather than eliminates:

- `syncDiscussionTurn`'s own top-of-function open-check
  (`channel.Status != "open" { return nil }`) catches the common case
  where the conclusion landed before the participant's turn-complete
  event was even processed.
- The narrower race (the open-check passes, then status flips
  underneath the call before the channel mirror's `PostMessage`
  actually runs) surfaces as `PostMessage`'s not-open rejection.
  `internal/discussion/channel.go` wraps that rejection in
  `discussion.ErrChannelNotOpen`, and `syncDiscussionTurn` checks
  `errors.Is(postErr, discussion.ErrChannelNotOpen)`: a match logs and
  returns `nil` (skipping `recordDiscussionPost` too: the discussion
  is over, nothing left to advance) instead of propagating a wire error
  onto the parent thread. Before this feature, hitting the not-open
  rejection here was always a fault (the channel shouldn't have gone
  non-open with a turn still in flight, since only `concludeDiscussionChannel`
  flipped status, and that only fires from a completed turn). With a
  human able to stop mid-turn, the interleaving is now an expected
  shape, and the late reply stays fully visible, just only in the
  participant's own child thread, not mirrored into the (concluded)
  channel.

`ConcludeDiscussion` rides `threads:operate`, same class as
`PostChannelMessage`: it is lifecycle control over the deliberation's
coordination state (it removes the live FSM and can race an in-flight
participant turn), not a plain data read.

## Restart Recovery

The service's runtime map is in-memory only per root `CLAUDE.md` principle 3 —
nothing about the FSM's turn state is persisted as such. Instead,
`discussionapp.Service.deliberationForChannel` rebuilds an
equivalent `Deliberation` purely from SQLite when the process restarted
since the channel was opened:

- **Roster** (`discussionRoster`): the parent thread's child threads
  (`ListChildThreads`, ordered by `created_at, rowid`: every
  participant thread shares the same millisecond `CreatedAt`, so the
  `rowid` tiebreak is what keeps roster order deterministic) filtered
  to the ones linked to this channel.
- **TurnCount** (`store.CountChannelMessagesByType(channelID, "agent")`)
  the FSM's turn counter tracks agent posts only. This count is also
  why `syncDiscussionTurn` resolves the deliberation BEFORE mirroring
  the triggering post (see "Turn Driving"): if the post committed
  first, a cold rebuild would count it here and `RecordPost` would
  increment it a second time. Note the count can undercount tool-only
  turns (they advance the live FSM without posting an agent message);
  the rebuilt counter is a best-effort reconstruction from what
  persisted.
- **CurrentSpeaker** (`rebuildCurrentSpeaker`): the participant after
  the last agent poster in round-robin order (`NextSpeakerAfter`,
  exported specifically so this rebuild path uses the exact same rule
  the live FSM does), or `participants[0]` if no agent has posted yet.
- **ConclusionProposals** (re-seeded): `ConclusionProposals` is
  in-memory-only state, same as everything else in `DeliberationState`;
  it does not survive a restart on its own. `scanAgentHistory`
  in `internal/discussionapp` walks the channel's full message history
  ONCE and returns both the last agent poster (feeding
  `rebuildCurrentSpeaker` above) and, per participant, that
  participant's most recent agent-message content, one query serving
  both needs instead of two. After `RestoreDeliberation` constructs the
  rebuilt FSM, `deliberationForChannel` runs that latest content back
  through `discussion.ParseConclusionProposal` per participant and
  calls `ProposeConclusionFrom` on a match. Without this, a discussion
  where every participant's last word before the restart carried a
  `CONCLUDE:` marker would silently lose its unanimity on rebuild. The
  FSM would come back merely at `turnCount`/`maxTurns` instead of
  concluded-by-consensus. If the re-seed alone reaches unanimity, the
  rebuilt FSM comes back `Concluded` while the channel row is still
  `"open"` (the crash landed between unanimity and the status flip
  pre-restart). The self-heal seam in "Conclusion" above resolves it
  on the next human post, posting the correct unanimous message because
  `concludeDiscussionChannel` reads the same re-seeded
  `ConclusionProposals` map.
- **MaxTurns**: `channels.max_turns` (see below).

`Service.deliberationForChannel` double-checks under the service's private
runtime mutex before installing the rebuilt instance, so a race between two concurrent callers (e.g. a
human post and a participant's turn-complete landing at the same
moment right after a restart) can't fork two different `Deliberation`
objects for the same channel — whichever rebuild wins the lock is the
one both callers end up sharing. Root owns neither the map nor its ward.

Only `"deliberation"`-type, `"open"`-status channels are rebuildable; a
concluded/closed channel has nothing left to coordinate, so a miss
there is a real not-found rather than something to reconstruct.

### `channels.max_turns`

Migration v12 (`channel_max_turns`) added `max_turns INTEGER NOT NULL
DEFAULT 8` to `channels`. Pre-v12 rows backfill to the same default
(`discussion.DefaultMaxTurns`) the FSM itself falls back to for a
non-positive value. `ChannelService.Create` normalizes `<= 0 →
DefaultMaxTurns` before persisting. `store` stays "dumb" (no
`discussion` import) and the fallback lives in the one place that
already imports both.

## Events

Two wire events keep the frontend live-updated instead of polling
(`app_discussion.go`):

- **`discussion:message`**: `{channelId, threadId, message}`, where
  `threadId` is the **parent** thread ID (`channel.ThreadID`). The
  frontend's DiscussionView is keyed by the parent thread the channel
  hangs off of, not any one participant's child thread. Emitted at
  every post site: a human `PostChannelMessage`, the agent mirror in
  `syncDiscussionTurn`, and the conclusion system message.
- **`discussion:state`**: the same `ChannelStatePayload` shape
  `GetChannelState` returns: `{channelId, threadId, status, turnCount,
  maxTurns, awaitingResponse, currentSpeakerThreadId,
  currentSpeakerRole, participants: [{threadId, role, provider, model,
  proposedConclusion}, ...]}`. `proposedConclusion` is true when that
  participant has a live entry in the FSM's `ConclusionProposals` map
  (see "Conclusion"); it's populated only on the live-FSM branch. The
  SQLite-fallback branch (concluded/non-open channels) always reports
  `false`, since `ConclusionProposals` doesn't exist to ask once the FSM
  is gone. The frontend badges a participant chip with it in
  `ChannelHeader.svelte`. Emitted whenever the FSM's turn-facing state changes:
  exactly once per turn advance (the post-claim snapshot, or the
  no-claim direct emission from `recordDiscussionPost`), plus a
  conclusion and a failed dispatch's un-claim.
  `buildChannelState` (the shared projector behind
  both the binding and this event) transparently rebuilds via
  `deliberationForChannel` when there's no live FSM, so a client
  opening a discussion after an app restart still gets a coherent
  snapshot; a channel that isn't rebuildable (concluded/closed, or not
  a deliberation channel) falls back to recomputing just the turn count
  and roster directly from SQLite.

Both are observe-tier (`threads:read`) the same way `GetChannelMessages`
and `GetChannelState` are: read-only projections of state a session
granted that scope is already allowed to poll for.

`PostChannelMessage`, however, rides `threads:operate`: unlike a pure
persistence call, it can now dispatch a live prompt into a
participant's provider session (`maybePromptNextDiscussionSpeaker` →
`promptDiscussionSpeakerAsync` → `a.sendMessage`), which puts it in the
same class as `SendMessage`.

## Why This Lives in Go

A single provider process can't know about its sibling participants:
Claude and Codex don't share wire formats, never mind a shared session.
The deliberation FSM is the minimum glue needed to drive "whose turn is
next" between two independent subprocesses while still letting each one
own its own transcript. Nothing here tries to reconstruct per-turn
provider state. That stays in the provider session files and in the
per-participant `items` rows.
