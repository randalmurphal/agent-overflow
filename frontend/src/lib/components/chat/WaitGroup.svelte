<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { timelineNodeKey, type WaitGroupNode } from '../../utils/subagentGrouping';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import type { UserMessageActions } from './userMessageActions';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';

  const INITIAL_VISIBLE_WAIT_CHILDREN = 25;

  let {
    pane,
    group,
    onImageExpand,
    userMessageActions,
  }: {
    pane: ThreadPane;
    group: WaitGroupNode;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
  } = $props();

  let showAllChildren = $state(false);
  let visibleChildren = $derived(
    showAllChildren ? group.children : group.children.slice(0, INITIAL_VISIBLE_WAIT_CHILDREN),
  );
  let hiddenChildCount = $derived(Math.max(0, group.children.length - visibleChildren.length));
</script>

<div data-testid="wait-group">
  <TimelineLeaf {pane} item={group.parent} {onImageExpand} {userMessageActions} />
  {#if group.children.length > 0}
    <div class="ml-5 max-h-[20rem] overflow-y-auto border-l border-border/70 pl-3" data-testid="wait-group-children">
      {#each visibleChildren as child (timelineNodeKey(child))}
        {#if child.kind === 'leaf'}
          <TimelineLeaf {pane} item={child.item} {onImageExpand} {userMessageActions} />
        {/if}
      {/each}
      {#if hiddenChildCount > 0}
        <button
          type="button"
          class="my-1 rounded-[var(--radius-control)] px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2/40 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          onclick={(event) => preservePaneScrollAnchor(pane, event, () => { showAllChildren = true; })}
          data-testid="wait-group-show-all"
        >
          Show {hiddenChildCount} more
        </button>
      {/if}
    </div>
  {/if}
</div>
