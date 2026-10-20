<script lang="ts">
  import FileText from '@lucide/svelte/icons/file-text';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import Icon from '../primitives/Icon.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewCIChips from './ReviewCIChips.svelte';
  import ReviewUnresolvedStepper from './ReviewUnresolvedStepper.svelte';
  import { OpenExternalURL } from '../../stores/bindings';
  import { truncateMiddle } from '../../utils/format';
  import { authorDisplayName } from '../../utils/reviewIdentity';
  import type { ForgeFailure } from '../../utils/forgeFailure';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { CIJob, CIPipeline, PRDetail } from '../../types/models';

  // The PR's fixed title bar above the rail and the diff, four lines that
  // each say one thing: what the PR is (title, number), who opened it and
  // where it stands (author, state), what it targets and who signed off
  // (base ← head, conflicts, verdicts), and CI. Each line keeps a minimum
  // height so chips arriving or leaving never shift the diff under the
  // reader; a line's content wraps inside it on a narrow pane. The
  // Description and Conversation sections are NOT here: they are the
  // overview row at the top of the diff list, which scrolls off under
  // this bar. Once it has, the title line grows peek controls that scroll
  // back to each section, and the unresolved stepper walks the diff's
  // thread rows instead of the conversation's cards.

  interface Props {
    detail: PRDetail;
    hasWorkspace?: boolean;
    onViewConflicts?: () => void;
    ciPipeline?: CIPipeline | null;
    /** No pipeline or failure observed yet. */
    ciLoading?: boolean;
    /** A manual refresh is in flight; the chips stay. */
    ciRefreshing?: boolean;
    ciError?: string | null;
    /** The kind of ciError. */
    ciFailure?: ForgeFailure | null;
    /** The PR's forge, which names a rate limit. */
    forge?: string;
    onOpenCIJob?: (stageName: string, job: CIJob) => void;
    /** Has the pump re-poll CI now, no diff or thread refresh. */
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
    ciRefreshing = false,
    ciError = null,
    ciFailure = null,
    forge = '',
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

  // The PR's state as the forge normalizes it (open, merged, closed,
  // locked; internal/git/forge.go NormalizePRState), a draft reading as
  // draft whatever its state.
  const stateLabel = $derived(detail.draft ? 'draft' : detail.state.toLowerCase());
  const STATE_PILL_CLASS: Record<string, string> = {
    open: 'bg-success/12 text-success',
    merged: 'bg-accent/12 text-accent',
    closed: 'bg-error/12 text-error',
    draft: 'bg-surface-2 text-fg-muted',
  };
  const statePillClass = $derived(STATE_PILL_CLASS[stateLabel] ?? 'bg-surface-2 text-fg-muted');

  // A branch name's distinguishing part is usually its tail
  // (BLITZ-664-action-queue), so a long one keeps both ends; the full
  // pair is the line's tooltip.
  const REF_MAX = 36;

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
  const lineClass = 'flex min-h-6 min-w-0 flex-wrap items-center gap-x-2.5 gap-y-1 text-[0.75rem] text-fg-muted compact:min-h-8';
</script>

<section class="flex shrink-0 flex-col gap-1.5 border-b border-border bg-surface-1 px-4 py-2.5" data-testid="review-pr-header">
  <div class="flex min-h-6 min-w-0 items-center gap-3 compact:min-h-8">
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
        {#if conversationTotal > 0 && unresolvedCount > 0}
          <!-- With open threads the peek is the label segment of the
               unresolved stepper, which walks the diff's thread rows. -->
          <ReviewUnresolvedStepper
            testid="review-unresolved-stepper-bar"
            labelTestid="review-peek-conversation"
            onLabel={() => review?.jumpToOverview('conversation')}
            onStep={(direction) => review?.stepUnresolvedThread(direction, false)}
          >
            {#snippet label()}
              <Icon icon={MessagesSquare} size={12} />
              <!-- The phone's row has no room for the section's name
                   beside the count; the icon names it there. -->
              <span class="compact:hidden">Conversation ·</span>
              {unresolvedCount} unresolved
            {/snippet}
          </ReviewUnresolvedStepper>
        {:else if conversationTotal > 0}
          <button
            type="button"
            class="{peekClass} border-border-subtle text-fg-muted hover:bg-surface-2 hover:text-fg"
            data-testid="review-peek-conversation"
            onclick={() => review?.jumpToOverview('conversation')}
          >
            <Icon icon={MessagesSquare} size={12} />
            Conversation
          </button>
        {/if}
      </div>
    {/if}
  </div>

  <div class={lineClass} data-testid="review-pr-author-line">
    <span class="inline-flex min-w-0 items-center gap-1.5" data-testid="review-pr-author">
      <ReviewAvatar login={detail.authorLogin} name={detail.authorName} size={16} />
      <span class="truncate font-medium text-fg">{authorDisplayName(detail)}</span>
      {#if detail.authorName}
        <span class="truncate text-fg-subtle">@{detail.authorLogin}</span>
      {/if}
    </span>
    <span class="inline-flex items-center rounded-full px-2 py-px text-[0.6875rem] {statePillClass}" data-testid="review-pr-state">{stateLabel}</span>
  </div>

  <div class={lineClass} data-testid="review-pr-refs-line">
    <span
      class="inline-flex min-w-0 items-center gap-1 font-mono text-[0.6875rem]"
      title="{detail.baseRefName} ← {detail.headRefName}"
      data-testid="review-pr-refs"
    >
      <span class="truncate text-fg">{truncateMiddle(detail.baseRefName, REF_MAX)}</span>
      <span class="shrink-0 text-fg-subtle">←</span>
      <span class="truncate text-fg">{truncateMiddle(detail.headRefName, REF_MAX)}</span>
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

  {#if onOpenCIJob}
    <div class={lineClass} data-testid="review-pr-ci-line">
      <ReviewCIChips
        pipeline={ciPipeline}
        loading={ciLoading}
        error={ciError}
        failure={ciFailure}
        {forge}
        onOpenJob={onOpenCIJob}
        onRefresh={onRefreshCI}
        refreshing={ciRefreshing}
      />
    </div>
  {/if}
</section>
