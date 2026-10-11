<script lang="ts">
  import { onDestroy } from 'svelte';
  import Check from '@lucide/svelte/icons/check';
  import Copy from '@lucide/svelte/icons/copy';
  import LoaderCircle from '@lucide/svelte/icons/loader-circle';
  import Icon from '../primitives/Icon.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import { threadClipboardText } from '../../utils/reviewComments';
  import type { ReviewThread } from '../../types/models';

  // A PR thread's actions, at its foot the way GitHub and GitLab place
  // them: a reply field that opens into the composer, Copy (the thread as
  // text, for pasting into an agent) and Resolve / Unresolve. Shared by
  // the inline diff strip (ReviewPRThreadRow) and the conversation card
  // (ReviewConversationThread) so the two surfaces cannot drift. The
  // composer's text is store-backed; the open flag belongs to the host.

  interface Props {
    thread: ReviewThread;
    /** Store-backed composer text, survives virtualizer row unmounts. */
    body: string;
    error: string | null;
    sending: boolean;
    replying: boolean;
    resolving: boolean;
    resolveError: string | null;
    onOpenReply: () => void;
    onCloseReply: () => void;
    onBodyChange: (body: string) => void;
    onSendReply: () => Promise<void> | void;
    /** Absent for outdated and non-resolvable threads: no resolve
     *  control renders. */
    onResolve?: (resolved: boolean) => void;
  }

  let {
    thread,
    body,
    error,
    sending,
    replying,
    resolving,
    resolveError,
    onOpenReply,
    onCloseReply,
    onBodyChange,
    onSendReply,
    onResolve,
  }: Props = $props();

  let textareaEl: HTMLTextAreaElement | undefined = $state();
  let copied = $state(false);
  let copiedTimer: ReturnType<typeof setTimeout> | undefined;

  // The composer opens from the reply field: put the caret there.
  $effect(() => {
    if (replying) textareaEl?.focus();
  });

  onDestroy(() => {
    if (copiedTimer) clearTimeout(copiedTimer);
  });

  async function copyThread(): Promise<void> {
    if (!(await copyToClipboard(threadClipboardText(thread)))) {
      addToast('error', 'Failed to copy');
      return;
    }
    copied = true;
    if (copiedTimer) clearTimeout(copiedTimer);
    copiedTimer = setTimeout(() => {
      copied = false;
      copiedTimer = undefined;
    }, 2000);
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      onCloseReply();
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

{#snippet actions()}
  <ReviewIconButton
    icon={copied ? Check : Copy}
    label={copied ? 'Copied' : 'Copy thread'}
    testid="review-thread-copy"
    onclick={() => { void copyThread(); }}
  />
  {#if onResolve}
    {@const resolve = onResolve}
    <button
      type="button"
      class="inline-flex h-6 items-center gap-1 rounded-[var(--radius-control)] border border-border-subtle px-2 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg disabled:opacity-45 disabled:hover:bg-transparent disabled:hover:text-fg-muted"
      disabled={resolving}
      data-testid="review-thread-resolve"
      onclick={() => resolve(!thread.isResolved)}
    >
      {#if resolving}<Icon icon={LoaderCircle} size={11} class="animate-spin" />{/if}
      {thread.isResolved ? 'Unresolve' : 'Resolve'}
    </button>
  {/if}
{/snippet}

<div class="border-t border-border-subtle px-3.5 py-2.5" data-testid="review-thread-footer">
  {#if replying}
    <div data-testid="review-thread-composer">
      <textarea
        bind:this={textareaEl}
        class="w-full resize-none rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-2 text-xs text-fg placeholder:text-fg-subtle focus:border-accent/60 focus:outline-none"
        rows="3"
        placeholder="Reply… (Ctrl+Enter to send)"
        value={body}
        oninput={(event) => onBodyChange(event.currentTarget.value)}
        onkeydown={onKeydown}
      ></textarea>
      {#if error}<div class="mt-1 text-[0.6875rem] text-error">{error}</div>{/if}
      <div class="mt-2 flex items-center gap-1.5">
        {@render actions()}
        <span class="min-w-0 flex-1"></span>
        <button
          type="button"
          class="h-6 rounded-[var(--radius-control)] px-2 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg"
          onclick={onCloseReply}
        >
          Cancel
        </button>
        <button
          type="button"
          class="h-6 rounded-[var(--radius-control)] bg-accent px-2.5 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45"
          disabled={sending || body.trim() === ''}
          onclick={() => { void onSendReply(); }}
        >
          Reply
        </button>
      </div>
    </div>
  {:else}
    <div class="flex items-center gap-1.5">
      <button
        type="button"
        class="h-7 min-w-0 flex-1 truncate rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 text-left text-xs text-fg-subtle hover:border-border-strong hover:text-fg-muted"
        aria-label="Reply"
        data-testid="review-thread-reply"
        onclick={onOpenReply}
      >
        Reply…
      </button>
      {@render actions()}
    </div>
  {/if}
  {#if resolveError}
    <div class="mt-1.5 text-[0.6875rem] text-error">{resolveError}</div>
  {/if}
</div>
