<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { relativeTime } from '../../utils/format';
  import type { ReviewThread } from '../../types/models';
  import type { CommentAnchor } from '../../stores/reviewPane.svelte';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  interface Props {
    thread: ReviewThread;
    anchor: CommentAnchor;
    collapsed: boolean;
    orphaned: boolean;
    body: string;
    error: string | null;
    sending: boolean;
    isTurnActive: boolean;
    onToggle: () => void;
    onBodyChange: (body: string) => void;
    onSendReply: () => Promise<void> | void;
    /** Absent when the pane has no thread to steer (a draft placeholder
     *  reviewing its workspace's PR): the button is not rendered rather
     *  than rendered and inert. */
    onSendToAgent?: () => Promise<void> | void;
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
    onToggle,
    onBodyChange,
    onSendReply,
    onSendToAgent,
  }: Props = $props();
  // Rows are virtualized: a windowing remount must not collapse a composer
  // that still holds drafted text (the text itself is store-backed). Only
  // the mount-time value matters, hence the deliberate initial-value read.
  // svelte-ignore state_referenced_locally
  let replying = $state(body !== '');

  const summary = $derived((thread.comments[0]?.body ?? '').replace(/\s+/g, ' ').slice(0, 96));
  const location = $derived(anchor.side === 'file'
    ? anchor.filePath
    : `${anchor.filePath}:${anchor.newLine || anchor.oldLine || ''}`);
  // The file header sits directly above this row, so the full path is
  // noise: show basename(:line), keep the full location as the tooltip.
  const shortLocation = $derived.by(() => {
    const slash = location.lastIndexOf('/');
    return slash < 0 ? location : location.slice(slash + 1);
  });

  function commentTime(createdAt: string): string {
    const ms = Date.parse(createdAt);
    return Number.isNaN(ms) ? createdAt : relativeTime(ms);
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      replying = false;
      return;
    }
    // Mid-composition the reply text is still in the IME buffer, so the
    // submit chord would post a truncated comment.
    if (event.key === 'Enter' && isImeComposingEvent(event)) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void onSendReply();
    }
  }
</script>

<article class="border-y border-border-subtle bg-surface-0/50 px-3 py-2 text-xs" data-testid="review-pr-thread">
  <div class="flex items-center gap-2">
    <button type="button" class="flex min-w-0 flex-1 items-center gap-1.5 overflow-hidden text-left" onclick={onToggle} title={location}>
      <span class="min-w-0 truncate font-mono text-[0.6875rem] text-fg-muted">{shortLocation}</span>
      {#if thread.isResolved}<span class="shrink-0 rounded-full bg-success/12 px-1.5 py-px text-[0.625rem] text-success">resolved</span>{/if}
      {#if thread.isOutdated || orphaned}<span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] text-fg-muted">outdated</span>{/if}
      <!-- Basis-0 so the summary only takes leftover width: with basis
           auto its long text would absorb the row and crush the location
           span to an ellipsis even when the summary itself truncates. -->
      {#if collapsed}<span class="min-w-0 flex-1 truncate text-fg-subtle">{summary}</span>{/if}
    </button>
    <button
      type="button"
      class="shrink-0 rounded-[var(--radius-control)] border border-border-subtle px-2 py-0.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg"
      onclick={() => { replying = !replying; }}
    >
      Reply
    </button>
    {#if onSendToAgent}
      {@const sendToAgent = onSendToAgent}
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-control)] border border-border-subtle px-2 py-0.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg disabled:opacity-45 disabled:hover:bg-transparent disabled:hover:text-fg-muted"
        disabled={isTurnActive}
        title={isTurnActive ? 'Agent turn is active' : 'Send to agent'}
        onclick={() => { void sendToAgent(); }}
      >
        Send to agent
      </button>
    {/if}
  </div>

  {#if !collapsed}
    <div class="mt-2 space-y-2">
      {#each thread.comments as comment (`${comment.databaseID}:${comment.createdAt}`)}
        <div class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 px-2.5 py-2">
          <div class="mb-1 flex items-baseline gap-1.5 text-[0.6875rem]">
            <span class="font-medium text-fg">{comment.authorLogin}</span>
            <span class="text-fg-subtle">{commentTime(comment.createdAt)}</span>
          </div>
          <ChatMarkdown source={comment.body} pathRefs={EMPTY_PATH_REFS} />
        </div>
      {/each}
    </div>
  {/if}

  {#if replying}
    <textarea
      class="mt-2 w-full resize-none rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1.5 text-xs text-fg focus:border-accent/60 focus:outline-none"
      rows="3"
      value={body}
      oninput={(event) => onBodyChange(event.currentTarget.value)}
      onkeydown={onKeydown}
    ></textarea>
    {#if error}<div class="mt-1 text-[0.6875rem] text-error">{error}</div>{/if}
    <div class="mt-2 flex justify-end">
      <button
        type="button"
        class="rounded-[var(--radius-control)] bg-accent px-2 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45"
        disabled={sending || body.trim() === ''}
        onclick={() => { void onSendReply(); }}
      >
        Reply
      </button>
    </div>
  {/if}
</article>
