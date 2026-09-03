<script lang="ts">
  import type { DiffReviewComment } from '../../types/models';
  import { isImeComposingEvent } from '../../utils/imeComposition';

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
  class="border-y border-border-subtle bg-surface-0/50 px-3 py-2 text-[0.75rem]"
  data-testid="review-comment-thread"
>
  <div class="mb-1 flex items-center gap-2">
    <span class="min-w-0 flex-1 truncate font-mono text-[0.6875rem] text-fg-muted">{commentLocation(comment)}</span>
    {#if orphaned}
      <span class="shrink-0 rounded-full bg-surface-2 px-1.5 py-px text-[0.625rem] text-fg-muted" title="Line no longer in diff">orphaned</span>
    {/if}
    {#if !editing}
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-control)] border border-border-subtle px-2 py-0.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg"
        onclick={startEdit}
      >
        Edit
      </button>
      <button
        type="button"
        class="shrink-0 rounded-[var(--radius-control)] border border-border-subtle px-2 py-0.5 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg disabled:opacity-45"
        disabled={busy}
        onclick={() => { void deleteComment(); }}
      >
        Delete
      </button>
    {/if}
  </div>
  {#if quote}
    <div class="mb-1 truncate border-l-2 border-border-subtle pl-2 font-mono text-[0.6875rem] text-fg-subtle">
      {quote}
    </div>
  {/if}
  {#if editing}
    <textarea
      bind:value={editBody}
      rows="2"
      class="w-full resize-none rounded border border-border-subtle bg-surface-1 px-2 py-1.5 text-[0.75rem] leading-relaxed text-fg focus:border-accent/60 focus:outline-none"
      onkeydown={onEditKeydown}
    ></textarea>
    <div class="mt-2 flex justify-end gap-2">
      <button
        type="button"
        class="rounded px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2"
        onclick={cancelEdit}
      >
        Cancel
      </button>
      <button
        type="button"
        class="rounded bg-accent px-2 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45"
        disabled={!canSave}
        onclick={() => { void saveEdit(); }}
      >
        Save
      </button>
    </div>
  {:else}
    <p class="whitespace-pre-wrap leading-relaxed text-fg">{comment.body}</p>
  {/if}
</article>
