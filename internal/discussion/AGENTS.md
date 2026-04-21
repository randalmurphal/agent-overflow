# internal/discussion/

Multi-agent discussion orchestration: persisted definitions, channel
messaging, and in-memory deliberation turn tracking.

## Layout

- `registry.go` — `Registry` service for persisted discussion
  definitions (templates + participant lists). Pure store wrapper.
- `channel.go` — `ChannelService` for ordered channel messages.
  Messages persist via `internal/store`. Takes a
  `*highlight.Renderer` at construction and pre-renders
  `ChannelMessage.Content` → `HighlightedContent` before insert so the
  frontend paints `{@html msg.highlightedContent}` with no per-message
  UI-thread cost; the raw `content` field is preserved for copy /
  export paths.
- `deliberation.go` — `Deliberation` + `DeliberationState`. The only
  in-memory state in this package: current speaker, turn count,
  conclusion proposals. Coordinates one discussion at a time.

## Responsibility boundary

- What BELONGS here:
  - Registering / listing / updating discussion definitions.
  - Appending channel messages in strict order.
  - Turn alternation and conclusion-proposal bookkeeping during an
    active deliberation.
- What does NOT belong here:
  - Provider calls — discussions drive multiple providers from
    `app.go`; this package only coordinates.
  - SQL schema. The `store` package owns the tables.
  - Frontend events — `app.go` fans channel messages out.

## Extension points

- To add a new discussion mode (e.g. round-robin vs. facilitator):
  extend `Deliberation` with a strategy and keep `DeliberationState`
  serializable.
- To persist conclusion artifacts: add a column + test in
  `internal/store/discussions.go`; keep deliberation logic in memory.

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
