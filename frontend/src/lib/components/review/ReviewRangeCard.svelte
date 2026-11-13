<script lang="ts">
  import { commitOrdinal } from '../../utils/commitRange';
  import type { BranchCommit } from '../../types/git';

  // A run of commits under review, as the diff list's first row in the
  // commit card's place (ReviewCommitCard): how many, its first and last
  // commit by short hash and subject, and `All commits` to leave it. Two
  // lines, one more than a commit card with no body. A head-ending run
  // whose PR lines could not be read says so on a third, with Retry:
  // until they are, no line takes a PR comment.

  interface Props {
    /** The run, newest first, as the list holds it. */
    commits: readonly BranchCommit[];
    /** The run's older end's index in the whole list. */
    olderIndex: number;
    count: number;
    onLeave: () => void;
    /** The whole PR's diff failed to read for a head-ending run. */
    indexError?: string | null;
    onRetryIndex?: () => void;
  }

  let { commits, olderIndex, count, onLeave, indexError = null, onRetryIndex }: Props = $props();

  const first = $derived(commits[commits.length - 1]);
  const last = $derived(commits[0]);
  const from = $derived(commitOrdinal(olderIndex, count));
  const to = $derived(commitOrdinal(olderIndex - commits.length + 1, count));
</script>

<div class="px-[var(--review-slab-inset)] pb-3 pt-2.5" data-testid="review-range-card" data-first={first?.sha} data-last={last?.sha}>
  <section class="overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/70 px-3.5 py-2 text-xs">
    <div class="flex min-w-0 items-baseline gap-2">
      <span class="font-semibold text-fg" data-testid="review-range-card-count">{commits.length} commits</span>
      <span class="min-w-0 flex-1 truncate text-[0.6875rem] text-fg-muted">{from}–{to} of {count}{to === count ? ' · to the head' : ''}</span>
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-field)] px-1.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg compact:select-none"
        data-testid="review-range-card-leave"
        onclick={onLeave}
      >All commits</button>
    </div>
    {#if first && last}
      <div class="mt-1 flex min-w-0 items-baseline gap-1.5 text-[0.6875rem]" data-testid="review-range-card-ends">
        <span class="shrink-0 font-mono text-accent">{first.shortSha}</span>
        <span class="min-w-0 truncate text-fg-muted" title={first.subject}>{first.subject}</span>
        <span class="shrink-0 text-fg-hint" aria-label="through">→</span>
        <span class="shrink-0 font-mono text-accent">{last.shortSha}</span>
        <span class="min-w-0 truncate text-fg-muted" title={last.subject}>{last.subject}</span>
      </div>
    {/if}
    {#if indexError}
      <div class="mt-1 flex min-w-0 items-baseline gap-2 text-[0.6875rem] text-warning" data-testid="review-range-card-index-error">
        <span class="min-w-0 flex-1 truncate" title={indexError}>The pull request's diff did not load, so no line here takes a PR comment.</span>
        {#if onRetryIndex}
          <button type="button" class="shrink-0 rounded-[var(--radius-field)] px-1.5 hover:bg-surface-2" onclick={onRetryIndex}>Retry</button>
        {/if}
      </div>
    {/if}
  </section>
</div>
