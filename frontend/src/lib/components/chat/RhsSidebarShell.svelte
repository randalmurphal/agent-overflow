<script lang="ts">
  import type { Component } from 'svelte';
  import { fly } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    makePanelContext,
    RHS_PANEL_MIN_WIDTH,
    type RhsPanel,
  } from '../../stores/rhsPanelSlot.svelte';
  import PlanSidebar from './PlanSidebar.svelte';
  import DiffPanelDrawer from './DiffPanelDrawer.svelte';
  import LazyDiffSidebar from './LazyDiffSidebar.svelte';
  import RhsSidebarResizer from './RhsSidebarResizer.svelte';

  // Registry of RHS panel kinds. The `satisfies` clause is the contract
  // that adding a new kind to the RhsPanel union (in rhsPanelSlot.svelte.ts)
  // forces a corresponding entry here at type-check time. New panels SHOULD
  // accept PanelContext (`ctx`), not `pane: ThreadPane` — see PanelContext
  // docs in rhsPanelSlot.svelte.ts. The diff panels still take `pane`
  // because they read pane-scoped diff stores; migrating them is YAGNI
  // until they need to grow. The const is used by the dispatcher below.
  // `Component<any>` is required here because props are contravariant —
  // narrowing to `unknown`/`never` would reject the per-panel concrete
  // prop types. Per-call-site prop shape is checked by the {#if} branches.
  const PANEL_COMPONENTS = {
    plan: PlanSidebar,
    'diff-checkpoint': DiffPanelDrawer,
    'diff-payload': LazyDiffSidebar,
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
  } satisfies Record<RhsPanel['kind'], Component<any>>;

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  let activePanel = $derived(pane.activeRhsPanel);
  let panelContext = $derived(makePanelContext(pane));
  let panelKey = $derived(
    pane.thread && activePanel ? `${pane.thread.id}:${activePanel.kind}` : '',
  );
</script>

{#if activePanel && pane.thread}
  {@const PanelComponent = PANEL_COMPONENTS[activePanel.kind] as unknown as Component<Record<string, unknown>>}
  <aside
    transition:fly={{ x: 320, duration: 150 }}
    aria-label="Right Sidebar"
    data-testid="rhs-sidebar-shell"
    style="width: {pane.rhsSidebarWidth}px"
    class="relative flex h-full shrink-0 flex-col border-l border-border bg-surface-1"
  >
    {#key panelKey}
      {#if activePanel.kind === 'plan'}
        <PanelComponent ctx={panelContext} />
      {:else}
        <PanelComponent {pane} />
      {/if}
    {/key}

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
