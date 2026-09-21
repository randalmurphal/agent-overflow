<script lang="ts">
  // One timeline node dispatched by kind: the leaf and group renderers the
  // transcript is made of. Every surface that renders transcript rows
  // mounts this (the main timeline, the agent pane's scoped timeline and
  // the background tray's digest), so a row can never look different
  // depending on which surface shows it. Container nodes take the same
  // component back as their `renderNode` snippet, which is how nesting
  // recurses without any surface knowing the node kinds.
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { TimelineNode } from '../../utils/subagentGrouping';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import type { UserMessageActions } from './userMessageActions';
  import ActivityRun from './ActivityRun.svelte';
  import ReadGroupRow from './ReadGroupRow.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import TimelineNodeView from './TimelineNodeView.svelte';
  import WaitGroup from './WaitGroup.svelte';

  let {
    pane,
    node,
    depth,
    onImageExpand,
    userMessageActions,
    codexSubagentReceiverLabels = new Map<string, string>(),
  }: {
    pane: ThreadPane;
    node: TimelineNode;
    /** Nesting depth of this node in the timeline tree; top-level rows are 1. */
    depth: number;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
    codexSubagentReceiverLabels?: ReadonlyMap<string, string>;
  } = $props();
</script>

{#snippet renderNode(child: TimelineNode, childDepth: number)}
  <TimelineNodeView
    {pane}
    node={child}
    depth={childDepth}
    {onImageExpand}
    {userMessageActions}
    {codexSubagentReceiverLabels}
  />
{/snippet}

{#if node.kind === 'leaf'}
  <TimelineLeaf
    {pane}
    item={node.item}
    orphan={node.orphan === true}
    {onImageExpand}
    {userMessageActions}
    {codexSubagentReceiverLabels}
  />
{:else if node.kind === 'group'}
  <SubagentGroup {pane} group={node} {depth} {renderNode} />
{:else if node.kind === 'wait_group'}
  <WaitGroup
    {pane}
    group={node}
    {onImageExpand}
    {userMessageActions}
    {codexSubagentReceiverLabels}
    {renderNode}
  />
{:else if node.kind === 'read_group'}
  <ReadGroupRow {pane} group={node} />
{:else if node.kind === 'activity_run'}
  <ActivityRun
    {pane}
    run={node}
    {depth}
    live={node.live}
    atTail={node.atTail}
    {renderNode}
  />
{/if}
