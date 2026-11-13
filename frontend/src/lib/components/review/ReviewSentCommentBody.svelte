<script lang="ts">
  import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
  import ReviewCommentMarkdown, { type SuggestionSource } from './ReviewCommentMarkdown.svelte';
  import ReviewReactionChips from './ReviewReactionChips.svelte';
  import ReviewSuggestionStatus, { suggestionStatus } from './ReviewSuggestionStatus.svelte';
  import { commentWriteKey } from '../../stores/reviewCommentWrites.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { visibleBody } from '../../utils/reviewComments';
  import type { CommentSuggestion, ReviewComment, ReviewThread } from '../../types/models';

  // A sent comment's body, or its edit: the raw body in the editor shell
  // with Save and Cancel. The edit's text is the review state's (the row
  // can unmount); Save waits, pending, for the forge's answer, and a
  // refusal keeps the text with its sentence. A failed delete says why
  // under the body. An open edit carries no find key, so find does not
  // count the body it replaces. A suggestion in the body renders as its
  // diff with Apply, and the comment's reactions as chips under it.

  interface Props {
    review: Pick<ReviewPaneState, 'sentComments' | 'suggestions' | 'reactions' | 'editorPreviews'> | null;
    thread: ReviewThread;
    comment: ReviewComment;
    /** `data-find-key` of the shown body; omitted where find does not read. */
    findKey?: string;
    class?: string;
  }

  let { review, thread, comment, findKey, class: className = '' }: Props = $props();

  const writes = $derived(review?.sentComments ?? null);
  // The open edit and everything its editor reads, or null: one value, so
  // nothing in its branch reads a closed edit.
  const edit = $derived.by(() => {
    const body = writes?.editBodyFor(thread, comment) ?? null;
    if (!writes || body === null) return null;
    const key = commentWriteKey.edit(thread.id, comment.id);
    return {
      body,
      pending: writes.pending(key),
      error: writes.error(key),
      unchanged: body.trim() === '' || body.trim() === comment.body.trim(),
      change: (next: string) => writes.setEditBody(thread, comment, next),
      save: () => writes.saveEdit(thread, comment),
      cancel: () => writes.setEditBody(thread, comment, null),
      preview: review?.editorPreviews.control(key) ?? null,
    };
  });
  const shown = $derived(visibleBody(comment.body));
  const deleteError = $derived(writes?.error(commentWriteKey.delete(thread.id, comment.id)) ?? null);
  const suggestions = $derived(review?.suggestions ?? null);
  // The thread's new-side lines, while the diff on screen is the PR's
  // head: an outdated thread's lines name older code.
  const anchorSource = $derived.by<SuggestionSource | null>(() => {
    if (thread.isOutdated || thread.path === '') return null;
    const file = suggestions?.currentFile(thread.path) ?? null;
    return file ? { file, from: 0, to: 0 } : null;
  });
  function sourceFor(_block: unknown, suggestion: CommentSuggestion | null): SuggestionSource | null {
    const from = suggestion?.startLine ?? 0;
    if (!anchorSource || from <= 0) return null;
    return { file: anchorSource.file, from, to: suggestion?.endLine ?? from };
  }
  let focusOnce = true;
</script>

{#snippet suggestionFooter(suggestion: CommentSuggestion)}
  {#if suggestions}
    <ReviewSuggestionStatus {suggestions} {thread} {comment} {suggestion} />
  {/if}
{/snippet}

{#if edit}
  <div class={className} data-testid="review-comment-edit-editor">
    <ReviewCommentEditorShell
      body={edit?.body ?? ''}
      onBodyChange={(body) => edit?.change(body)}
      primary={{ label: 'Save', run: () => edit?.save(), disabled: edit?.unchanged ?? true, testid: 'review-comment-save' }}
      onCancel={() => edit?.cancel()}
      pending={edit?.pending ?? false}
      error={edit?.error ?? null}
      preview={edit?.preview ?? null}
      previewSource={suggestions?.threadSource(thread) ?? null}
      takeFocus={() => {
        const take = focusOnce;
        focusOnce = false;
        return take;
      }}
    />
  </div>
{:else if shown !== ''}
  <div class={className}>
    <div data-find-key={findKey}>
      <ReviewCommentMarkdown
        body={shown}
        suggestions={comment.suggestions ?? null}
        {sourceFor}
        footer={suggestionFooter}
        showFooter={(suggestion) => (suggestions ? suggestionStatus(thread, suggestion, suggestions.available) !== 'none' || suggestions.error(thread, comment, suggestion) !== null : false)}
      />
    </div>
    {#if deleteError}
      <p class="mt-1 text-[0.6875rem] text-error" data-testid="review-comment-delete-error">{deleteError}</p>
    {/if}
    {#if review}
      <ReviewReactionChips reactions={review.reactions} {thread} {comment} />
    {/if}
  </div>
{/if}
