<script lang="ts">
  import GitCommitHorizontal from '@lucide/svelte/icons/git-commit-horizontal';
  import Icon from '../primitives/Icon.svelte';
  import { relativeTime } from '../../utils/format';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { BranchCommit } from '../../types/git';

  // One push's worth of commits in the Conversation feed: "author added
  // N commits" plus the commits themselves as one-liners, each a button
  // that shows that commit's diff (the toolbar commit selector's action).
  // Long runs fold past a few lines — commits are context, not the
  // conversation, so they don't get to push a thread off screen.

  interface Props {
    review: ReviewPaneState;
    author: string;
    commits: readonly BranchCommit[];
  }

  let { review, author, commits }: Props = $props();

  const VISIBLE_COLLAPSED = 3;
  let showAll = $state(false);
  const visible = $derived(showAll ? commits : commits.slice(0, VISIBLE_COLLAPSED));
  const hiddenCount = $derived(commits.length - visible.length);
  const time = $derived.by(() => {
    const ms = commits[0]?.authoredAt ?? 0;
    return ms > 0 ? relativeTime(ms) : '';
  });
</script>

<div
  class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/30 px-2.5 py-1.5"
  data-testid="review-conversation-commits"
>
  <div class="flex items-center gap-1.5 text-[0.6875rem]">
    <Icon icon={GitCommitHorizontal} size={12} class="shrink-0 text-fg-subtle" />
    <span class="max-w-36 truncate font-medium text-fg-muted" title={author}>{author}</span>
    <span class="text-fg-subtle">added {commits.length} {commits.length === 1 ? 'commit' : 'commits'}</span>
    {#if time}
      <span class="shrink-0 text-[0.625rem] text-fg-subtle">{time}</span>
    {/if}
  </div>
  <div class="mt-1 space-y-0.5">
    {#each visible as commit (commit.sha)}
      <button
        type="button"
        class="flex w-full min-w-0 items-baseline gap-1.5 rounded px-1 py-px text-left text-[0.6875rem] hover:bg-surface-2"
        title="Show this commit's diff"
        onclick={() => { void review.selectCommit(commit.sha); }}
      >
        <span class="shrink-0 font-mono text-[0.625rem] text-accent">{commit.shortSha}</span>
        <span class="min-w-0 truncate text-fg-muted">{commit.subject}</span>
      </button>
    {/each}
  </div>
  {#if hiddenCount > 0}
    <button
      type="button"
      class="mt-0.5 px-1 text-[0.625rem] text-fg-subtle hover:text-fg"
      onclick={() => { showAll = true; }}
    >
      +{hiddenCount} more
    </button>
  {/if}
</div>
