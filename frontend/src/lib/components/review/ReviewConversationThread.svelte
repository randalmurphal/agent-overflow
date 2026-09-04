<script lang="ts">
  import Bot from '@lucide/svelte/icons/bot';
  import Check from '@lucide/svelte/icons/check';
  import ChevronDown from '@lucide/svelte/icons/chevron-down';
  import ChevronRight from '@lucide/svelte/icons/chevron-right';
  import FileDiff from '@lucide/svelte/icons/file-diff';
  import Reply from '@lucide/svelte/icons/reply';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewThreadComments from './ReviewThreadComments.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { visibleBody } from '../../utils/reviewComments';
  import { relativeTime } from '../../utils/format';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { ReviewThread } from '../../types/models';

  // One thread's card in the Conversation feed. The first comment always
  // renders IN FULL — the card must answer "what is this about" with no
  // click. What folds is only the REPLIES, and only on settled threads
  // (resolved/outdated), behind an "N replies" toggle; unresolved threads
  // show theirs. Settled cards dim but stay readable. Anchored threads
  // carry a file:line strip that jumps the diff body to their row —
  // rendered as a button only when that row exists.

  interface Props {
    review: ReviewPaneState;
    thread: ReviewThread;
    canSendToAgent: boolean;
    /** The thread's file is in the rendered diff (the jump has a target). */
    inDiff: boolean;
  }

  let { review, thread, canSendToAgent, inDiff }: Props = $props();

  // The composer's text is store-backed; only the open/closed flag is
  // local, seeded open when drafted text survives a section close.
  // svelte-ignore state_referenced_locally
  let replying = $state(review.replyBodyFor(thread.id) !== '');

  const repliesOpen = $derived(review.conversationThreadExpanded(thread.id));
  const unresolved = $derived(thread.isResolvable && !thread.isResolved && !thread.isOutdated);
  const settled = $derived(thread.isResolvable && (thread.isResolved || thread.isOutdated));
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
  const resolveError = $derived(review.resolveErrorFor(thread.id));

  function openReply(): void {
    replying = !replying;
    // Replying into a folded thread: unfold so the reply lands in view.
    if (replying && replyCount > 0 && !repliesOpen) review.toggleConversationThread(thread.id);
  }
</script>

<article
  class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/50 {unresolved ? 'border-l-2 border-l-warning' : ''} {settled ? 'opacity-75' : ''}"
  data-testid="review-conversation-thread"
  data-thread-id={thread.id}
>
  <div class="flex items-center gap-1.5 pl-2.5 pr-1.5 pt-1.5 {firstBody === '' ? 'pb-1.5' : ''}">
    {#if unresolved}
      <span class="shrink-0 rounded-full bg-warning/12 px-1.5 py-px text-[0.625rem] text-warning">unresolved</span>
    {:else if thread.isOutdated}
      <span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] text-fg-muted">outdated</span>
    {:else if thread.isResolvable && thread.isResolved}
      <span class="shrink-0 rounded-full bg-success/12 px-1.5 py-px text-[0.625rem] text-success">resolved</span>
    {/if}
    <span
      class="max-w-36 truncate text-[0.6875rem] font-medium text-fg"
      title={first?.authorLogin ?? ''}
    >{first?.authorLogin ?? ''}</span>
    {#if firstTime}
      <span class="shrink-0 text-[0.625rem] text-fg-subtle">{firstTime}</span>
    {/if}
    <span class="min-w-0 flex-1"></span>
    {#if location !== ''}
      {#if inDiff}
        <button
          type="button"
          class="inline-flex max-w-[40%] min-w-0 shrink items-center gap-1 rounded-[var(--radius-control)] px-1.5 py-0.5 font-mono text-[0.625rem] text-fg-muted hover:bg-surface-2 hover:text-fg"
          title="Jump to {location} in the diff"
          data-testid="review-conversation-jump-diff"
          onclick={() => review.jumpToDiffThread(thread)}
        >
          <Icon icon={FileDiff} size={11} class="shrink-0" />
          <span class="min-w-0 truncate">{location}</span>
        </button>
      {:else}
        <span class="max-w-[40%] min-w-0 truncate font-mono text-[0.625rem] text-fg-subtle" title={location}>{location}</span>
      {/if}
    {/if}
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
  </div>

  {#if firstBody !== ''}
    <div class="px-2.5 pb-2 pt-1">
      <ChatMarkdown source={firstBody} pathRefs={EMPTY_PATH_REFS} embeddedHtml />
    </div>
  {/if}

  {#if resolveError}
    <div class="px-2.5 pb-1.5 text-[0.6875rem] text-error">{resolveError}</div>
  {/if}

  {#if replyCount > 0}
    <button
      type="button"
      class="flex w-full items-center gap-1 border-t border-border-subtle px-2.5 py-1 text-left text-[0.6875rem] text-fg-muted hover:text-fg"
      aria-expanded={repliesOpen}
      data-testid="review-conversation-replies"
      onclick={() => review.toggleConversationThread(thread.id)}
    >
      <Icon icon={repliesOpen ? ChevronDown : ChevronRight} size={11} class="shrink-0" />
      {replyCount} {replyCount === 1 ? 'reply' : 'replies'}
    </button>
  {/if}

  {#if (replyCount > 0 && repliesOpen) || replying}
    <div class="px-2.5 pb-2.5 {replyCount > 0 ? '' : 'pt-0.5'}">
      <ReviewThreadComments
        {thread}
        skipFirst
        showComments={replyCount > 0 && repliesOpen}
        body={review.replyBodyFor(thread.id)}
        error={review.replyErrorFor(thread.id)}
        sending={review.sendingReply(thread.id)}
        {replying}
        onBodyChange={(body) => review.setReplyBody(thread.id, body)}
        onSendReply={() => review.sendPRThreadReply(thread)}
        onCloseReply={() => { replying = false; }}
      />
    </div>
  {/if}
</article>
