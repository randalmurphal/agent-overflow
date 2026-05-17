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
    <!--
      `ml-[5.5625rem]` lines the child rail up with the parent row's
      body column (where "Waiting for N agents" starts) rather than
      the gutter under the icon/label. Math:
        outer px-1 (0.25rem)
        + chevron size-3 (0.75rem) + gap-2 (0.5rem)
        + icon    size-3.5 (0.875rem) + gap-2 (0.5rem)
        + label   w-12 (3rem) + gap-2 (0.5rem)
        = 6.375rem to body-column start
      Subtract the 1px border-l + 0.75rem pl-3 (≈ 0.8125rem) the
      children container adds to land inside its content box:
        6.375rem - 0.8125rem ≈ 5.5625rem.
      If the chevron / icon / label / gap utilities ever change in
      TranscriptDisclosureHeader, recompute this value to keep the
      child rail aligned with the body column.
    -->
    <div class="ml-[5.5625rem] max-h-[20rem] overflow-y-auto border-l border-border/70 pl-3" data-testid="wait-group-children">
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
