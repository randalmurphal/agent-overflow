# Out of Scope (v1)

> Features deliberately deferred. None are blockers; each can be added later without architectural rework.

## Deferred by explicit decision

- **OS / system notifications** for pane attention (approval, error, completion). Per-pane dots + sticky-edge dots cover visual awareness; OS notifications are a future addition.
- **Audio cues** for attention. Same reason — visual is enough for v1.
- **Drag-to-detach** a pane into a new OS window. Wails v3 supports multiple windows but it's a whole second app context (separate stores, separate scroll controllers across processes). Out of scope.
- **Vertical splits / arbitrary tiling.** v1 is horizontal flow only.
- **Non-thread pane kinds** beyond design-preview-as-RHS. Settings stays as a modal overlay (not a pane kind). `PaneLayoutKind` enum remains extensible for future use.
- **Direct-numbered pane focus** (`Alt+1` / `Alt+2` / `Alt+3`). Only `Alt+ArrowLeft/Right` and `Alt+H/L` for v1.
- **Saved layout templates / workspace presets** ("save my 3-agent monitoring layout as a named preset").
- **Cross-client layout sync.** Layout is per-client `localStorage`.
- **Preview vs committed thread activation** (VS Code-style single-click preview / double-click commit). Plumbing exists in `frontend/src/lib/stores/panes.svelte.ts` (`PaneActivation: 'preview' | 'committed'`), no UX trigger.
- **Pane focus wraparound.** `Alt+ArrowRight` from the rightmost pane is a no-op, not a wrap to leftmost.
- **Click-outside-to-close on RHS overlay.** Explicit close only (`×`, `Esc`, re-toggle the panel button).
- **Composer accessible during RHS overlay mode.** Overlay covers everything inside the pane (chat + composer). Side-panel mode is unaffected — the composer is alongside the panel as always.

## Deferred due to architectural cost

- **Terminal as RHS panel option.** Kept as the existing per-pane bottom drawer for v1. `frontend/AGENTS.md` anticipates moving the terminal to an RHS panel option in the future; that's a separate change.
- **`localStorage` durability hardening.** Browser-cache wipe loses the saved layout. Acceptable for v1 — layout is preference, not data. The user re-opens their threads and re-arranges if it happens.

## Revisit if evidence appears

These are decisions we're comfortable shipping, but should be revisited if user feedback or telemetry indicates real friction:

- If users put design-mode threads in narrow panes regularly and complain about the cramped split, consider:
  - A higher `min-pane-width` for design-mode threads.
  - A Compact-only restriction where design-mode threads force Comfortable-density behavior locally.
- If audio / OS notifications become missed by users actually monitoring multiple agents, prioritize the OS notification path.
- If `--connect` clients want layout to follow them across devices, add backend storage with client-id keying. The analysis is in the design conversation; not needed v1.
- If the "cover everything" overlay model bites for the plan-panel approval flow (close-type-reopen dance to send a clarifying message before approving), revisit overlay shape to keep the composer accessible.

## Explicit non-features

These are not features we plan to add. Listed for clarity.

- Multiple instances of the same thread visible in different panes simultaneously. The no-duplicates rule is permanent: focus-existing always wins, regardless of gesture.
- Project-scoped pane layouts. Panes can span projects freely; layout is global per-client.
- Per-pane sidebar (mini-sidebar inside each pane). The sidebar stays global, shows all projects.
