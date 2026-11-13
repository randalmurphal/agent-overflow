<script lang="ts">
  import { onDestroy } from 'svelte';
  import Check from '@lucide/svelte/icons/check';
  import Copy from '@lucide/svelte/icons/copy';
  import LoaderCircle from '@lucide/svelte/icons/loader-circle';
  import Icon from '../primitives/Icon.svelte';
  import ReviewCommentEditorShell, { type EditorAction } from './ReviewCommentEditorShell.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import { threadClipboardText } from '../../utils/reviewComments';
  import type { ReviewThread } from '../../types/models';

  // A PR thread's actions, at its foot the way GitHub and GitLab place
  // them: a reply field that opens into the composer, Copy (the thread as
  // text, for pasting into an agent) and Resolve / Unresolve. Shared by
  // the inline diff strip (ReviewPRThreadRow) and the conversation card
  // (ReviewConversationThread) so the two surfaces cannot drift. The
  // composer's text is store-backed; the open flag belongs to the host.
  // Where a reply can be held for the batch the composer offers Reply now
  // and Add draft, the primary as the review state says (`hold`);
  // elsewhere one Reply that posts now. A reply posted now keeps the
  // composer open, pending, until the forge's answer shows it.

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
    /** Holding a reply as a draft: which action is primary and how. Absent
     * where a reply only posts now. */
    hold?: { primary: 'now' | 'draft'; onHold: () => Promise<void> | void } | null;
    /** Why posting now is withheld, or null. */
    nowWithheld?: string | null;
    /** Absent for non-resolvable threads: no resolve control renders. */
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
    hold = null,
    nowWithheld = null,
    onResolve,
  }: Props = $props();

  let copied = $state(false);
  let copiedTimer: ReturnType<typeof setTimeout> | undefined;


  const empty = $derived(body.trim() === '');
  const nowAction = $derived<EditorAction>({
    label: hold ? 'Reply now' : 'Reply',
    run: onSendReply,
    disabled: empty || nowWithheld !== null,
    title: nowWithheld ?? undefined,
    testid: 'review-thread-reply-now',
  });
  const draftAction = $derived<EditorAction | null>(hold ? { label: 'Add draft', run: hold.onHold, disabled: empty, testid: 'review-thread-reply-draft' } : null);
  const primaryAction = $derived(hold?.primary === 'draft' && draftAction ? draftAction : nowAction);
  const secondaryAction = $derived(hold ? (primaryAction === nowAction ? draftAction : nowAction) : null);

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
    <ReviewCommentEditorShell
      testid="review-thread-composer"
      {body}
      {onBodyChange}
      placeholder="Reply…"
      primary={primaryAction}
      secondary={secondaryAction}
      onCancel={onCloseReply}
      pending={sending}
      {error}
      takeFocus={() => true}
      leading={actions}
    />
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
