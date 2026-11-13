<script lang="ts">
  import { onDestroy } from 'svelte';
  import Check from '@lucide/svelte/icons/check';
  import Link from '@lucide/svelte/icons/link';
  import Pencil from '@lucide/svelte/icons/pencil';
  import SmilePlus from '@lucide/svelte/icons/smile-plus';
  import Trash2 from '@lucide/svelte/icons/trash-2';
  import ReviewCommentActions, { type CommentAction } from './ReviewCommentActions.svelte';
  import ReviewReactionPicker from './ReviewReactionPicker.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import { commentWriteKey } from '../../stores/reviewCommentWrites.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { copyToClipboard } from '../../utils/clipboard';
  import type { ReviewComment, ReviewThread } from '../../types/models';

  // A sent comment's hover actions, in order: Add reaction (every
  // comment, where the backend has reactions), Copy link (the forge's URL
  // for it), Edit and Delete (where the forge says the viewer may). Add
  // reaction opens the eight shared reactions; Copy link copies through
  // the shared clipboard helper, whose failure is reported and toasted.
  // Edit opens the body's editor (ReviewSentCommentBody); Delete
  // confirms in place and then waits, the confirm pending, until the
  // forge's answer shows the comment gone.

  interface Props {
    review: Pick<ReviewPaneState, 'sentComments' | 'sentCommentActions' | 'nowWithheldReason' | 'reactions'>;
    thread: ReviewThread;
    comment: ReviewComment;
    /** The thread's first comment, shown as the card's header and body. */
    first?: boolean;
  }

  let { review, thread, comment, first = false }: Props = $props();

  const writes = $derived(review.sentComments);
  const reactions = $derived(review.reactions);
  const editing = $derived(writes.editBodyFor(thread, comment) !== null);
  const deleteKey = $derived(commentWriteKey.delete(thread.id, comment.id));
  const picking = $derived(reactions.pickerOpen(thread, comment));

  let copied = $state(false);
  let copiedTimer: ReturnType<typeof setTimeout> | undefined;
  onDestroy(() => clearTimeout(copiedTimer));

  async function copyLink(url: string): Promise<void> {
    if (!(await copyToClipboard(url))) {
      addToast('error', 'Could not copy the link.');
      return;
    }
    copied = true;
    clearTimeout(copiedTimer);
    copiedTimer = setTimeout(() => { copied = false; }, 2000);
  }

  const actions = $derived.by<CommentAction[]>(() => {
    if (editing || comment.id === '') return [];
    const out: CommentAction[] = [];
    if (reactions.available) {
      out.push({
        id: 'react', icon: SmilePlus, label: 'Add reaction', haspopup: 'dialog', expanded: picking,
        disabled: reactions.withheld !== null, disabledLabel: reactions.withheld ?? undefined, testid: 'review-comment-react',
        run: () => reactions.setPickerOpen(thread, comment, !picking),
      });
    }
    const url = comment.url ?? '';
    if (url !== '') {
      out.push({ id: 'link', icon: copied ? Check : Link, label: copied ? 'Copied' : 'Copy link', testid: 'review-comment-copy-link', run: () => copyLink(url) });
    }
    if (!review.sentCommentActions) return out;
    const withheld = review.nowWithheldReason !== null;
    if (comment.viewerCanEdit) {
      out.push({ id: 'edit', icon: Pencil, label: 'Edit', disabled: withheld, disabledLabel: review.nowWithheldReason ?? undefined, testid: 'review-comment-edit', run: () => writes.setEditBody(thread, comment, comment.body) });
    }
    if (comment.viewerCanDelete) {
      out.push({ id: 'delete', icon: Trash2, label: 'Delete', disabled: withheld, disabledLabel: review.nowWithheldReason ?? undefined, confirm: 'Delete?', testid: 'review-comment-delete', run: () => writes.deleteComment(thread, comment) });
    }
    return out;
  });
</script>

{#snippet picker(anchor: (id: string) => HTMLButtonElement | undefined)}
  {#if reactions.available}
    <ReviewReactionPicker {reactions} {thread} {comment} anchor={anchor('react')} />
  {/if}
{/snippet}

<ReviewCommentActions
  {actions}
  confirming={writes.confirmingDelete(thread, comment) ? 'delete' : null}
  onConfirmingChange={(id) => writes.setConfirmDelete(thread, comment, id === 'delete')}
  pending={writes.pending(deleteKey)}
  reveal={first ? 'thread' : 'comment'}
  popover={picker}
/>
