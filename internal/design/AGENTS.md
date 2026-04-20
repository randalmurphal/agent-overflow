# internal/design/

Design-mode lifecycle: artifact storage, the bundled design-mode system
prompt, and the reactor that brokers `present_options` tool calls
between the provider process and the user.

## Layout

- `types.go` — public request/response shapes (`DesignArtifact`,
  `DesignOption`, `DesignOptionsRequest`, `ChoiceResult`).
- `artifacts.go` — store-backed artifact metadata CRUD.
- `prompts.go` — `LoadDesignSystemPrompt`: bundled default plus an
  override at `<configDir>/prompts/design-mode.md`.
- `reactor.go` — pending-choice reactor. Maps `requestID → pendingChoice`,
  emits `design:options` / `design:chosen` events, serializes the user's
  selection back to the provider.

## Responsibility boundary

- What BELONGS here:
  - Persisting artifact metadata (HTML lives as a payload via `store`).
  - Brokering a `present_options` tool call: waiting for the user's
    choice, returning the `ChoiceResult` to the provider.
  - System-prompt loading + override precedence.
- What does NOT belong here:
  - Rendering HTML. That's the frontend.
  - Provider tool-use dispatch. `app.go` maps the tool call to the
    reactor; the reactor brokers the choice.

## Extension points

- To add a new design tool: extend `types.go` with the input shape,
  teach the reactor how to handle it, add a bound method in `app.go`.
- To change the default system prompt: edit the `defaultDesignSystemPrompt`
  literal; the override file still wins at runtime.

## Anti-patterns

- Do NOT cache artifacts in memory. Store is the source of truth.
- Do NOT let a pending choice leak if the design session ends — use
  `ErrDesignSessionEnded` and drain the resultCh.
- Do NOT tie artifact storage to discussion / thread lifecycle beyond
  the FK. Threads come and go; artifacts are independent records.

## References

- `internal/store/designs.go` — artifact metadata schema.
- `docs/architecture/data-flow.md` — where design events sit in the
  overall pipeline.
