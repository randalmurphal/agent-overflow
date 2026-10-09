<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewConversationCommits from './ReviewConversationCommits.svelte';
  import ReviewConversationThread from './ReviewConversationThread.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { relativeTime } from '../../utils/format';
  import { visibleBody } from '../../utils/reviewComments';
  import { authorDisplayName } from '../../utils/reviewIdentity';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';

  // The Conversation section's body: ONE chronological feed (newest
  // first) interleaving thread cards, review verdicts, and commit pushes,
  // the forge's overview timeline made compact. Entries hang off one
  // vertical rail so the sequence reads at a glance; the avatar sits on
  // the rail and the card beside it. The store owns the feed and freezes
  // its order once shown; new arrivals wait behind the header's chip.

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

  function verdictKind(state: string): 'approved' | 'changes' | 'comment' {
    switch (state.toUpperCase()) {
      case 'APPROVED':
        return 'approved';
      case 'CHANGES_REQUESTED':
        return 'changes';
      default:
        return 'comment';
    }
  }

  const VERDICT_LABEL = { approved: 'approved', changes: 'requested changes', comment: 'reviewed' } as const;
  const VERDICT_CHIP = {
    approved: 'bg-success/12 text-success',
    changes: 'bg-error/12 text-error',
    comment: 'bg-surface-2 text-fg-muted',
  } as const;
  const VERDICT_CARD = {
    approved: 'border-success/45',
    changes: 'border-error/45',
    comment: 'border-border',
  } as const;
  const VERDICT_HEAD = {
    approved: 'bg-success/8',
    changes: 'bg-error/8',
    comment: 'bg-surface-2/60',
  } as const;

  // One-shot scroll to a jumped-to thread (inline strip, stepper or peek
  // control). Runs after the cards render; consuming clears it so a later
  // remount of the row does not replay the scroll.
  $effect(() => {
    const target = review.pendingConversationThreadId;
    if (!target || !rootEl) return;
    const el = rootEl.querySelector(`[data-thread-id="${CSS.escape(target)}"]`);
    el?.scrollIntoView({ block: 'nearest' });
    review.consumePendingConversationThreadId();
  });
</script>

<div bind:this={rootEl} class="relative px-3 py-3.5 text-xs" data-testid="review-conversation">
  {#if review.conversationFeed.length > 1}
    <!-- The rail: avatar discs are 28px wide from x=12, so their center
         is x=26; the line runs from the first disc to the last. -->
    <div class="pointer-events-none absolute bottom-7 left-[25.5px] top-7 w-px bg-border" aria-hidden="true"></div>
  {/if}
  <div class="flex flex-col gap-4">
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
        {@const kind = verdictKind(entry.verdict.state)}
        <article
          class="grid grid-cols-[28px_minmax(0,1fr)] gap-x-3"
          data-testid="review-conversation-verdict"
        >
          <div class="relative z-10 rounded-full ring-[3px] ring-surface-0/70">
            <ReviewAvatar login={entry.verdict.authorLogin} name={entry.verdict.authorName} size={28} />
          </div>
          <div class="min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 {VERDICT_CARD[kind]}">
            <div class="flex min-w-0 items-center gap-1.5 px-3 py-2 {VERDICT_HEAD[kind]} {body !== '' ? 'border-b border-border-subtle' : ''}">
              <span class="truncate font-semibold text-fg">{authorDisplayName(entry.verdict)}</span>
              <span class="shrink-0 text-fg-muted">{VERDICT_LABEL[kind]}</span>
              {#if verdictTime(entry.verdict.submittedAt)}
                <span class="shrink-0 text-fg-subtle">· {verdictTime(entry.verdict.submittedAt)}</span>
              {/if}
              <span class="min-w-0 flex-1"></span>
              <span class="shrink-0 rounded-full px-2 py-px text-[0.6875rem] {VERDICT_CHIP[kind]}">{VERDICT_LABEL[kind] === 'reviewed' ? 'commented' : VERDICT_LABEL[kind]}</span>
            </div>
            {#if body !== ''}
              <div class="px-3.5 py-3">
                <ChatMarkdown source={body} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
              </div>
            {/if}
          </div>
        </article>
      {:else}
        <ReviewConversationCommits {review} author={entry.author} commits={entry.commits} />
      {/if}
    {/each}
  </div>

  {#if review.conversationFeed.length === 0}
    <div class="px-1 py-1 text-fg-muted">No conversation yet.</div>
  {/if}
</div>
