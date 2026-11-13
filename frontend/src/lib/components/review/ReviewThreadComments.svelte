<script lang="ts">
  import ReviewAvatar from './ReviewAvatar.svelte';
  import ReviewSentCommentActions from './ReviewSentCommentActions.svelte';
  import ReviewSentCommentBody from './ReviewSentCommentBody.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { relativeTime } from '../../utils/format';
  import { visibleBody } from '../../utils/reviewComments';
  import { findThreadCommentKey, reviewCommentKey } from '../../utils/reviewFindScan';
  import { authorDisplayName, isBotLogin } from '../../utils/reviewIdentity';
  import type { ReviewThread } from '../../types/models';

  // One PR thread's comment bodies, shared by the inline diff strip
  // (ReviewPRThreadRow) and the conversation card
  // (ReviewConversationThread) so the two surfaces cannot drift. Each
  // comment is an avatar, the author's name and login, the time, then
  // the body. A comment the viewer may change has Edit and Delete at the
  // right end of its header (ReviewSentCommentActions). The thread's
  // actions live in ReviewThreadFooter.
  //
  // Bodies render with `embeddedHtml`: forge comments are authored against
  // GitHub/GitLab's HTML subset (collapsible <details> prompts, badge
  // tables), and this is exactly the opt-in surface for it. Marker-only
  // bot replies (`<!-- coderabbit resolve -->`) render as nothing at all.

  interface Props {
    thread: ReviewThread;
    /** True renders replies only, for hosts that already render the
     *  thread's first comment themselves (the conversation card). */
    skipFirst?: boolean;
    /** Avatar diameter for the comment rows. */
    avatarSize?: number;
    /** Mark each body for the diff's find (utils/reviewFindScan.ts). */
    findKeys?: boolean;
    /** The review whose comment actions these comments offer. */
    review?: Pick<ReviewPaneState, 'sentComments' | 'sentCommentActions' | 'nowWithheldReason'> | null;
  }

  let { thread, skipFirst = false, avatarSize = 22, findKeys = false, review = null }: Props = $props();

  function commentTime(createdAt: string): string {
    const ms = Date.parse(createdAt);
    return Number.isNaN(ms) ? createdAt : relativeTime(ms);
  }
</script>

{#each skipFirst ? thread.comments.slice(1) : thread.comments as comment (`${comment.databaseID}:${comment.createdAt}`)}
  {@const shown = visibleBody(comment.body)}
  {#if shown !== '' || review?.sentComments.editBodyFor(thread, comment) != null}
    <div
      class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2.5 border-t border-border-subtle px-3.5 py-3"
      style:grid-template-columns="{avatarSize}px minmax(0, 1fr)"
      data-testid="review-thread-comment"
    >
      <ReviewAvatar login={comment.authorLogin} name={comment.authorName} size={avatarSize} />
      <div class="group/comment flex min-w-0 items-center gap-1.5 text-[0.75rem]" style:min-height="{avatarSize}px">
        <span class="truncate font-semibold text-fg">{authorDisplayName(comment)}</span>
        {#if isBotLogin(comment.authorLogin)}
          <span class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1 text-[0.625rem] leading-4 text-fg-muted">bot</span>
        {/if}
        {#if comment.authorName}
          <span class="min-w-0 truncate text-fg-subtle" title="@{comment.authorLogin}">@{comment.authorLogin}</span>
        {/if}
        <span class="shrink-0 text-fg-subtle">· {commentTime(comment.createdAt)}</span>
        {#if comment.editedAt}
          <span class="shrink-0 text-fg-subtle" title="Edited {commentTime(comment.editedAt)}" data-testid="review-comment-edited">edited</span>
        {/if}
        <span class="min-w-0 flex-1"></span>
        {#if review}
          <ReviewSentCommentActions {review} {thread} {comment} />
        {/if}
      </div>
      <ReviewSentCommentBody
        {review}
        {thread}
        {comment}
        class="col-start-2 mt-1"
        findKey={findKeys ? findThreadCommentKey(thread.id, reviewCommentKey(comment)) : undefined}
      />
    </div>
  {/if}
{/each}
