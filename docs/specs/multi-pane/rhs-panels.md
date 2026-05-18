# RHS Panels in Panes

> Right-side panels render as side panels when pane width allows, overlay when pane is narrow.

## Decision

- **Side-panel mode**: pane width ≥ 880px. Panel sits to the right of chat as a flex sibling (current `RhsSidebarShell` behavior).
- **Overlay mode**: pane width < 880px. Panel renders as an absolute overlay covering the chat area (and composer) inside that pane only, clipped to the pane's bounds.
- The mode is **derived from current pane width on every render** — not remembered, not persisted. Resizing a pane across the 880 threshold morphs the panel between modes live.
- In overlay mode, the panel **covers everything** inside the pane (chat + composer). To interact with chat or composer, close the panel first. This is a "focus on the panel" mode — accept the cost.
- Close affordances: panel `×` button, `Esc`, re-toggle the same button that opened it. **No click-outside-to-close** in either mode.

## Threshold math

- Panel min width: 380px (`RHS_PANEL_MIN_WIDTH`).
- Chat min width when RHS is open: 500px (reduced from current 640).
- Side-panel mode threshold: `500 + 380 = 880px`.
- Above 880, RHS gets at least 380; chat gets the rest up to its `max-w-62rem` (992px), then panel can grow further if the pane is wider.

## Per-thread state

The existing `frontend/src/lib/stores/rhsPanelSlot.svelte.ts` per-thread snapshot mechanism is unchanged. Open/closed survives thread switch and pane resize. The render mode (overlay vs side-panel) is **not** part of the snapshot — it's a pure function of current pane width at render time.

## Interaction with density modes

The 880px threshold is the **Compact**-mode value, where pane `min-pane-width` is 560 — so overlay mode is reachable. In **Comfortable** mode (`min-pane-width = 880`) and **Spacious** mode (`min-pane-width = 1400`), pane width is always at-or-above 880, so RHS is always in side-panel mode by definition. The overlay code path only runs in Compact mode. See [density-modes.md](./density-modes.md).

## Implementation notes

- `RhsSidebarShell.svelte` gets two render paths gated on `pane.width >= 880`:
  - **Side-panel path**: today's flex sibling layout.
  - **Overlay path**: absolute positioning over the pane interior, full pane width, full pane height, clipped to pane bounds.
- `RHS_PANEL_CHAT_RESERVE_WIDTH` in `frontend/src/lib/stores/rhsPanelSlot.svelte.ts:13` drops from 640 to 500. Audit other downstream math (`getRhsSidebarMaxWidth` etc.).
- Live mode swap: subscribe to the pane's measured width (already published by `layoutMetrics.svelte.ts`); the threshold check is a `$derived` in the shell.
- The overlay must be clipped to the pane bounds — never extend over an adjacent pane. CSS: `overflow: hidden` on the pane container, and absolute positioning relative to the pane.

## Inherited by design-mode

Design preview becomes a RHS panel variant in v1 — it inherits all of the above for free. See [design-mode-integration.md](./design-mode-integration.md).
