<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { timelineNodeKey, type WaitGroupNode } from '../../utils/subagentGrouping';
  import TimelineLeaf from './TimelineLeaf.svelte';

  let {
    pane,
    group,
    onImageExpand,
  }: {
    pane: ThreadPane;
    group: WaitGroupNode;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
  } = $props();
</script>

<div data-testid="wait-group">
  <TimelineLeaf {pane} item={group.parent} {onImageExpand} />
  {#if group.children.length > 0}
    <div class="ml-5 border-l border-border/70 pl-3" data-testid="wait-group-children">
      {#each group.children as child (timelineNodeKey(child))}
        {#if child.kind === 'leaf'}
          <TimelineLeaf {pane} item={child.item} {onImageExpand} />
        {/if}
      {/each}
    </div>
  {/if}
</div>
