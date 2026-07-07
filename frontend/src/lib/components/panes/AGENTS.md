# components/panes/

Pane host and layout components. This directory owns the boundary
between layout metadata and mounted pane surfaces.

`PaneHost.svelte` renders layout items into registered panes. Keep the
contract explicit:

- Layout items come from `stores/paneLayout.svelte.ts`.
- Runtime pane state comes from `stores/panes.svelte.ts`.
- A source pane and its companions are ONE unit. The layout store
  enforces it (`resnapCompanionItems` on add/move, block-wise ±1 moves);
  drop targeting must offer only block-edge slots (`paneBlockRangeAt`) so
  the preview and the landing agree. Never add an insert path that can
  wedge a pane between a source and its companions.
- A missing pane must render an explicit broken-state surface. Do not
  fall back to `main`; that hides registry/layout drift and will
  duplicate the wrong pane in multi-pane layouts.
- Per-pane measurements are published through
  `stores/layoutMetrics.svelte.ts` by pane id. Panel sizing and future
  split constraints should read those pane-scoped metrics, not
  `window.innerWidth` or total app-shell width.
- Global app surfaces do not belong in the pane loop unless the feature
  is intentionally one-instance-per-pane.

Do not put chat behavior in this directory. Pane components mount and
measure; chat/terminal/sidebar behavior stays in the owning feature
surface and communicates through explicit pane contracts.
