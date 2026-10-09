<script lang="ts">
  import Pencil from '@lucide/svelte/icons/pencil';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import type { DiffReviewComment } from '../../types/models';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  // The reader's own draft comment on the diff surface: the same card
  // shape as a PR thread, with an accent edge in place of a state edge
  // (a draft has no forge state yet) and edit/delete in place of
  // reply/resolve.

  interface Props {
    comment: DiffReviewComment;
    orphaned?: boolean;
    onUpdate: (commentId: string, body: string) => Promise<void> | void;
    onDelete: (commentId: string) => Promise<void> | void;
  }

  let { comment, orphaned = false, onUpdate, onDelete }: Props = $props();
  let editing = $state(false);
  let editBody = $state('');
  let busy = $state(false);
  const canSave = $derived(editBody.trim().length > 0 && !busy);
  const quote = $derived(comment.selectedText.trim().replace(/\s+/g, ' '));

  function commentLocation(comment: DiffReviewComment): string {
    if (comment.side === 'file') return comment.filePath;
    const line = comment.side === 'old' ? comment.oldLine : (comment.newLine || comment.oldLine);
    return line ? `${comment.filePath}:${line}` : comment.filePath;
  }

  function startEdit(): void {
    editBody = comment.body;
    editing = true;
  }

  function cancelEdit(): void {
    editing = false;
    editBody = '';
  }

  async function saveEdit(): Promise<void> {
    if (!canSave) return;
    busy = true;
    try {
      await onUpdate(comment.id, editBody);
      editing = false;
      editBody = '';
    } catch {
      // The review-pane store exposes the user-facing error.
    } finally {
      busy = false;
    }
  }

  async function deleteComment(): Promise<void> {
    if (busy) return;
    busy = true;
    try {
      await onDelete(comment.id);
    } catch {
      // The review-pane store exposes the user-facing error.
    } finally {
      busy = false;
    }
  }

  function onEditKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      cancelEdit();
      return;
    }
    // Mid-composition the edited text is still in the IME buffer, so the
    // submit chord would save a truncated comment.
    if (event.key === 'Enter' && isImeComposingEvent(event)) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void saveEdit();
    }
  }
</script>

<article
  class="mx-2 my-1.5 grid grid-cols-[20px_minmax(0,1fr)] gap-x-2 text-[0.75rem]"
  data-testid="review-comment-thread"
>
  <div class="pt-1">
    <ReviewAvatar login="you" name="You" initials="Y" size={20} />
  </div>
  <div class="min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 {orphaned ? 'border-dashed border-border-strong' : 'border-accent/40 border-l-[3px] border-l-accent'}">
    <div class="flex min-w-0 items-center gap-1.5 py-1 pl-2.5 pr-1 {orphaned ? 'bg-surface-2/40' : 'bg-accent/6'}" title={commentLocation(comment)}>
      <span class="shrink-0 font-semibold text-fg">You</span>
      <span class="shrink-0 rounded-full bg-accent/12 px-1.5 py-px text-[0.625rem] text-accent">draft</span>
      {#if orphaned}
        <span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] text-fg-muted" title="Line no longer in diff">orphaned</span>
      {/if}
      <span class="min-w-0 flex-1"></span>
      {#if !editing}
        <ReviewIconButton icon={Pencil} label="Edit" onclick={startEdit} />
        <ReviewIconButton icon={Trash2} label="Delete" disabled={busy} onclick={() => { void deleteComment(); }} />
      {/if}
    </div>
    <div class="px-3 py-2.5">
      {#if quote}
        <div class="mb-2 truncate border-l-2 border-border-subtle pl-2 font-mono text-[0.6875rem] text-fg-subtle">
          {quote}
        </div>
      {/if}
      {#if editing}
        <textarea
          bind:value={editBody}
          rows="3"
          class="w-full resize-none rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-2 text-xs leading-relaxed text-fg focus:border-accent/60 focus:outline-none"
          onkeydown={onEditKeydown}
        ></textarea>
        <div class="mt-2 flex justify-end gap-2">
          <button
            type="button"
            class="rounded-[var(--radius-control)] px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg"
            onclick={cancelEdit}
          >
            Cancel
          </button>
          <button
            type="button"
            class="rounded-[var(--radius-control)] bg-accent px-2.5 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45"
            disabled={!canSave}
            onclick={() => { void saveEdit(); }}
          >
            Save
          </button>
        </div>
      {:else}
        <p class="whitespace-pre-wrap leading-relaxed text-fg">{comment.body}</p>
      {/if}
    </div>
  </div>
</article>
