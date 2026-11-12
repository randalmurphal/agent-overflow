<script lang="ts">
  import type { Snippet } from 'svelte';
  import ReviewFileHeaderRow from './ReviewFileHeaderRow.svelte';
  import ReviewLineBlockRow from './ReviewLineBlockRow.svelte';
  import type { ReviewThread } from '../../types/models';
  import type { ExpandDirection } from '../../utils/diffContextExpansion';
  import type { PaintedSpans, PatchScopeContext } from '../../utils/diffSpanCache.svelte';
  import type { DiffGap } from '../../utils/patchFiles';
  import type { ReviewFile } from '../../utils/patchStore';
  import { REVIEW_SURFACE_END_PX, type BlockRowsCache, type CommentAnchor, type EditorRow, type ReviewRow } from '../../utils/reviewRows';

  // One row of ReviewDiffBody's list (utils/reviewRows.ts kinds). The body
  // owns the list, scrolling and the jump pulse; a row only renders.

  interface Props {
    row: ReviewRow;
    /** The row's file; undefined while the build and props race. */
    file: ReviewFile | undefined;
    blockRows: BlockRowsCache;
    collapsed: boolean;
    onToggleCollapsed: (path: string) => void;
    spanOwner: string;
    spanContext: PatchScopeContext | null;
    painted: PaintedSpans | null;
    wordWrap: boolean;
    /** The jump that landed on this row (utils/reviewJumpPulse); 0 for none. */
    pulse: number;
    /** Rows of this file a detached editor suggests, marked as targets. */
    targets?: readonly CommentAnchor[];
    overview?: Snippet;
    draftEditor?: Snippet<[editor: EditorRow]>;
    commentThread?: Snippet<[threadKey: string, anchor: CommentAnchor, pulse: number]>;
    prThread?: Snippet<[thread: ReviewThread, anchor: CommentAnchor, collapsed: boolean, orphaned: boolean, pulse: number]>;
    onAddComment?: (anchor: CommentAnchor) => void;
    onExpandFold?: (path: string, foldId: number) => void;
    onExpandGap?: (path: string, gap: DiffGap, dir: ExpandDirection) => void;
  }

  let {
    row, file, blockRows, collapsed, onToggleCollapsed, spanOwner, spanContext, painted, wordWrap, pulse, targets,
    overview, draftEditor, commentThread, prThread, onAddComment, onExpandFold, onExpandGap,
  }: Props = $props();

  // Per-file gutter width (ch) from the file's max line number — a true
  // max over every row, not the last numbered row: a merged edits-scope
  // file whose sections couldn't be renumbered (overlap fallback in
  // mergePatchFilesByPath) renders sections in edit order, where
  // numbering is non-monotonic.
  function gutterChars(of: ReviewFile): number {
    return Math.max(2, String(of.body.maxLine).length);
  }
</script>

{#if row.kind === 'overview'}
  {@render overview?.()}
{:else if row.kind === 'gone-group'}
  <!-- Editors whose file is not in the diff: kept to copy or discard. -->
  <div class="mx-[var(--review-slab-inset)] mt-2 rounded-t-[var(--radius-control)] border-x border-t border-border-subtle bg-surface-1 px-3 py-1.5 text-[0.6875rem] font-medium text-fg-muted" data-testid="review-gone-editors">
    Comments on files not in this diff
  </div>
{:else if row.kind === 'draft-editor' && row.fileIndex < 0}
  <div class="mx-[var(--review-slab-inset)] border-x border-border-subtle bg-surface-1">
    {@render draftEditor?.(row.editor)}
  </div>
{:else if !file}
  <!-- Build/props raced; the next flush re-renders coherent rows. -->
{:else if row.kind === 'file-header'}
  <ReviewFileHeaderRow
    {file}
    {collapsed}
    onToggle={() => onToggleCollapsed(file.path)}
    first={row.fileIndex === 0}
  />
{:else if row.kind === 'surface-end'}
  <!-- Closing cap for the last file's slab (exact height:
       REVIEW_SURFACE_END_PX; box-border keeps the border inside it). -->
  <div style:height="{REVIEW_SURFACE_END_PX}px">
    <div class="mx-[var(--review-slab-inset)] h-full rounded-b-[var(--radius-control)] border-x border-b border-border-subtle bg-surface-1"></div>
  </div>
{:else if row.kind === 'line-block'}
  {@const materialized = blockRows.get(file, row)}
  <ReviewLineBlockRow
    rows={materialized.rows}
    splitRows={materialized.splitRows}
    {file}
    path={file.path}
    {spanOwner}
    {spanContext}
    {painted}
    {wordWrap}
    gutterCh={gutterChars(file)}
    {targets}
    {onAddComment}
    {onExpandFold}
    {onExpandGap}
  />
{:else if row.kind === 'draft-editor'}
  <!-- The slab inset + border-x + bg-surface-1 keep the file slab
       continuous behind the inserted rows (the page behind is darker
       surface-0). -->
  <div class="mx-[var(--review-slab-inset)] border-x border-border-subtle bg-surface-1">
    {@render draftEditor?.(row.editor)}
  </div>
{:else if row.kind === 'comment-thread'}
  <div class="mx-[var(--review-slab-inset)] border-x border-border-subtle bg-surface-1">
    {@render commentThread?.(row.threadKey, row.anchor, pulse)}
  </div>
{:else}
  <div class="mx-[var(--review-slab-inset)] border-x border-border-subtle bg-surface-1">
    {@render prThread?.(row.thread, row.anchor, row.collapsed, row.orphaned, pulse)}
  </div>
{/if}
