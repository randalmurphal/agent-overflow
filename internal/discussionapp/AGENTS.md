# internal/discussionapp/

Store-backed application coordination for multi-provider discussions.

## Ownership

- `Service` owns the definition registry, ordered channel service, and every
  process-local `Deliberation` instance behind one private mutex.
- `internal/discussion` remains the provider-agnostic domain layer. It owns
  validation, prompt rendering, ordered message rules, and the FSM itself.
- `internal/app.App` retains the stable Wails methods and wire DTOs. It supplies a
  narrow `ParticipantRuntime` adapter for start, stop, prompt cleanup, and
  message dispatch. Session maps and provider processes never move here.
- `internal/app` supplies typed event projection. This package knows committed
  discussion events, not Wails or transport channel policy.

## Load-bearing ordering

- On turn completion, resolve or rebuild the deliberation before persisting
  the participant's channel message. Reversing this counts the triggering
  turn twice after restart.
- Post the conclusion system message and emit it before flipping the channel
  status. `ChannelService.PostMessage` rejects a non-open channel.
- Remove a concluded runtime before emitting its final state so the projector
  serves the durable concluded snapshot.
- A failed participant prompt clears `AwaitingResponse`, emits the corrected
  state, and surfaces an error on the parent thread. It must remain retryable.

## Guardrails

- Do not add a broad App/Host interface. Add the smallest capability to
  `ParticipantRuntime` or `Events` and keep provider decisions in `internal/app`.
- Do not retain channel history in memory. SQLite channel rows remain source
  of truth and runtime reconstruction scans them only when needed.
- Do not split the runtime map or its mutex back across `internal/app` and this package.
- Keep provider-spawn isolation in every application fixture that can start a real
  participant session.
