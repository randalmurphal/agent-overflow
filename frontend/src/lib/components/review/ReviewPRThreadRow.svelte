<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import type { ReviewThread } from '../../types/models';
  import type { CommentAnchor } from '../../stores/reviewPane.svelte';

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
    onSendToAgent: () => Promise<void> | void;
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

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      replying = false;
      return;
    }
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void onSendReply();
    }
  }
</script>

<article class="border-y border-border-subtle bg-surface-0 px-3 py-2 text-xs" data-testid="review-pr-thread">
  <div class="flex items-center gap-2">
    <button type="button" class="min-w-0 flex-1 truncate text-left font-mono text-[0.6875rem] text-fg-muted" onclick={onToggle}>
      {location}
      {#if thread.isResolved}<span class="ml-1 rounded border border-border-subtle px-1">resolved</span>{/if}
      {#if thread.isOutdated || orphaned}<span class="ml-1 rounded border border-border-subtle px-1">outdated</span>{/if}
      {#if collapsed}<span class="ml-2 normal-case">{summary}</span>{/if}
    </button>
    <button type="button" class="rounded px-1.5 py-0.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2" onclick={() => { replying = !replying; }}>
      Reply
    </button>
    <button
      type="button"
      class="rounded px-1.5 py-0.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 disabled:opacity-45"
      disabled={isTurnActive}
      title={isTurnActive ? 'Agent turn is active' : 'Send to agent'}
      onclick={() => { void onSendToAgent(); }}
    >
      Send to agent
    </button>
  </div>

  {#if !collapsed}
    <div class="mt-2 space-y-2">
      {#each thread.comments as comment (`${comment.databaseID}:${comment.createdAt}`)}
        <div class="rounded border border-border-subtle bg-surface-1 px-2 py-1.5">
          <div class="mb-1 text-[0.6875rem] text-fg-muted">{comment.authorLogin} · {comment.createdAt}</div>
          <ChatMarkdown source={comment.body} pathRefs={[]} />
        </div>
      {/each}
    </div>
  {/if}

  {#if replying}
    <textarea
      class="mt-2 w-full resize-none rounded border border-border-subtle bg-surface-1 px-2 py-1.5 text-xs text-fg"
      rows="3"
      value={body}
      oninput={(event) => onBodyChange(event.currentTarget.value)}
      onkeydown={onKeydown}
    ></textarea>
    {#if error}<div class="mt-1 text-[0.6875rem] text-error">{error}</div>{/if}
    <div class="mt-2 flex justify-end">
      <button
        type="button"
        class="rounded bg-accent px-2 py-1 text-[0.6875rem] font-medium text-accent-contrast disabled:opacity-45"
        disabled={sending || body.trim() === ''}
        onclick={() => { void onSendReply(); }}
      >
        Reply
      </button>
    </div>
  {/if}
</article>
