<script lang="ts">
  import { paneWorkspacePath, type ThreadPane } from '../../stores/thread.svelte';
  import { getThreadCurrentProposedPlan } from '../../stores/proposedPlans.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Item, ProposedPlanMeta } from '../../types/models';
  import {
    proposedPlanVersionForItem,
    shouldCapProposedPlanBody,
  } from '../../utils/proposedPlan';
  import { createPlanSaveDialog } from '../../utils/planSaveDialog.svelte';
  import { EMPTY_PATH_REFS, getPathRefsFromMeta } from '../../utils/pathLinkify';
  import ProposedPlanActions from './ProposedPlanActions.svelte';
  import ProposedPlanBody from './ProposedPlanBody.svelte';
  import ProposedPlanSaveModal from './ProposedPlanSaveModal.svelte';
  import { useLeasedItemExpansion } from './useLeasedPayloadExpansion.svelte';

  let {
    pane,
    item,
    meta,
  }: {
    pane: ThreadPane;
    item: Item;
    meta: ProposedPlanMeta;
  } = $props();

  // Plan rows live under the virtualizer, so row-local async state is the wrong
  // lifetime: recycling the DOM used to reset this expansion and bounce
  // the row between preview height and full height. Keep the handle in
  // the pane row registry instead; the DOM can remount without throwing
  // away loaded chunks or auto-load progress.
  const expansionRef = useLeasedItemExpansion({
    getPane: () => pane,
    getItem: () => item,
    getFallback: () => null,
    getOptions: () => ({
      loadMode: 'full',
      loadOnMount: true,
      stateKey: 'proposed-plan-history',
      // Module-scope helper only: the pane registry retains this callback
      // for the entry's lifetime (see RowExpansionStateOptions). It derives
      // the version from the current item rather than this card's
      // `item`/`meta` props so the entry cannot pin the card instance.
      payloadVersion: proposedPlanVersionForItem,
    }),
  });
  const expansion = $derived(expansionRef.current!);

  const currentPlan = $derived(getThreadCurrentProposedPlan(pane.threadId));
  const canOpenCurrentPlanSidebar = $derived(Boolean(item?.id) && currentPlan?.id === item?.id);
  const cappedBody = $derived(shouldCapProposedPlanBody(meta));
  const planMarkdown = $derived(expansion.displayData);
  const displayedMarkdown = $derived(planMarkdown ?? meta.preview);
  // pathRefs is stamped onto the proposed_plan item meta at write time
  // (internal/triage/payload_items.go#handleProposedPlan). The body is
  // the same plan markdown the validator saw, so the allowlist applies
  // to both the inline card body and the review surface.
  const pathRefs = $derived(getPathRefsFromMeta(item.meta) ?? EMPTY_PATH_REFS);

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
    <div class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/95 px-1 py-0.5 shadow-sheet backdrop-blur">
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
      {pathRefs}
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
