# ADR-003: FIFO Streaming-Phase Interrupt Queue

Status: accepted
Date: 2026-04-18

## Context

When assistant text is streaming, a backgrounded tool can complete
mid-stream. The completion produces a NEW `tool_completion` row —
distinct from the launch, because the launch row is still needed as
the "scrolled past" placeholder.

If the new row inserts immediately, it gets the next available
`item_index` and renders between the streaming text's begin (lower
index) and end (same row, updated summary). The user sees:

```
[streaming text mid-sentence] [tool completion] [more streaming text]
```

Which is wrong — the completion should render below the fully-settled
text, not interleaved with it.

## Decision

Triage maintains a per-thread **streaming-phase interrupt queue**.
Events that would create fresh rows during an active streaming phase
are deferred into the queue; the queue drains FIFO when the streaming
phase settles (at segment completion or turn completion).

The queue uses a slice (not a map) and drains in insertion order.

## Rationale

- Deferring assignment until the stream settles is the only way to
  give the new row a correct `item_index` under the
  server-assigned-index model (ADR-001).
- FIFO matters: if two tools complete mid-stream and we drain out of
  order, a tool's `_End` could render before its `_Begin` — the
  original "end card renders before begin card" bug.
- A slice keeps iteration order deterministic; a map would introduce
  non-determinism.

Considered alternatives:

- **Animation-cycle queue** (drain every N milliseconds regardless
  of stream state). Rejected: timing-dependent, flaky under
  backpressure.
- **Sort on frontend after every upsert.** Rejected: violates the
  "frontend memory is bounded" principle; ordering logic must live in
  one place.
- **Never background-complete mid-stream.** Not under our control —
  providers emit when providers emit.

## Consequences

- Invariant #4 ("FIFO drain") and #11 ("item_index assigned in
  intended-appearance order") are consequences of this ADR.
- The queue must drain on turn-complete even if streaming didn't
  naturally settle (truncated turns). `handleTurnComplete` explicitly
  drains with the errored status flag.
- Any new event kind that produces fresh rows mid-turn must also
  route through the queue. This is called out in invariant #11 and
  in the add-a-new-event-kind recipe in
  [`how-to.md`](../how-to.md).
