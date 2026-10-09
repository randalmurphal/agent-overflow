<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import ReviewAvatar from './ReviewAvatar.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { relativeTime } from '../../utils/format';
  import { visibleBody } from '../../utils/reviewComments';
  import { authorDisplayName, isBotLogin } from '../../utils/reviewIdentity';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import type { ReviewThread } from '../../types/models';

  // One PR thread's comment bodies plus the reply composer, shared by the
  // inline diff strip (ReviewPRThreadRow) and the conversation card
  // (ReviewConversationThread) so the two surfaces cannot drift. Each
  // comment is an avatar, the author's name and login, the time, then
  // the body.
  //
  // Bodies render with `embeddedHtml`: forge comments are authored against
  // GitHub/GitLab's HTML subset (collapsible <details> prompts, badge
  // tables), and this is exactly the opt-in surface for it. Marker-only
  // bot replies (`<!-- coderabbit resolve -->`) render as nothing at all.

  interface Props {
    thread: ReviewThread;
    /** Store-backed composer text, survives virtualizer row unmounts. */
    body: string;
    error: string | null;
    sending: boolean;
    replying: boolean;
    /** False keeps the bodies folded while the composer stays available
     *  (a collapsed strip with a drafted reply). */
    showComments?: boolean;
    /** True renders replies only, for hosts that already render the
     *  thread's first comment themselves (the conversation card). */
    skipFirst?: boolean;
    /** Avatar diameter for the comment rows. */
    avatarSize?: number;
    onBodyChange: (body: string) => void;
    onSendReply: () => Promise<void> | void;
    onCloseReply: () => void;
  }

  let {
    thread,
    body,
    error,
    sending,
    replying,
    showComments = true,
    skipFirst = false,
    avatarSize = 22,
    onBodyChange,
    onSendReply,
    onCloseReply,
  }: Props = $props();

  let textareaEl: HTMLTextAreaElement | undefined = $state();

  // The composer opens from a click on "Reply": put the caret there.
  $effect(() => {
    if (replying) textareaEl?.focus();
  });

  function commentTime(createdAt: string): string {
    const ms = Date.parse(createdAt);
    return Number.isNaN(ms) ? createdAt : relativeTime(ms);
  }

  function onKeydown(event: KeyboardEvent): void {
    if (event.key === 'Escape') {
      event.preventDefault();
      onCloseReply();
      return;
    }
    // Mid-composition the reply text is still in the IME buffer, so the
    // submit chord would post a truncated comment.
    if (event.key === 'Enter' && isImeComposingEvent(event)) return;
    if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      void onSendReply();
    }
  }
</script>

{#if showComments}
  {#each skipFirst ? thread.comments.slice(1) : thread.comments as comment (`${comment.databaseID}:${comment.createdAt}`)}
    {@const shown = visibleBody(comment.body)}
    {#if shown !== ''}
      <div
        class="grid grid-cols-[auto_minmax(0,1fr)] gap-x-2.5 border-t border-border-subtle px-3.5 py-3"
        style:grid-template-columns="{avatarSize}px minmax(0, 1fr)"
        data-testid="review-thread-comment"
      >
        <ReviewAvatar login={comment.authorLogin} name={comment.authorName} size={avatarSize} />
        <div class="flex min-w-0 items-center gap-1.5 text-[0.75rem]" style:min-height="{avatarSize}px">
          <span class="truncate font-semibold text-fg">{authorDisplayName(comment)}</span>
          {#if isBotLogin(comment.authorLogin)}
            <span class="shrink-0 rounded-[var(--radius-field)] border border-border-subtle px-1 text-[0.625rem] leading-4 text-fg-muted">bot</span>
          {/if}
          {#if comment.authorName}
            <span class="min-w-0 truncate text-fg-subtle" title="@{comment.authorLogin}">@{comment.authorLogin}</span>
          {/if}
          <span class="shrink-0 text-fg-subtle">· {commentTime(comment.createdAt)}</span>
        </div>
        <div class="col-start-2 mt-1">
          <ChatMarkdown source={shown} pathRefs={EMPTY_PATH_REFS} embeddedHtml class="review-prose" />
        </div>
      </div>
    {/if}
  {/each}
{/if}

{#if replying}
  <div class="border-t border-border-subtle px-3.5 py-3" data-testid="review-thread-composer">
    <textarea
      bind:this={textareaEl}
      class="w-full resize-none rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2.5 py-2 text-xs text-fg placeholder:text-fg-subtle focus:border-accent/60 focus:outline-none"
      rows="3"
      placeholder="Reply… (Ctrl+Enter to send)"
      value={body}
      oninput={(event) => onBodyChange(event.currentTarget.value)}
      onkeydown={onKeydown}
    ></textarea>
    {#if error}<div class="mt-1 text-[0.6875rem] text-error">{error}</div>{/if}
    <div class="mt-2 flex items-center justify-end gap-2">
      <button
        type="button"
        class="rounded-[var(--radius-control)] px-2 py-1 text-[0.6875rem] text-fg-muted hover:bg-surface-2 hover:text-fg"
        onclick={onCloseReply}
      >
        Cancel
      </button>
      <button
        type="button"
        class="rounded-[var(--radius-control)] bg-accent px-2.5 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45"
        disabled={sending || body.trim() === ''}
        onclick={() => { void onSendReply(); }}
      >
        Reply
      </button>
    </div>
  </div>
{/if}
