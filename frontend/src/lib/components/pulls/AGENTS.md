# components/pulls/

The Pull requests page: the body of the `pulls` tool pane
(`stores/toolPanes.svelte.ts`, mounted by `panes/ToolPane.svelte`). Product
behavior and the attention vocabulary live in
[`pull-requests.md`](../../../../../docs/specs/pull-requests.md); the list
and pipeline pumps in
[`forge-transport.md`](../../../../../docs/architecture/forge-transport.md#project-lists).

## Pane structure

`PullsPane.svelte` owns the shell: the tabs, the resizable list column and
the detail beside it (the review pane pinned to the picked MR through
`reviewSubjectForPR`, or `PipelineView` on the picked pipeline). It
attaches `stores/pullsPage.svelte.ts` for its lifetime and
`stores/pipelinesPage.svelte.ts` while the Pipelines tab shows, and
detaches both on destroy. The Merge requests tab's filter, projects and
sort (`PullsToolbar`) are per computer in
`stores/pullsPageFilters.svelte.ts`; the tab and the list width are
appStorage. The list column is a container (`@container/pulls`): under
380px the toolbar's buttons keep their icons and drop their labels. `listWidth.svelte.ts` holds the width the reader asked for and
the width rendered, clamped to leave the detail its minimum in the pane as
measured; the pointer and keyboard paths of `PullsListResizer` set it
through that one clamp.

A picked MR or pipeline reports through the tool's `toolDetailOpen` probe,
which gates `pulls.escape`; `stepPullsBack` (`stores/pullsCommands.svelte.ts`)
runs the pane's ladder: a CI log or the conflict view open in the detail
closes first, then the pick clears. On compact the list is the page, the
detail replaces it, and the pane header's Back is the one Back: the pane
provides `panes/hostedBack.ts`, and the hosted views drop their own Back
controls while it owns Back.

## Rows and reads

Row components (`PullRow`, `PipelineRow`) render the views in
`pullRowView.ts` and `pipelineRowView.ts`; the state labels, tones and
order live in `utils/pullsAttention.ts`, the sort in
`stores/pullsPageRows.ts`. A `PullRow` wraps its title (two lines) and
its facts rather than cutting them, and shows CI only on the reader's
own MRs. `PullsList` and `PipelinesList` take their rows as props and
render through `PullsListBody`, which virtualizes headings, project
errors and rows in one `LongListVirtualizer` and, for a list that asks
(`onVisible`), reports the rows in its viewport range (none while the
list is hidden, and none when it unmounts). The Pipelines list asks, and
its store reads the stages for those rows (`ReadPipelineStages`); the
Merge requests list does not, its rows arriving whole. Do not read a
row's detail from a component. The Pipelines tab's filter chips take
`primitives/chipClasses.ts`, which carries the shared compact hit target;
the toolbar's buttons carry the same.

`OpenWithAgentMenu.svelte` is one menu for the header button (the picked
MR) and the row menu (that row's MR, by key, without picking it, so the
list and the anchor stay on screen); the thread choices come from
`stores/pullsPageAgent.svelte.ts`, which calls `OpenPRThread` with the
seed so the backend owns the whole checkout-record-draft sequence and its
rollback.

A failure renders through the shared error report controls with the
error behind it: `PullsErrorNotice` for the refresh, `PullsProjectError`
for a project (`causeCapture.ts` captures each distinct failure once,
across snapshots and row remounts).
