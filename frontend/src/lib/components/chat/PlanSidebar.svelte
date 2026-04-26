<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
  import { fly } from 'svelte/transition';
  import Send from 'lucide-svelte/icons/send';
  import X from 'lucide-svelte/icons/x';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import {
    getPlanComments,
    getThreadCurrentProposedPlan,
    refreshPlanComments,
    refreshThreadProposedPlans,
    retainProposedPlanEventListener,
  } from '../../stores/proposedPlans.svelte';
  import {
    getPlanSidebarWidth,
    persistPlanSidebarWidth,
    setPlanSidebarWidthLive,
  } from '../../stores/planSidebarLayout.svelte';
  import {
    GetPayloadData,
    SendPlanRevisionComments,
  } from '../../stores/bindings';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ThreadPane } from '../../stores/thread.svelte';
  import type { Thread } from '../../types/models';
  import {
    normalizePlanMarkdownForExport,
    parseProposedPlanItemMeta,
    parseProposedPlanPayloadMeta,
  } from '../../utils/proposedPlan';
  import { createPlanSaveDialog } from '../../utils/planSaveDialog.svelte';
  import { isUiRenderTraceEnabled, recordUiTrace, scheduleDomUiTrace } from '../../utils/uiRenderTrace';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import PlanSidebarResizer from './PlanSidebarResizer.svelte';
  import ProposedPlanActions from './ProposedPlanActions.svelte';
  import ProposedPlanReviewSurface from './ProposedPlanReviewSurface.svelte';
  import ProposedPlanSaveModal from './ProposedPlanSaveModal.svelte';

  interface Props {
    pane: ThreadPane;
  }

  let { pane }: Props = $props();

  let sidebarRoot: HTMLElement | undefined = $state(undefined);
  let visible = $derived(pane.showPlanSidebar);
  let threadId = $derived(pane.thread?.id ?? null);
  let currentPlan = $derived(getThreadCurrentProposedPlan(threadId, pane.items));
  let currentPlanMeta = $derived(parseProposedPlanPayloadMeta(currentPlan));
  let currentPlanItemMeta = $derived(parseProposedPlanItemMeta(currentPlan));
  let isAccepted = $derived(Boolean(currentPlanItemMeta.planImplementedAt));
  let title = $derived(currentPlanMeta.title || 'Proposed plan');
  // Depend the reset $effect on these primitives, not on `currentPlan` itself.
  // Otherwise an unrelated `pane.items` upsert (every streaming tick) re-fires
  // the effect and blanks the body mid-fetch.
  let planKey = $derived(currentPlan?.id ?? null);
  let planPayloadIdKey = $derived(currentPlan?.payloadId ?? null);

  let planMarkdown = $state<string | null>(null);
  let planLoadError = $state<string | null>(null);
  let sendingDrafts = $state(false);
  let planMarkdownRequest: Promise<string> | null = null;
  let planMarkdownRequestKey: string | null = null;

  // Comments live in the per-(threadId, planItemId) store cache so PlanSidebar
  // and Composer share one source of truth — no more two-place desync after
  // sendDrafts.
  const comments = $derived(getPlanComments(threadId, planKey));
  const draftCommentCount = $derived(comments.filter((c) => c.status === 'draft').length);
  // Compute once per markdown change so ReviewSurface's `sourceBlocks` derivation
  // doesn't re-split on every comment add/edit.
  const normalizedMarkdown = $derived(planMarkdown !== null ? normalizePlanMarkdownForExport(planMarkdown) : '');

  const planExport = createPlanSaveDialog(ensurePlanMarkdown, () => pane.threadId);

  $effect(() => {
    const id = threadId;
    untrack(() => { void refreshThreadProposedPlans(id); });
  });

  // Reset and reload markdown + comments whenever the current plan id or payload changes.
  $effect(() => {
    const planId = planKey;
    const payloadId = planPayloadIdKey;
    const tid = pane.threadId;
    untrack(() => {
      planMarkdown = null;
      planLoadError = null;
      planMarkdownRequest = null;
      planMarkdownRequestKey = null;
      if (planId && payloadId && tid) {
        void loadPlanMarkdown(tid, payloadId, planId);
        void refreshPlanComments(tid, planId);
      }
    });
  });

  $effect(() => {
    threadId;
    currentPlan?.id;
    visible;

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('plan-sidebar.state', {
      threadId,
      visible,
      currentPlanId: currentPlan?.id ?? null,
      currentPlanTitle: currentPlanMeta.title,
    });
    scheduleDomUiTrace('plan-sidebar', 'plan-sidebar.dom', () => ({
      threadId,
      visible,
      currentPlanId: currentPlan?.id ?? null,
      textPreview: (sidebarRoot?.textContent ?? '').replace(/\s+/g, ' ').trim().slice(0, 200),
    }));
  });

  let releasePlanEvents: (() => void) | null = null;
  onMount(() => {
    releasePlanEvents = retainProposedPlanEventListener(() => threadId);
  });

  onDestroy(() => {
    releasePlanEvents?.();
  });

  async function loadPlanMarkdown(tid: string, payloadId: string, planId: string): Promise<string> {
    if (planMarkdownRequestKey === planId && planMarkdownRequest) {
      return planMarkdownRequest;
    }
    planMarkdownRequestKey = planId;
    planMarkdownRequest = (async () => {
      try {
        const content = await GetPayloadData(tid, payloadId);
        if (planMarkdownRequestKey === planId) {
          planMarkdown = content.data;
          planLoadError = null;
        }
        return content.data;
      } catch (err) {
        console.error('Failed to load proposed plan:', err);
        if (planMarkdownRequestKey === planId) {
          planLoadError = err instanceof Error ? err.message : 'Failed to load plan';
        }
        return '';
      } finally {
        if (planMarkdownRequestKey === planId) {
          planMarkdownRequest = null;
        }
      }
    })();
    return planMarkdownRequest;
  }

  async function ensurePlanMarkdown(): Promise<string> {
    if (planMarkdown !== null) return planMarkdown;
    const planId = currentPlan?.id;
    const payloadId = currentPlan?.payloadId;
    const tid = pane.threadId;
    if (!planId || !payloadId || !tid) {
      addToast('error', 'Failed to load proposed plan');
      return '';
    }
    return loadPlanMarkdown(tid, payloadId, planId);
  }

  function retryLoadPlanMarkdown(): void {
    const planId = currentPlan?.id;
    const payloadId = currentPlan?.payloadId;
    const tid = pane.threadId;
    if (!planId || !payloadId || !tid) return;
    planLoadError = null;
    void loadPlanMarkdown(tid, payloadId, planId);
  }

  async function sendDrafts(): Promise<void> {
    if (sendingDrafts) return;
    const tid = pane.threadId;
    const planId = currentPlan?.id;
    if (!tid || !planId) return;
    const draftIds = comments.filter((c) => c.status === 'draft').map((c) => c.id);
    if (draftIds.length === 0) return;

    sendingDrafts = true;
    try {
      const updated = (await SendPlanRevisionComments(tid, planId, draftIds)) as Thread;
      pane.replaceThread(updated);
      replaceThread(updated);
      await refreshPlanComments(tid, planId);
    } catch (err) {
      console.error('Failed to send plan comments:', err);
      addToast('error', 'Failed to send comments');
    } finally {
      sendingDrafts = false;
    }
  }
</script>

{#if visible}
  <aside
    bind:this={sidebarRoot}
    transition:fly={{ x: 280, duration: 150 }}
    aria-label="Proposed Plan"
    data-testid="plan-sidebar"
    style="width: {getPlanSidebarWidth()}px"
    class="relative flex shrink-0 flex-col border-l border-border bg-surface-1"
  >
    <div class="flex items-center justify-between gap-2 px-3 pt-3 pb-2">
      <div class="flex min-w-0 items-center gap-1.5">
        <h3 class="truncate text-sm font-medium text-text-primary">{title}</h3>
        {#if isAccepted}
          <span class="shrink-0 text-[12px] font-medium text-success">· Accepted</span>
        {/if}
      </div>
      <div class="flex items-center gap-1">
        {#if currentPlan}
          <ProposedPlanActions
            getCopyText={planExport.getCopyableMarkdown}
            onSave={planExport.openSaveDialog}
          />
        {/if}
        <button
          type="button"
          onclick={() => pane.setShowPlanSidebar(false)}
          data-testid="plan-sidebar-close"
          aria-label="Close Plan Sidebar"
          class="rounded p-1 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          <Icon icon={X} size={14} />
        </button>
      </div>
    </div>

    <div class="flex-1 overflow-y-auto px-3 pb-2">
      {#if currentPlan}
        {#if planLoadError}
          <div class="flex flex-col items-start gap-2 py-3 text-xs text-text-secondary" data-testid="plan-sidebar-error">
            <p>Couldn't load this plan: <span class="text-fg-muted">{planLoadError}</span></p>
            <button
              type="button"
              onclick={retryLoadPlanMarkdown}
              class="inline-flex items-center gap-1 rounded text-accent hover:underline cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
              data-testid="plan-sidebar-retry"
            >
              <Icon icon={RefreshCw} size={12} />
              Retry
            </button>
          </div>
        {:else if planMarkdown !== null}
          {#key currentPlan.id}
            <ProposedPlanReviewSurface
              threadId={pane.threadId ?? ''}
              planItemId={currentPlan.id}
              markdown={normalizedMarkdown}
              {comments}
              onRefresh={() => refreshPlanComments(threadId, planKey)}
            />
          {/key}
        {/if}
      {:else}
        <p class="text-xs text-text-secondary" data-testid="plan-sidebar-empty">
          No plan yet.
        </p>
      {/if}
    </div>

    {#if currentPlan && draftCommentCount > 0}
      <div class="flex items-center justify-end px-3 pt-2 pb-3">
        <Button
          variant="tinted"
          size="sm"
          loading={sendingDrafts}
          onclick={() => void sendDrafts()}
          testId="plan-comments-send"
        >
          {#snippet children()}
            <span class="inline-flex items-center gap-1.5">
              <Icon icon={Send} size={12} />
              Send {draftCommentCount} {draftCommentCount === 1 ? 'comment' : 'comments'}
            </span>
          {/snippet}
        </Button>
      </div>
    {/if}

    <PlanSidebarResizer
      width={getPlanSidebarWidth()}
      onResizeLive={setPlanSidebarWidthLive}
      onResizeEnd={persistPlanSidebarWidth}
    />
  </aside>
{/if}

<ProposedPlanSaveModal
  open={planExport.saveDialogOpen}
  workspacePath={pane.thread?.workspacePath}
  savePath={planExport.savePath}
  saving={planExport.saving}
  onPathChange={planExport.setSavePath}
  onClose={planExport.closeSaveDialog}
  onSave={planExport.handleSave}
/>
