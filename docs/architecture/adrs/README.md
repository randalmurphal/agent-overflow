# Architecture Decision Records

Short records of the decisions that shaped this codebase. Each ADR
captures: the context we were in, what we chose, why we chose it over
the alternatives, and what we gave up.

ADRs are how the codebase remembers decisions that aren't obvious from
the code itself. If you find yourself asking "why did we do it this
way?" — that's an ADR moment. Write one.

## Index

| # | Title | Status |
|---|---|---|
| [001](ADR-001-server-assigned-item-index.md) | Server-Assigned `item_index` | accepted |
| [002](ADR-002-subagents-flatten-onto-parent-thread.md) | Codex Subagents Flatten Onto Parent Thread | accepted |
| [003](ADR-003-fifo-streaming-phase-interrupt-queue.md) | FIFO Streaming-Phase Interrupt Queue | accepted |
| [004](ADR-004-task-id-persisted-to-items-meta.md) | `task_id` Persisted to `items.meta` | accepted |
| [005](ADR-005-schema-v15-wipes-items.md) | Schema v15 Wipes `items` and `payloads` | accepted |
| [006](ADR-006-persistitem-single-chokepoint.md) | `persistItem` Is the Single Write+Emit Chokepoint | accepted |
| [007](ADR-007-seteventhook-test-seam.md) | `SetEventHook` Test Seam | accepted |
| [008](ADR-008-cost-computation-in-provider-adapters.md) | Cost Computation in Provider Adapters | accepted |
| [009](ADR-009-stop-is-turn-interrupt.md) | Stop Is a Turn Interrupt, Not a Per-Tool Kill | accepted |
| [010](ADR-010-one-tool-call-card-per-subagent.md) | One `tool_call` Card per Subagent | accepted |

## Template

Start from this shape when writing a new ADR. Keep it under one screen.

```markdown
# ADR-NNN: <Decision>

Status: accepted | superseded | deprecated
Date: YYYY-MM-DD

## Context

What problem we faced. What constraints shaped the decision.

## Decision

What we chose. Be specific — name functions, files, or invariants.

## Rationale

Why this over the alternatives. Include the rejected options and
why they were rejected.

## Consequences

What we accepted. Tradeoffs. Follow-on work implied by the decision.
```

## When to Write an ADR

- The decision is load-bearing and its "why" isn't visible from the
  code.
- The decision chose a path that contradicts a reasonable default.
- Future maintainers would otherwise be tempted to "clean it up" in
  a way that would break the decision.

Don't write an ADR for:

- Obvious decisions the code itself explains.
- Style choices covered by [`conventions.md`](../conventions.md).
- Implementation details the relevant `AGENTS.md` already documents.

## Superseding an ADR

When a later decision replaces an earlier one:

1. Write the new ADR. Reference the superseded one in Context.
2. Flip the old ADR's Status to `superseded` and add a line at the
   top pointing at the replacement.
3. Do NOT delete the old ADR. The history of "we used to do X, now
   we do Y" is load-bearing.

## See Also

- [`../invariants.md`](../invariants.md) — the load-bearing rules
  these ADRs established.
- [`../conventions.md`](../conventions.md) — contributor guardrails.
- [`../how-to.md`](../how-to.md) — extension playbooks for common
  changes.
