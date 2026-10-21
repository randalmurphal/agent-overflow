# components/workflows/

The workflows pane presents runs from local and attached computers. Engine
and phase semantics live in
[`workflows-system.md`](../../../../../docs/specs/workflows-system.md); run-map
layout and motion live in
[`workflow-run-map.md`](../../../../../docs/architecture/workflow-run-map.md).

## Pane structure

`WorkflowsPane.svelte` is the body of the `workflows` tool pane
(`stores/toolPanes.svelte.ts`, mounted by `panes/ToolPane.svelte`). The pane
exists exactly while Workflows is open; navigation state outlives it in
`stores/workflowsOverlay.svelte.ts`. Home, run detail, and terminal all-clear
share one scroller. Supply that scroller through `workflowsScroller.ts`; the
context value must remain a getter because the bound element arrives after
first render. Fail at mount when the provider is absent instead of searching
ancestors.

A detail step (run detail, all-clear, a dialog, an armed confirm) reports
itself through the tool's `toolDetailOpen` probe, which gates
`workflows.escape`. At home the gate is closed, so Escape and the phone's Back
pass through instead of being consumed.

## Run-map scroll

`runMapFollow.svelte.ts` owns follow intent independently of the chat scroll
controller. Preserve these rules:

- Every `scrollTop` write uses `writeScrollTop` with `place`, `jump`, `follow`,
  or `compensate` as its cause.
- Wheel, key, touch, and pointer input escape follow. Scroll events do not.
- Re-engagement is explicit. Hiding the travel chip does not re-engage follow.
- Geometry calculations stay pure in `runMapGeometry.ts`.
- Do not add content transforms or `will-change`.

Run-map events are partial hints. When a patch cannot determine the complete
view, invalidate and refetch while retaining the last value.

Route actions to each run's owning computer. Engine status is per computer;
pause-all reports partial failures, and creation uses the selected project's
computer.
