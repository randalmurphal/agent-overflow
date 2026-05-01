<script lang="ts">
  import MessageSquarePlus from 'lucide-svelte/icons/message-square-plus';
  import X from 'lucide-svelte/icons/x';
  import Pencil from 'lucide-svelte/icons/pencil';
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
    /** Absolute base directory for resolving relative file paths the
     *  linkifier finds in the plan markdown. Threaded down from the
     *  caller so an in-thread plan review hits the editor correctly. */
    workspacePath?: string;
  }

  interface PendingSelection {
    text: string;
    startLine: number;
    endLine: number;
    // Trigger button anchors above the selection: its bottom-center
    // lands at (triggerLeft, triggerTop) so the downward arrow points
    // at the selected text. Composer drops below the selection.
    triggerTop: number;
    triggerLeft: number;
    composerTop: number;
    composerLeft: number;
  }

  const COMPOSER_WIDTH = 288;
  const TRIGGER_GAP = 8;

  interface BlockCommentView {
    block: ReturnType<typeof splitProposedPlanMarkdownBlocks>[number];
    anchoredComments: ProposedPlanComment[];
    highlighted: boolean;
  }

  let { threadId, planItemId, markdown, comments, onRefresh, workspacePath = '' }: Props = $props();

  let surfaceRoot: HTMLDivElement | undefined = $state(undefined);
  let pendingSelection: PendingSelection | null = $state(null);
  let commentBody = $state('');
  let saving = $state(false);
  let composerOpen = $state(false);
  let editingCommentId = $state<string | null>(null);
  let editBody = $state('');

  const sourceBlocks = $derived(splitProposedPlanMarkdownBlocks(markdown));
  const blockCommentViews = $derived.by<BlockCommentView[]>(() => {
    return sourceBlocks.map((block) => ({
      block,
      anchoredComments: comments.filter((comment) => comment.endLine >= block.startLine && comment.endLine <= block.endLine),
      highlighted: comments.some((comment) => comment.startLine <= block.endLine && comment.endLine >= block.startLine),
    }));
  });

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
    const selTop = selectionRect.top - rootRect.top + surfaceRoot.scrollTop;
    const selBottom = selectionRect.bottom - rootRect.top + surfaceRoot.scrollTop;
    const selCenterX = selectionRect.left + selectionRect.width / 2 - rootRect.left + surfaceRoot.scrollLeft;
    const triggerLeft = Math.min(
      Math.max(selCenterX, 24),
      Math.max(rootRect.width - 24, 24),
    );
    const composerHalf = COMPOSER_WIDTH / 2;
    const minComposerX = composerHalf + 12;
    const maxComposerX = Math.max(rootRect.width - composerHalf - 12, minComposerX);
    pendingSelection = {
      text: selectedText,
      startLine: lineRange.startLine,
      endLine: lineRange.endLine,
      // Clamp trigger to keep it on-screen if the selection sits near
      // the top: the button is anchored bottom-center, so a triggerTop
      // below ~40px guarantees the icon + arrow stay visible.
      triggerTop: Math.max(selTop - TRIGGER_GAP, 40),
      triggerLeft,
      composerTop: selBottom + TRIGGER_GAP,
      composerLeft: Math.min(Math.max(selCenterX, minComposerX), maxComposerX),
    };
    composerOpen = false;
  }

  function clearSelection(): void {
    pendingSelection = null;
    commentBody = '';
    composerOpen = false;
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

  function handleDocumentPointerDown(event: PointerEvent): void {
    if (!pendingSelection || !surfaceRoot) return;
    if (surfaceRoot.contains(event.target as Node)) return;
    clearSelection();
  }

  function handleSelectionChange(): void {
    // Once the user has committed to commenting, the textarea takes
    // focus and the body selection naturally collapses — leave the
    // composer alone in that case.
    if (composerOpen || !pendingSelection) return;
    const selection = window.getSelection();
    if (!selection || selection.isCollapsed) {
      pendingSelection = null;
    }
  }

  $effect(() => {
    document.addEventListener('pointerdown', handleDocumentPointerDown);
    document.addEventListener('mouseup', handleMouseUp);
    document.addEventListener('selectionchange', handleSelectionChange);
    return () => {
      document.removeEventListener('pointerdown', handleDocumentPointerDown);
      document.removeEventListener('mouseup', handleMouseUp);
      document.removeEventListener('selectionchange', handleSelectionChange);
    };
  });

</script>

<div
  bind:this={surfaceRoot}
  class="relative"
  role="region"
  aria-label="Selectable proposed plan"
>
  <div class="space-y-3">
    {#each blockCommentViews as view (view.block.id)}
      {@const block = view.block}
      <div>
        <div
          data-plan-source-block
          data-line-start={block.startLine}
          data-line-end={block.endLine}
          class={[
            'rounded-md px-2 py-1 -mx-2 transition-colors',
            view.highlighted ? 'bg-accent/5' : '',
          ].join(' ')}
        >
          <ChatMarkdown source={block.markdown} {workspacePath} class="select-text" />
        </div>
        {#if view.anchoredComments.length > 0}
          <div class="mt-2 space-y-2 pl-3">
            {#each view.anchoredComments as comment (comment.id)}
              <div
                class={[
                  'border-l-2 pl-3 py-0.5 text-[12px]',
                  comment.status === 'draft'
                    ? 'border-accent/60'
                    : comment.status === 'sent'
                      ? 'border-success/40'
                      : 'border-border-subtle',
                ].join(' ')}
                data-testid="plan-comment"
              >
                <div class="mb-1 flex items-center justify-between gap-2">
                  <span class={[
                    'text-[10px] font-medium uppercase tracking-wide',
                    comment.status === 'resolved'
                      ? 'text-fg-hint'
                      : comment.status === 'sent'
                        ? 'text-success'
                        : 'text-accent',
                  ].join(' ')}>
                    {comment.status === 'resolved' ? 'Resolved' : comment.status === 'sent' ? 'Sent' : 'Draft'}
                  </span>
                  {#if comment.status === 'draft'}
                    <div class="flex items-center gap-0.5">
                      <IconButton label="Edit comment" size="sm" onClick={() => beginEdit(comment)}>
                        {#snippet children()}<Icon icon={Pencil} size={12} />{/snippet}
                      </IconButton>
                      <IconButton label="Delete comment" size="sm" onClick={() => void deleteComment(comment)}>
                        {#snippet children()}<Icon icon={X} size={12} />{/snippet}
                      </IconButton>
                    </div>
                  {/if}
                </div>
                <p class="mb-1.5 line-clamp-2 italic text-[11px] text-fg-muted">
                  "{comment.selectedText}"
                </p>
                {#if editingCommentId === comment.id && comment.status === 'draft'}
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
    {/each}
  </div>

  {#if pendingSelection && !composerOpen}
    <button
      type="button"
      aria-label="Comment on selection"
      title="Comment on selection"
      class="absolute z-10 inline-flex items-center justify-center rounded-md border border-border bg-surface-2 p-2 text-text-secondary shadow-menu transition-colors cursor-pointer hover:text-text-primary focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/50"
      style={`top: ${pendingSelection.triggerTop}px; left: ${pendingSelection.triggerLeft}px; transform: translate(-50%, -100%);`}
      onpointerdown={(e) => e.preventDefault()}
      onclick={() => (composerOpen = true)}
      data-testid="plan-comment-trigger"
    >
      <Icon icon={MessageSquarePlus} size={14} />
      <!-- Border arrow: slightly larger triangle in the border color
           sits behind the fill to give the popup's bottom edge a
           continuous outline. -->
      <span
        class="pointer-events-none absolute left-1/2 top-full h-0 w-0 -translate-x-1/2 border-x-[7px] border-t-[7px] border-x-transparent border-t-border"
        aria-hidden="true"
      ></span>
      <!-- Fill arrow: matches the popup background and sits 1px above
           the border arrow so the seam between popup and arrow is
           covered. -->
      <span
        class="pointer-events-none absolute left-1/2 top-full h-0 w-0 -translate-x-1/2 -translate-y-px border-x-[6px] border-t-[6px] border-x-transparent border-t-surface-2"
        aria-hidden="true"
      ></span>
    </button>
  {:else if pendingSelection}
    <div
      class="absolute z-10 w-[18rem] rounded-md border border-border bg-surface-1 p-2.5 shadow-menu"
      style={`top: ${pendingSelection.composerTop}px; left: ${pendingSelection.composerLeft}px; transform: translate(-50%, 0);`}
      data-testid="plan-comment-composer"
    >
      <p class="mb-1 line-clamp-2 italic text-[11px] text-fg-muted">"{pendingSelection.text}"</p>
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
