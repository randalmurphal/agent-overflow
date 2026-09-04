<script lang="ts">
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import ReviewCIChips from './ReviewCIChips.svelte';
  import ReviewCollapsibleSection from './ReviewCollapsibleSection.svelte';
  import ReviewConversation from './ReviewConversation.svelte';
  import Icon from '../primitives/Icon.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { CIJob, CIPipeline, PRDetail } from '../../types/models';

  interface Props {
    detail: PRDetail;
    hasWorkspace?: boolean;
    onViewConflicts?: () => void;
    ciPipeline?: CIPipeline | null;
    ciLoading?: boolean;
    ciError?: string | null;
    onOpenCIJob?: (stageName: string, job: CIJob) => void;
    /** Re-polls CI status alone — no diff or thread refresh. */
    onRefreshCI?: () => void;
    /** Present in the review pane proper; absent in narrow test mounts —
     *  the Conversation section renders only with a store to read. */
    review?: ReviewPaneState | null;
    canSendToAgent?: boolean;
  }

  let {
    detail,
    hasWorkspace = false,
    onViewConflicts,
    ciPipeline = null,
    ciLoading = false,
    ciError = null,
    onOpenCIJob,
    onRefreshCI,
    review = null,
    canSendToAgent = false,
  }: Props = $props();

  let descriptionOpen = $state(false);

  const openThreadCount = $derived(
    (review?.prThreads ?? []).reduce(
      (sum, thread) =>
        sum + (thread.isResolvable && !thread.isResolved && !thread.isOutdated ? 1 : 0),
      0,
    ),
  );
  const conversationTotal = $derived(
    (review?.prThreads.length ?? 0) + detail.latestReviews.length,
  );

  async function openURL(url: string | undefined): Promise<void> {
    if (!url) return;
    await OpenExternalURL(url);
  }

  // Forge review states arrive as wire enums (APPROVED,
  // CHANGES_REQUESTED, REVIEW_REQUIRED, COMMENTED, ...): render them as
  // words with a semantic tint instead of raw constants.
  function reviewStateLabel(state: string): string {
    return state.replaceAll('_', ' ').toLowerCase();
  }

  function reviewStatePillClass(state: string): string {
    switch (state.toUpperCase()) {
      case 'APPROVED':
        return 'bg-success/12 text-success';
      case 'CHANGES_REQUESTED':
        return 'bg-error/12 text-error';
      default:
        return 'bg-surface-2 text-fg-muted';
    }
  }
</script>

<section class="shrink-0 border-b border-border bg-surface-1 px-4 py-3.5" data-testid="review-pr-header">
  <div class="min-w-0">
    <button
      type="button"
      class="truncate text-left text-sm font-semibold text-fg hover:text-accent"
      onclick={() => { void openURL(detail.url); }}
    >
      {detail.title} <span class="text-fg-muted">#{detail.number}</span>
    </button>
    <div class="mt-2 flex flex-wrap items-center gap-2 text-[0.6875rem] text-fg-muted">
      <span>{detail.authorLogin}</span>
      {#if detail.mergeability === 'conflicts'}
        {#if hasWorkspace}
          <button
            type="button"
            class="rounded border border-error/40 bg-error/10 px-1.5 py-0.5 text-error hover:bg-error/15"
            onclick={onViewConflicts}
          >
            View conflicts
          </button>
        {:else}
          <span class="rounded border border-error/40 bg-error/10 px-1.5 py-0.5 text-error">Conflicts</span>
        {/if}
      {:else if detail.mergeability === 'checking'}
        <span class="text-fg-subtle">Checking mergeability...</span>
      {/if}
      {#if onOpenCIJob}
        <ReviewCIChips
          pipeline={ciPipeline}
          loading={ciLoading}
          error={ciError}
          onOpenJob={onOpenCIJob}
        />
        {#if onRefreshCI}
          <button
            type="button"
            class="inline-flex size-5 items-center justify-center rounded text-fg-subtle hover:bg-surface-2 hover:text-fg disabled:opacity-50"
            aria-label="Refresh CI status"
            title="Refresh CI status"
            data-testid="review-ci-refresh"
            disabled={ciLoading}
            onclick={onRefreshCI}
          >
            <Icon icon={RefreshCw} size={12} class={ciLoading ? 'animate-spin' : ''} />
          </button>
        {/if}
      {/if}
    </div>
  </div>

  {#if detail.body}
    <ReviewCollapsibleSection
      label="Description"
      open={descriptionOpen}
      onToggle={() => { descriptionOpen = !descriptionOpen; }}
      testid="review-pr-description"
    >
      <div class="max-h-96 overflow-y-auto px-2.5 py-2 text-xs text-fg">
        <ChatMarkdown source={detail.body} pathRefs={EMPTY_PATH_REFS} embeddedHtml />
      </div>
    </ReviewCollapsibleSection>
  {/if}

  {#if review && conversationTotal > 0}
    <ReviewCollapsibleSection
      label="Conversation"
      open={review.conversationOpen}
      onToggle={() => review?.setConversationOpen(!review.conversationOpen)}
      testid="review-pr-conversation"
    >
      {#snippet badge()}
        {#if openThreadCount > 0}
          <span class="shrink-0 rounded-full bg-warning/12 px-1.5 py-px text-[0.625rem] tabular-nums text-warning" data-testid="review-conversation-open-count">
            {openThreadCount} open
          </span>
        {:else}
          <span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] tabular-nums text-fg-muted">{conversationTotal}</span>
        {/if}
      {/snippet}
      {#snippet trailing()}
        {#if review && review.conversationOpen && review.conversationNewCount > 0}
          <button
            type="button"
            class="shrink-0 rounded-full bg-accent/12 px-2 py-px text-[0.625rem] tabular-nums text-accent hover:bg-accent/20"
            title="Show threads that arrived while you were reading"
            data-testid="review-conversation-new"
            onclick={() => review?.revealNewConversationThreads()}
          >
            {review.conversationNewCount} new
          </button>
        {/if}
      {/snippet}
      <!-- Capped at roughly half the pane so the diff below stays in
           reach; the section scrolls internally past that. -->
      <!-- Capped low enough that the diff stays usable below it on a
           laptop screen; the section scrolls internally instead. The vh
           leg only bites on very short viewports. -->
      <div class="max-h-[min(35vh,20rem)] overflow-y-auto">
        <ReviewConversation {review} reviews={detail.latestReviews} {canSendToAgent} />
      </div>
    </ReviewCollapsibleSection>
  {/if}

  {#if detail.latestReviews.length > 0 || detail.reviewDecision}
    <div class="mt-3 flex flex-wrap items-center gap-1.5 text-[0.6875rem]">
      {#if detail.reviewDecision}
        <span class="inline-flex items-center gap-1 rounded-full px-2 py-px {reviewStatePillClass(detail.reviewDecision)}">
          {reviewStateLabel(detail.reviewDecision)}
        </span>
      {/if}
      {#each detail.latestReviews as verdict (`${verdict.authorLogin}:${verdict.submittedAt}`)}
        <span class="inline-flex items-center gap-1 rounded-full bg-surface-2/60 py-px pl-2 pr-1 text-fg-muted">
          {verdict.authorLogin}
          <span class="rounded-full px-1.5 {reviewStatePillClass(verdict.state)}">{reviewStateLabel(verdict.state)}</span>
        </span>
      {/each}
    </div>
  {/if}
</section>
