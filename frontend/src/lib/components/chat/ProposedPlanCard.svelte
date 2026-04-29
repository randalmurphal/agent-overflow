<script lang="ts">
  import type { ThreadPane } from '../../stores/thread.svelte';
  import { getThreadCurrentProposedPlan } from '../../stores/proposedPlans.svelte';
  import { GetPayloadData } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { Item, ProposedPlanMeta } from '../../types/models';
  import {
    parseProposedPlanItemMeta,
    stripDisplayedPlanMarkdown,
  } from '../../utils/proposedPlan';
  import { createPlanSaveDialog } from '../../utils/planSaveDialog.svelte';
  import ProposedPlanActions from './ProposedPlanActions.svelte';
  import ProposedPlanBody from './ProposedPlanBody.svelte';
  import ProposedPlanSaveModal from './ProposedPlanSaveModal.svelte';

  let {
    pane,
    item,
    payloadId,
    meta,
  }: {
    pane: ThreadPane;
    item?: Item;
    payloadId: string;
    meta: ProposedPlanMeta;
  } = $props();

  let planMarkdown = $state<string | null>(null);
  let planMarkdownRequest: Promise<string> | null = null;

  const title = $derived(meta.title || 'Proposed plan');
  const itemMeta = $derived(parseProposedPlanItemMeta(item));
  const isAccepted = $derived(Boolean(itemMeta.planImplementedAt));
  const currentPlan = $derived(getThreadCurrentProposedPlan(pane.threadId, pane.items));
  const canOpenCurrentPlanSidebar = $derived(Boolean(item?.id) && currentPlan?.id === item?.id);
  const previewOnly = $derived(meta.charCount > 900 || meta.lineCount > 20);
  const displayedMarkdown = $derived.by(() => {
    const source = planMarkdown ?? meta.preview;
    return planMarkdown ? stripDisplayedPlanMarkdown(source) : source;
  });

  const planExport = createPlanSaveDialog(ensurePlanMarkdown, () => pane.threadId);

  // The body's scrollable viewport (max-h-96 + overflow-y-auto) needs the
  // full markdown to scroll. meta.preview caps at 10 visible lines, so
  // load the payload eagerly; preview is the first-paint placeholder.
  $effect(() => {
    void ensurePlanMarkdown();
  });

  async function ensurePlanMarkdown(): Promise<string> {
    const threadId = pane.threadId;
    if (planMarkdown !== null) return planMarkdown;
    if (planMarkdownRequest) return planMarkdownRequest;
    if (!threadId) {
      addToast('error', 'Failed to load proposed plan');
      return '';
    }
    planMarkdownRequest = (async () => {
      try {
        const content = await GetPayloadData(threadId, payloadId);
        planMarkdown = content.data;
        return content.data;
      } catch (err) {
        console.error('Failed to load proposed plan:', err);
        addToast('error', 'Failed to load proposed plan');
        return '';
      } finally {
        planMarkdownRequest = null;
      }
    })();
    return planMarkdownRequest;
  }

  function openInSidebar(): void {
    pane.setShowPlanSidebar(true);
  }
</script>

<div class="group mb-3 border-l-2 border-accent/70 pl-3 sm:pl-4 py-1">
  <div class="flex flex-wrap items-center justify-between gap-3">
    <div class="flex min-w-0 items-center gap-1.5">
      <p class="truncate text-sm font-medium text-text-primary">{title}</p>
      {#if isAccepted}
        <span class="text-[12px] font-medium text-success">· Accepted</span>
      {/if}
    </div>
    <div class="opacity-50 transition-opacity group-hover:opacity-100 focus-within:opacity-100">
      <ProposedPlanActions
        getCopyText={planExport.getCopyableMarkdown}
        onSave={planExport.openSaveDialog}
        onOpenInSidebar={canOpenCurrentPlanSidebar ? openInSidebar : undefined}
      />
    </div>
  </div>

  <ProposedPlanBody
    markdown={displayedMarkdown}
    {previewOnly}
  />
</div>

<ProposedPlanSaveModal
  open={planExport.saveDialogOpen}
  workspacePath={pane.thread?.workspacePath}
  savePath={planExport.savePath}
  saving={planExport.saving}
  onPathChange={planExport.setSavePath}
  onClose={planExport.closeSaveDialog}
  onSave={planExport.handleSave}
/>
