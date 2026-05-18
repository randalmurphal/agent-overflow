# Multi-pane

> Side-by-side view of multiple thread sessions, so a user can watch and interact with several agents in parallel.

## Goal

The motivating use case is **watching multiple agent sessions simultaneously**. A user needs to:

- Observe progress from N concurrent agents at a glance.
- Interact (type, approve, scroll) with each pane independently.
- Move between panes via keyboard or pointer without losing context.
- Be alerted when an unfocused pane needs attention (approval, error, completion).

## Non-goals (v1)

See [out-of-scope.md](./out-of-scope.md) for the full list. Highlights:

- No vertical splits or arbitrary tiling — horizontal flow only.
- No drag-to-detach (pane to a new OS window).
- No OS / system notifications or audio cues for pane attention (per-pane visual dots only).
- No non-thread pane kinds beyond design-preview-as-RHS. Settings and other config UIs stay as overlays/modals.

## Glossary

- **Pane**: A vertical slice of the workspace area, hosting a single thing (today: a thread). Multiple panes flow horizontally in the pane row.
- **Pane row**: The horizontal container that holds all panes. Sits to the right of the sidebar, fills the rest of the viewport.
- **Layout**: The ordered set of panes plus per-pane size ratios. Persisted per-client in `localStorage`.
- **Ratio**: Per-pane size weight. Total pane area is divided by sum-of-ratios; each pane gets `(its ratio / total) * pane-row-width`, clamped to `min-pane-width`.
- **Density mode**: User setting (Compact / Comfortable / Spacious) controlling `min-pane-width`.
- **RHS panel**: Right-side panel inside a pane (plan, diff, design preview). Renders as side panel above 880px pane width, overlay below — in Compact mode only. Higher density modes' `min-pane-width` keeps RHS in side-panel mode by definition.

## What lives in a pane vs outside

**Inside a pane (v1):** thread surface only. Chat timeline, composer, attention dot, optional RHS panel, optional terminal drawer.

**Outside any pane (overlays / modals):**

- Sidebar (always present; shows all projects + threads).
- Settings.
- Command palette, message search, thread picker, keybindings cheat sheet.
- Discussion-start flow, toast notifications, diagram interaction host.

A pane always contains a thing. There is no "open empty pane" gesture. Creating a pane is always paired with picking what goes in it (today: a thread; future: other `PaneLayoutKind` variants if a real use case arises).

## Decisions index

Each linked doc captures the locked decisions for one topic, plus implementation notes pointing at the code paths that need to change.

| Topic | Doc |
|---|---|
| Sizing, ratios, min-width, overflow scroll | [layout-model.md](./layout-model.md) |
| How threads enter and switch within panes | [thread-routing.md](./thread-routing.md) |
| Drag a thread from sidebar onto the pane row | [drag-and-drop.md](./drag-and-drop.md) |
| Pane creation, close, reorder, post-close focus | [pane-lifecycle.md](./pane-lifecycle.md) |
| Keyboard bindings for pane operations | [keyboard-nav.md](./keyboard-nav.md) |
| Per-pane status dots, sticky-edge for off-screen | [attention-indicators.md](./attention-indicators.md) |
| RHS side-panel vs overlay rendering | [rhs-panels.md](./rhs-panels.md) |
| Design preview folded into RHS as a variant | [design-mode-integration.md](./design-mode-integration.md) |
| localStorage layout, restore behavior | [persistence.md](./persistence.md) |
| Compact / Comfortable / Spacious user setting | [density-modes.md](./density-modes.md) |
| Explicit v1 deferrals | [out-of-scope.md](./out-of-scope.md) |

## Prior prep already on the branch

A significant amount of pane-aware infrastructure landed on `ui-redesign` before this spec was written. None of it needs to be redone; the specs below build on it.

- `PaneHost.svelte` iterates a `getPaneLayoutItems()` array (currently a single-item array hardcoded to `main`).
- `panes.svelte.ts`, `paneLayout.svelte.ts`, `layoutMetrics.svelte.ts` are pane-id keyed.
- RHS panel widths clamp per-pane via `pane.getRhsSidebarMaxWidth`.
- Per-pane composer drafts (`composerDraftRegistry.svelte.ts`), per-pane design-split widths (`designLayout.svelte.ts`).
- Events fan out by `threadId` across all panes (28 sites in `events.ts` already loop `for (const pane of iterPanes())`).
- Two-pane scenarios already exercised by existing tests (`panes.svelte.test.ts:80-183`, `PaneHost.test.ts:74-99`, `rhsPanelSlot.svelte.test.ts:102-127`).
- `PaneActivation: 'preview' | 'committed'` plumbing exists (no UX trigger yet — see [out-of-scope.md](./out-of-scope.md)).

What these specs add: the **UX and policy layer** on top of that infrastructure, plus a small number of architectural changes called out in each doc's "Implementation notes" section. Most notable architectural change: folding the design-mode chat split into the RHS panel infrastructure (one split implementation in the codebase instead of two — see [design-mode-integration.md](./design-mode-integration.md)).
