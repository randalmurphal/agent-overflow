<script lang="ts">
  import { makeItem } from '../../../test/helpers/chat';
  import type { TimelineNode } from '../../utils/subagentGrouping';
  import SubagentBodyClip from './SubagentBodyClip.svelte';

  let count = $state(180);
  let nodes = $derived.by<TimelineNode[]>(() =>
    Array.from({ length: count }, (_, index) => ({
      kind: 'leaf' as const,
      item: makeItem({
        id: `clip-row-${index}`,
        itemIndex: index,
        threadId: 'clip-thread',
        kind: 'tool_call',
        toolName: 'Read',
        summary: `Read row ${index}`,
      }),
    })),
  );
</script>

<button type="button" data-testid="append-row" onclick={() => count += 1}>Append</button>
{#snippet renderNode(node: TimelineNode)}
  <div class="h-8" data-testid="clip-row">
    {node.kind === 'leaf' ? node.item.summary : node.kind}
  </div>
{/snippet}
<SubagentBodyClip {nodes} depth={1} live={true} {renderNode} />
