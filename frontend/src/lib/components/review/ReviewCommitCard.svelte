<script lang="ts">
  import { relativeTime } from '../../utils/format';
  import type { BranchCommit } from '../../types/git';

  // The commit under review, as the diff list's first row: hash, the whole
  // subject, author and age, and the message body when there is one. It
  // scrolls off with the diff like a page heading, so the fixed chrome
  // above never changes height for it; in "All commits" it does not exist.

  interface Props {
    commit: BranchCommit;
    /** Its place in the picker's list, newest first. */
    ordinal: number;
    count: number;
  }

  let { commit, ordinal, count }: Props = $props();

  const age = $derived(commit.authoredAt > 0 ? relativeTime(commit.authoredAt) : '');
  // One string, so the separators survive template whitespace handling.
  const metaRest = $derived([age, `${ordinal} of ${count}`].filter(Boolean).map((part) => ` · ${part}`).join(''));
</script>

<div class="px-[var(--review-slab-inset)] pb-3 pt-2.5" data-testid="review-commit-card" data-sha={commit.sha}>
  <section class="overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-surface-0/70">
    <div class="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1 px-3.5 py-2.5 text-xs">
      <span class="shrink-0 font-mono text-[0.6875rem] text-accent">{commit.shortSha}</span>
      <span class="min-w-0 flex-1 basis-48 font-semibold text-fg" data-testid="review-commit-card-subject">{commit.subject}</span>
      <span class="shrink-0 text-[0.6875rem] text-fg-muted">
        <span class="text-fg">{commit.author}</span><span>{metaRest}</span>
      </span>
    </div>
    {#if commit.body}
      <div class="whitespace-pre-wrap break-words border-t border-border-subtle px-3.5 py-2.5 text-xs leading-relaxed text-fg-muted" data-testid="review-commit-card-body">{commit.body}</div>
    {/if}
  </section>
</div>
