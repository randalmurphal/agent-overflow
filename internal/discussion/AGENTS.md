# internal/discussion/

Multi-agent discussion orchestration: persisted definitions, channel messaging,
and in-memory deliberation turn tracking. Root `AGENTS.md` principle 1 names
deliberation as the "lightweight coordination" exception.

## Rules

- **`Deliberation` + `DeliberationState` are the only in-memory state here**:
  roster, current speaker, turn count, awaiting-response flag, conclusion
  proposals, for one discussion at a time. `NewDeliberation` seeds the full roster
  up front (round-robin order equals discussion-definition order);
  `RestoreDeliberation` reconstructs an equivalent FSM from persisted SQLite state
  after a restart.
- **`TryClaimCurrentSpeaker` / `ClearAwaitingResponse` are the only ways
  `AwaitingResponse` changes.** Their doc comments carry the double-dispatch race
  `TryClaimCurrentSpeaker`'s atomicity closes.
- **`PostMessage`'s not-open rejection wraps `ErrChannelNotOpen`** so callers can
  `errors.Is` it. `syncDiscussionTurn` (`app_discussion_runtime.go`) uses it to
  tell "the discussion concluded while this participant's turn was in flight"
  (benign, drop the mirror) from a genuine store error (propagate).
- **Conclusion is a marker protocol.** A participant proposes by ending its message
  with a final line starting `CONCLUDE:` (case-insensitive, optional one-line
  summary). `syncDiscussionTurn` parses every turn through
  `conclusion.ParseConclusionProposal` and calls `ProposeConclusionFrom` /
  `WithdrawConclusionProposal`, so the FSM always reflects each participant's
  LATEST message. Once every roster participant (>= 2) has a live proposal it
  concludes exactly like the `MaxTurns` circuit breaker.
- **There are three `ConclusionCause` values**: turn-limit, unanimous, and
  moderator. `App.ConcludeDiscussion` (`app_discussion.go`) is the moderator form:
  it deliberately skips FSM resolution/rebuild (it needs no proposals or roster)
  and does not interrupt an in-flight participant turn, whose late reply mirror is
  dropped as a benign no-op through `ErrChannelNotOpen`.
- `conclusion.go` and `turnprompt.go` are pure and independent of the FSM's
  locking: marker parsing, the cause-aware conclusion notice, and the
  unseen-messages turn prompt. The app layer resolves WHICH messages a speaker has
  not seen and dispatches the result.
- `participant.go` derives the per-participant child-thread blueprint plus the
  shared `discussionProtocolPreamble` every participant's system prompt carries.
  `app_discussion_start.go` consumes a slice of `ParticipantPlan` and runs the
  orchestration (CreateThread / startSession / channel link / cleanup) around it.

## Responsibility boundary

What does NOT belong here:

- Provider calls. Discussions drive multiple providers from
  `app_discussion_drive.go`; this package only coordinates.
- SQL schema. `store` owns the tables (`discussions.go`, `channels.go`).
- Frontend events. `app_discussion_events.go` fans channel messages and FSM-state
  snapshots out (`discussion:message` / `discussion:state`).
- Deciding WHEN to prompt (claim-then-dispatch timing, async dispatch,
  retry-on-failure). That is app-layer turn driving; this package exposes only the
  atomic primitives (`TryClaimCurrentSpeaker` / `ClearAwaitingResponse` /
  `RecordPost`) the app layer composes.

## Extension points

- A new discussion mode (round-robin vs facilitator) extends `Deliberation` with a
  strategy and keeps `DeliberationState` serializable.
- Persisting conclusion artifacts means a column plus a test in
  `internal/store/discussions.go`; deliberation logic stays in memory.

## Anti-patterns

- Do NOT grow `DeliberationState` into a long-lived cache of channel history. The
  channel is the source of truth; in-memory state is turn coordination only.
- Do NOT bypass `ChannelService` to write messages directly against `store`.
  Ordering invariants live here.
- Do NOT couple to a specific provider. `app.go` supplies the participant list.

## References

- `docs/architecture/discussion-deliberation.md`: turn lifecycle and conclusion
  flow.
