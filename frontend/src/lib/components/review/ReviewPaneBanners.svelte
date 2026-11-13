<script lang="ts">
  // The review pane's banners under its toolbar: the load's error with
  // Retry, a branch list failure, a submit failure, a conflict read
  // failure, the PR pump's failure by kind, and the stale-PR Reload.
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { rateLimitMessage } from '../../utils/forgeFailure';

  let { review, branchesError }: { review: ReviewPaneState; branchesError: string | null } = $props();
</script>

{#if review.error}
  <!-- A failed refresh keeps the diff it was refreshing below this
       banner; Retry runs the same load again. -->
  <div class="flex items-start justify-between gap-3 border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-error">
    <span class="max-h-32 min-w-0 overflow-y-auto whitespace-pre-wrap break-words" data-testid="review-error-message">{review.error}</span>
    <button
      type="button"
      class="shrink-0 rounded border border-error/40 px-2 py-0.5 disabled:opacity-50"
      disabled={review.loading}
      onclick={() => { void review.reload({ userInitiated: true }); }}
      data-testid="review-error-retry"
    >Retry</button>
  </div>
{/if}
{#if branchesError && review.scope === 'branch'}
  <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-branches-error">
    {branchesError}
  </div>
{/if}
{#if review.submitError}
  <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-submit-error">
    {review.submitError}
  </div>
{/if}
{#if review.conflictsError}
  <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-conflicts-error">
    {review.conflictsError}
  </div>
{/if}
{#if review.prUpdateError}
  {@const failure = review.prUpdateFailure}
  {#if failure?.kind === 'rate_limited'}
    <div class="border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning" data-testid="review-pr-rate-limited">
      {rateLimitMessage(review.prRef?.forge, failure)}
    </div>
  {:else if failure?.kind === 'setup' || failure?.kind === 'forbidden'}
    <!-- A login to fix outside the app (missing, expired, or without
         the scope or the project): the message names it, and no retry
         will change it until the user does. -->
    <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-pr-setup">
      {review.prUpdateError}
    </div>
  {:else}
    <div class="border-b border-error/30 bg-error/10 px-3 py-2 text-xs text-error" data-testid="review-pr-update-error">
      Retrying: {review.prUpdateError}
    </div>
  {/if}
{/if}
{#if review.commitSelectionLost}
  <!-- A held commit or run named commits the PR no longer lists (a
       rebase, an amend): the pane shows All commits instead. -->
  <div class="flex items-center justify-between gap-3 border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning" data-testid="review-commits-gone">
    <span>PR updated. The picked commits are gone, so this is all commits.</span>
    <button type="button" class="rounded border border-warning/40 px-2 py-0.5" onclick={() => review.dismissCommitSelectionLost()}>Dismiss</button>
  </div>
{/if}
{#if review.prStale}
  <div class="flex items-center justify-between gap-3 border-b border-warning/30 bg-warning/10 px-3 py-2 text-xs text-warning" data-testid="review-pr-stale">
    <span>PR updated.</span>
    <button type="button" class="rounded border border-warning/40 px-2 py-0.5" onclick={() => { void review.reload({ userInitiated: true }); }}>Reload</button>
  </div>
{/if}
