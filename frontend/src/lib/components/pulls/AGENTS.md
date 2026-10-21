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
detaches both on destroy. Filters are per computer in
`stores/pullsPageFilters.svelte.ts`; the tab and the list width are
appStorage.

A picked MR or pipeline reports through the tool's `toolDetailOpen` probe,
which gates `pulls.escape`; `stepPullsBack` (`stores/pullsCommands.svelte.ts`)
clears the pick. On compact the list is the page, the detail replaces it,
and the pane header's Back is the one Back: neither detail view carries
its own.

## Rows and reads

Row components (`PullRow`, `PipelineRow`) render the views in
`pullRowView.ts` and `pipelineRowView.ts`; attention text and sort live in
`utils/pullsAttention.ts`. `PullsList` and `PipelinesList` take their rows
as props and render through `PullsListBody`, which virtualizes headings,
project errors and rows in one `LongListVirtualizer` and reports the rows
in its viewport range (none while the list is hidden). The stores forward
those as the list's focus (`SetPRListFocus`) and the stage reads
(`ReadPipelineStages`); do not read a row's detail from a component.

`OpenWithAgentMenu.svelte` is one menu for the header button and the row
menu; the thread choices come from `stores/pullsPageAgent.svelte.ts`, which
calls `OpenPRThread` with the seed so the backend owns the whole
checkout-record-draft sequence and its rollback.
