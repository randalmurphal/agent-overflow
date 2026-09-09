# `internal/triage`

Classifies normalized provider events, persists canonical history, and emits bounded live projections. It does not parse provider wire formats and must not become an in-memory read model.

Read [triage routing](../../docs/architecture/triage-routing.md) before adding an event and [turn lifecycle](../../docs/architecture/turn-lifecycle.md) before changing tool, task, queue, streaming, or active-turn behavior.

## Event routing

- `Router.Handle` must handle every `provider.EventKind` explicitly and return `ErrUnhandledEventKind` for unknown kinds.
- Add a new kind to the provider vocabulary, `AllEventKinds`, the routing table and handler, destination projection, and exhaustive backend/frontend tests together.
- Persist canonical items through the shared store write paths. Streaming text,
  thinking, summaries, and command payloads are inserted at stream start and
  flushed to SQLite on bounded time or byte thresholds, then settled at their
  lifecycle boundary. Keep only the short flush window and explicitly live-only
  projections in memory.
- Emit item payloads through `itemwire` projection. Heavy raw content stays in SQLite for hydration.
- Provider-specific interpretation stays in the corresponding handler or adapter. Do not infer wire facts from display text.

## Identity and ordering

Tool, task, and turn lifecycles have separate identities and terminal signals. Preserve provider item ids, AO turn ids, task ids, `completion_of`, and parent relationships exactly as described in the lifecycle document.

Allocate a turn index once in `turn_lifecycle.go`; every item, usage row, interruption, and queued-send promotion for that turn uses the same value. Do not derive the current turn from item counts or arrival order.

Pending sends are keyed by durable send identity. Acceptance, confirmation, retry, and provider echo may arrive in different orders and must settle idempotently. Promotion from the interrupt queue occurs only after the prior streaming item and turn have settled.

Stopped-thread events route according to their recorded ownership and lifecycle, not whichever thread is currently selected. Late completion remains attached to the turn that started it.

## Subagents and background work

Subagent scope is provider-established. Preserve `ParentToolUseID` and recursive ownership; never flatten a child event into the parent merely because the child thread is not visible.

For Claude resumed async agents, the resume carrier owns that round's lifecycle while the original launch remains the transcript root. Resolve roots through `transcript_root.go` on live routing, backfill, prompts, and compaction paths. Nothing is parented to the carrier.

Background classification comes only from typed provider signals documented in the lifecycle reference. Model prose and timing heuristics are not evidence. Live correlation maps must be bounded, cleared on terminal/session teardown, and protected by their owning mutex.

Transcript backfill is additive and idempotent. It must preserve provider identity, parentage, and existing richer live rows while filling history that was unavailable during streaming.

## Streaming and exported shapes

At most one mutable streaming item exists per logical stream identity. Completion settles that item before turn completion or queued-send promotion is announced. Reconnect and duplicate terminal events must not duplicate rows.

Shapes used by session import, store migrations, or frontend bindings are compatibility surfaces. Keep constructors and metadata helpers centralized, preserve unknown metadata where required, and update all producers and consumers in one change.

### Exported shape surface

`Shape*` helpers and exported DTOs are consumed outside triage. Treat field names, identity semantics, and omission behavior as a wire or import compatibility contract.

Host-maintenance activity is not provider transcript history. Route it to its dedicated activity projection rather than manufacturing provider items.
