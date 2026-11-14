<script lang="ts">
  import ReviewCommentThread from './ReviewCommentThread.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import type { DiffReviewComment } from '../../types/models';

  // A thread's held replies at its foot, on the diff row and the
  // Conversation card alike: each the draft card a line draft is, with its
  // Edit and Delete. Normally one; recovering a delivery can leave more.

  interface Props {
    review: Pick<ReviewPaneState, 'draftEditBodyFor' | 'setDraftEditBody' | 'updateComment' | 'deleteComment'>;
    replies: readonly DiffReviewComment[];
    /** On the diff row, where find paints them; not in the Conversation. */
    findable?: boolean;
  }

  let { review, replies, findable = false }: Props = $props();
</script>

{#if replies.length > 0}
  <div class="border-t border-border-subtle py-1" data-testid="review-held-replies">
    {#each replies as reply (reply.id)}
      <ReviewCommentThread
        comment={reply}
        editBody={review.draftEditBodyFor(reply.id)}
        onEditBodyChange={(commentId, body) => review.setDraftEditBody(commentId, body)}
        onUpdate={(commentId, body) => review.updateComment(commentId, body)}
        onDelete={(commentId) => review.deleteComment(commentId)}
        {findable}
      />
    {/each}
  </div>
{/if}
