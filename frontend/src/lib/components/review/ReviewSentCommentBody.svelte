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
  const editBody = $derived(writes?.editBodyFor(thread, comment) ?? null);
  const shown = $derived(visibleBody(comment.body));
  const editKey = $derived(commentWriteKey.edit(thread.id, comment.id));
  const deleteError = $derived(writes?.error(commentWriteKey.delete(thread.id, comment.id)) ?? null);
  let focusOnce = true;
</script>

{#if writes && editBody !== null}
  <div class={className} data-testid="review-comment-edit-editor">
    <ReviewCommentEditorShell
      body={editBody}
      onBodyChange={(body) => writes.setEditBody(thread, comment, body)}
      primary={{ label: 'Save', run: () => writes.saveEdit(thread, comment), disabled: editBody.trim() === '' || editBody.trim() === comment.body.trim(), testid: 'review-comment-save' }}
      onCancel={() => writes.setEditBody(thread, comment, null)}
      pending={writes.pending(editKey)}
      error={writes.error(editKey)}
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
