<script lang="ts">
  import { makeItem } from '../../../test/helpers/chat';
  import { timelineNodeKey, type TimelineNode } from '../../utils/subagentGrouping';
  import type { PaneScrollController } from '../../stores/threadPaneShared';
  import SubagentBodyClip from './SubagentBodyClip.svelte';

  let { onController }: { onController?: (controller: PaneScrollController | null) => void } = $props();
  const windowOwner = {
    attach: (controller: PaneScrollController) => onController?.(controller),
    detach: () => onController?.(null),
    itemIds: (node: TimelineNode) => node.kind === 'leaf' ? [node.item.id] : [],
  };
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
  <div class="h-8" data-testid="clip-row" data-item-id={node.kind === 'leaf' ? node.item.id : undefined}>
    {node.kind === 'leaf' ? node.item.summary : node.kind}
  </div>
{/snippet}
<SubagentBodyClip getKey={timelineNodeKey} {nodes} depth={1} live={true} {renderNode} {windowOwner} />
