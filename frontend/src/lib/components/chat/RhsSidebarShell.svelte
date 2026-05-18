<script lang="ts">
  import type { Component } from 'svelte';
  import { fly } from 'svelte/transition';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import {
    makePanelContext,
    RHS_PANEL_MIN_WIDTH,
    type RhsPanel,
  } from '../../stores/rhsPanelSlot.svelte';
  import { PANE_DENSITY_MIN_WIDTHS } from '../../stores/paneDensity.svelte';
  import { getPaneWidth } from '../../stores/layoutMetrics.svelte';
  import PlanSidebar from './PlanSidebar.svelte';
  import DiffPanelDrawer from './DiffPanelDrawer.svelte';
  import LazyDiffSidebar from './LazyDiffSidebar.svelte';
  import RhsSidebarResizer from './RhsSidebarResizer.svelte';
  import DesignPreviewRhsPanel from '../design/DesignPreviewRhsPanel.svelte';

  // Registry of RHS panel kinds. The `satisfies` clause is the contract
  // that adding a new kind to the RhsPanel union (in rhsPanelSlot.svelte.ts)
  // forces a corresponding entry here at type-check time. New panels SHOULD
  // accept PanelContext (`ctx`), not `pane: ThreadPane` — see PanelContext
  // docs in rhsPanelSlot.svelte.ts. The diff panels still take `pane`
  // because they read pane-scoped diff stores; migrating them is YAGNI
  // until they need to grow. The const is used by the dispatcher below.
  // `Component<any>` is required here because props are contravariant —
  // narrowing to `unknown`/`never` would reject the per-panel concrete
  // prop types. The registry also owns the prop shape so adding a panel
  // cannot silently update the component mapping without updating the
  // dispatcher below.
  type PanelRegistryEntry = {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    component: Component<any>;
    props: 'ctx' | 'pane';
  };

  const PANEL_COMPONENTS = {
    plan: { component: PlanSidebar, props: 'ctx' },
    'design-preview': { component: DesignPreviewRhsPanel, props: 'ctx' },
    'diff-checkpoint': { component: DiffPanelDrawer, props: 'pane' },
    'diff-payload': { component: LazyDiffSidebar, props: 'pane' },
  } satisfies Record<RhsPanel['kind'], PanelRegistryEntry>;

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();
  let activePanel = $derived(pane.activeRhsPanel);
  const SIDE_PANEL_THRESHOLD_PX = PANE_DENSITY_MIN_WIDTHS.comfortable;
  let paneWidth = $derived(getPaneWidth(pane.paneId));
  let overlayMode = $derived(paneWidth < SIDE_PANEL_THRESHOLD_PX);
  let panelContext = $derived(makePanelContext(pane));
  let panelKey = $derived(
    pane.thread && activePanel ? `${pane.thread.id}:${activePanel.kind}` : '',
  );

</script>

{#if activePanel && pane.thread}
  {@const panelEntry = PANEL_COMPONENTS[activePanel.kind]}
  {@const PanelComponent = panelEntry.component as unknown as Component<Record<string, unknown>>}
  <aside
    transition:fly={{ x: 320, duration: 150 }}
    aria-label="Right Sidebar"
    data-testid="rhs-sidebar-shell"
    data-rhs-mode={overlayMode ? 'overlay' : 'side-panel'}
    style={overlayMode ? undefined : `width: ${pane.rhsSidebarWidth}px`}
    class={[
      'flex h-full flex-col border-l border-border bg-surface-1',
      overlayMode
        ? 'absolute inset-0 z-30 w-full border-l-0 shadow-sheet'
        : 'relative shrink-0',
    ].join(' ')}
  >
    {#key panelKey}
      {#if panelEntry.props === 'ctx'}
        <PanelComponent ctx={panelContext} />
      {:else}
        <PanelComponent {pane} />
      {/if}
    {/key}

    {#if !overlayMode}
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
    {/if}
  </aside>
{/if}
