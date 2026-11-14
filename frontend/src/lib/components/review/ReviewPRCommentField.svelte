<script lang="ts">
  import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
  import { commentWriteKey } from '../../stores/reviewCommentWrites.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';

  // The Conversation's one "Comment…" field: a comment on the PR itself
  // (GitHub's conversation, a GitLab merge request note), posted now. Its
  // held form is the review summary in the send strip. Like a thread's
  // reply field it opens into the editor shell; the text and open state
  // are the review state's, since the overview row unmounts.

  interface Props {
    review: Pick<ReviewPaneState, 'sentComments' | 'nowWithheldReason' | 'commentWritePending' | 'commentWriteError' | 'editorPreviews'>;
  }

  let { review }: Props = $props();

  const writes = $derived(review.sentComments);
  const body = $derived(writes.prCommentBody);
</script>

<div class="mt-4 pl-10" data-testid="review-pr-comment">
  {#if writes.prCommentOpen}
    <ReviewCommentEditorShell
      testid="review-pr-comment-composer"
      {body}
      onBodyChange={(next) => writes.setPRCommentBody(next)}
      placeholder="Comment…"
      primary={{
        label: 'Comment now',
        run: () => writes.postPRComment(),
        disabled: body.trim() === '' || review.nowWithheldReason !== null,
        title: review.nowWithheldReason ?? undefined,
        testid: 'review-pr-comment-now',
      }}
      onCancel={() => writes.setPRCommentOpen(false)}
      pending={review.commentWritePending(commentWriteKey.prComment)}
      error={review.commentWriteError(commentWriteKey.prComment)}
      preview={review.editorPreviews.control(commentWriteKey.prComment)}
      takeFocus={() => true}
    />
  {:else}
    <button
      type="button"
      class="h-7 w-full truncate rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 text-left text-xs text-fg-subtle hover:border-border-strong hover:text-fg-muted"
      data-testid="review-pr-comment-open"
      onclick={() => writes.setPRCommentOpen(true)}
    >
      Comment…
    </button>
  {/if}
</div>
