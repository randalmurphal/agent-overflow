# Discussion domain

This package owns discussion definitions, ordered channel messages, and the
small in-memory state machine that alternates participant turns. Provider
sessions, dispatch, recovery orchestration, wire DTOs, and frontend events
belong to `internal/discussionapp` and `internal/app`.

## Contracts

- Persist messages through `ChannelService`. Direct store writes bypass channel
  ordering and open-state checks.
- Preserve `ErrChannelNotOpen` wrapping from `PostMessage`; callers use
  `errors.Is` to treat a late participant reply after conclusion as benign.
- Keep `DeliberationState` limited to coordination state. Persisted channel
  messages remain the conversation source of truth.
- Claim a speaker through `TryClaimCurrentSpeaker` before dispatch and clear the
  claim through `ClearAwaitingResponse`. Their atomicity prevents duplicate
  turns.
- The participant order in a definition is the round-robin order and must
  survive restoration from persisted state.
- A participant's latest final `CONCLUDE:` marker determines its current
  proposal. The discussion concludes only when every participant in a roster of
  at least two has a live proposal, the turn limit is reached, or the moderator
  concludes it.
- Prompt builders in this package are pure. They label external messages and
  participant roles but do not decide when or where to send them.
- Keep provider-specific behavior outside this package.

See `docs/architecture/discussion-deliberation.md` for the turn and conclusion
lifecycle.
