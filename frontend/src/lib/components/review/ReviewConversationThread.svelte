<script lang="ts">
  import Bot from '@lucide/svelte/icons/bot';
  import Check from '@lucide/svelte/icons/check';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import Reply from '@lucide/svelte/icons/reply';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewThreadComments from './ReviewThreadComments.svelte';
  import ReviewThreadContext from './ReviewThreadContext.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { visibleBody } from '../../utils/reviewComments';
  import { authorDisplayName, isBotLogin } from '../../utils/reviewIdentity';
  import {
    THREAD_CARD_CLASS,
    THREAD_CHIP_CLASS,
    THREAD_HEAD_CLASS,
    reviewThreadState,
    threadBodyClass,
  } from '../../utils/reviewThreadStyle';
  import { relativeTime } from '../../utils/format';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { ReviewThread } from '../../types/models';

  // One thread's card in the Conversation feed: the author's avatar on
  // the rail, a bubble beside it. The header says who, where (the file
  // and line, a jump into the diff when that row exists), when, and the
  // thread's state; the state also reads from the edge, so a scan down
  // the feed tells unresolved (warning edge) from resolved (success
  // edge) from outdated (dashed) without reading a word. The first
  // comment always renders IN FULL; only the REPLIES fold, and only on
  // settled threads. Anchored threads fold the code they were written
  // against under the header.

  interface Props {
    review: ReviewPaneState;
    thread: ReviewThread;
    canSendToAgent: boolean;
    /** The thread's file is in the rendered diff (the jump has a target). */
    inDiff: boolean;
  }

  let { review, thread, canSendToAgent, inDiff }: Props = $props();

  // The composer's text is store-backed; only the open/closed flag is
  // local, seeded open when drafted text survives a row unmount.
  // svelte-ignore state_referenced_locally
  let replying = $state(review.replyBodyFor(thread.id) !== '');

  const repliesOpen = $derived(review.conversationThreadExpanded(thread.id));
  const threadState = $derived(reviewThreadState(thread));
  const unresolved = $derived(threadState === 'unresolved');
  const settled = $derived(threadState === 'resolved' || threadState === 'outdated');
  const first = $derived(thread.comments[0]);
  const firstBody = $derived(visibleBody(first?.body ?? ''));
  const replyCount = $derived(Math.max(0, thread.comments.length - 1));
  const firstTime = $derived.by(() => {
    const ms = Date.parse(first?.createdAt ?? '');
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  });
  const location = $derived(
    thread.path === '' ? '' : thread.line ? `${thread.path}:${thread.line}` : thread.path,
  );
  // The file bar above the diff already carries directories; the card
  // names the file, and the full path is the tooltip.
  const shortLocation = $derived.by(() => {
    const slash = location.lastIndexOf('/');
    return slash < 0 ? location : location.slice(slash + 1);
  });
  const contextFile = $derived(
    inDiff && thread.line ? (review.files.find((file) => file.path === thread.path) ?? null) : null,
  );
  const resolveError = $derived(review.resolveErrorFor(thread.id));
  const isCurrent = $derived(review.unresolvedCursor === thread.id && unresolved);


  function openReply(): void {
    replying = !replying;
    // Replying into a folded thread: unfold so the reply lands in view.
    if (replying && replyCount > 0 && !repliesOpen) review.toggleConversationThread(thread.id);
  }
</script>

<article
  class="grid grid-cols-[28px_minmax(0,1fr)] gap-x-3"
  data-testid="review-conversation-thread"
  data-thread-id={thread.id}
  data-state={threadState}
>
  <div class="relative z-10 rounded-full ring-[3px] ring-surface-0/70">
    <ReviewAvatar login={first?.authorLogin ?? ''} name={first?.authorName} size={28} />
  </div>
  <div
    class="min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 transition-shadow {THREAD_CARD_CLASS[threadState]} {isCurrent ? 'shadow-[0_0_0_2px_var(--color-accent)]' : ''}"
  >
    <div class="flex min-w-0 items-center gap-1.5 py-1.5 pl-3 pr-1.5 text-[0.75rem] {THREAD_HEAD_CLASS[threadState]} {firstBody === '' && !contextFile ? '' : 'border-b border-border-subtle'}">
      <span class="shrink-0 font-semibold text-fg" title={first ? `@${first.authorLogin}` : ''}>{first ? authorDisplayName(first) : ''}</span>
      {#if first && isBotLogin(first.authorLogin)}
        <span class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1 text-[0.625rem] leading-4 text-fg-muted">bot</span>
      {/if}
      {#if first?.authorName}
        <span class="min-w-0 shrink truncate text-fg-subtle" title="@{first.authorLogin}">@{first.authorLogin}</span>
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
        <span class="shrink-0 text-fg-subtle">· {firstTime}</span>
      {/if}
      <span class="min-w-0 flex-1"></span>
      {#if threadState !== 'none'}
        <span class="shrink-0 rounded-full px-1.5 py-px text-[0.625rem] {THREAD_CHIP_CLASS[threadState]}">{threadState}</span>
      {/if}
      <span class="flex shrink-0 items-center">
        <ReviewIconButton
          icon={Reply}
          label={replying ? 'Hide reply box' : 'Reply'}
          onclick={openReply}
        />
        {#if thread.isResolvable && !thread.isOutdated}
          <ReviewIconButton
            icon={thread.isResolved ? RotateCcw : Check}
            label={thread.isResolved ? 'Unresolve thread' : 'Resolve thread'}
            spinning={review.resolvingThread(thread.id)}
            disabled={review.resolvingThread(thread.id)}
            testid="review-conversation-resolve"
            onclick={() => { void review.setPRThreadResolved(thread, !thread.isResolved); }}
          />
        {/if}
        {#if canSendToAgent}
          <ReviewIconButton
            icon={Bot}
            label="Send to agent"
            disabled={review.isTurnActive}
            disabledLabel="Agent turn is active"
            onclick={() => { void review.sendPRThreadToAgent(thread); }}
          />
        {/if}
      </span>
    </div>

    {#if contextFile}
      <ReviewThreadContext file={contextFile} {thread} />
    {/if}

    <div class={threadBodyClass(threadState)}>
      {#if firstBody !== ''}
        <div class="px-3.5 py-3">
          <ChatMarkdown source={firstBody} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
        </div>
      {/if}

      {#if resolveError}
        <div class="px-3.5 pb-2 text-[0.6875rem] text-error">{resolveError}</div>
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

      {#if (replyCount > 0 && (repliesOpen || !settled)) || replying}
        <ReviewThreadComments
          {thread}
          skipFirst
          showComments={replyCount > 0 && (repliesOpen || !settled)}
          body={review.replyBodyFor(thread.id)}
          error={review.replyErrorFor(thread.id)}
          sending={review.sendingReply(thread.id)}
          {replying}
          onBodyChange={(body) => review.setReplyBody(thread.id, body)}
          onSendReply={() => review.sendPRThreadReply(thread)}
          onCloseReply={() => { replying = false; }}
        />
      {/if}
    </div>
  </div>
</article>
