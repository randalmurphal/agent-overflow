<script lang="ts">
  import Bot from '@lucide/svelte/icons/bot';
  import Check from '@lucide/svelte/icons/check';
  import FileDiff from '@lucide/svelte/icons/file-diff';
  import Reply from '@lucide/svelte/icons/reply';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import Icon from '../primitives/Icon.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewThreadComments from './ReviewThreadComments.svelte';
  import { commentSnippet } from '../../utils/reviewComments';
  import { relativeTime } from '../../utils/format';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { ReviewThread } from '../../types/models';

  // One thread's card in the PR header's Conversation section. Unresolved
  // threads open with a warning edge; resolved/outdated collapse to one
  // line. Anchored threads carry a file:line strip that jumps the diff
  // body to their row — rendered as a button only when that row exists.

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

  const expanded = $derived(review.conversationThreadExpanded(thread.id));
  const unresolved = $derived(thread.isResolvable && !thread.isResolved && !thread.isOutdated);
  const first = $derived(thread.comments[0]);
  const firstTime = $derived.by(() => {
    const ms = Date.parse(first?.createdAt ?? '');
    return Number.isNaN(ms) ? '' : relativeTime(ms);
  });
  const location = $derived(
    thread.path === '' ? '' : thread.line ? `${thread.path}:${thread.line}` : thread.path,
  );
  const resolveError = $derived(review.resolveErrorFor(thread.id));
</script>

<article
  class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/50 {unresolved ? 'border-l-2 border-l-warning' : ''}"
  data-testid="review-conversation-thread"
  data-thread-id={thread.id}
>
  <div class="flex items-center gap-1.5 pl-2.5 pr-1.5 py-1.5">
    <button
      type="button"
      class="flex min-w-0 flex-1 items-center gap-1.5 overflow-hidden text-left"
      aria-expanded={expanded}
      onclick={() => review.toggleConversationThread(thread.id)}
    >
      {#if unresolved}
        <span class="shrink-0 rounded-full bg-warning/12 px-1.5 py-px text-[0.625rem] text-warning">unresolved</span>
      {:else if thread.isOutdated}
        <span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] text-fg-muted">outdated</span>
      {:else if thread.isResolvable && thread.isResolved}
        <span class="shrink-0 rounded-full bg-success/12 px-1.5 py-px text-[0.625rem] text-success">resolved</span>
      {/if}
      <span class="shrink-0 text-[0.6875rem] font-medium text-fg">{first?.authorLogin ?? ''}</span>
      {#if firstTime}
        <span class="shrink-0 text-[0.625rem] text-fg-subtle">{firstTime}</span>
      {/if}
      {#if thread.comments.length > 1}
        <span class="shrink-0 text-[0.625rem] tabular-nums text-fg-subtle">+{thread.comments.length - 1}</span>
      {/if}
      {#if !expanded}
        <span class="min-w-0 flex-1 truncate text-[0.6875rem] text-fg-subtle">{commentSnippet(first?.body ?? '')}</span>
      {/if}
    </button>
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
    {#if expanded}
      <ReviewIconButton
        icon={Reply}
        label={replying ? 'Hide reply box' : 'Reply'}
        onclick={() => { replying = !replying; }}
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
    {/if}
  </div>

  {#if resolveError}
    <div class="px-2.5 pb-1.5 text-[0.6875rem] text-error">{resolveError}</div>
  {/if}

  {#if expanded}
    <div class="px-2.5 pb-2.5">
      <ReviewThreadComments
        {thread}
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
