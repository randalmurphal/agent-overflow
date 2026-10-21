<script lang="ts">
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';

  // The drafts' send strip: the count, the target (the linked agent or the
  // PR as a review; a pinned review has only the PR), and for a PR review
  // its verdict and summary.

  interface Props {
    review: ReviewPaneState;
  }

  let { review }: Props = $props();
</script>

<section class="border-t border-border bg-surface-1/85 px-3 py-2" aria-label="Review comments" data-testid="review-send-strip">
  <div class="flex items-center justify-between gap-3">
    <div class="text-[0.6875rem] font-medium uppercase tracking-[0.08em] text-fg-muted">
      {review.drafts.length} {review.drafts.length === 1 ? 'draft' : 'drafts'}
    </div>
    <div class="flex items-center gap-2">
      {#if review.scope === 'pr' && !review.selectedCommitSHA && !review.pinned}
        <!-- Single-commit view: drafts anchor to that commit's diff,
             not the PR head diff — PR submission would mis-place
             them, so the only target is the linked agent. -->
        <select
          class="rounded border border-border-subtle bg-surface-0 px-2 py-1 text-[0.6875rem]"
          value={review.submitTarget}
          data-testid="review-submit-target"
          onchange={(event) => review.setSubmitTarget(event.currentTarget.value === 'pr' ? 'pr' : 'agent')}
        >
          <option value="agent">Linked agent</option>
          <option value="pr">{review.prScopeLabel ?? 'PR'} review</option>
        </select>
      {/if}
      <button
        type="button"
        class="rounded border border-accent/45 px-2 py-1 text-[0.6875rem] font-medium text-accent hover:bg-accent/10 disabled:opacity-45"
        disabled={!review.commentsGranted || review.sendingComments || (review.effectiveSubmitTarget === 'agent' && review.isTurnActive)}
        title={!review.commentsGranted ? 'Not granted to this device' : review.isTurnActive && review.effectiveSubmitTarget === 'agent' ? 'Send from the chat box while the agent is working' : 'Send comments'}
        data-testid="review-send-comments"
        onclick={() => { void (review.effectiveSubmitTarget === 'pr' ? review.submitPRReview() : review.sendComments()); }}
      >
        Send comments
      </button>
    </div>
  </div>
  {#if review.effectiveSubmitTarget === 'pr'}
    <div class="mt-2 flex flex-wrap items-center gap-2">
      {#each ['comment', 'approve', 'request-changes'] as nextVerdict}
        {@const ownPRBlocked = review.prRef?.forge === 'github' && review.prDetail?.viewerIsAuthor && nextVerdict !== 'comment'}
        <button
          type="button"
          class="rounded border px-2 py-1 text-[0.6875rem] transition-colors disabled:cursor-not-allowed disabled:opacity-40 {review.verdict === nextVerdict ? 'border-accent bg-accent/10 text-accent' : 'border-border-subtle text-fg-muted hover:text-fg'}"
          disabled={ownPRBlocked}
          title={ownPRBlocked ? 'GitHub rejects approving or requesting changes on your own PR' : ''}
          onclick={() => review.setVerdict(nextVerdict as 'comment' | 'approve' | 'request-changes')}
        >
          {nextVerdict === 'request-changes' ? 'Request changes' : nextVerdict[0].toUpperCase() + nextVerdict.slice(1)}
        </button>
      {/each}
    </div>
    <textarea
      class="mt-2 w-full resize-none rounded border border-border-subtle bg-surface-0 px-2 py-1.5 text-xs"
      rows="2"
      value={review.summaryBody}
      oninput={(event) => review.setSummaryBody(event.currentTarget.value)}
    ></textarea>
  {/if}
</section>
