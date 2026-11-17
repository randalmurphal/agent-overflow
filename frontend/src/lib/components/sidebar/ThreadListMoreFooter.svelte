<script lang="ts">
  // "Show N More" / "Show Less" under a cut thread list: a project's top
  // level, or an expanded group's members. The cut itself is
  // previewSidebarThreads; this only renders its result.
  import { THREAD_REVEAL_INCREMENT } from '../../utils/sidebarThreadLimits';
  import type { SidebarStatusRollup } from '../../utils/sidebarTreeView';

  interface Props {
    hiddenCount: number;
    /** The most important status behind the cut, shown as a dot. */
    hiddenStatus: SidebarStatusRollup | null;
    canShowLess: boolean;
    /** Prefix of the controls' test ids: `<prefix>-show-more` and so on. */
    testIdPrefix: string;
    onShowMore: () => void;
    onShowLess: () => void;
  }

  let { hiddenCount, hiddenStatus, canShowLess, testIdPrefix, onShowMore, onShowLess }: Props = $props();
  let nextRevealCount = $derived(Math.min(THREAD_REVEAL_INCREMENT, hiddenCount));

  function handleShowMore(e: MouseEvent): void {
    e.stopPropagation();
    onShowMore();
  }

  function handleShowLess(e: MouseEvent): void {
    e.stopPropagation();
    onShowLess();
  }
</script>

<div class="flex items-center gap-1 mr-1">
  {#if hiddenCount > 0}
    <button
      type="button"
      onclick={handleShowMore}
      data-testid="{testIdPrefix}-show-more"
      class="group/more flex items-center gap-1.5 h-6 compact:h-9 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    >
      {#if hiddenStatus}
        <span
          class="w-1.5 h-1.5 rounded-full shrink-0 {hiddenStatus.pill.dotClass} {hiddenStatus.pill.pulse ? 'animate-pulse' : ''}"
          aria-hidden="true"
          data-testid="{testIdPrefix}-hidden-status"
          data-status={hiddenStatus.liveStatus}
        ></span>
      {/if}
      <span>
        Show {nextRevealCount} More{hiddenCount > THREAD_REVEAL_INCREMENT ? ` (${hiddenCount})` : ''}
      </span>
    </button>
  {/if}

  {#if canShowLess}
    <button
      type="button"
      onclick={handleShowLess}
      data-testid="{testIdPrefix}-show-less"
      class="flex items-center h-6 compact:h-9 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    >
      Show Less
    </button>
  {/if}
</div>
