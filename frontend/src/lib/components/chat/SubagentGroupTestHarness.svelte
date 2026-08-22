<!--
  Test-only harness for <SubagentGroup>. Provides a deterministic renderNode
  snippet that stamps nodes with enough info for assertions, so the component
  test can focus on the header/expand behavior rather than rebuilding a full
  timeline dispatch.
-->
<script lang="ts">
  import SubagentGroup from './SubagentGroup.svelte';
  import type { SubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';
  import type { ThreadPane } from '../../stores/thread.svelte';

  let {
    group,
    startDepth = 0,
    pane,
  }: {
    group: SubagentGroupNode;
    /**
     * Starting depth passed to the outermost SubagentGroup. Default 0
     * matches the pre-depth-cap test behavior. Tests that exercise the
     * grandchild marker (depth>=3) pass startDepth=3 to trigger the
     * plateau rendering on the outer card directly.
     */
    startDepth?: number;
    /**
     * Optional pane stub for tests exercising the pane-backed
     * expansion registry and the expand-triggered child hydration.
     */
    pane?: ThreadPane;
    /** Passthrough for the open-in-pane routing override. */
  } = $props();
</script>

{#snippet renderNode(node: TimelineNode, depth: number)}
  {#if node.kind === 'leaf'}
    <div data-testid="leaf" data-depth={depth} data-id={node.item.id}>
      {node.item.summary}
    </div>
  {:else if node.kind === 'group'}
    <SubagentGroup group={node} depth={depth} renderNode={renderNode} />
  {:else if node.kind === 'wait_group'}
    <div data-testid="wait-group" data-depth={depth} data-id={node.parent.id}></div>
  {:else if node.kind === 'read_group'}
    <div data-testid="read-group" data-depth={depth} data-id={node.groupKey}></div>
  {/if}
{/snippet}

<SubagentGroup group={group} depth={startDepth} renderNode={renderNode} pane={pane} />
