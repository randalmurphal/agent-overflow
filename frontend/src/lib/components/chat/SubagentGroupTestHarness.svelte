<!--
  Test-only harness for <SubagentGroup>. Provides a deterministic renderNode
  snippet that stamps nodes with enough info for assertions, so the component
  test can focus on the header/expand behavior rather than rebuilding a full
  timeline dispatch.
-->
<script lang="ts">
  import SubagentGroup from './SubagentGroup.svelte';
  import type { SubagentGroupNode, TimelineNode } from '../../utils/subagentGrouping';

  let { group }: { group: SubagentGroupNode } = $props();
</script>

{#snippet renderNode(node: TimelineNode, depth: number)}
  {#if node.kind === 'leaf'}
    <div data-testid="leaf" data-depth={depth} data-id={node.item.id}>
      {node.item.summary}
    </div>
  {:else}
    <SubagentGroup group={node} depth={depth} renderNode={renderNode} />
  {/if}
{/snippet}

<SubagentGroup group={group} depth={0} renderNode={renderNode} />
