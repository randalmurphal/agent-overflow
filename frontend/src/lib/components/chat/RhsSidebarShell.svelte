<script lang="ts">
  import { fly } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { RHS_PANEL_MIN_WIDTH } from '../../stores/rhsPanelSlot.svelte';
  import PlanSidebar from './PlanSidebar.svelte';
  import DiffPanelDrawer from './DiffPanelDrawer.svelte';
  import LazyDiffSidebar from './LazyDiffSidebar.svelte';
  import RhsSidebarResizer from './RhsSidebarResizer.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  let activePanel = $derived(pane.activeRhsPanel);
</script>

{#if activePanel && pane.thread}
  <aside
    transition:fly={{ x: 320, duration: 150 }}
    aria-label="Right Sidebar"
    data-testid="rhs-sidebar-shell"
    style="width: {pane.rhsSidebarWidth}px"
    class="relative flex h-full shrink-0 flex-col border-l border-border bg-surface-1"
  >
    {#if activePanel.kind === 'plan'}
      <PlanSidebar {pane} />
    {:else if activePanel.kind === 'diff-checkpoint'}
      {#key pane.thread.id}
        <DiffPanelDrawer {pane} />
      {/key}
    {:else if activePanel.kind === 'diff-payload'}
      {#key pane.thread.id}
        <LazyDiffSidebar {pane} />
      {/key}
    {/if}

    <RhsSidebarResizer
      width={pane.rhsSidebarWidth}
      minWidth={RHS_PANEL_MIN_WIDTH}
      getMaxWidth={() => pane.getRhsSidebarMaxWidth()}
      onResizeLive={(next) => pane.setRhsSidebarWidthLive(next)}
      onResizeEnd={() => pane.persistRhsSidebarWidth()}
      ariaLabel="Resize Right Sidebar"
      testId="rhs-sidebar-resizer"
      {pane}
    />
  </aside>
{/if}
