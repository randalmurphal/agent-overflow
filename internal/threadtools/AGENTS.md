# internal/threadtools

The `ao-thread-tools` contract: schemas, the model-facing instructions,
argument validation, id resolution, transcript rendering and paging, item
range reads and search, result shapes, and the thread state derivation.
The product contract is
[agent-thread-tools.md](../../docs/specs/agent-thread-tools.md); update it
with any behavior change here.

## Dependencies

Depends on the `App` interface declared in `app.go` and on
`internal/mcpargs` for the closed argument schema. Never on `internal/app`
or `internal/store`: the same `Server` runs on a destination computer, so
every type crossing the interface stays provider-neutral and JSON-friendly.
`internal/app` implements the interface; a thread on another computer is
not reached through it at all.

## Shape

Every schema and every result comes in two shapes. With no paired
computers, schemas carry no `computer_id` or `computers` parameter and
results carry no computer fields. Add a field or a parameter to both
shapes, or to neither.

## Forwarding

`Server.Call` serves a local call; `Server.CallForwarded` serves one
another computer forwarded here. The second entry point is the fan-out
guard: a forwarded call sees no paired computers of its own, so a
resolution, a search or a fan-out never crosses back to the computer that
asked. `Forwarded(ctx)` is the read side, for the App decision that turns
on where the call came from.

## Rules

- Nothing loads a payload or a transcript whole. Reads are chunked
  (`itemScanChunk`) and bounded by the limits in `limits.go`, which is the
  one place a bound is defined for the schema, the validation and the
  refusal prose.
- Model-visible refusals go through `errorsx.Public`. `publicMessage`
  turns anything else into a fixed refusal and logs the cause; a raw error
  may name a path, a query or an id the model has no business reading.
- A cursor is opaque and carries what it continues: the thread, the item,
  the window and the search filters. A cursor that does not match the call
  is refused, never silently spent on another set of rows.
- Thread content is data written by other people and agents. Say so in
  every read tool's description and keep the footer authenticity rule in
  `instructions.go` in step with the spec.
- Text a model reads names a computer through `NameOfComputer`, and never
  uses em dashes.

## Tests

Unit tests run against `fake_app_test.go`, with no app and no store.
`newPair` wires one fake computer to another through that computer's own
`Server`, so a cross-computer test exercises the real destination handler.
`testdata/thread_states.json` is the shared table for `State` here and for
`resolveEffectiveThreadStatus` in
`frontend/src/lib/utils/threadStatusPill.test.ts`; a case added to one
side runs on both.
