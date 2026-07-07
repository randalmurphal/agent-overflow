<script lang="ts">
  import type { Component } from 'svelte';
  import PlanSidebar from '../chat/PlanSidebar.svelte';
  import DesignPreviewRhsPanel from '../design/DesignPreviewRhsPanel.svelte';
  import ReviewPane from '../review/ReviewPane.svelte';
  import {
    closeCompanion,
    type CompanionKind,
  } from '../../stores/companionPanes.svelte';
  import { makePanelContext } from '../../stores/panelContext.svelte';
  import { getPane } from '../../stores/panes.svelte';

  interface Props {
    paneId: string;
    kind: CompanionKind;
    sourcePaneId: string;
  }

  type CompanionRegistryEntry = {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    component: Component<any>;
  };

  const COMPANION_COMPONENTS = {
    plan: { component: PlanSidebar },
    'design-preview': { component: DesignPreviewRhsPanel },
    review: { component: ReviewPane },
  } satisfies Record<CompanionKind, CompanionRegistryEntry>;

  let { paneId, kind, sourcePaneId }: Props = $props();
  let sourcePane = $derived(getPane(sourcePaneId));
  let panelContext = $derived(
    sourcePane ? makePanelContext(sourcePane, () => closeCompanion(paneId)) : null,
  );
  let panelKey = $derived(
    sourcePane?.thread && panelContext ? `${sourcePane.thread.id}:${kind}` : '',
  );
</script>

{#if sourcePane && panelContext}
  {@const panelEntry = COMPANION_COMPONENTS[kind]}
  {@const PanelComponent = panelEntry.component as unknown as Component<Record<string, unknown>>}
  <aside
    aria-label={kind === 'plan' ? 'Proposed Plan' : kind === 'review' ? 'Review' : 'Design Preview'}
    class="flex h-full min-h-0 flex-col border-l border-border bg-surface-1"
    data-testid={`companion-pane-${kind}`}
    data-companion-pane-id={paneId}
    data-companion-source-pane-id={sourcePaneId}
  >
    {#key panelKey}
      <PanelComponent ctx={panelContext} />
    {/key}
  </aside>
{:else}
  <div
    class="flex h-full min-h-0 items-center justify-center px-4 text-sm text-error"
    data-testid="companion-pane-broken"
    data-companion-pane-id={paneId}
    data-companion-source-pane-id={sourcePaneId}
  >
    Companion pane unavailable.
  </div>
{/if}
