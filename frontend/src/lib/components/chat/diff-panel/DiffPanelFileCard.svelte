<script lang="ts">
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import MessageSquarePlus from 'lucide-svelte/icons/message-square-plus';
  import Plus from 'lucide-svelte/icons/plus';
  import Icon from '../../primitives/Icon.svelte';
  import EditorLink from '../../common/EditorLink.svelte';
  import {
    buildPatchDisplayRows,
    buildSplitDisplayRows,
    type PatchDisplayRow,
    type PatchFile,
    type SplitDisplayRow,
  } from '../../../utils/patchFiles';
  import { lineTintClass } from '../../../utils/diffLineTint';
  import type { DiffReviewComment, DiffReviewCommentInput, DiffReviewScope } from '../../../types/models';

  interface CommentAnchor {
    filePath: string;
    oldLine: number;
    newLine: number;
    side: DiffReviewComment['side'];
    selectedText: string;
    label: string;
  }

  interface Props {
    file: PatchFile;
    open: boolean;
    /** Absolute base directory for resolving the repo-relative
     *  `file.path` when the user hits the editor-link affordance. */
    workspacePath: string;
    viewMode: 'stacked' | 'split';
    wordWrap: boolean;
    commentable?: boolean;
    reviewScope?: DiffReviewScope | null;
    sourceKey?: string;
    comments?: readonly DiffReviewComment[];
    onToggle: () => void;
    onCreateComment?: (input: DiffReviewCommentInput) => Promise<void>;
  }

  let {
    file,
    open,
    workspacePath,
    viewMode,
    wordWrap,
    commentable = false,
    reviewScope = null,
    sourceKey = '',
    comments = [],
    onToggle,
    onCreateComment,
  }: Props = $props();

  let draftAnchor: CommentAnchor | null = $state(null);
  let draftBody = $state('');
  let saving = $state(false);

  // Line numbers come from hunk headers in file.lines; displayRows
  // intentionally omits diff metadata while retaining real old/new
  // anchors for review comments.
  const displayRows = $derived(buildPatchDisplayRows(file.lines));
  // buildSplitDisplayRows is pure over displayRows, so $derived gives us a
  // per-instance cache that invalidates when the diff text changes.
  const splitRows = $derived(viewMode === 'split' && open ? buildSplitDisplayRows(displayRows) : null);
  const commentsByAnchor = $derived.by(() => {
    const index = new Map<string, DiffReviewComment[]>();
    for (const comment of comments) {
      if (comment.filePath !== file.path) continue;
      const key = anchorKey(comment);
      const bucket = index.get(key);
      if (bucket) bucket.push(comment);
      else index.set(key, [comment]);
    }
    return index;
  });
  const fileComments = $derived(commentsByAnchor.get('file:0:0') ?? []);

  function splitCellClass(row: PatchDisplayRow | null): string {
    if (!row) return 'text-fg-muted/40';
    return lineTintClass(row.line.type);
  }

  function anchorKey(anchor: Pick<DiffReviewComment, 'side' | 'oldLine' | 'newLine'>): string {
    return `${anchor.side}:${anchor.oldLine || 0}:${anchor.newLine || 0}`;
  }

  function rowAnchor(row: PatchDisplayRow): CommentAnchor {
    const selectedText = row.line.content;
    if (row.side === 'old') {
      return {
        filePath: file.path,
        oldLine: row.oldLine,
        newLine: 0,
        side: 'old',
        selectedText,
        label: `${file.path}:${row.oldLine}`,
      };
    }
    if (row.side === 'new') {
      return {
        filePath: file.path,
        oldLine: 0,
        newLine: row.newLine,
        side: 'new',
        selectedText,
        label: `${file.path}:${row.newLine}`,
      };
    }
    return {
      filePath: file.path,
      oldLine: row.oldLine,
      newLine: row.newLine,
      side: 'context',
      selectedText,
      label: `${file.path}:${row.newLine || row.oldLine}`,
    };
  }

  function fileAnchor(): CommentAnchor {
    return {
      filePath: file.path,
      oldLine: 0,
      newLine: 0,
      side: 'file',
      selectedText: file.path,
      label: file.path,
    };
  }

  function commentsForRow(row: PatchDisplayRow): DiffReviewComment[] {
    return commentsByAnchor.get(anchorKey(rowAnchor(row))) ?? [];
  }

  function startComment(anchor: CommentAnchor): void {
    if (!commentable || !reviewScope || !sourceKey) return;
    draftAnchor = anchor;
    draftBody = '';
  }

  async function saveComment(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (!draftAnchor || !reviewScope || !sourceKey || !onCreateComment || saving) return;
    const body = draftBody.trim();
    if (!body) return;
    saving = true;
    try {
      await onCreateComment({
        scope: reviewScope,
        sourceKey,
        filePath: draftAnchor.filePath,
        oldLine: draftAnchor.oldLine || undefined,
        newLine: draftAnchor.newLine || undefined,
        side: draftAnchor.side,
        selectedText: draftAnchor.selectedText,
        body,
      });
      draftAnchor = null;
      draftBody = '';
    } catch (err) {
      console.error('Failed to save diff review comment:', err);
    } finally {
      saving = false;
    }
  }

  function cancelComment(): void {
    draftAnchor = null;
    draftBody = '';
  }
</script>

<section class="overflow-hidden rounded-[var(--radius-control)] border border-border-subtle bg-card/30">
  <!--
    Header: button toggles open/closed, EditorLink sibling opens the
    file in the user's editor. Same dual-control layout used by
    DiffFileBlock to avoid nested interactives.
  -->
  <div class="group/diff-panel-file flex w-full items-center gap-2 px-3 py-2 hover:bg-surface-2/40">
    <button
      class="flex flex-1 min-w-0 items-center gap-2 text-left bg-transparent border-0 p-0 cursor-pointer"
      onclick={onToggle}
      data-testid="diff-panel-file-toggle"
      data-path={file.path}
    >
      <Icon icon={ChevronDown} size={14} class={open ? '' : '-rotate-90'} />
      <span class="rounded bg-accent/15 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-[0.08em] text-accent">FileChange</span>
      <span class="min-w-0 flex-1 truncate font-mono text-[12px] text-fg">{file.path}</span>
      <span class="text-[11px] text-success">+{file.additions}</span>
      <span class="text-[11px] text-error">-{file.deletions}</span>
    </button>
    {#if commentable}
      <button
        type="button"
        class="rounded border border-border-subtle p-1 text-fg-muted opacity-0 transition hover:border-accent/50 hover:text-accent group-hover/diff-panel-file:opacity-100 focus-visible:opacity-100"
        title="Comment on file"
        aria-label="Comment on file"
        onclick={() => startComment(fileAnchor())}
      >
        <Icon icon={MessageSquarePlus} size={14} />
      </button>
    {/if}
    <EditorLink
      path={file.path}
      {workspacePath}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/diff-panel-file:opacity-100 focus-visible:opacity-100"
    />
  </div>
  {#if open}
    {#if draftAnchor?.side === 'file'}
      <form
        class="border-t border-border-subtle bg-surface-1/80 px-3 py-2"
        onsubmit={saveComment}
      >
        <div class="mb-1 font-mono text-[11px] text-fg-muted">{draftAnchor.label}</div>
        <textarea
          bind:value={draftBody}
          rows="2"
          class="w-full resize-none rounded border border-border-subtle bg-surface-0 px-2 py-1.5 text-[12px] text-fg focus:border-accent/60 focus:outline-none"
          placeholder="Comment on this file"
        ></textarea>
        <div class="mt-2 flex justify-end gap-2">
          <button type="button" class="rounded px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={cancelComment}>Cancel</button>
          <button type="submit" disabled={saving || !draftBody.trim()} class="rounded bg-accent px-2 py-1 text-[11px] font-medium text-accent-contrast disabled:opacity-45">Add comment</button>
        </div>
      </form>
    {/if}
    {#if fileComments.length > 0}
      <div class="border-t border-border-subtle bg-surface-1/50 px-3 py-1.5 text-[11px] text-fg-muted">
        {fileComments.length} file comment{fileComments.length === 1 ? '' : 's'}
      </div>
    {/if}
    {#if viewMode === 'split' && splitRows}
      <div class="max-h-[42rem] overflow-auto border-t border-border-subtle bg-surface-0 font-mono text-[12px] leading-relaxed">
        {#each splitRows as row}
          <div class="grid grid-cols-2 border-b border-border-subtle/40 last:border-b-0">
            <div class="min-w-0 border-r border-border-subtle/50 {splitCellClass(row.left)}">
              {#if row.left}
                {@const left = row.left}
                {@render diffLineRow(left, wordWrap, commentable, commentsForRow(left), () => startComment(rowAnchor(left)))}
              {:else}
                <div class="px-3 py-0.5 text-fg-muted/40">&nbsp;</div>
              {/if}
            </div>
            <div class="min-w-0 {splitCellClass(row.right)}">
              {#if row.right}
                {@const right = row.right}
                {@render diffLineRow(right, wordWrap, commentable, commentsForRow(right), () => startComment(rowAnchor(right)))}
              {:else}
                <div class="px-3 py-0.5 text-fg-muted/40">&nbsp;</div>
              {/if}
            </div>
          </div>
          {#if draftAnchor && (row.left && anchorKey(draftAnchor) === anchorKey(rowAnchor(row.left)) || row.right && anchorKey(draftAnchor) === anchorKey(rowAnchor(row.right)))}
            {@render commentForm(draftAnchor)}
          {/if}
        {/each}
      </div>
    {:else}
      <div class="max-h-[42rem] overflow-auto border-t border-border-subtle bg-surface-0 font-mono text-[12px] leading-relaxed">
        {#each displayRows as row (row.id)}
          {@render diffLineRow(row, wordWrap, commentable, commentsForRow(row), () => startComment(rowAnchor(row)))}
          {#if draftAnchor && anchorKey(draftAnchor) === anchorKey(rowAnchor(row))}
            {@render commentForm(draftAnchor)}
          {/if}
        {/each}
      </div>
    {/if}
  {/if}
</section>

{#snippet diffLineRow(row: PatchDisplayRow, wordWrap: boolean, commentable: boolean, comments: DiffReviewComment[], onComment: () => void)}
  <div class="group/diff-line grid grid-cols-[1.75rem_2.5rem_2.5rem_minmax(0,1fr)_auto] items-start {lineTintClass(row.line.type)}">
    <div class="flex h-full items-center justify-center">
      {#if commentable}
        <button
          type="button"
          class="m-0.5 flex size-5 items-center justify-center rounded border border-accent/45 bg-surface-1 text-accent opacity-0 shadow-sm transition group-hover/diff-line:opacity-100 focus-visible:opacity-100"
          title="Add comment"
          aria-label="Add comment"
          onclick={onComment}
        >
          <Icon icon={Plus} size={12} strokeWidth={2.5} />
        </button>
      {/if}
    </div>
    <div class="select-none px-1 py-0.5 text-right text-[10px] tabular-nums text-fg-subtle/65">{row.oldLine || ''}</div>
    <div class="select-none px-1 py-0.5 text-right text-[10px] tabular-nums text-fg-subtle/65">{row.newLine || ''}</div>
    <pre class="min-w-0 px-2 py-0.5 {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'}">{row.line.content}</pre>
    <div class="min-w-8 px-2 py-0.5 text-right">
      {#if comments.length > 0}
        <span class="rounded-full border border-accent/35 bg-accent/12 px-1.5 text-[10px] text-accent">{comments.length}</span>
      {/if}
    </div>
  </div>
{/snippet}

{#snippet commentForm(anchor: CommentAnchor)}
  <form class="border-y border-border-subtle bg-surface-1/90 px-9 py-2" onsubmit={saveComment}>
    <div class="mb-1 font-mono text-[11px] text-fg-muted">{anchor.label}</div>
    <textarea
      bind:value={draftBody}
      rows="2"
      class="w-full resize-none rounded border border-border-subtle bg-surface-0 px-2 py-1.5 text-[12px] text-fg focus:border-accent/60 focus:outline-none"
      placeholder="Comment on this line"
    ></textarea>
    <div class="mt-2 flex justify-end gap-2">
      <button type="button" class="rounded px-2 py-1 text-[11px] text-fg-muted hover:bg-surface-2" onclick={cancelComment}>Cancel</button>
      <button type="submit" disabled={saving || !draftBody.trim()} class="rounded bg-accent px-2 py-1 text-[11px] font-medium text-accent-contrast disabled:opacity-45">Add comment</button>
    </div>
  </form>
{/snippet}
