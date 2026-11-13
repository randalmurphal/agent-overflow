<script lang="ts">
  import GitCommitHorizontal from '@lucide/svelte/icons/git-commit-horizontal';
  import Icon from '../primitives/Icon.svelte';
  import { relativeTime } from '../../utils/format';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { BranchCommit } from '../../types/git';

  // One push's worth of commits in the Conversation feed: a dot on the
  // rail, "author added N commits", then the commits as one-liners, each
  // a button that shows that commit's diff (the toolbar commit
  // selector's action), and `Changes from here` shows the group's first
  // commit through the newest. Commits are context, not the conversation,
  // so they get no card and long runs fold past a few lines.

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
  // The group's first commit: `Changes from here` runs from it to the head.
  const first = $derived(commits.length > 0 ? commits[commits.length - 1] : null);
  function showChangesFromFirst(): void {
    const from = first;
    if (from) void review.showChangesFrom(from.sha);
  }
  const time = $derived.by(() => {
    const ms = commits[0]?.authoredAt ?? 0;
    return ms > 0 ? relativeTime(ms) : '';
  });
</script>

<div
  class="grid grid-cols-[28px_minmax(0,1fr)] gap-x-3"
  data-testid="review-conversation-commits"
>
  <div class="relative z-10 flex h-7 items-center justify-center">
    <span class="inline-flex size-6 items-center justify-center rounded-full border border-border bg-surface-2 text-fg-muted ring-[3px] ring-surface-0/70">
      <Icon icon={GitCommitHorizontal} size={13} />
    </span>
  </div>
  <div class="group min-w-0 pt-1">
    <div class="flex min-w-0 items-center gap-1.5 text-[0.75rem]">
      <span class="truncate font-semibold text-fg" title={author}>{author}</span>
      <span class="shrink-0 text-fg-muted">added {commits.length} {commits.length === 1 ? 'commit' : 'commits'}</span>
      {#if time}
        <span class="shrink-0 text-fg-subtle">· {time}</span>
      {/if}
      {#if first}
        <!-- Revealed on hover; it holds its place, so nothing moves. -->
        <button
          type="button"
          class="ml-auto shrink-0 rounded-[var(--radius-field)] px-1.5 text-[0.6875rem] text-fg-muted opacity-0 hover:bg-surface-2 hover:text-fg focus-visible:opacity-100 group-hover:opacity-100 compact:opacity-100 compact:select-none"
          title="Show the changes from {first.shortSha} through the newest commit"
          data-testid="review-conversation-changes-from"
          onclick={showChangesFromFirst}
        >Changes from here</button>
      {/if}
    </div>
    <div class="mt-1.5 flex flex-col gap-0.5">
      {#each visible as commit (commit.sha)}
        <button
          type="button"
          class="flex w-full min-w-0 items-baseline gap-2 rounded-[var(--radius-field)] px-1.5 py-0.5 text-left text-[0.75rem] hover:bg-surface-2"
          title="Show this commit's diff"
          onclick={() => { void review.selectCommit(commit.sha, { userInitiated: true }); }}
        >
          <span class="shrink-0 font-mono text-[0.6875rem] text-accent">{commit.shortSha}</span>
          <span class="min-w-0 truncate text-fg-muted">{commit.subject}</span>
        </button>
      {/each}
    </div>
    {#if hiddenCount > 0}
      <button
        type="button"
        class="mt-1 px-1.5 text-[0.6875rem] text-fg-subtle hover:text-fg"
        onclick={() => { showAll = true; }}
      >
        +{hiddenCount} more
      </button>
    {/if}
  </div>
</div>
