<!-- Test-only harness for <InlineSubagentGroup>. -->
<script lang="ts">
  import InlineSubagentGroup from './InlineSubagentGroup.svelte';
  import type { InlineSubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';

  let {
    group,
    startDepth = 1,
  }: {
    group: InlineSubagentGroupNode;
    startDepth?: number;
  } = $props();
</script>

{#snippet renderNode(node: TimelineNode, depth: number)}
  {#if node.kind === 'group'}
    <div
      data-testid="inline-subagent-member"
      data-depth={depth}
      data-group-key={node.groupKey}
      data-parent-id={node.parent.id}
    >
      {node.parent.summary}
    </div>
  {:else if node.kind === 'leaf'}
    <div data-testid="leaf" data-depth={depth} data-id={node.item.id}>
      {node.item.summary}
    </div>
  {/if}
{/snippet}

<InlineSubagentGroup group={group} depth={startDepth} renderNode={renderNode} />
