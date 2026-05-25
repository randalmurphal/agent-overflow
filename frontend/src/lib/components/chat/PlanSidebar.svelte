<script lang="ts">
  import { onDestroy, onMount, untrack } from 'svelte';
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
    GetPayloadData,
    SendPlanRevisionComments,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { PanelContext } from '../../stores/rhsPanelSlot.svelte';
  import type { Thread } from '../../types/models';
  import {
    normalizePlanMarkdownForExport,
    parseProposedPlanItemMeta,
    parseProposedPlanPayloadMeta,
  } from '../../utils/proposedPlan';
  import { createPlanSaveDialog } from '../../utils/planSaveDialog.svelte';
  import { getPathRefsFromMeta } from '../../utils/pathLinkify';
  import { isUiRenderTraceEnabled, recordUiTrace, scheduleDomUiTrace } from '../../utils/uiRenderTrace';
  import Button from '../primitives/Button.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ProposedPlanActions from './ProposedPlanActions.svelte';
  import ProposedPlanReviewSurface from './ProposedPlanReviewSurface.svelte';
  import ProposedPlanSaveModal from './ProposedPlanSaveModal.svelte';

  interface Props {
    ctx: PanelContext;
  }

  let { ctx }: Props = $props();

  let sidebarRoot: HTMLElement | undefined = $state(undefined);
  let threadId = $derived(ctx.threadId);
  // Plan derivation reads ONLY from the per-thread plan cache, not pane.items.
  // The cache is kept current synchronously by retainProposedPlanEventListener
  // (see proposedPlans.svelte.ts), so chat streaming chunks no longer reach
  // this surface — the body cannot blank mid-fetch from an unrelated upsert.
  let currentPlan = $derived(getThreadCurrentProposedPlan(threadId));
  let currentPlanMeta = $derived(parseProposedPlanPayloadMeta(currentPlan));
  let currentPlanItemMeta = $derived(parseProposedPlanItemMeta(currentPlan));
  let isAccepted = $derived(Boolean(currentPlanItemMeta.planImplementedAt));
  let title = $derived(currentPlanMeta.title || 'Proposed plan');
  // Depend the reset $effect on these primitives, not on `currentPlan` itself,
  // so a same-id replacement (e.g. updated meta after sendDrafts) does not
  // blank the body.
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
  // pathRefs lands on the proposed_plan item.meta at handleProposedPlan
  // time, so the review surface and the chat card show the same
  // allowlist for the same plan markdown.
  const pathRefs = $derived(getPathRefsFromMeta(currentPlan?.meta) ?? []);

  const planExport = createPlanSaveDialog(ensurePlanMarkdown, () => ctx.threadId);

  $effect(() => {
    const id = threadId;
    untrack(() => { void refreshThreadProposedPlans(id); });
  });

  // Reset and reload markdown + comments whenever the current plan id or payload changes.
  // CRITICAL: read the local `threadId` $derived, NOT `ctx.threadId` directly.
  // ctx.threadId is a getter that reads pane.thread reactively — pane.thread
  // is reassigned to a new (same-value) Thread reference many times per chat
  // message via patchThreadDurableStatus / updateThreadUsageCache / syncThread.
  // Reading the getter inside an effect tracks pane.thread itself, so the
  // effect fires per chat message and blanks planMarkdown. The local $derived
  // has value-equality short-circuiting; reading it tracks only the actual
  // string-id value, which is genuinely stable across thread mutations.
  $effect(() => {
    const planId = planKey;
    const payloadId = planPayloadIdKey;
    const tid = threadId;
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

    if (!isUiRenderTraceEnabled()) return;
    recordUiTrace('plan-sidebar.state', {
      threadId,
      currentPlanId: currentPlan?.id ?? null,
      currentPlanTitle: currentPlanMeta.title,
    });
    scheduleDomUiTrace('plan-sidebar', 'plan-sidebar.dom', () => ({
      threadId,
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
    const tid = ctx.threadId;
    if (!planId || !payloadId || !tid) {
      addToast('error', 'Failed to load proposed plan');
      return '';
    }
    return loadPlanMarkdown(tid, payloadId, planId);
  }

  function retryLoadPlanMarkdown(): void {
    const planId = currentPlan?.id;
    const payloadId = currentPlan?.payloadId;
    const tid = ctx.threadId;
    if (!planId || !payloadId || !tid) return;
    planLoadError = null;
    void loadPlanMarkdown(tid, payloadId, planId);
  }

  async function sendDrafts(): Promise<void> {
    if (sendingDrafts) return;
    const tid = ctx.threadId;
    const planId = currentPlan?.id;
    if (!tid || !planId) return;
    const draftIds = comments.filter((c) => c.status === 'draft').map((c) => c.id);
    if (draftIds.length === 0) return;

    sendingDrafts = true;
    try {
      const updated = (await SendPlanRevisionComments(tid, planId, draftIds)) as Thread;
      ctx.replaceThread(updated);
      await refreshPlanComments(tid, planId);
    } catch (err) {
      console.error('Failed to send plan comments:', err);
      addToast('error', 'Failed to send comments');
    } finally {
      sendingDrafts = false;
    }
  }
</script>

<section
  bind:this={sidebarRoot}
  aria-label="Proposed Plan"
  data-testid="plan-sidebar"
  class="flex min-h-0 flex-1 flex-col"
>
    <div class="flex items-center justify-between gap-2 px-8 pt-3 pb-2">
      <div class="flex min-w-0 items-center gap-1.5">
        <h3 class="truncate text-sm font-medium text-text-primary">{title}</h3>
        {#if isAccepted}
          <span class="shrink-0 text-[0.75rem] font-medium text-success">· Accepted</span>
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
          onclick={() => ctx.close()}
          data-testid="plan-sidebar-close"
          aria-label="Close Plan Sidebar"
          class="rounded p-1 text-text-secondary hover:text-text-primary cursor-pointer focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
        >
          <Icon icon={X} size={14} />
        </button>
      </div>
    </div>

    <div class="flex-1 overflow-y-auto px-8 pb-2">
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
              threadId={ctx.threadId ?? ''}
              planItemId={currentPlan.id}
              markdown={normalizedMarkdown}
              {comments}
              workspacePath={ctx.workspacePath}
              {pathRefs}
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
      <div class="flex items-center justify-end px-8 pt-2 pb-3">
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

  </section>

<ProposedPlanSaveModal
  open={planExport.saveDialogOpen}
  workspacePath={ctx.workspacePath}
  savePath={planExport.savePath}
  saving={planExport.saving}
  onPathChange={planExport.setSavePath}
  onClose={planExport.closeSaveDialog}
  onSave={planExport.handleSave}
/>
