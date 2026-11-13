<script lang="ts">
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewHeldReplies from './ReviewHeldReplies.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewSentCommentActions from './ReviewSentCommentActions.svelte';
  import ReviewSentCommentBody from './ReviewSentCommentBody.svelte';
  import ReviewThreadComments from './ReviewThreadComments.svelte';
  import ReviewThreadFooter from './ReviewThreadFooter.svelte';
  import { commentSnippet, visibleBody } from '../../utils/reviewComments';
  import { rangeLabel, threadRange } from '../../utils/reviewRange';
  import { authorDisplayName, isBotLogin } from '../../utils/reviewIdentity';
  import {
    THREAD_CARD_CLASS,
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
  import { findThreadCommentKey, reviewCommentKey } from '../../utils/reviewFindScan';
  import type { ReviewThread } from '../../types/models';
  import type { CommentAnchor, ReviewPaneState } from '../../stores/reviewPane.svelte';

  // A PR review thread on the diff surface: the same card as the
  // Conversation feed (avatar, author, state edge and chip, the actions at
  // the foot), so
  // a thread looks like itself wherever it is read. Collapsed, the
  // header carries the first comment's lead sentence and the reply
  // count; expanded, the body is the full first comment and its
  // replies. Unresolved threads render expanded (reviewRows collapses
  // only settled ones). The line is in the gutter directly above, so the
  // header names no location; the tooltip does.

  interface Props {
    thread: ReviewThread;
    anchor: CommentAnchor;
    collapsed: boolean;
    orphaned: boolean;
    body: string;
    /** Whether the reply composer is open; the owner keeps it, the row
     * is virtualized. */
    replying: boolean;
    error: string | null;
    sending: boolean;
    resolving: boolean;
    resolveError: string | null;
    onToggle: () => void;
    onBodyChange: (body: string) => void;
    onOpenReply: () => void;
    /** Closes the composer; the review state keeps its text. */
    onCloseReply: () => void;
    onSendReply: () => Promise<void> | void;
    /** Absent for non-resolvable threads: no resolve control renders. */
    onResolve?: (resolved: boolean) => void;
    /** Opens the overview's Conversation section at this thread; absent
     *  when that section does not exist (no PR overview on screen). */
    onJumpToConversation?: () => void;
    /** The jump that landed here (utils/reviewJumpPulse); 0 for none. */
    pulse?: number;
    /** The review whose comment actions and held replies the card shows. */
    review?: ReviewPaneState | null;
  }

  let {
    thread,
    anchor,
    collapsed,
    orphaned,
    body,
    replying,
    error,
    sending,
    resolving,
    resolveError,
    onToggle,
    onBodyChange,
    onOpenReply,
    onCloseReply,
    onSendReply,
    onResolve,
    onJumpToConversation,
    pulse = 0,
    review = null,
  }: Props = $props();


  const threadState = $derived(reviewThreadState(thread));
  const outdated = $derived(reviewThreadOutdated(thread, orphaned));
  const first = $derived(thread.comments[0]);
  const range = $derived(threadRange(thread));
  const heldReplies = $derived(review?.replyDraftsFor(thread.id) ?? []);
  const editingFirst = $derived(first !== undefined && (review?.sentComments.editBodyFor(thread, first) ?? null) !== null);
  const firstBody = $derived(visibleBody(first?.body ?? ''));
  const summary = $derived(commentSnippet(first?.body ?? ''));
  const replyCount = $derived(Math.max(0, thread.comments.length - 1));
  const firstTime = $derived.by(() => {
    const ms = Date.parse(first?.createdAt ?? '');
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  });
  // An orphaned thread names where it was written when the forge still
  // knows (GitHub reports the original line once the thread is outdated).
  const location = $derived.by(() => {
    const line = anchor.side === 'file' ? thread.originalLine : (anchor.newLine || anchor.oldLine);
    return line ? `${anchor.filePath}:${line}` : anchor.filePath;
  });
</script>

<article
  class="mx-2 my-1.5 grid grid-cols-[20px_minmax(0,1fr)] gap-x-2 text-xs"
  data-testid="review-pr-thread"
  data-state={threadState}
  data-outdated={outdated ? 'true' : undefined}
>
  <div class="pt-1">
    <ReviewAvatar login={first?.authorLogin ?? ''} name={first?.authorName} size={20} />
  </div>
  <div class="min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 {THREAD_CARD_CLASS[threadState]} {outdated ? THREAD_OUTDATED_CARD_CLASS : ''}" {@attach jumpPulse(pulse)}>
    <div class="group/comment flex min-w-0 items-center gap-1.5 py-1 pl-2.5 {onJumpToConversation ? 'pr-1' : 'pr-2.5'} text-[0.75rem] {THREAD_HEAD_CLASS[threadState]} {collapsed || (firstBody === '' && !editingFirst) ? '' : 'border-b border-border-subtle'}">
      <button
        type="button"
        class="flex min-w-0 flex-1 flex-wrap items-center gap-x-1.5 gap-y-1 overflow-hidden text-left"
        aria-expanded={!collapsed}
        title={location}
        onclick={onToggle}
      >
        <span class="shrink-0 font-semibold text-fg">{first ? authorDisplayName(first) : ''}</span>
        {#if first && isBotLogin(first.authorLogin)}
          <span class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1 text-[0.625rem] leading-4 text-fg-muted">bot</span>
        {/if}
        {#if threadState !== 'none'}
          <span class="shrink-0 rounded-full px-1.5 py-px text-[0.625rem] {THREAD_CHIP_CLASS[threadState]}">{threadState}</span>
        {/if}
        {#if outdated}
          <span class="shrink-0 rounded-full px-1.5 py-px text-[0.625rem] {THREAD_OUTDATED_CHIP_CLASS}" title={outdatedTitle(thread)} data-testid="review-thread-outdated">outdated</span>
        {/if}
        <!-- Basis-0 so the summary only takes leftover width: with basis
             auto its long text would absorb the row and crush the chips. -->
        {#if collapsed}
          <span class="min-w-0 flex-1 basis-0 truncate text-fg-muted">{summary}</span>
          {#if replyCount > 0}
            <span class="shrink-0 text-fg-subtle">{replyCount} {replyCount === 1 ? 'reply' : 'replies'}</span>
          {/if}
        {:else}
          {#if first?.authorName}
            <span class="min-w-0 shrink truncate text-fg-subtle">@{first.authorLogin}</span>
          {/if}
          {#if firstTime}
            <span class="shrink-0 text-fg-subtle">· {firstTime}</span>
          {/if}
          {#if first?.editedAt}
            <span class="shrink-0 text-fg-subtle" data-testid="review-comment-edited">edited</span>
          {/if}
          {#if range}
            <span class="shrink-0 text-fg-subtle" data-testid="review-thread-range">{rangeLabel(range)}</span>
          {/if}
          <span class="min-w-0 flex-1 basis-0"></span>
        {/if}
      </button>
      {#if !collapsed && first && review}
        <ReviewSentCommentActions {review} {thread} comment={first} />
      {/if}
      {#if onJumpToConversation}
        {@const jump = onJumpToConversation}
        <ReviewIconButton
          icon={MessagesSquare}
          label="Open in conversation"
          testid="review-pr-thread-jump-conversation"
          onclick={() => jump()}
        />
      {/if}
    </div>

    <div class={threadBodyClass(threadState)}>
      {#if !collapsed && first && (firstBody !== '' || editingFirst)}
        <ReviewSentCommentBody {review} {thread} comment={first} class="px-3 py-2.5" findKey={findThreadCommentKey(thread.id, reviewCommentKey(first))} />
      {/if}

      {#if !collapsed && replyCount > 0}
        <ReviewThreadComments {thread} {review} skipFirst avatarSize={18} findKeys />
      {/if}

      {#if !collapsed && review}
        <ReviewHeldReplies {review} replies={heldReplies} />
      {/if}

      {#if !collapsed || replying}
        <ReviewThreadFooter
          {thread}
          {body}
          {error}
          {sending}
          {replying}
          {resolving}
          {resolveError}
          {onOpenReply}
          {onCloseReply}
          {onBodyChange}
          {onSendReply}
          {onResolve}
          hold={review?.canHoldReply ? { primary: review.primaryCommentAction, onHold: () => review?.holdPRThreadReply(thread) } : null}
          nowWithheld={review?.nowWithheldReason ?? null}
        />
      {/if}
    </div>
  </div>
</article>
