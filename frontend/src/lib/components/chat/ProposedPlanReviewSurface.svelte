<script lang="ts">
  import MessageSquarePlus from 'lucide-svelte/icons/message-square-plus';
  import X from 'lucide-svelte/icons/x';
  import Pencil from 'lucide-svelte/icons/pencil';
  import Send from 'lucide-svelte/icons/send';
  import Check from 'lucide-svelte/icons/check';
  import Icon from '../primitives/Icon.svelte';
  import IconButton from '../primitives/IconButton.svelte';
  import Button from '../primitives/Button.svelte';
  import ChatMarkdown from './ChatMarkdown.svelte';
  import { splitProposedPlanMarkdownBlocks } from '../../utils/proposedPlan';
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

  interface PendingSelection {
    text: string;
    startLine: number;
    endLine: number;
    top: number;
    left: number;
  }

  let { threadId, planItemId, markdown, comments, onRefresh, onSendDrafts }: Props = $props();

  let surfaceRoot: HTMLDivElement | undefined = $state(undefined);
  let pendingSelection: PendingSelection | null = $state(null);
  let commentBody = $state('');
  let saving = $state(false);
  let sending = $state(false);
  let editingCommentId = $state<string | null>(null);
  let editBody = $state('');

  const sourceBlocks = $derived(splitProposedPlanMarkdownBlocks(markdown));
  const draftCommentIds = $derived(comments.filter((c) => c.status === 'draft').map((c) => c.id));

  function selectionIsInsideSurface(selection: Selection): boolean {
    if (!surfaceRoot || selection.rangeCount === 0) return false;
    const range = selection.getRangeAt(0);
    return surfaceRoot.contains(range.commonAncestorContainer);
  }

  function sourceBlockForNode(node: Node): HTMLElement | null {
    const element = node.nodeType === Node.ELEMENT_NODE
      ? node as Element
      : node.parentElement;
    return element?.closest<HTMLElement>('[data-plan-source-block]') ?? null;
  }

  function selectedLineRange(range: Range): { startLine: number; endLine: number } | null {
    const startBlock = sourceBlockForNode(range.startContainer);
    const endBlock = sourceBlockForNode(range.endContainer);
    const startLine = Number(startBlock?.dataset.lineStart ?? 0);
    const endLine = Number(endBlock?.dataset.lineEnd ?? 0);
    if (!startLine || !endLine) return null;
    return {
      startLine: Math.min(startLine, endLine),
      endLine: Math.max(startLine, endLine),
    };
  }

  function handleMouseUp(): void {
    const selection = window.getSelection();
    if (!selection || selection.isCollapsed || !selectionIsInsideSurface(selection)) return;
    const selectedText = selection.toString().trim();
    const selectedRange = selection.getRangeAt(0);
    const lineRange = selectedLineRange(selectedRange);
    if (!selectedText || !lineRange || !surfaceRoot) return;

    const selectionRect = selectedRange.getBoundingClientRect();
    const rootRect = surfaceRoot.getBoundingClientRect();
    pendingSelection = {
      text: selectedText,
      startLine: lineRange.startLine,
      endLine: lineRange.endLine,
      top: selectionRect.bottom - rootRect.top + surfaceRoot.scrollTop + 8,
      left: Math.min(
        Math.max(selectionRect.left - rootRect.left + surfaceRoot.scrollLeft, 12),
        Math.max(rootRect.width - 300, 12),
      ),
    };
  }

  function clearSelection(): void {
    pendingSelection = null;
    commentBody = '';
    window.getSelection()?.removeAllRanges();
  }

  async function saveComment(): Promise<void> {
    if (!pendingSelection || saving) return;
    if (!commentBody.trim()) {
      addToast('warning', 'Add a comment first');
      return;
    }
    saving = true;
    try {
      await CreateProposedPlanComment(threadId, {
        planItemId,
        startLine: pendingSelection.startLine,
        endLine: pendingSelection.endLine,
        body: commentBody,
      });
      clearSelection();
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

  function handleDocumentPointerDown(event: PointerEvent): void {
    if (!pendingSelection || !surfaceRoot) return;
    if (surfaceRoot.contains(event.target as Node)) return;
    clearSelection();
  }

  $effect(() => {
    document.addEventListener('pointerdown', handleDocumentPointerDown);
    document.addEventListener('mouseup', handleMouseUp);
    return () => {
      document.removeEventListener('pointerdown', handleDocumentPointerDown);
      document.removeEventListener('mouseup', handleMouseUp);
    };
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

  <div
    bind:this={surfaceRoot}
    class="relative max-h-[60vh] overflow-auto px-3 py-3"
    role="region"
    aria-label="Selectable proposed plan"
  >
    <div class="space-y-3">
      {#each sourceBlocks as block (block.id)}
        <div
          data-plan-source-block
          data-line-start={block.startLine}
          data-line-end={block.endLine}
        >
          <ChatMarkdown source={block.markdown} class="select-text" />
        </div>
      {/each}
    </div>

    {#if pendingSelection}
      <div
        class="absolute z-10 w-[18rem] rounded-md border border-border bg-surface-1 p-2.5 shadow-menu"
        style={`top: ${pendingSelection.top}px; left: ${pendingSelection.left}px;`}
        data-testid="plan-comment-composer"
      >
        <p class="mb-1 line-clamp-2 text-[11px] text-fg-muted">"{pendingSelection.text}"</p>
        <textarea
          bind:value={commentBody}
          rows="3"
          placeholder="Leave a revision note..."
          class="w-full resize-y rounded-md border border-border-subtle bg-surface-0 px-2 py-1.5 text-[12px] text-fg outline-none placeholder:text-fg-hint focus:border-accent focus:ring-2 focus:ring-accent/30"
        ></textarea>
        <div class="mt-2 flex justify-end gap-1.5">
          <Button variant="ghost" size="xs" onclick={clearSelection}>
            {#snippet children()}Cancel{/snippet}
          </Button>
          <Button variant="tinted" size="xs" loading={saving} onclick={() => void saveComment()} testId="plan-comment-save">
            {#snippet children()}Comment{/snippet}
          </Button>
        </div>
      </div>
    {/if}
  </div>

  {#if comments.length > 0}
    <div class="space-y-2 border-t border-border-subtle bg-surface-1/60 p-2.5">
      {#each comments as comment (comment.id)}
        <div class="rounded-md border border-border-subtle bg-surface-0 p-2.5 text-[12px]" data-testid="plan-comment">
          <div class="mb-1 flex items-center justify-between gap-2">
            <span class="text-[10px] font-semibold uppercase tracking-wide {comment.status === 'resolved' ? 'text-fg-hint' : comment.status === 'sent' ? 'text-success' : 'text-accent'}">
              {comment.status === 'resolved' ? 'Resolved' : comment.status === 'sent' ? 'Sent' : 'Draft'}
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
          <p class="mb-2 line-clamp-2 border-l border-border-subtle pl-2 text-[11px] text-fg-muted">
            {comment.selectedText}
          </p>
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
    </div>
  {/if}
</div>
