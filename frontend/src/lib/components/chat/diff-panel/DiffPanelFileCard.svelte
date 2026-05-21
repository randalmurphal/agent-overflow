<script lang="ts">
  import { untrack } from 'svelte';
  import ChevronDown from 'lucide-svelte/icons/chevron-down';
  import MessageSquarePlus from 'lucide-svelte/icons/message-square-plus';
  import Plus from 'lucide-svelte/icons/plus';
  import Icon from '../../primitives/Icon.svelte';
  import EditorLink from '../../common/EditorLink.svelte';
  import DiffLineContent from '../DiffLineContent.svelte';
  import { dispatchInlineFileTokens } from '../diffInlineTokenize';
  import { getDiffTheme } from '../../../stores/diffTheme.svelte';
  import type { DiffTheme } from '../../../utils/diffHighlighterPool';
  import { languageFromPath } from '../../../utils/diffLanguage';
  import {
    buildPatchDisplayRows,
    buildSplitDisplayRows,
    type PatchDisplayRow,
    type PatchFile,
    type PatchLine,
  } from '../../../utils/patchFiles';
  import { lineTintClass } from '../../../utils/diffLineTint';
  import type { LineToken } from '../../../utils/tokenCache';
  import { getCachedTokensForLine } from '../../../utils/tokenCacheReactive.svelte';
  import type { DiffReviewComment, DiffReviewCommentInput, DiffReviewScope } from '../../../types/models';

  interface CommentAnchor {
    filePath: string;
    oldLine: number;
    newLine: number;
    side: DiffReviewComment['side'];
    selectedText: string;
  }

  interface Props {
    file: PatchFile;
    open: boolean;
    /** Owning thread id — partitions the Shiki token cache so a
     *  thread switch's eviction in `clearTokensForThread` drops only
     *  the outgoing thread's lines. */
    threadId: string;
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
    threadId,
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

  // Tokenization gated on `open`: workspace diffs commonly have 50+
  // files closed by default, so dispatching for unopened files would
  // be wasted work. The shared `dispatchInlineFileTokens` writes into
  // the (theme, threadId, lang, lineHash)-partitioned cache, and
  // `getCachedTokensForLine` reads back through a reactive generation
  // counter so tokens fade in as the worker resolves.
  let theme: DiffTheme = $derived(getDiffTheme());
  let lang = $derived(languageFromPath(file.path));

  $effect(() => {
    if (!open) return;
    const themeNow = theme;
    const langNow = lang;
    const idNow = threadId;
    const linesNow = file.lines;
    untrack(() => {
      void dispatchInlineFileTokens(linesNow, idNow, langNow, themeNow);
    });
  });

  function getTokens(line: PatchLine): LineToken[] | null {
    return getCachedTokensForLine(line, threadId, theme, lang);
  }

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
      return { filePath: file.path, oldLine: row.oldLine, newLine: 0, side: 'old', selectedText };
    }
    if (row.side === 'new') {
      return { filePath: file.path, oldLine: 0, newLine: row.newLine, side: 'new', selectedText };
    }
    return { filePath: file.path, oldLine: row.oldLine, newLine: row.newLine, side: 'context', selectedText };
  }

  function fileAnchor(): CommentAnchor {
    return { filePath: file.path, oldLine: 0, newLine: 0, side: 'file', selectedText: file.path };
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

<section class="overflow-hidden" data-testid="diff-panel-file">
  <!--
    Header: button toggles open/closed, EditorLink sibling opens the
    file in the user's editor. Same dual-control layout used by
    DiffFileBlock to avoid nested interactives. The outer card frame
    moves onto the expanded body — collapsed rows read as a clean
    path list, expanded rows carry the visible edge.
  -->
  <div class="group/diff-panel-file flex w-full items-center gap-2 rounded-[var(--radius-control)] px-2 py-1 hover:bg-surface-2/40">
    <button
      class="flex flex-1 min-w-0 items-center gap-2 text-left bg-transparent border-0 p-0 cursor-pointer"
      onclick={onToggle}
      data-testid="diff-panel-file-toggle"
      data-path={file.path}
    >
      <Icon icon={ChevronDown} size={12} class={open ? '' : '-rotate-90'} />
      <span class="min-w-0 truncate font-mono text-[12px] text-fg">{file.path}</span>
      <span class="shrink-0 text-[11px] text-success">+{file.additions}</span>
      <span class="shrink-0 text-[11px] text-error">-{file.deletions}</span>
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
        class="mt-1 rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/80 px-3 py-2"
        onsubmit={saveComment}
      >
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
      <div class="mt-1 rounded-[var(--radius-control)] border border-border-subtle bg-surface-1/50 px-3 py-1.5 text-[11px] text-fg-muted">
        {fileComments.length} file comment{fileComments.length === 1 ? '' : 's'}
      </div>
    {/if}
    {#if viewMode === 'split' && splitRows}
      <div class="mt-1 max-h-[42rem] overflow-auto rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 font-mono text-[12px] leading-relaxed">
        {#each splitRows as row}
          <div class="grid grid-cols-2 border-b border-border-subtle/40 last:border-b-0">
            <div class="min-w-0 border-r border-border-subtle/50 {splitCellClass(row.left)}">
              {#if row.left}
                {@const left = row.left}
                {@render diffLineRow(left, left.oldLine, wordWrap, commentable, () => startComment(rowAnchor(left)))}
              {:else}
                <div class="px-3 py-0.5 text-fg-muted/40">&nbsp;</div>
              {/if}
            </div>
            <div class="min-w-0 {splitCellClass(row.right)}">
              {#if row.right}
                {@const right = row.right}
                {@render diffLineRow(right, right.newLine, wordWrap, commentable, () => startComment(rowAnchor(right)))}
              {:else}
                <div class="px-3 py-0.5 text-fg-muted/40">&nbsp;</div>
              {/if}
            </div>
          </div>
          {#if draftAnchor && (row.left && anchorKey(draftAnchor) === anchorKey(rowAnchor(row.left)) || row.right && anchorKey(draftAnchor) === anchorKey(rowAnchor(row.right)))}
            {@render commentForm()}
          {/if}
        {/each}
      </div>
    {:else}
      <div class="mt-1 max-h-[42rem] overflow-auto rounded-[var(--radius-control)] border border-border-subtle bg-surface-0 font-mono text-[12px] leading-relaxed">
        {#each displayRows as row (row.id)}
          {@render diffLineRow(row, row.newLine || row.oldLine, wordWrap, commentable, () => startComment(rowAnchor(row)))}
          {#if draftAnchor && anchorKey(draftAnchor) === anchorKey(rowAnchor(row))}
            {@render commentForm()}
          {/if}
        {/each}
      </div>
    {/if}
  {/if}
</section>

{#snippet diffLineRow(row: PatchDisplayRow, displayLine: number, wordWrap: boolean, commentable: boolean, onComment: () => void)}
  <!--
    Single line-number column flush with the card's left edge. On hover (or
    keyboard focus inside the row) the line number fades and the "+" button
    paints in its place — same cell, no separate gutter.
  -->
  <div class="group/diff-line relative grid grid-cols-[2rem_minmax(0,1fr)] items-start {lineTintClass(row.line.type)}">
    <div class="relative h-full">
      <div class="select-none px-1 py-0.5 text-right text-[10px] tabular-nums text-fg-subtle/65 transition-opacity {commentable ? 'group-hover/diff-line:opacity-0 group-focus-within/diff-line:opacity-0' : ''}">{displayLine || ''}</div>
      {#if commentable}
        <button
          type="button"
          class="group/diff-add absolute inset-0 flex items-center justify-end pr-0.5 opacity-0 transition-opacity group-hover/diff-line:opacity-100 focus-visible:opacity-100"
          title="Add comment"
          aria-label="Add comment"
          onclick={onComment}
        >
          <span class="flex size-4 items-center justify-center rounded border border-accent/45 bg-surface-1 text-accent shadow-sm transition-colors group-hover/diff-add:bg-surface-2 group-focus-visible/diff-add:bg-surface-2">
            <Icon icon={Plus} size={10} strokeWidth={2.5} />
          </span>
        </button>
      {/if}
    </div>
    <pre class="min-w-0 px-2 py-0.5 {wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre'}"><DiffLineContent line={row.line} tokens={getTokens(row.line)} /></pre>
  </div>
{/snippet}

{#snippet commentForm()}
  <form class="border-y border-border-subtle bg-surface-1/90 px-3 py-2" onsubmit={saveComment}>
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
