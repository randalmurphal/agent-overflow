<script lang="ts">
  import ListTodo from '@lucide/svelte/icons/list-todo';
  import type { ThreadPane } from '../../../stores/thread.svelte';
  import Icon from '../../primitives/Icon.svelte';

  interface Props {
    pane: ThreadPane;
    hasCurrentPlan?: boolean;
  }

  let { pane, hasCurrentPlan = false }: Props = $props();
</script>

{#if hasCurrentPlan}
  <button
    type="button"
    onclick={() => pane.togglePlanSidebar()}
    data-testid="composer-plan-sidebar-toggle"
    aria-label="Toggle Plan Sidebar"
    aria-pressed={pane.showPlanSidebar ? 'true' : 'false'}
    title="Toggle Plan Sidebar"
    class={[
      'relative inline-flex items-center gap-1.5 rounded-[var(--radius-field)]',
      'px-1.5 py-1 text-[0.6875rem] transition-colors cursor-pointer',
      pane.showPlanSidebar
        ? 'bg-surface-2/60 text-fg ring-1 ring-inset ring-border-subtle'
        : 'text-fg-muted hover:text-fg hover:bg-surface-2/30',
      'focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40',
    ].join(' ')}
  >
    <Icon icon={ListTodo} size={13} strokeWidth={1.75} class="opacity-80" />
    <span data-composer-toolbar-label="collapsible">Plan</span>
  </button>
{/if}
