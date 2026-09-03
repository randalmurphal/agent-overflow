<script lang="ts">
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import ReviewCIChips from './ReviewCIChips.svelte';
  import Icon from '../primitives/Icon.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
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
  }: Props = $props();

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
    <details class="mt-3 text-xs">
      <summary class="cursor-pointer text-fg-muted">Description</summary>
      <div class="mt-2 max-h-96 overflow-auto text-fg">
        <ChatMarkdown source={detail.body} pathRefs={EMPTY_PATH_REFS} />
      </div>
    </details>
  {/if}

  {#if detail.latestReviews.length > 0 || detail.reviewDecision}
    <div class="mt-3 flex flex-wrap items-center gap-1.5 text-[0.6875rem]">
      {#if detail.reviewDecision}
        <span class="inline-flex items-center gap-1 rounded-full px-2 py-px {reviewStatePillClass(detail.reviewDecision)}">
          {reviewStateLabel(detail.reviewDecision)}
        </span>
      {/if}
      {#each detail.latestReviews as review (`${review.authorLogin}:${review.submittedAt}`)}
        <span class="inline-flex items-center gap-1 rounded-full bg-surface-2/60 py-px pl-2 pr-1 text-fg-muted">
          {review.authorLogin}
          <span class="rounded-full px-1.5 {reviewStatePillClass(review.state)}">{reviewStateLabel(review.state)}</span>
        </span>
      {/each}
    </div>
  {/if}
</section>
