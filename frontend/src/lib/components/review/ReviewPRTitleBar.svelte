<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronUp from '@lucide/svelte/icons/chevron-up';
  import FileText from '@lucide/svelte/icons/file-text';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import RefreshCw from '@lucide/svelte/icons/refresh-cw';
  import Icon from '../primitives/Icon.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewCIChips from './ReviewCIChips.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import { authorDisplayName } from '../../utils/reviewIdentity';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { CIJob, CIPipeline, PRDetail } from '../../types/models';

  // The PR's fixed title bar above the rail and the diff: what the PR is
  // (title, number, author) and where it stands (conflicts, CI, review
  // decision). The Description and Conversation sections are NOT here:
  // they are the overview row at the top of the diff list, which scrolls
  // off under this bar. Once it has, the bar grows peek controls that
  // scroll back to each section, and the unresolved stepper walks the
  // diff's thread rows instead of the conversation's cards.

  interface Props {
    detail: PRDetail;
    hasWorkspace?: boolean;
    onViewConflicts?: () => void;
    ciPipeline?: CIPipeline | null;
    ciLoading?: boolean;
    ciError?: string | null;
    onOpenCIJob?: (stageName: string, job: CIJob) => void;
    /** Re-polls CI status alone, no diff or thread refresh. */
    onRefreshCI?: () => void;
    /** Present in the review pane proper; absent in narrow test mounts. */
    review?: ReviewPaneState | null;
    /** The overview row has scrolled off under this bar. */
    overviewOff?: boolean;
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
    overviewOff = false,
  }: Props = $props();

  const unresolvedCount = $derived(review?.unresolvedThreads.length ?? 0);
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

  const peekClass = 'inline-flex h-6 items-center gap-1.5 rounded-[var(--radius-field)] border px-2 text-[0.6875rem] font-medium compact:h-8';
</script>

<section class="shrink-0 border-b border-border bg-surface-1 px-4 py-2.5" data-testid="review-pr-header">
  <div class="flex min-w-0 items-center gap-3">
    <button
      type="button"
      class="min-w-0 flex-1 truncate text-left text-sm font-semibold text-fg hover:text-accent"
      title={detail.title}
      onclick={() => { void openURL(detail.url); }}
    >
      {detail.title} <span class="font-normal text-fg-muted">#{detail.number}</span>
    </button>
    {#if review && overviewOff}
      <!-- Peek controls: the overview is above the viewport; each button
           scrolls the diff back to it with that section open. -->
      <div class="flex shrink-0 items-center gap-1.5" data-testid="review-overview-peek">
        {#if detail.body}
          <button
            type="button"
            class="{peekClass} border-border-subtle text-fg-muted hover:bg-surface-2 hover:text-fg"
            data-testid="review-peek-description"
            onclick={() => review?.jumpToOverview('description')}
          >
            <Icon icon={FileText} size={12} />
            Description
          </button>
        {/if}
        {#if conversationTotal > 0}
          <span class="inline-flex items-center gap-0.5">
            <button
              type="button"
              class="{peekClass} {unresolvedCount > 0 ? 'border-warning/40 text-warning hover:bg-warning/10' : 'border-border-subtle text-fg-muted hover:bg-surface-2 hover:text-fg'} {unresolvedCount > 0 ? 'rounded-r-none' : ''}"
              data-testid="review-peek-conversation"
              onclick={() => review?.jumpToOverview('conversation')}
            >
              <Icon icon={MessagesSquare} size={12} />
              Conversation
              {#if unresolvedCount > 0}
                <span class="tabular-nums">· {unresolvedCount} unresolved</span>
              {/if}
            </button>
            {#if unresolvedCount > 0}
              <span class="inline-flex h-6 items-center rounded-r-[var(--radius-field)] border border-l-0 border-warning/40 text-warning compact:h-8" data-testid="review-unresolved-stepper-bar">
                <ReviewIconButton icon={ChevronUp} label="Previous unresolved thread" onclick={() => review?.stepUnresolvedThread(-1, false)} />
                <ReviewIconButton icon={ChevronDown} label="Next unresolved thread" onclick={() => review?.stepUnresolvedThread(1, false)} />
              </span>
            {/if}
          </span>
        {/if}
      </div>
    {/if}
  </div>
  <div class="mt-1.5 flex flex-wrap items-center gap-x-2.5 gap-y-1 text-[0.75rem] text-fg-muted">
    <span class="inline-flex min-w-0 items-center gap-1.5" data-testid="review-pr-author">
      <ReviewAvatar login={detail.authorLogin} name={detail.authorName} size={16} />
      <span class="truncate font-medium text-fg">{authorDisplayName(detail)}</span>
      {#if detail.authorName}
        <span class="truncate text-fg-subtle">@{detail.authorLogin}</span>
      {/if}
    </span>
    {#if detail.mergeability === 'conflicts'}
      {#if hasWorkspace}
        <button
          type="button"
          class="rounded-[var(--radius-field)] border border-error/40 bg-error/10 px-1.5 py-px text-[0.6875rem] text-error hover:bg-error/15"
          onclick={onViewConflicts}
        >
          View conflicts
        </button>
      {:else}
        <span class="rounded-[var(--radius-field)] border border-error/40 bg-error/10 px-1.5 py-px text-[0.6875rem] text-error">Conflicts</span>
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
    <!-- The PR's decision, unless a reviewer's own verdict pill already
         says the same thing beside it. -->
    {#if detail.reviewDecision && !detail.latestReviews.some((verdict) => verdict.state.toUpperCase() === detail.reviewDecision.toUpperCase())}
      <span class="inline-flex items-center rounded-full px-2 py-px text-[0.6875rem] {reviewStatePillClass(detail.reviewDecision)}" data-testid="review-pr-decision">
        {reviewStateLabel(detail.reviewDecision)}
      </span>
    {/if}
    {#each detail.latestReviews as verdict (`${verdict.authorLogin}:${verdict.submittedAt}`)}
      <span class="inline-flex items-center gap-1 rounded-full bg-surface-2/60 py-px pl-1 pr-1 text-[0.6875rem] text-fg-muted" title="@{verdict.authorLogin}">
        <ReviewAvatar login={verdict.authorLogin} name={verdict.authorName} size={14} />
        <span class="max-w-32 truncate">{authorDisplayName(verdict)}</span>
        <span class="rounded-full px-1.5 {reviewStatePillClass(verdict.state)}">{reviewStateLabel(verdict.state)}</span>
      </span>
    {/each}
  </div>
</section>
