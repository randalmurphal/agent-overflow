<script lang="ts">
  import Bot from '@lucide/svelte/icons/bot';
  import Check from '@lucide/svelte/icons/check';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import Reply from '@lucide/svelte/icons/reply';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewThreadComments from './ReviewThreadComments.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { commentSnippet, visibleBody } from '../../utils/reviewComments';
  import { authorDisplayName, isBotLogin } from '../../utils/reviewIdentity';
  import {
    THREAD_CARD_CLASS,
    THREAD_CHIP_CLASS,
    THREAD_HEAD_CLASS,
    reviewThreadState,
    threadBodyClass,
  } from '../../utils/reviewThreadStyle';
  import { relativeTime } from '../../utils/format';
  import type { ReviewThread } from '../../types/models';
  import type { CommentAnchor } from '../../stores/reviewPane.svelte';

  // A PR review thread on the diff surface: the same card as the
  // Conversation feed (avatar, author, state edge and chip, actions), so
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
    error: string | null;
    sending: boolean;
    isTurnActive: boolean;
    resolving: boolean;
    resolveError: string | null;
    onToggle: () => void;
    onBodyChange: (body: string) => void;
    onSendReply: () => Promise<void> | void;
    /** Absent when the pane has no thread to steer (a draft placeholder
     *  reviewing its workspace's PR): the button is not rendered rather
     *  than rendered and inert. */
    onSendToAgent?: () => Promise<void> | void;
    /** Absent for non-resolvable threads: no resolve control renders. */
    onResolve?: (resolved: boolean) => void;
    /** Opens the overview's Conversation section at this thread; absent
     *  when that section does not exist (no PR overview on screen). */
    onJumpToConversation?: () => void;
  }

  let {
    thread,
    anchor,
    collapsed,
    orphaned,
    body,
    error,
    sending,
    isTurnActive,
    resolving,
    resolveError,
    onToggle,
    onBodyChange,
    onSendReply,
    onSendToAgent,
    onResolve,
    onJumpToConversation,
  }: Props = $props();
  // Rows are virtualized: a windowing remount must not collapse a composer
  // that still holds drafted text (the text itself is store-backed). Only
  // the mount-time value matters, hence the deliberate initial-value read.
  // svelte-ignore state_referenced_locally
  let replying = $state(body !== '');

  const threadState = $derived(reviewThreadState(thread, orphaned));
  const first = $derived(thread.comments[0]);
  const firstBody = $derived(visibleBody(first?.body ?? ''));
  const summary = $derived(commentSnippet(first?.body ?? ''));
  const replyCount = $derived(Math.max(0, thread.comments.length - 1));
  const firstTime = $derived.by(() => {
    const ms = Date.parse(first?.createdAt ?? '');
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  });
  const location = $derived(anchor.side === 'file'
    ? anchor.filePath
    : `${anchor.filePath}:${anchor.newLine || anchor.oldLine || ''}`);
</script>

<article
  class="mx-2 my-1.5 grid grid-cols-[20px_minmax(0,1fr)] gap-x-2 text-xs"
  data-testid="review-pr-thread"
  data-state={threadState}
>
  <div class="pt-1">
    <ReviewAvatar login={first?.authorLogin ?? ''} name={first?.authorName} size={20} />
  </div>
  <div class="min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 {THREAD_CARD_CLASS[threadState]}">
    <div class="flex min-w-0 items-center gap-1.5 py-1 pl-2.5 pr-1 text-[0.75rem] {THREAD_HEAD_CLASS[threadState]} {collapsed || firstBody === '' ? '' : 'border-b border-border-subtle'}">
      <button
        type="button"
        class="flex min-w-0 flex-1 items-center gap-1.5 overflow-hidden text-left"
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
          <span class="min-w-0 flex-1 basis-0"></span>
        {/if}
      </button>
      <span class="flex shrink-0 items-center">
        <ReviewIconButton
          icon={Reply}
          label={replying ? 'Hide reply box' : 'Reply'}
          testid="review-pr-thread-reply"
          onclick={() => { replying = !replying; }}
        />
        {#if onResolve}
          {@const resolve = onResolve}
          <ReviewIconButton
            icon={thread.isResolved ? RotateCcw : Check}
            label={thread.isResolved ? 'Unresolve thread' : 'Resolve thread'}
            spinning={resolving}
            disabled={resolving}
            testid="review-pr-thread-resolve"
            onclick={() => resolve(!thread.isResolved)}
          />
        {/if}
        {#if onSendToAgent}
          {@const sendToAgent = onSendToAgent}
          <ReviewIconButton
            icon={Bot}
            label="Send to agent"
            disabled={isTurnActive}
            disabledLabel="Agent turn is active"
            testid="review-pr-thread-send-agent"
            onclick={() => { void sendToAgent(); }}
          />
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
      </span>
    </div>

    <div class={threadBodyClass(threadState)}>
      {#if !collapsed && firstBody !== ''}
        <div class="px-3 py-2.5">
          <ChatMarkdown source={firstBody} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
        </div>
      {/if}

      {#if resolveError}
        <div class="px-3 pb-2 text-[0.6875rem] text-error">{resolveError}</div>
      {/if}

      {#if (!collapsed && replyCount > 0) || replying}
        <ReviewThreadComments
          {thread}
          skipFirst
          avatarSize={18}
          showComments={!collapsed && replyCount > 0}
          {body}
          {error}
          {sending}
          {replying}
          {onBodyChange}
          {onSendReply}
          onCloseReply={() => { replying = false; }}
        />
      {/if}
    </div>
  </div>
</article>
