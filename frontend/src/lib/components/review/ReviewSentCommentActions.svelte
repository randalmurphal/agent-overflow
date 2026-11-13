<script lang="ts">
  import Pencil from '@lucide/svelte/icons/pencil';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import ReviewCommentActions, { type CommentAction } from './ReviewCommentActions.svelte';
  import { commentWriteKey } from '../../stores/reviewCommentWrites.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { ReviewComment, ReviewThread } from '../../types/models';

  // Edit and Delete on a sent comment the forge says the viewer may
  // change, as the header's hover cluster. Edit opens the body's editor
  // (ReviewSentCommentBody); Delete confirms in place and then waits, the
  // confirm pending, until the forge's answer shows the comment gone.

  interface Props {
    review: Pick<ReviewPaneState, 'sentComments' | 'sentCommentActions' | 'nowWithheldReason'>;
    thread: ReviewThread;
    comment: ReviewComment;
    /** The thread's first comment, shown as the card's header and body. */
    first?: boolean;
  }

  let { review, thread, comment, first = false }: Props = $props();

  const writes = $derived(review.sentComments);
  const editing = $derived(writes.editBodyFor(thread, comment) !== null);
  const deleteKey = $derived(commentWriteKey.delete(thread.id, comment.id));
  const actions = $derived.by<CommentAction[]>(() => {
    if (!review.sentCommentActions || editing || comment.id === '') return [];
    const withheld = review.nowWithheldReason !== null;
    const out: CommentAction[] = [];
    if (comment.viewerCanEdit) {
      out.push({ id: 'edit', icon: Pencil, label: 'Edit', disabled: withheld, disabledLabel: review.nowWithheldReason ?? undefined, testid: 'review-comment-edit', run: () => writes.setEditBody(thread, comment, comment.body) });
    }
    if (comment.viewerCanDelete) {
      out.push({ id: 'delete', icon: Trash2, label: 'Delete', disabled: withheld, disabledLabel: review.nowWithheldReason ?? undefined, confirm: 'Delete?', testid: 'review-comment-delete', run: () => writes.deleteComment(thread, comment) });
    }
    return out;
  });
</script>

<ReviewCommentActions
  {actions}
  confirming={writes.confirmingDelete(thread, comment) ? 'delete' : null}
  onConfirmingChange={(id) => writes.setConfirmDelete(thread, comment, id === 'delete')}
  pending={writes.pending(deleteKey)}
  reveal={first ? 'thread' : 'comment'}
/>
