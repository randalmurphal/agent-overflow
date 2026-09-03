<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewConversationThread from './ReviewConversationThread.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { relativeTime } from '../../utils/format';
  import { visibleBody } from '../../utils/reviewComments';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { ReviewVerdict } from '../../types/models';

  // The Conversation section's body: review verdict summaries first, then
  // EVERY thread (file-anchored and PR-level) in the store's frozen triage
  // order — unresolved first, settled and outdated after. The section
  // never reorders under the reader; new arrivals wait behind the header's
  // "N new" chip.

  interface Props {
    review: ReviewPaneState;
    reviews: readonly ReviewVerdict[];
    canSendToAgent: boolean;
  }

  let { review, reviews, canSendToAgent }: Props = $props();

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
  {#each reviews as verdict (`${verdict.authorLogin}:${verdict.submittedAt}`)}
    {@const body = visibleBody(verdict.body)}
    <article
      class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/50 px-2.5 py-1.5"
      data-testid="review-conversation-verdict"
    >
      <div class="flex items-center gap-1.5">
        <span class="shrink-0 rounded-full px-1.5 py-px text-[0.625rem] {verdictPillClass(verdict.state)}">{verdictLabel(verdict.state)}</span>
        <span class="min-w-0 truncate text-[0.6875rem] font-medium text-fg">{verdict.authorLogin}</span>
        {#if verdictTime(verdict.submittedAt)}
          <span class="shrink-0 text-[0.625rem] text-fg-subtle">{verdictTime(verdict.submittedAt)}</span>
        {/if}
      </div>
      {#if body !== ''}
        <div class="mt-1.5">
          <ChatMarkdown source={body} pathRefs={EMPTY_PATH_REFS} embeddedHtml />
        </div>
      {/if}
    </article>
  {/each}

  {#each review.conversationThreads as thread (thread.id)}
    <ReviewConversationThread
      {review}
      {thread}
      {canSendToAgent}
      inDiff={thread.path !== '' && diffPaths.has(thread.path)}
    />
  {/each}

  {#if reviews.length === 0 && review.conversationThreads.length === 0}
    <div class="px-1 py-1 text-fg-muted">No conversation yet.</div>
  {/if}
</div>
