<script lang="ts">
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Icon from '../primitives/Icon.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewHeldReplies from './ReviewHeldReplies.svelte';
  import ReviewSentCommentActions from './ReviewSentCommentActions.svelte';
  import ReviewSentCommentBody from './ReviewSentCommentBody.svelte';
  import ReviewThreadComments from './ReviewThreadComments.svelte';
  import ReviewThreadFooter from './ReviewThreadFooter.svelte';
  import ReviewThreadContext from './ReviewThreadContext.svelte';
  import { visibleBody } from '../../utils/reviewComments';
  import { threadRange } from '../../utils/reviewRange';
  import { authorDisplayName, isBotLogin } from '../../utils/reviewIdentity';
  import {
    THREAD_CARD_CLASS,
    CONFIRM_HIDES,
    THREAD_CHIP_CLASS,
    THREAD_HEAD_CLASS,
    THREAD_OUTDATED_CARD_CLASS,
    THREAD_OUTDATED_CHIP_CLASS,
    outdatedTitle,
    reviewThreadOutdated,
    reviewThreadState,
    threadBodyClass,
  } from '../../utils/reviewThreadStyle';
  import { relativeTime } from '../../utils/format';
  import { jumpPulse } from '../../utils/reviewJumpPulse';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { ReviewThread } from '../../types/models';

  // One thread's card in the Conversation feed: the author's avatar on
  // the rail, a bubble beside it. The header says who, where (the file
  // and line, a jump into the diff when that row exists), when, and the
  // thread's state; the actions (reply, copy, resolve) sit at the foot
  // of the card, the way the forges place them. The state also reads from the edge, so a scan down
  // the feed tells unresolved (warning edge) from resolved (success
  // edge) without reading a word; a dashed edge and an "outdated" chip
  // mark a thread written against code that has since moved, whatever
  // its state. The first comment always renders IN FULL; only the
  // REPLIES fold, and only on settled threads. Anchored threads fold the code they were written
  // against under the header.

  interface Props {
    review: ReviewPaneState;
    thread: ReviewThread;
    /** The thread's file is in the rendered diff (the jump has a target). */
    inDiff: boolean;
    /** The jump that landed here (utils/reviewJumpPulse); 0 for none. */
    pulse?: number;
  }

  let { review, thread, inDiff, pulse = 0 }: Props = $props();

  // The composer's text and open state are store-backed: the card
  // unmounts with the overview row.
  const replying = $derived(review.replyOpen(thread.id, 'conversation'));

  const repliesOpen = $derived(review.conversationThreadExpanded(thread.id));
  const threadState = $derived(reviewThreadState(thread));
  const outdated = $derived(reviewThreadOutdated(thread));
  const settled = $derived(threadState === 'resolved');
  const first = $derived(thread.comments[0]);
  const firstBody = $derived(visibleBody(first?.body ?? ''));
  const replyCount = $derived(Math.max(0, thread.comments.length - 1));
  const firstTime = $derived.by(() => {
    const ms = Date.parse(first?.createdAt ?? '');
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  });
  // An outdated GitHub thread has no current line; it names the line it
  // was written at instead.
  const range = $derived(threadRange(thread));
  const location = $derived.by(() => {
    if (thread.path === '') return '';
    if (range) return `${thread.path}:${range.from}–${range.to}`;
    const line = thread.line ?? thread.originalLine;
    return line ? `${thread.path}:${line}` : thread.path;
  });
  const heldReplies = $derived(review.replyDraftsFor(thread.id));
  const editingFirst = $derived(first !== undefined && review.sentComments.editBodyFor(thread, first) !== null);
  // The file bar above the diff already carries directories; the card
  // names the file, and the full path is the tooltip.
  const shortLocation = $derived.by(() => {
    const slash = location.lastIndexOf('/');
    return slash < 0 ? location : location.slice(slash + 1);
  });
  const contextFile = $derived(
    inDiff && thread.line ? (review.files.find((file) => file.path === thread.path) ?? null) : null,
  );
  function openReply(): void {
    review.openReply(thread.id, 'conversation');
    // Replying into a folded thread: unfold so the reply lands in view.
    if (replyCount > 0 && !repliesOpen) review.toggleConversationThread(thread.id);
  }
</script>

<article
  class="grid grid-cols-[28px_minmax(0,1fr)] gap-x-3"
  data-testid="review-conversation-thread"
  data-thread-id={thread.id}
  data-state={threadState}
  data-outdated={outdated ? 'true' : undefined}
>
  <div class="relative z-10 rounded-full ring-[3px] ring-surface-0/70">
    <ReviewAvatar login={first?.authorLogin ?? ''} name={first?.authorName} size={28} />
  </div>
  <div
    class="group/thread min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 {THREAD_CARD_CLASS[threadState]} {outdated ? THREAD_OUTDATED_CARD_CLASS : ''}"
    {@attach jumpPulse(pulse)}
  >
    <!-- Wraps so the chips land on a second line in a narrow feed instead
         of clipping. -->
    <div class="group/head flex min-w-0 flex-wrap items-center gap-x-1.5 gap-y-1 py-1.5 px-3 text-[0.75rem] {THREAD_HEAD_CLASS[threadState]} {firstBody === '' && !contextFile && !editingFirst ? '' : 'border-b border-border-subtle'}" data-first-comment>
      <span class="shrink-0 font-semibold text-fg" title={first ? `@${first.authorLogin}` : ''}>{first ? authorDisplayName(first) : ''}</span>
      {#if first && isBotLogin(first.authorLogin)}
        <span class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1 text-[0.625rem] leading-4 text-fg-muted">bot</span>
      {/if}
      {#if first?.authorName}
        <span class="min-w-0 shrink truncate text-fg-subtle {CONFIRM_HIDES}" title="@{first.authorLogin}">@{first.authorLogin}</span>
      {/if}
      {#if location !== ''}
        <span class="shrink-0 text-fg-muted">on</span>
        {#if inDiff}
          <button
            type="button"
            class="min-w-0 shrink-0 truncate rounded-[var(--radius-field)] font-mono text-[0.6875rem] font-semibold text-fg hover:text-accent"
            title="Show {location} in the diff"
            data-testid="review-conversation-jump-diff"
            onclick={() => review.jumpToDiffThread(thread)}
          >{shortLocation}</button>
        {:else}
          <span class="min-w-0 shrink-0 truncate font-mono text-[0.6875rem] text-fg-muted" title="{location} (not in this diff)">{shortLocation}</span>
        {/if}
      {:else if !thread.isResolvable}
        <span class="shrink-0 text-fg-muted">commented</span>
      {/if}
      {#if firstTime}
        <span class="shrink-0 text-fg-subtle {CONFIRM_HIDES}">· {firstTime}</span>
      {/if}
      {#if first?.editedAt}
        <span class="shrink-0 text-fg-subtle {CONFIRM_HIDES}" data-testid="review-comment-edited">edited</span>
      {/if}
      <span class="min-w-0 flex-1"></span>
      <!-- The actions' slot sits left of the chips, so the chips keep the
           right edge; its confirm lays over the text to its left. -->
      {#if first}
        <ReviewSentCommentActions {review} {thread} comment={first} first />
      {/if}
      {#if outdated}
        <span class="shrink-0 rounded-full px-1.5 py-px text-[0.625rem] {THREAD_OUTDATED_CHIP_CLASS}" title={outdatedTitle(thread)} data-testid="review-thread-outdated">outdated</span>
      {/if}
      {#if threadState !== 'none'}
        <span class="shrink-0 rounded-full px-1.5 py-px text-[0.625rem] {THREAD_CHIP_CLASS[threadState]}" data-testid="review-thread-state">{threadState}</span>
      {/if}
    </div>

    {#if contextFile}
      <ReviewThreadContext file={contextFile} {thread} />
    {/if}

    <div class={threadBodyClass(threadState)}>
      {#if first && (firstBody !== '' || editingFirst)}
        <div data-first-comment>
          <ReviewSentCommentBody {review} {thread} comment={first} class="px-3.5 py-3" />
        </div>
      {/if}

      {#if replyCount > 0 && settled}
        <button
          type="button"
          class="flex w-full items-center gap-1.5 border-t border-border-subtle px-3.5 py-2 text-left text-[0.6875rem] text-fg-muted hover:text-fg"
          aria-expanded={repliesOpen}
          data-testid="review-conversation-replies"
          onclick={() => review.toggleConversationThread(thread.id)}
        >
          <Icon icon={repliesOpen ? ChevronDown : ChevronRight} size={11} class="shrink-0" />
          {replyCount} {replyCount === 1 ? 'reply' : 'replies'}
        </button>
      {/if}

      {#if replyCount > 0 && (repliesOpen || !settled)}
        <ReviewThreadComments {thread} {review} skipFirst />
      {/if}

      <ReviewHeldReplies {review} replies={heldReplies} />

      <ReviewThreadFooter
        {thread}
        body={review.replyBodyFor(thread.id)}
        error={review.replyErrorFor(thread.id)}
        sending={review.sendingReply(thread.id)}
        {replying}
        resolving={review.resolvingThread(thread.id)}
        resolveError={review.resolveErrorFor(thread.id)}
        onOpenReply={openReply}
        onCloseReply={() => review.closeReply(thread.id, 'conversation')}
        onBodyChange={(body) => review.setReplyBody(thread.id, body)}
        onSendReply={() => review.sendPRThreadReply(thread)}
        hold={review.canHoldReply ? { primary: review.primaryCommentAction, onHold: () => review.holdPRThreadReply(thread) } : null}
        nowWithheld={review.nowWithheldReason}
        onResolve={thread.isResolvable
          ? (resolved) => { void review.setPRThreadResolved(thread, resolved); }
          : undefined}
      />
    </div>
  </div>
</article>
