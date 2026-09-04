<script lang="ts">
  import ChatMarkdown from '../chat/ChatMarkdown.svelte';
  import { EMPTY_PATH_REFS } from '../../utils/pathLinkify';
  import { relativeTime } from '../../utils/format';
  import { visibleBody } from '../../utils/reviewComments';
  import { isImeComposingEvent } from '../../utils/imeComposition';
  import type { ReviewThread } from '../../types/models';

  // One PR thread's comment bodies plus the reply composer, shared by the
  // inline diff strip (ReviewPRThreadRow) and the conversation card
  // (ReviewConversationThread) so the two surfaces cannot drift.
  //
  // Bodies render with `embeddedHtml`: forge comments are authored against
  // GitHub/GitLab's HTML subset (collapsible <details> prompts, badge
  // tables), and this is exactly the opt-in surface for it. Marker-only
  // bot replies (`<!-- coderabbit resolve -->`) render as nothing at all.

  interface Props {
    thread: ReviewThread;
    /** Store-backed composer text — survives virtualizer row unmounts. */
    body: string;
    error: string | null;
    sending: boolean;
    replying: boolean;
    /** False keeps the bodies folded while the composer stays available
     *  (a collapsed strip with a drafted reply). */
    showComments?: boolean;
    /** True renders replies only — for hosts that already render the
     *  thread's first comment themselves (the conversation card). */
    skipFirst?: boolean;
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
    onBodyChange,
    onSendReply,
    onCloseReply,
  }: Props = $props();

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
  <div class="space-y-2">
    {#each skipFirst ? thread.comments.slice(1) : thread.comments as comment (`${comment.databaseID}:${comment.createdAt}`)}
      {@const shown = visibleBody(comment.body)}
      {#if shown !== ''}
        <div class="rounded-[var(--radius-control)] border border-border-subtle bg-surface-1 px-2.5 py-2">
          <div class="mb-1 flex items-baseline gap-1.5 text-[0.6875rem]">
            <span class="font-medium text-fg">{comment.authorLogin}</span>
            <span class="text-fg-subtle">{commentTime(comment.createdAt)}</span>
          </div>
          <ChatMarkdown source={shown} pathRefs={EMPTY_PATH_REFS} embeddedHtml />
        </div>
      {/if}
    {/each}
  </div>
{/if}

{#if replying}
  <textarea
    class="mt-2 w-full resize-none rounded-[var(--radius-field)] border border-border-subtle bg-surface-1 px-2 py-1.5 text-xs text-fg focus:border-accent/60 focus:outline-none"
    rows="3"
    value={body}
    oninput={(event) => onBodyChange(event.currentTarget.value)}
    onkeydown={onKeydown}
  ></textarea>
  {#if error}<div class="mt-1 text-[0.6875rem] text-error">{error}</div>{/if}
  <div class="mt-2 flex justify-end">
    <button
      type="button"
      class="rounded-[var(--radius-control)] bg-accent px-2 py-1 text-[0.6875rem] font-medium text-accent-fg disabled:opacity-45"
      disabled={sending || body.trim() === ''}
      onclick={() => { void onSendReply(); }}
    >
      Reply
    </button>
  </div>
{/if}
