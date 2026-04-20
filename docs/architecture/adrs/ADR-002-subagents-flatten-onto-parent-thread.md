# ADR-002: Codex Subagents Flatten Onto Parent Thread

Status: accepted
Date: 2026-04-18

## Context

Codex's `CollabAgentToolCall` runs a subagent on a separate Codex
`thread_id` (`receiverThreadIds`). There were two ways to surface
the child's conversation:

- **Thread-switching.** Treat the child's thread as a first-class
  thread the user can switch into. Each subagent appears in the
  sidebar.
- **Flatten onto parent thread.** Persist child events in the parent
  thread's `items` with `parent_id` pointing at the subagent
  `tool_call` card. The child's conversation renders inside an
  expandable card on the parent timeline.

Claude's `Task` tool already uses the flatten model —
`parent_tool_use_id` correlates child events with the parent launch,
and the child never becomes its own thread. For Codex we had the
choice.

## Decision

Codex subagents flatten onto the parent thread, matching Claude's
model. The adapter subscribes to child threads on SpawnAgent
completion and re-emits events onto the parent thread's store with
`parent_id = card.id`.

## Rationale

- **One provider, one thread.** A "thread" in our model is a user
  conversation. Child subagents are provider-internal machinery; the
  user initiated the parent's turn, not the child's.
- **No mode switch.** The user never has to choose between "watch the
  parent" and "watch the child." Both render, one inside the other.
- **Crash recovery is symmetric.** Claude's replay already delivers
  child events re-threaded under the parent id; matching that for
  Codex means the same recovery path works for both providers.
- **UX parity with Claude.** Claude Code's CLI renders Task children
  inline under their launch; copying that UX for Codex avoids a
  provider-specific mental model.

Considered alternatives:

- **Thread-switching model.** Rejected: clutters the sidebar,
  fragments conversations, requires per-provider UX branches. The
  sidebar would grow every time an agent spawned a subagent, which
  on long sessions is dozens of entries.
- **Hybrid (show in sidebar, also render inline).** Rejected:
  duplicates state and adds confusion — which is the "real" view?

## Consequences

- Invariant #10 ("subagent child events inherit the subagent card's
  `turn_index`") exists because of this flattening: child events
  persist under the parent thread, so their turn_index has to come
  from the launching card, not the parent thread's active turn.
- Invariant #9 (scoped `segmentIndexByScope` / `blockIndexByScope`)
  exists because parallel subagents in the same turn would collide
  on id generation without per-card scope counters.
- The Codex adapter is more complex than it would be otherwise — it
  opens child-thread subscriptions and re-emits onto the parent. The
  complexity lives in `internal/provider/codex/session.go`.
- Nesting cap: Codex subagents spawning sub-subagents render as a
  minimal marker, not a full child timeline. See the "Nesting depth
  cap" section in [`chat-rewrite.md`](../chat-rewrite.md). Claude's
  Task doesn't allow recursive spawning, so this is Codex-only.
