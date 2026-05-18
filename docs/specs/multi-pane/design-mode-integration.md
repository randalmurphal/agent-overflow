# Design Mode Integration

> Design preview becomes a RHS panel variant. The dedicated design-mode chat split is removed.

## Decision

- The design preview UI moves into the `RhsPanel` shell as a new variant (e.g. `'design-preview'`).
- `DesignSplitResizer.svelte` and the dedicated design-split container branch in `ChatView.svelte` are removed.
- Per-pane chat-width state in `frontend/src/lib/stores/designLayout.svelte.ts` is replaced by the standard RHS panel width state in `rhsPanelSlot.svelte.ts`. The two have been solving the same problem; collapse to one.
- Design preview **does not auto-open** when a thread enters design mode. The user opens it explicitly via a "show design preview" toggle in the chat header (similar to the plan-sidebar toggle).
- Design preview is **closable** like any other RHS panel.
- **One RHS panel at a time**. While design preview is open, the diff panel is unavailable. The chat header **hides the diff button** when the thread is in design mode (diff isn't relevant to design iteration). If the user opens another panel kind explicitly, the design preview closes (snapshot persists for re-open).

## Other design-mode UI

- `DesignFeedbackPanel`, `DesignClarificationPicker` — stay in the chat column. These are conversation-like elements that belong with the thread timeline, not with the preview.
- `DesignOptionsPanel` — goes into the RHS panel alongside the preview body. It's design-iteration scaffolding, naturally co-located with the preview.

## Inherited behavior

Because the design preview is just a RHS panel kind, it gets:

- Side-panel vs overlay rendering at the 880px threshold (see [rhs-panels.md](./rhs-panels.md)).
- Per-thread snapshot / restore on thread switch.
- The parent pane's attention dot reflects thread status; there is **no separate design-preview dot**.

## Why fold it in

Two parallel split implementations in the codebase:

1. **Chat ↔ design preview** — `DesignSplitResizer.svelte` + `designLayout.svelte.ts`.
2. **Chat ↔ RHS panel (plan / diff)** — `RhsSidebarShell.svelte` + `rhsPanelSlot.svelte.ts`.

They occupy the same physical space (right of chat), use the same resize gesture util, and store per-pane chat width with the same shape. Keeping both is the kind of duplication CLAUDE.md flags as worth fixing while you're in the area. After this change there is one split in the codebase.

## Implementation notes

- Extend the `RhsPanel` discriminated union in `rhsPanelSlot.svelte.ts` with the new variant.
- Extend `PANEL_COMPONENTS` in `RhsSidebarShell.svelte` with the design-preview component. The `satisfies` clause will fail type-check until this lands — a good safety net.
- Move design preview rendering from `frontend/src/lib/components/chat/ChatView.svelte:571-637` (design-mode branch) into the new panel component (`DesignPreviewPanel` body).
- Delete `frontend/src/lib/components/chat/DesignSplitResizer.svelte` and the chat-split container layout in `ChatView.svelte`.
- Delete `frontend/src/lib/stores/designLayout.svelte.ts`, or fold any remaining design-specific state into `rhsPanelSlot.svelte.ts`. Per-pane chat-width keyed map collapses into the per-pane panel-width clamp that already exists.
- Chat header toggle button visibility:
  - `thread.mode === 'design'`: show design-preview toggle, hide diff toggle.
  - `thread.mode === 'chat'` (or whatever the non-design states are): show diff toggle (when there's a diff to show), hide design-preview toggle.
