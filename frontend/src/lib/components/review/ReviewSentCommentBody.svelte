<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
  import { commentWriteKey } from '../../stores/reviewCommentWrites.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { visibleBody } from '../../utils/reviewComments';
  import type { ReviewComment, ReviewThread } from '../../types/models';

  // A sent comment's body, or its edit: the raw body in the editor shell
  // with Save and Cancel. The edit's text is the review state's (the row
  // can unmount); Save waits, pending, for the forge's answer, and a
  // refusal keeps the text with its sentence. A failed delete says why
  // under the body. An open edit carries no find key, so find does not
  // count the body it replaces.

  interface Props {
    review: Pick<ReviewPaneState, 'sentComments'> | null;
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
    };
  });
  const shown = $derived(visibleBody(comment.body));
  const deleteError = $derived(writes?.error(commentWriteKey.delete(thread.id, comment.id)) ?? null);
  let focusOnce = true;
</script>

{#if edit}
  <div class={className} data-testid="review-comment-edit-editor">
    <ReviewCommentEditorShell
      body={edit?.body ?? ''}
      onBodyChange={(body) => edit?.change(body)}
      primary={{ label: 'Save', run: () => edit?.save(), disabled: edit?.unchanged ?? true, testid: 'review-comment-save' }}
      onCancel={() => edit?.cancel()}
      pending={edit?.pending ?? false}
      error={edit?.error ?? null}
      takeFocus={() => {
        const take = focusOnce;
        focusOnce = false;
        return take;
      }}
    />
  </div>
{:else if shown !== ''}
  <div class={className} data-find-key={findKey}>
    <ChatMarkdown source={shown} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
    {#if deleteError}
      <p class="mt-1 text-[0.6875rem] text-error" data-testid="review-comment-delete-error">{deleteError}</p>
    {/if}
  </div>
{/if}
