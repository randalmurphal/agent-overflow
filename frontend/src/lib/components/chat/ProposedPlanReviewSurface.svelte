<script lang="ts">
  import { onDestroy, onMount } from 'svelte';
  import MessageSquarePlus from 'lucide-svelte/icons/message-square-plus';
  import X from 'lucide-svelte/icons/x';
  import Pencil from 'lucide-svelte/icons/pencil';
  import Send from 'lucide-svelte/icons/send';
  import Check from 'lucide-svelte/icons/check';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Button from '../primitives/Button.svelte';
  import {
    CreateProposedPlanComment,
    DeleteProposedPlanComment,
    UpdateProposedPlanComment,
  } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { ProposedPlanComment } from '../../types/models';

  interface Props {
    threadId: string;
    planItemId: string;
    markdown: string;
    comments: ProposedPlanComment[];
    onRefresh: () => Promise<void> | void;
    onSendDrafts: (commentIds: string[]) => Promise<void>;
  }

  let { threadId, planItemId, markdown, comments, onRefresh, onSendDrafts }: Props = $props();

  let dragging = $state(false);
  let selectionStart = $state<number | null>(null);
  let selectionEnd = $state<number | null>(null);
  let commentBody = $state('');
  let saving = $state(false);
  let sending = $state(false);
  let editingCommentId = $state<string | null>(null);
  let editBody = $state('');

  const lines = $derived(markdown.replace(/\s+$/g, '').split('\n'));
  const draftCommentIds = $derived(comments.filter((c) => c.status === 'draft').map((c) => c.id));
  const commentsByEndLine = $derived.by(() => {
    const grouped = new Map<number, ProposedPlanComment[]>();
    for (const comment of comments) {
      const bucket = grouped.get(comment.endLine) ?? [];
      bucket.push(comment);
      grouped.set(comment.endLine, bucket);
    }
    return grouped;
  });
  const selectedRange = $derived.by(() => {
    if (selectionStart === null || selectionEnd === null) return null;
    return {
      start: Math.min(selectionStart, selectionEnd),
      end: Math.max(selectionStart, selectionEnd),
    };
  });

  function isSelected(line: number): boolean {
    const range = selectedRange;
    return Boolean(range && line >= range.start && line <= range.end);
  }

  function commentsAfterLine(line: number): ProposedPlanComment[] {
    return commentsByEndLine.get(line) ?? [];
  }

  function startSelection(line: number, event: MouseEvent): void {
    if (event.button !== 0) return;
    dragging = true;
    selectionStart = line;
    selectionEnd = line;
  }

  function extendSelection(line: number): void {
    if (!dragging) return;
    selectionEnd = line;
  }

  function finishSelection(): void {
    dragging = false;
  }

  async function saveComment(): Promise<void> {
    const range = selectedRange;
    if (!range || saving) return;
    if (!commentBody.trim()) {
      addToast('warning', 'Add a comment first');
      return;
    }
    saving = true;
    try {
      await CreateProposedPlanComment(threadId, {
        planItemId,
        startLine: range.start,
        endLine: range.end,
        body: commentBody,
      });
      commentBody = '';
      selectionStart = null;
      selectionEnd = null;
      await onRefresh();
    } catch (err) {
      console.error('CreateProposedPlanComment failed:', err);
      addToast('error', 'Failed to save comment');
    } finally {
      saving = false;
    }
  }

  async function deleteComment(comment: ProposedPlanComment): Promise<void> {
    try {
      await DeleteProposedPlanComment(threadId, comment.id);
      await onRefresh();
    } catch (err) {
      console.error('DeleteProposedPlanComment failed:', err);
      addToast('error', 'Failed to remove comment');
    }
  }

  function beginEdit(comment: ProposedPlanComment): void {
    editingCommentId = comment.id;
    editBody = comment.body;
  }

  async function saveEdit(comment: ProposedPlanComment): Promise<void> {
    if (!editBody.trim()) {
      addToast('warning', 'Add a comment first');
      return;
    }
    try {
      await UpdateProposedPlanComment(threadId, comment.id, { body: editBody });
      editingCommentId = null;
      editBody = '';
      await onRefresh();
    } catch (err) {
      console.error('UpdateProposedPlanComment failed:', err);
      addToast('error', 'Failed to update comment');
    }
  }

  async function sendDrafts(): Promise<void> {
    if (draftCommentIds.length === 0 || sending) return;
    sending = true;
    try {
      await onSendDrafts(draftCommentIds);
      await onRefresh();
    } finally {
      sending = false;
    }
  }

  function handleWindowMouseup(): void {
    if (dragging) finishSelection();
  }

  onMount(() => {
    window.addEventListener('mouseup', handleWindowMouseup);
  });

  onDestroy(() => {
    window.removeEventListener('mouseup', handleWindowMouseup);
  });
</script>

<div class="rounded-md border border-border-subtle bg-surface-0/60">
  <div class="flex items-center justify-between gap-2 border-b border-border-subtle px-2.5 py-2">
    <div class="flex items-center gap-2 text-[11px] font-medium uppercase tracking-wide text-fg-muted">
      <Icon icon={MessageSquarePlus} size={13} />
      Review
    </div>
    <Button
      variant="tinted"
      size="xs"
      disabled={draftCommentIds.length === 0 || sending}
      loading={sending}
      onclick={() => void sendDrafts()}
      testId="plan-comments-send"
    >
      {#snippet children()}
        <span class="inline-flex items-center gap-1">
          <Icon icon={Send} size={12} />
          Send {draftCommentIds.length || ''}
        </span>
      {/snippet}
    </Button>
  </div>

  <div class="max-h-[60vh] overflow-auto font-mono text-[12px] leading-5">
    {#each lines as line, index (index)}
      {@const lineNumber = index + 1}
      <button
        type="button"
        class={[
          'grid w-full grid-cols-[2.75rem_minmax(0,1fr)] border-b border-border-subtle/50 text-left',
          'cursor-crosshair focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50',
          isSelected(lineNumber) ? 'bg-accent/12' : 'hover:bg-surface-2/45',
        ].join(' ')}
        onmousedown={(event) => startSelection(lineNumber, event)}
        onmouseenter={() => extendSelection(lineNumber)}
        onmouseup={finishSelection}
        data-testid="plan-review-line"
        data-line={lineNumber}
      >
        <span class="select-none border-r border-border-subtle px-2 py-1 text-right text-[10px] text-fg-hint">
          {lineNumber}
        </span>
        <span class="whitespace-pre-wrap px-2 py-1 text-fg">{line || ' '}</span>
      </button>

      {#each commentsAfterLine(lineNumber) as comment (comment.id)}
        <div class="border-b border-border-subtle/70 bg-surface-1 px-3 py-2 text-[12px]" data-testid="plan-comment">
          <div class="mb-1 flex items-center justify-between gap-2">
            <span class="text-[10px] font-semibold uppercase tracking-wide {comment.status === 'resolved' ? 'text-fg-hint' : comment.status === 'sent' ? 'text-success' : 'text-accent'}">
              {comment.status === 'resolved' ? 'Resolved' : comment.status === 'sent' ? 'Sent' : 'Draft'} - Lines {comment.startLine}-{comment.endLine}
            </span>
            <div class="flex items-center gap-1">
              {#if comment.status !== 'resolved'}
                <IconButton label="Edit comment" size="sm" onClick={() => beginEdit(comment)}>
                  {#snippet children()}<Icon icon={Pencil} size={12} />{/snippet}
                </IconButton>
                <IconButton label={comment.status === 'draft' ? 'Delete comment' : 'Resolve comment'} size="sm" onClick={() => void deleteComment(comment)}>
                  {#snippet children()}<Icon icon={X} size={12} />{/snippet}
                </IconButton>
              {/if}
            </div>
          </div>
          {#if editingCommentId === comment.id}
            <textarea
              bind:value={editBody}
              rows="3"
              class="w-full resize-y rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-[12px] text-fg outline-none focus:border-accent focus:ring-2 focus:ring-accent/30"
            ></textarea>
            <div class="mt-1 flex justify-end gap-1">
              <Button variant="ghost" size="xs" onclick={() => { editingCommentId = null; editBody = ''; }}>
                {#snippet children()}Cancel{/snippet}
              </Button>
              <Button variant="tinted" size="xs" onclick={() => void saveEdit(comment)}>
                {#snippet children()}
                  <span class="inline-flex items-center gap-1"><Icon icon={Check} size={12} />Save</span>
                {/snippet}
              </Button>
            </div>
          {:else}
            <p class="whitespace-pre-wrap text-fg-muted">{comment.body}</p>
          {/if}
        </div>
      {/each}
    {/each}
  </div>

  {#if selectedRange}
    <div class="border-t border-border-subtle bg-surface-1 p-2.5" data-testid="plan-comment-composer">
      <div class="mb-1 text-[11px] font-medium text-fg-muted">
        Comment on lines {selectedRange.start}-{selectedRange.end}
      </div>
      <textarea
        bind:value={commentBody}
        rows="3"
        placeholder="Leave a revision note..."
        class="w-full resize-y rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-[12px] text-fg outline-none placeholder:text-fg-hint focus:border-accent focus:ring-2 focus:ring-accent/30"
      ></textarea>
      <div class="mt-2 flex justify-end gap-1.5">
        <Button variant="ghost" size="xs" onclick={() => { selectionStart = null; selectionEnd = null; commentBody = ''; }}>
          {#snippet children()}Cancel{/snippet}
        </Button>
        <Button variant="tinted" size="xs" loading={saving} onclick={() => void saveComment()} testId="plan-comment-save">
          {#snippet children()}Comment{/snippet}
        </Button>
      </div>
    </div>
  {/if}
</div>
