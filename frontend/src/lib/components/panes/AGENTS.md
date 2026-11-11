# components/panes/

This directory is the only place that turns layout items into mounted panes.
`PaneHost.svelte` owns pane selection and lazy companion mounting;
`PaneFrame.svelte` owns the common frame and title controls.

Tool panes (`pulls`, `workflows`) have no ThreadPane. `ToolPane.svelte` loads
the tool body lazily and the body renders `ToolPaneHeader.svelte`, whose title
is the drag handle. Open, close and placement belong to
`stores/toolPanes.svelte.ts`; titles and icons to `toolPaneMeta.ts`.

- Preserve mounted pane identity across selection and compact-screen changes.
  Thread state, scroll position, and observers must survive navigation.
- A thread may appear in only one pane. Multiple panes may share a workspace.
- Resolve workspace actions from `pane.workspace`. A terminal-only pane has no
  workspace, so workspace controls do not render.
- Keep conditional companion surfaces lazy. Capture the import promise once;
  constructing it reactively remounts the surface on unrelated updates.
- Wrap each pane body in `shared/RenderBoundary.svelte` so one render failure
  does not stop updates in other panes.
- Desktop owns resizers and multi-pane layout. Compact mode shows one pane at a
  time while keeping the strip mounted; use the shared layout mode rather than
  device or runtime detection.

## Find

Every pane kind with find mounts `PaneFindBar.svelte` directly under its
header, in the flow, keyed by its layout pane id (a companion's own id, not
its source's), and registers a `PaneFindProvider` for that id while its body
is mounted (`stores/paneFind.svelte.ts`). The store owns open, query and
focus return; the provider owns matching, stepping and clearing. Escape
belongs to the `find.close` command, never the bar. A bar's `fallbackFocus`
must be focusable by script (a scroller takes `tabindex="-1"`); the bar
checks that focus landed and reports a fallback that refused it. Paint
matches with `utils/findHighlights.ts` under `findHighlightName(paneId)`.
Thread timelines use `chat/ThreadPaneFind.svelte`; a plain scroll container
uses `scrollTextFind.svelte.ts`. A provider with asynchronous reads runs
them through `paneFindReads(paneId)`, which keeps one in flight per layout
pane across body remounts, applies an answer only for the query and owner
it asked, updates in place on a content change without moving the view,
and withdraws any jump it requested when its query changes or it closes.
Find acts on the focused pane, or under compact on the pane on screen, and
on no overlay above it; the store drops a pane's state and reads when the
pane leaves the layout. Review, terminal, browser and discussion panes
have no provider yet, so mod+f falls through there.

A pane whose body holds a list beside a detail declares find regions
(`declarePaneFindRegions`) and keys each region's provider, bar, reads
and highlights by its region key (`paneFindListKey`, `paneFindDetailKey`)
instead of the layout pane id. Find acts on one region at a time
(`paneFindTarget`): the one on screen when only one is (compact, a stacked
level), else the one last focused or pressed (`paneFindRegion.ts` on the
region's element), else the declared initial one. Each region's state is
its own, so both bars may be open; a region with no provider has no find
and mod+f falls through. Everything the store does per pane (open, close,
focus return, `paneFindAvailable`, drop on layout removal) holds per
region. A list region's provider is a filter: it has no `next` or
`previous`, the bar shows the matching row count and Enter calls
`focusFirst`, and its query narrows the rows in the page's store so every
count follows one source. Matching uses `utils/findMatch.ts`, the rule
thread find paints with. The Pull requests page
(`pulls/pullsPaneFind.svelte.ts`) and Workflows home
(`workflows/workflowsPaneFind.svelte.ts`) filter this way.

Timeline scroll ownership belongs to
[`components/chat/`](../chat/AGENTS.md). Generic virtualization belongs to
[`components/virtual/`](../virtual/AGENTS.md).
