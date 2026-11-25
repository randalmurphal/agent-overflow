# components/panes/

This directory is the only place that turns layout items into mounted panes.
`PaneHost.svelte` owns pane selection and lazy companion mounting;
each pane renders its own header from the shared title controls
(`PaneTitleHandle.svelte`, `PaneHeaderLine.svelte`, `PaneCloseButton.svelte`,
`PanePromoteButton.svelte`).

Tool panes (`pulls`, `workflows`) have no ThreadPane. `ToolPane.svelte` loads
the tool body lazily and the body renders `ToolPaneHeader.svelte`, whose title
is the drag handle. Open, close and placement belong to
`stores/toolPanes.svelte.ts`; titles and icons to `toolPaneMeta.ts`.
A ticket pane (`ticket`) has no ThreadPane either: `TicketPane.svelte`
loads the Issues chunk's `issues/TicketPaneBody.svelte`, whose ticket
header is the pane's (its key is the drag handle). Open, close and
restore belong to `stores/ticketPanes.svelte.ts`.

- Preserve mounted pane identity across selection and compact-screen changes.
  Thread state, scroll position, and observers must survive navigation.
- A thread may appear in only one pane. Multiple panes may share a workspace.
- Resolve workspace actions from `pane.workspace`. A terminal-only pane has no
  workspace, so workspace controls do not render.
- Keep conditional companion surfaces lazy. Capture the import promise once;
  constructing it reactively remounts the surface on unrelated updates.
- Wrap each pane body in `shared/RenderBoundary.svelte` so one render failure
  does not stop updates in other panes.
- A drag that places a pane (thread row, tool, pane title) is under the
  strip's shield from its `dragstart` to its end, wherever the pointer is,
  so `usePaneThreadDrag` is its only handler, pane bodies never see it, and
  a native browser view, which would take the drag from the page, is
  hidden. Do not make the shield wait for a later drag event. A pane body
  sees the drags that place nothing: dragged text and files. A thread
  row's DataTransfer carries only its id (`encodeThreadDragPayload`).
- A pane kind's primary scroll container carries `data-pane-scroller` and
  is focusable (a `tabindex`, `-1` unless it is a tab stop): a keyboard pane move with no
  composer to take the caret focuses it, so PageDown and the arrows scroll
  it. The pane section itself has no tabindex, so a click never focuses it.
- Desktop owns resizers and multi-pane layout. Compact mode shows one pane at a
  time while keeping the strip mounted; use the shared layout mode rather than
  device or runtime detection.
- A promoted pane (`paneLayout.svelte#getPromotedPaneId`, animated by
  `panePromotion.svelte.ts`) keeps its section, laid over the strip, while
  a `data-pane-slot` placeholder holds its flex slot. Code that reads a
  pane's place in the strip (offsets, reveals, drop slots, the measured
  width a divider drag starts from) reads the slot, never the promoted
  section's box. Native browser views read the promoted layer through
  `utils/panePromotion.svelte.ts`.
- Every pane section is its own stacking context (`isolate`): a z-index
  inside a pane orders only that pane's layers. A surface that must paint
  outside its pane (a menu, popover or dialog) mounts on the body, as
  `Popover`, `Modal` and `ContextMenu` do.

## Find

How pane find works, browser find included, is in
[Pane find](../../../../../docs/architecture/pane-find.md).

- A pane kind with find mounts `PaneFindBar.svelte` under its header and
  registers a `PaneFindProvider` for its find key (its layout pane id, or a
  region key) while its body is mounted (`stores/paneFind.svelte.ts`). The
  store owns open, query and focus return; the provider owns matching,
  stepping and clearing.
- Escape belongs to the `find.close` command, never the bar.
- A bar's `fallbackFocus` must be focusable by script (a scroller takes
  `tabindex="-1"`).
- Paint matches with `utils/findHighlights.ts` under
  `findHighlightName(paneId)`, and match with `utils/findMatch.ts`.
- A provider with asynchronous reads runs them through
  `paneFindReads(paneId)`; the browser pane's provider keeps its own queue.
- A list beside a detail declares regions (`declarePaneFindRegions`) and a
  nested surface with its own tabs declares a surface
  (`declarePaneFindSurface`); do not key find state any other way.

Timeline scroll ownership belongs to
[`components/chat/`](../chat/AGENTS.md). Generic virtualization belongs to
[`components/virtual/`](../virtual/AGENTS.md).
