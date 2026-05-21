<script lang="ts">
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { getThreadCurrentProposedPlan } from '../../stores/proposedPlans.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Item, ProposedPlanMeta } from '../../types/models';
  import {
    parseProposedPlanItemMeta,
    parseProposedPlanPayloadMeta,
    proposedPlanPayloadVersion,
    shouldCapProposedPlanBody,
    stripDisplayedPlanMarkdown,
  } from '../../utils/proposedPlan';
  import { createPlanSaveDialog } from '../../utils/planSaveDialog.svelte';
  import ProposedPlanActions from './ProposedPlanActions.svelte';
  import ProposedPlanBody from './ProposedPlanBody.svelte';
  import ProposedPlanSaveModal from './ProposedPlanSaveModal.svelte';

  let {
    pane,
    item,
    meta,
  }: {
    pane: ThreadPane;
    item: Item;
    meta: ProposedPlanMeta;
  } = $props();

  // Plan rows live under virtua, so row-local async state is the wrong
  // lifetime: recycling the DOM used to reset this expansion and bounce
  // the row between preview height and full height. Keep the handle in
  // the pane row registry instead; the DOM can remount without throwing
  // away loaded chunks or auto-load progress.
  const expansion = $derived(pane.expansionStateFor(item, {
    loadMode: 'full',
    loadOnMount: true,
    stateKey: 'proposed-plan-history',
    payloadVersion: (currentItem) => {
      const currentMeta = currentItem?.payloadMeta
        ? parseProposedPlanPayloadMeta(currentItem)
        : meta;
      return proposedPlanPayloadVersion(currentItem ?? item, currentMeta);
    },
  }));

  const itemMeta = $derived(parseProposedPlanItemMeta(item));
  const isAccepted = $derived(Boolean(itemMeta.planImplementedAt));
  const currentPlan = $derived(getThreadCurrentProposedPlan(pane.threadId));
  const canOpenCurrentPlanSidebar = $derived(Boolean(item?.id) && currentPlan?.id === item?.id);
  const cappedBody = $derived(shouldCapProposedPlanBody(meta));
  const planMarkdown = $derived(expansion.displayData);
  const displayedMarkdown = $derived.by(() => {
    const source = planMarkdown ?? meta.preview;
    return planMarkdown ? stripDisplayedPlanMarkdown(source) : source;
  });

  const planExport = createPlanSaveDialog(ensurePlanMarkdown, () => pane.threadId);

  async function ensurePlanMarkdown(): Promise<string> {
    if (expansion.displayData !== null && expansion.isComplete) {
      return expansion.displayData;
    }
    try {
      await expansion.expand();
      if (expansion.hasMore) await expansion.showFull();
    } catch (err) {
      console.error('Failed to load proposed plan:', err);
      addToast('error', 'Failed to load proposed plan');
      return '';
    }
    if (expansion.error) {
      addToast('error', 'Failed to load proposed plan');
      return '';
    }
    return expansion.displayData ?? '';
  }

  function openInSidebar(): void {
    pane.setShowPlanSidebar(true);
  }
</script>

<div class="group/proposed-plan relative px-1 py-1" data-testid="proposed-plan-card">
  <div
    class="pointer-events-none absolute right-1 top-1 z-10 opacity-0 transition-opacity duration-150 group-hover/proposed-plan:pointer-events-auto group-hover/proposed-plan:opacity-100 focus-within:pointer-events-auto focus-within:opacity-100"
    data-testid="proposed-plan-actions"
  >
    <div class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/95 px-1 py-0.5 shadow-sm backdrop-blur">
      <ProposedPlanActions
        getCopyText={planExport.getCopyableMarkdown}
        onSave={planExport.openSaveDialog}
        onOpenInSidebar={canOpenCurrentPlanSidebar ? openInSidebar : undefined}
      />
    </div>
  </div>

  <div data-testid="proposed-plan-body-shell">
    <ProposedPlanBody
      markdown={displayedMarkdown}
      capped={cappedBody}
      loading={expansion.loading}
      error={expansion.error}
      workspacePath={paneWorkspacePath(pane)}
    />
    {#if isAccepted}
      <p class="mt-2 text-[11px] text-fg-hint" data-testid="proposed-plan-accepted">
        Accepted
      </p>
    {/if}
  </div>
</div>

<ProposedPlanSaveModal
  open={planExport.saveDialogOpen}
  workspacePath={paneWorkspacePath(pane)}
  savePath={planExport.savePath}
  saving={planExport.saving}
  onPathChange={planExport.setSavePath}
  onClose={planExport.closeSaveDialog}
  onSave={planExport.handleSave}
/>
