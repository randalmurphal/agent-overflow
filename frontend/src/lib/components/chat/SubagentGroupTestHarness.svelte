<!--
  Test-only harness for <SubagentGroup>. Provides a deterministic renderNode
  snippet that stamps nodes with enough info for assertions, so the component
  test can focus on the header/expand behavior rather than rebuilding a full
  timeline dispatch.
-->
<script lang="ts">
  import InlineSubagentGroup from './InlineSubagentGroup.svelte';
  import SubagentGroup from './SubagentGroup.svelte';
  import type { SubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';

  let {
    group,
    startDepth = 0,
  }: {
    group: SubagentGroupNode;
    /**
     * Starting depth passed to the outermost SubagentGroup. Default 0
     * matches the pre-depth-cap test behavior. Tests that exercise the
     * grandchild marker (depth>=3) pass startDepth=3 to trigger the
     * plateau rendering on the outer card directly.
     */
    startDepth?: number;
  } = $props();
</script>

{#snippet renderNode(node: TimelineNode, depth: number)}
  {#if node.kind === 'leaf'}
    <div data-testid="leaf" data-depth={depth} data-id={node.item.id}>
      {node.item.summary}
    </div>
  {:else if node.kind === 'group'}
    <SubagentGroup group={node} depth={depth} renderNode={renderNode} />
  {:else if node.kind === 'inline_subagent_group'}
    <InlineSubagentGroup group={node} depth={depth} renderNode={renderNode} />
  {:else if node.kind === 'wait_group'}
    <div data-testid="wait-group" data-depth={depth} data-id={node.parent.id}></div>
  {:else}
    <div data-testid="read-group" data-depth={depth} data-id={node.groupKey}></div>
  {/if}
{/snippet}

<SubagentGroup group={group} depth={startDepth} renderNode={renderNode} />
