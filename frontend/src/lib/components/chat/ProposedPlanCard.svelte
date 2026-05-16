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
  import ToolKindIcon from './ToolKindIcon.svelte';
  import TranscriptDisclosureHeader from './TranscriptDisclosureHeader.svelte';

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

  const title = $derived(meta.title || 'Proposed plan');
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

<div class="group/proposed-plan px-1 py-1" data-testid="proposed-plan-card">
  <TranscriptDisclosureHeader
    expanded={false}
    expandable={false}
    testId="proposed-plan-header"
    class="rounded-[var(--radius-control)] py-1"
    buttonClass="text-[12px] text-fg-muted"
  >
    {#snippet icon()}<ToolKindIcon kind="checklist" ariaLabel="Proposed plan" />{/snippet}
    {#snippet label()}<span data-testid="proposed-plan-label">plan</span>{/snippet}
    {#snippet body()}
      <span class="min-w-0 flex-1 truncate text-[12px] text-fg-muted/75">
        {title}{#if isAccepted}<span class="ml-1 text-fg-hint">(accepted)</span>{/if}
      </span>
    {/snippet}
    {#snippet actions()}
      <div class="opacity-50 transition-opacity group-hover/proposed-plan:opacity-100 focus-within:opacity-100">
      <ProposedPlanActions
        getCopyText={planExport.getCopyableMarkdown}
        onSave={planExport.openSaveDialog}
        onOpenInSidebar={canOpenCurrentPlanSidebar ? openInSidebar : undefined}
      />
      </div>
    {/snippet}
  </TranscriptDisclosureHeader>

  <div class="ml-[5.25rem] px-3">
    <ProposedPlanBody
      markdown={displayedMarkdown}
      capped={cappedBody}
      loading={expansion.loading}
      error={expansion.error}
      workspacePath={paneWorkspacePath(pane)}
    />
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
