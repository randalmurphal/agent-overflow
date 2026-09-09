# components/panes/

This directory is the only place that turns layout items into mounted panes.
`PaneHost.svelte` owns pane selection and lazy companion mounting;
`PaneFrame.svelte` owns the common frame and title controls.

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

Timeline scroll ownership belongs to
[`components/chat/`](../chat/AGENTS.md). Generic virtualization belongs to
[`components/virtual/`](../virtual/AGENTS.md).
