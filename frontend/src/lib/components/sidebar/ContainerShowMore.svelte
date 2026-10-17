<script lang="ts">
  // The show-more footer under a group's members or an agent's live
  // children once they pass the container bound (boundContainerMembers),
  // styled as the project's own show-more footer: the hidden members'
  // rolled-up dot, then the count the next click reveals.
  import type { SidebarContainerMoreNode } from '../../utils/sidebarTreeView';
  import { THREAD_REVEAL_INCREMENT } from '../../utils/sidebarThreadLimits';
  import { sidebarGroupMemberPaddingLeftPx } from '../../utils/sidebarRowMetrics';

  interface Props {
    node: SidebarContainerMoreNode;
    onReveal: () => void;
  }

  let { node, onReveal }: Props = $props();
  let nextReveal = $derived(Math.min(THREAD_REVEAL_INCREMENT, node.hiddenCount));
  // The label starts where a member row's title does.
  let paddingLeftPx = $derived(sidebarGroupMemberPaddingLeftPx(node.depth + 1));
</script>

<div class="flex items-center pr-1" data-testid="sidebar-container-more-row">
  <button
    type="button"
    onclick={(e) => { e.stopPropagation(); onReveal(); }}
    data-testid="sidebar-container-show-more"
    data-container={node.containerKey}
    class="flex items-center gap-1.5 h-6 compact:h-9 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    style="padding-left: {paddingLeftPx}px; padding-right: 0.5rem"
  >
    {#if node.hiddenStatus}
      <span
        class="w-1.5 h-1.5 rounded-full shrink-0 {node.hiddenStatus.pill.dotClass} {node.hiddenStatus.pill.pulse ? 'animate-pulse' : ''}"
        aria-hidden="true"
        data-testid="sidebar-container-hidden-status"
        data-status={node.hiddenStatus.liveStatus}
      ></span>
    {/if}
    <span>Show {nextReveal} More{node.hiddenCount > THREAD_REVEAL_INCREMENT ? ` (${node.hiddenCount})` : ''}</span>
  </button>
</div>
