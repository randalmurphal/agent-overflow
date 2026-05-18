# Pane Attention Indicators

> Per-pane status dots, with sticky-edge handling for off-screen panes.

## Decision

- Each pane carries a **single status dot** at its top edge (header strip).
- The dot's color, pulse, glow, and clear-trigger are computed by the **existing sidebar pill resolver** (`frontend/src/lib/components/sidebar/threadStatusPill.ts:95-168` — `resolveThreadStatusPill`). **No new state machine.**
- States and treatment mirror the sidebar exactly. The `ThreadLiveStatus` union (`frontend/src/lib/stores/threadStatuses.svelte.ts:61-68`) — `idle | running | awaiting-input | pending-approval | plan-ready | error | interrupted` — flows through the same priority resolver. Same colors, same pulse behavior, same glow ring for `awaiting-input` / `pending-approval`.

## Off-screen sticky behavior

When a pane is scrolled out of view due to horizontal overflow in the pane row, its dot **parks at the visible edge** — right edge for panes off the right, left edge for panes off the left. Multiple off-screen panes stack their dots at the same edge in pane-row order.

As the user scrolls the pane row horizontally, each parked dot smoothly transitions from the parked position back to its anchored position once the pane enters view.

**Click a parked dot** → scrolls the pane row to focus that pane (same scroll-into-view helper as [keyboard-nav.md](./keyboard-nav.md)).

## Per-pane vs centralized

There is **no separate centralized "attention strip"**. The sticky-edge behavior on the per-pane dots makes the per-pane and centralized views the same thing: when panes are visible, the dots sit over their pane; when panes scroll off, the dots sit at the edge. One UI surface, two visual states.

## Implementation notes

- Place each pane's dot in a per-pane header strip element above the chat content.
- Sticky-to-edge: the dots are siblings of (or descendants of an overlay positioned over) the pane row, with their `left` coordinate clamped: `dotLeft = clamp(visibleLeft, anchorX, visibleRight - dotWidth)`.
- Reuse `resolveThreadStatusPill(thread).dotClass`, `.pulseClass`, `.glowClass` directly. No CSS duplication.
- Re-render frequency: the dot reads from the existing per-thread status store; the resolver already updates reactively. No new event subscriptions.
- Click-to-focus on a parked dot routes through the same store action used by `Alt+ArrowLeft/Right` and the routing auto-scroll-into-view.
