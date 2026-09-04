<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewConversationCommits from './ReviewConversationCommits.svelte';
  import ReviewConversationThread from './ReviewConversationThread.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { relativeTime } from '../../utils/format';
  import { visibleBody } from '../../utils/reviewComments';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';

  // The Conversation section's body: ONE chronological feed (newest
  // first) interleaving thread cards, review verdicts, and commit pushes
  // — GitLab's overview timeline, compact. The store owns the feed and
  // freezes its order while the section is open; new arrivals wait
  // behind the header's "N new" chip.

  interface Props {
    review: ReviewPaneState;
    canSendToAgent: boolean;
  }

  let { review, canSendToAgent }: Props = $props();

  let rootEl: HTMLElement | undefined = $state();

  const diffPaths = $derived(new Set(review.files.map((file) => file.path)));

  function verdictTime(submittedAt: string): string {
    const ms = Date.parse(submittedAt);
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  }

  function verdictPillClass(state: string): string {
    switch (state.toUpperCase()) {
      case 'APPROVED':
        return 'bg-success/12 text-success';
      case 'CHANGES_REQUESTED':
        return 'bg-error/12 text-error';
      default:
        return 'bg-surface-2 text-fg-muted';
    }
  }

  function verdictLabel(state: string): string {
    return state.replaceAll('_', ' ').toLowerCase();
  }

  // One-shot scroll to a jumped-to thread (inline strip or rail row →
  // conversation). Runs after the cards render; consuming clears it so a
  // later section re-open does not replay the scroll.
  $effect(() => {
    const target = review.pendingConversationThreadId;
    if (!target || !rootEl) return;
    const el = rootEl.querySelector(`[data-thread-id="${CSS.escape(target)}"]`);
    el?.scrollIntoView({ block: 'nearest' });
    review.consumePendingConversationThreadId();
  });
</script>

<div bind:this={rootEl} class="space-y-1.5 px-2 py-2 text-xs" data-testid="review-conversation">
  {#each review.conversationFeed as entry (entry.id)}
    {#if entry.kind === 'thread'}
      <ReviewConversationThread
        {review}
        thread={entry.thread}
        {canSendToAgent}
        inDiff={entry.thread.path !== '' && diffPaths.has(entry.thread.path)}
      />
    {:else if entry.kind === 'verdict'}
      {@const body = visibleBody(entry.verdict.body)}
      <article
        class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/50 px-2.5 py-1.5"
        data-testid="review-conversation-verdict"
      >
        <div class="flex items-center gap-1.5">
          <span class="shrink-0 rounded-full px-1.5 py-px text-[0.625rem] {verdictPillClass(entry.verdict.state)}">{verdictLabel(entry.verdict.state)}</span>
          <span class="max-w-36 truncate text-[0.6875rem] font-medium text-fg" title={entry.verdict.authorLogin}>{entry.verdict.authorLogin}</span>
          {#if verdictTime(entry.verdict.submittedAt)}
            <span class="shrink-0 text-[0.625rem] text-fg-subtle">{verdictTime(entry.verdict.submittedAt)}</span>
          {/if}
        </div>
        {#if body !== ''}
          <div class="mt-1.5">
            <ChatMarkdown source={body} pathRefs={EMPTY_PATH_REFS} embeddedHtml />
          </div>
        {/if}
      </article>
    {:else}
      <ReviewConversationCommits {review} author={entry.author} commits={entry.commits} />
    {/if}
  {/each}

  {#if review.conversationFeed.length === 0}
    <div class="px-1 py-1 text-fg-muted">No conversation yet.</div>
  {/if}
</div>
