# ADR-010: One `tool_call` Card per Subagent

Status: accepted
Date: 2026-04-18

## Context

Codex's `CollabAgentToolCall` variants describe distinct lifecycle
events for a single subagent: `SpawnAgent`, `SendInput`, `Wait`,
`CloseAgent`, `ResumeAgent`. Each arrives as its own item on the
wire, each with its own `tool` field discriminator.

Claude's `Task` tool is simpler: one launch event, one completion.

The question: how many `tool_call` cards does the parent timeline
render for a subagent?

- **One card per lifecycle event.** Each spawn/send/wait/close
  appears as its own row. The parent timeline fills up fast when a
  subagent is long-lived.
- **One card per subagent.** The SpawnAgent creates the card;
  subsequent lifecycle events fold into the card's existing state
  (status transition, summary update, child-event population).

## Decision

One `tool_call` card per subagent. The SpawnAgent event mints the
card; subsequent lifecycle events mutate the card's state rather
than creating new rows.

Exception: `SendInput` (parent-to-child message) renders as a
separate line item in the parent timeline, NOT folded into the
subagent card. It's a distinct action the parent agent took.

## Rationale

- **UX coherence.** A subagent is one conceptual unit. The user
  clicks the card to see its conversation. Fragmenting the unit
  across multiple cards makes the timeline incoherent.
- **Matches Claude.** Claude Task emits one launch + one
  completion for the subagent; by collapsing Codex's richer
  lifecycle into the same shape, both providers render the same
  way.
- **Lifecycle → status.** `Wait` is implicit in `status=running`;
  `CloseAgent` → `completed` or `errored`; `ResumeAgent` →
  `running` again. A single row with a mutating status is simpler
  than a row-per-event timeline.
- **SendInput is different.** It's not a lifecycle event. It's
  the parent AGENT sending a message to the child. That's
  semantically a distinct action the user wants to see inline in
  the parent's timeline. Folding it into the card would hide it.

Considered alternatives:

- **Card per lifecycle event.** Rejected: fragments the unit,
  fills the timeline with provider-internal machinery the user
  doesn't need to see.
- **Card per lifecycle event but collapsed by default.** Rejected:
  still fragments on expand; complexity without benefit.

## Consequences

- The Codex adapter is responsible for lifecycle → status
  translation. `SpawnAgent` / `SendInput` / `Wait` / `Close` /
  `Resume` are dispatched on the `tool` field of a
  `CollabAgentToolCall`; only SpawnAgent creates a new item.
- The card's meta carries `receiverThreadId` (populated on
  SpawnAgent's `item/completed`, not `item/started`, because
  `receiverThreadIds` doesn't exist until spawn completes). This
  is invariant-adjacent: reconnect recovery needs the
  `receiverThreadId` to resubscribe.
- Live activity (last child event summary, live count) updates on
  the card's meta/summary as child events flow in. This is a
  denormalized projection: the triage handler for child events
  updates the card as part of persisting the child item.
- Nesting cap applies here: if a Codex subagent spawns a
  sub-subagent, the grandchild is represented as a minimal marker
  inside the child's expansion. No full grandchild card, no
  grandchild subscription. See the "Nesting depth cap" section in
  [`chat-rewrite.md`](../chat-rewrite.md).
