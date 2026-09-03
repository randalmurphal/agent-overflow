<script lang="ts">
  import Bot from '@lucide/svelte/icons/bot';
  import Check from '@lucide/svelte/icons/check';
  import MessagesSquare from '@lucide/svelte/icons/messages-square';
  import Reply from '@lucide/svelte/icons/reply';
  import RotateCcw from '@lucide/svelte/icons/rotate-ccw';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import ReviewThreadComments from './ReviewThreadComments.svelte';
  import { commentSnippet } from '../../utils/reviewComments';
  import type { ReviewThread } from '../../types/models';
  import type { CommentAnchor } from '../../stores/reviewPane.svelte';

  // A PR review thread's strip on the diff surface. Unresolved threads
  // carry a warning edge and render expanded (reviewRows collapses only
  // resolved/outdated ones); settled threads fold to one line.

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
    /** Opens the PR header's Conversation section at this thread; absent
     *  when that section does not exist (no PR header on screen). */
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

  const unresolved = $derived(thread.isResolvable && !thread.isResolved && !thread.isOutdated && !orphaned);
  const summary = $derived(commentSnippet(thread.comments[0]?.body ?? ''));
  const location = $derived(anchor.side === 'file'
    ? anchor.filePath
    : `${anchor.filePath}:${anchor.newLine || anchor.oldLine || ''}`);
  // The file header sits directly above this row, so the full path is
  // noise: show basename(:line), keep the full location as the tooltip.
  const shortLocation = $derived.by(() => {
    const slash = location.lastIndexOf('/');
    return slash < 0 ? location : location.slice(slash + 1);
  });
</script>

<article
  class="border-y border-border-subtle bg-surface-0/50 px-3 py-2 text-xs {unresolved ? 'border-l-2 border-l-warning' : ''}"
  data-testid="review-pr-thread"
>
  <div class="flex items-center gap-1.5">
    <button type="button" class="flex min-w-0 flex-1 items-center gap-1.5 overflow-hidden text-left" onclick={onToggle} title={location}>
      <span class="min-w-0 truncate font-mono text-[0.6875rem] text-fg-muted">{shortLocation}</span>
      {#if unresolved}<span class="shrink-0 rounded-full bg-warning/12 px-1.5 py-px text-[0.625rem] text-warning">unresolved</span>{/if}
      {#if thread.isResolved}<span class="shrink-0 rounded-full bg-success/12 px-1.5 py-px text-[0.625rem] text-success">resolved</span>{/if}
      {#if thread.isOutdated || orphaned}<span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] text-fg-muted">outdated</span>{/if}
      <!-- Basis-0 so the summary only takes leftover width: with basis
           auto its long text would absorb the row and crush the location
           span to an ellipsis even when the summary itself truncates. -->
      {#if collapsed}<span class="min-w-0 flex-1 truncate text-fg-subtle">{summary}</span>{/if}
    </button>
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
  </div>

  {#if resolveError}
    <div class="mt-1 text-[0.6875rem] text-error">{resolveError}</div>
  {/if}

  {#if !collapsed || replying}
    <div class="mt-2">
      <ReviewThreadComments
        {thread}
        {body}
        {error}
        {sending}
        {replying}
        showComments={!collapsed}
        {onBodyChange}
        {onSendReply}
        onCloseReply={() => { replying = false; }}
      />
    </div>
  {/if}
</article>
