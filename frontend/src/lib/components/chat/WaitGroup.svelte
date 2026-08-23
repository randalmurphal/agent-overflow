<script lang="ts">
  import type { Snippet } from 'svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { ExpandedImagePreview } from '../../utils/attachmentPreview.svelte';
  import { timelineNodeKey, type TimelineNode, type WaitGroupNode } from '../../utils/subagentGrouping';
  import TimelineLeaf from './TimelineLeaf.svelte';
  import type { UserMessageActions } from './userMessageActions';
  import { preservePaneScrollAnchor } from './preserveScrollAnchor';
  import { nestedScroll } from '../../utils/scroll/wheelAttribution';

  const INITIAL_VISIBLE_WAIT_CHILDREN = 25;

  let {
    pane,
    group,
    onImageExpand,
    userMessageActions,
    codexSubagentReceiverLabels = new Map<string, string>(),
    renderNode,
  }: {
    pane: ThreadPane;
    group: WaitGroupNode;
    onImageExpand?: (preview: ExpandedImagePreview) => void;
    userMessageActions?: UserMessageActions;
    codexSubagentReceiverLabels?: ReadonlyMap<string, string>;
    /** The timeline's node renderer: a waited Codex spawn's completion
     * is that spawn's CARD (`SubagentGroupNode.anchor`), not a leaf. */
    renderNode: Snippet<[TimelineNode, number]>;
  } = $props();

  // Registry state, not local: `group.groupKey` is the `wait:` key in the
  // pane's subagent-expansion registry, so the reader's "Show N more" answer
  // survives a windowing remount and reads as engagement to the activity-run
  // auto-collapse gate (`hasUserExpansionWithin`) while the row is off-screen.
  let showAllChildren = $derived(pane.isSubagentGroupExpanded(group.groupKey));
  let visibleChildren = $derived(
    showAllChildren ? group.children : group.children.slice(0, INITIAL_VISIBLE_WAIT_CHILDREN),
  );
  let hiddenChildCount = $derived(Math.max(0, group.children.length - visibleChildren.length));
</script>

<div data-testid="wait-group">
  <!-- Header renders the folded `wait_agent` completion when it has loaded, so a
       finished wait reads "Finished waiting" + the waited agent list; until then
       (and at a page boundary where the completion isn't loaded) it falls back to
       the carrier tool_call's "Waiting for N agents". Same TimelineLeaf instance
       across the swap — an in-place prop update, not a remount. -->
  <TimelineLeaf {pane} item={group.completion ?? group.parent} {onImageExpand} {userMessageActions} {codexSubagentReceiverLabels} />
  {#if group.children.length > 0}
    <!--
      `ml-[6.375rem]` lines the completion rail up with the parent
      row's body column (where the receiver list above sits) so the
      completion rows continue the same column rather than the
      gutter under the icon/label. No `border-l` / `pl-3` — the
      vertical bar re-introduces the separate-list look the
      body-column alignment is meant to defeat. Math from the
      disclosure primitive's gutter widths (see
      TranscriptDisclosureHeader):
        outer px-1 (0.25rem)
        + chevron size-3 (0.75rem) + gap-2 (0.5rem)
        + icon    size-3.5 (0.875rem) + gap-2 (0.5rem)
        + label   w-12 (3rem) + gap-2 (0.5rem)
        = 6.375rem to body-column start.
      If the chevron / icon / label / gap utilities ever change in
      TranscriptDisclosureHeader, recompute this value.
    -->
    <div class="ml-[6.375rem] max-h-[20rem] overflow-y-auto" data-testid="wait-group-children" use:nestedScroll>
      {#each visibleChildren as child (timelineNodeKey(child))}
        {#if child.kind === 'leaf'}
          <TimelineLeaf {pane} item={child.item} {onImageExpand} {userMessageActions} {codexSubagentReceiverLabels} />
        {:else}
          {@render renderNode(child, 1)}
        {/if}
      {/each}
      {#if hiddenChildCount > 0}
        <button
          type="button"
          class="my-1 rounded-[var(--radius-control)] px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2/40 hover:text-fg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
          onclick={(event) => preservePaneScrollAnchor(pane, event, () => { pane.toggleSubagentGroupExpanded(group.groupKey); })}
          data-testid="wait-group-show-all"
        >
          Show {hiddenChildCount} more
        </button>
      {/if}
    </div>
  {/if}
</div>
