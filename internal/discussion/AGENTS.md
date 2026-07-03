# internal/discussion/

Multi-agent discussion orchestration: persisted definitions, channel
messaging, and in-memory deliberation turn tracking.

## Layout

- `registry.go` — `Registry` service for persisted discussion
  definitions (templates + participant lists). Pure store wrapper.
- `channel.go` — `ChannelService` for ordered channel messages.
  Messages persist raw `ChannelMessage.Content` via `internal/store`.
  The frontend renders discussion markdown. `Create` also normalizes
  a non-positive `maxTurns` to `DefaultMaxTurns` before persisting.
- `deliberation.go` — `Deliberation` + `DeliberationState`. The only
  in-memory state in this package: roster, current speaker, turn
  count, awaiting-response flag, conclusion proposals. Coordinates one
  discussion at a time. `NewDeliberation` seeds the full roster up
  front (round-robin order = discussion-definition order);
  `RestoreDeliberation` reconstructs an equivalent FSM from persisted
  SQLite state after a restart. `TryClaimCurrentSpeaker` /
  `ClearAwaitingResponse` are the only ways `AwaitingResponse` changes
  — see their doc comments for the double-dispatch race
  `TryClaimCurrentSpeaker`'s atomicity closes.
- `turnprompt.go` — `BuildTurnPrompt`. Pure renderer: unseen channel
  messages (labeled `Human:` / `Moderator:` / `<Role>:`) followed by a
  your-turn instruction naming the speaker's own role. The app layer
  (`app_discussion_drive.go`) resolves which messages a speaker hasn't
  seen yet and dispatches the result to the provider session — this
  file only shapes the text.
- `participant.go` — `ParticipantPlan`, `BuildParticipantPlans`,
  `BuildParticipantPrompt`, `FormatRole`, `RoleFromThreadTitle`. Pure
  derivation of the per-participant child-thread blueprint from a
  parent thread + `DiscussionDefinition`, plus the shared
  `discussionProtocolPreamble` every participant's system prompt
  carries (explains that other participants' and the human's messages
  arrive as labeled user messages). The App in `app_discussion_start.go`
  consumes a slice of `ParticipantPlan` and runs the orchestration
  (CreateThread / startSession / channel link / cleanup) around it.

## Responsibility boundary

- What BELONGS here:
  - Registering / listing / updating discussion definitions.
  - Appending channel messages in strict order.
  - Turn alternation, awaiting-response tracking, and
    conclusion-proposal bookkeeping during an active deliberation.
  - Rendering the unseen-messages turn prompt (pure text; no I/O).
- What does NOT belong here:
  - Provider calls — discussions drive multiple providers from
    `app_discussion_drive.go`; this package only coordinates.
  - SQL schema. The `store` package owns the tables.
  - Frontend events — `app_discussion_events.go` fans channel messages
    and FSM-state snapshots out (`discussion:message` / `discussion:state`).
  - Deciding WHEN to prompt (claim-then-dispatch timing, async
    dispatch, retry-on-failure). That's app-layer turn driving; this
    package only exposes the atomic primitives
    (`TryClaimCurrentSpeaker` / `ClearAwaitingResponse` / `RecordPost`)
    the app layer composes.

## Extension points

- To add a new discussion mode (e.g. round-robin vs. facilitator):
  extend `Deliberation` with a strategy and keep `DeliberationState`
  serializable.
- To persist conclusion artifacts: add a column + test in
  `internal/store/discussions.go`; keep deliberation logic in memory.
- `ProposeConclusionFrom` (unanimous early-exit) is implemented but not
  currently driven by any app-layer trigger — only the `MaxTurns`
  circuit breaker concludes a discussion today. Wiring a "propose to
  conclude" UI affordance is a natural extension point.

## Anti-patterns

- Do NOT grow `DeliberationState` into a long-lived cache of channel
  history. The channel itself is the source of truth; in-memory state
  is turn coordination only.
- Do NOT bypass `ChannelService` to write messages directly against
  `store`. Ordering invariants live in this package.
- Do NOT couple to a specific provider. Deliberation is provider-
  agnostic; `app.go` supplies the participant list.

## References

- `internal/store/discussions.go`, `internal/store/channels.go` —
  persisted shapes.
- `docs/architecture/discussion-deliberation.md` — turn lifecycle and
  conclusion flow.
- Root `CLAUDE.md` principle 1 — deliberation is the "lightweight
  coordination" exception called out in principle 1.
