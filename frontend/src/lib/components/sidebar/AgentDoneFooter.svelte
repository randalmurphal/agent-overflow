<script lang="ts">
  // The `N done` footer under a standing agent's children: the count of the
  // threads it spawned that are now archived, opening to list them inline
  // above it, newest first, paged like the project's own show-more footer.
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import type { SidebarAgentDoneNode } from '../../utils/sidebarTreeView';
  import { isAgentDoneExpanded, revealMoreAgentDone, toggleAgentDone } from '../../stores/sidebar.svelte';
  import { forgetArchivedChildren } from '../../stores/agentArchivedChildren.svelte';
  import { THREAD_REVEAL_INCREMENT } from '../../utils/sidebarThreadLimits';
  import { GROUP_MEMBER_PIN_SLOT_LEFT_PX, sidebarGroupMemberPaddingLeftPx } from '../../utils/sidebarRowMetrics';

  interface Props {
    node: SidebarAgentDoneNode;
  }

  let { node }: Props = $props();
  let nextReveal = $derived(Math.min(THREAD_REVEAL_INCREMENT, node.hiddenCount));
  // The label starts where a child row's title does (a child row is one
  // indent step deeper than its depth), and the chevron centres on the
  // child rows' pin column.
  let paddingLeftPx = $derived(sidebarGroupMemberPaddingLeftPx(node.depth + 1));
</script>

<div class="flex items-center gap-1 pr-1" data-testid="thread-row-agent-done" data-expanded={node.expanded}>
  <button
    type="button"
    onclick={(e) => { e.stopPropagation(); if (!isAgentDoneExpanded(node.agent.id)) forgetArchivedChildren(node.agent.id); toggleAgentDone(node.agent.id); }}
    aria-expanded={node.expanded}
    aria-label={node.expanded ? 'Hide finished threads' : 'Show finished threads'}
    data-testid="thread-row-agent-done-toggle"
    class="relative flex items-center gap-1 h-6 compact:h-9 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    style="padding-left: {paddingLeftPx}px; padding-right: 0.5rem"
  >
    <span class="absolute inset-y-0 flex items-center justify-center w-6 pointer-events-none" style="left: {GROUP_MEMBER_PIN_SLOT_LEFT_PX}px">
      <Icon
        icon={ChevronRight}
        size={11}
        strokeWidth={2.5}
        class={'opacity-80 transition-transform ' + (node.expanded ? 'rotate-90' : '')}
      />
    </span>
    <span data-testid="thread-row-agent-done-label">{node.count} done</span>
    {#if node.loading}
      <span class="text-fg-hint/70" data-testid="thread-row-agent-done-loading">…</span>
    {/if}
  </button>
  {#if node.expanded && node.hiddenCount > 0}
    <button
      type="button"
      onclick={(e) => { e.stopPropagation(); revealMoreAgentDone(node.agent.id); }}
      data-testid="thread-row-agent-done-more"
      class="flex items-center h-6 compact:h-9 px-2 rounded-[var(--radius-field)] text-[0.625rem] text-fg-hint hover:bg-surface-2/30 hover:text-fg cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40"
    >
      Show {nextReveal} More{node.hiddenCount > THREAD_REVEAL_INCREMENT ? ` (${node.hiddenCount})` : ''}
    </button>
  {/if}
</div>
