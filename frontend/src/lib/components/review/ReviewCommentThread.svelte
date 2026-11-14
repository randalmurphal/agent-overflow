<script lang="ts">
  import Pencil from '@lucide/svelte/icons/pencil';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
  import type { SuggestionSource } from './ReviewCommentMarkdown.svelte';
  import type { EditorPreviewControl } from '../../stores/reviewEditorPreview.svelte';
  import ReviewIconButton from './ReviewIconButton.svelte';
  import type { DiffReviewComment } from '../../types/models';
  import { draftRange, rangeLabel } from '../../utils/reviewRange';
  import { jumpPulse } from '../../utils/reviewJumpPulse';
  import { findDraftKey } from '../../utils/reviewFindScan';

  // The reader's own draft comment on the diff surface: the same card
  // shape as a PR thread, with an accent edge in place of a state edge
  // (a draft has no forge state yet) and edit/delete in place of
  // reply/resolve. The row is virtualized, so an open edit's text lives in
  // the review state (`draftEditBodyFor`) and survives the row unmounting.
  // A held reply renders the same card at the foot of its thread.

  interface Props {
    comment: DiffReviewComment;
    orphaned?: boolean;
    /** The unsaved text of this draft's open edit; null when not editing. */
    editBody: string | null;
    /** Opens (a body), changes or, with null, discards the edit. */
    onEditBodyChange: (commentId: string, body: string | null) => void;
    /** Saves the body; the owner ends the edit on success. */
    onUpdate: (commentId: string, body: string) => Promise<void> | void;
    onDelete: (commentId: string) => Promise<void> | void;
    /** The jump that landed here (utils/reviewJumpPulse); 0 for none. */
    pulse?: number;
    /** Its body carries its find key: the diff's card does, the
     * Conversation's (which find does not search) does not. */
    findable?: boolean;
    /** The edit's Preview toggle and where a previewed suggestion reads
     * the lines it replaces. */
    preview?: EditorPreviewControl | null;
    previewSource?: SuggestionSource | null;
  }

  let { comment, orphaned = false, editBody, onEditBodyChange, onUpdate, onDelete, pulse = 0, findable = true, preview = null, previewSource = null }: Props = $props();
  let busy = $state(false);
  const editing = $derived(editBody !== null);
  const canSave = $derived((editBody ?? '').trim().length > 0 && !busy);
  const quote = $derived(comment.selectedText.trim().replace(/\s+/g, ' '));

  const range = $derived(draftRange(comment));

  function commentLocation(comment: DiffReviewComment): string {
    if (comment.side === 'file' || comment.side === 'reply') return comment.filePath;
    const line = comment.side === 'old' ? comment.oldLine : (comment.newLine || comment.oldLine);
    return line ? `${comment.filePath}:${line}` : comment.filePath;
  }

  function startEdit(): void {
    onEditBodyChange(comment.id, comment.body);
  }

  function cancelEdit(): void {
    onEditBodyChange(comment.id, null);
  }

  async function saveEdit(): Promise<void> {
    if (!canSave || editBody === null) return;
    busy = true;
    try {
      await onUpdate(comment.id, editBody);
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

</script>

<article
  class="mx-2 my-1.5 grid grid-cols-[20px_minmax(0,1fr)] gap-x-2 text-[0.75rem]"
  data-testid="review-comment-thread"
>
  <div class="pt-1">
    <ReviewAvatar login="you" name="You" initials="Y" size={20} />
  </div>
  <div class="min-w-0 overflow-hidden rounded-[var(--radius-control)] border bg-surface-1 {orphaned ? 'border-dashed border-border-strong' : 'border-accent/40 border-l-[3px] border-l-accent'}" {@attach jumpPulse(pulse)}>
    <div class="flex min-w-0 items-center gap-1.5 py-1 pl-2.5 pr-1 {orphaned ? 'bg-surface-2/40' : 'bg-accent/6'}" title={commentLocation(comment)}>
      <span class="shrink-0 font-semibold text-fg">You</span>
      <span class="shrink-0 rounded-full bg-accent/12 px-1.5 py-px text-[0.625rem] text-accent">{comment.side === 'reply' ? 'draft reply' : 'draft'}</span>
      {#if range}
        <span class="shrink-0 text-[0.6875rem] text-fg-subtle" data-testid="review-draft-range">{rangeLabel(range)}</span>
      {/if}
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
        <ReviewCommentEditorShell
          body={editBody ?? ''}
          onBodyChange={(body) => onEditBodyChange(comment.id, body)}
          primary={{ label: 'Save', run: saveEdit, disabled: !canSave }}
          onCancel={cancelEdit}
          {preview}
          {previewSource}
          takeFocus={() => true}
        />
      {:else}
        <p class="whitespace-pre-wrap leading-relaxed text-fg" data-find-key={findable ? findDraftKey(comment.id) : undefined}>{comment.body}</p>
      {/if}
    </div>
  </div>
</article>
