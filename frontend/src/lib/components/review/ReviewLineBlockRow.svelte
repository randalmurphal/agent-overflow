<script lang="ts">
  import { untrack } from 'svelte';
  import DiffLineContent from '../chat/DiffLineContent.svelte';
  import type { CommentAnchor, EditorAnchor } from '../../stores/reviewPane.svelte';
  import { rowMatchesAnchor, type RowType } from '../../utils/patchRows';
  import { DIFF_CONTEXT_EXPAND_STEP, type ExpandDirection } from '../../utils/diffContextExpansion';
  import { gutterTintClass, lineTintClass } from '../../utils/diffLineTint';
  import { diffSpanCacheEvictionGeneration, diffSpanCacheGeneration, getSpansForReviewFile, hasIncompleteSpansForReviewFile, requestReviewFileSpans, resolveReviewLineSpan, type PaintedSpans, type PatchScopeContext } from '../../utils/diffSpanCache.svelte';
  import { stripPatchLinePrefix, type DiffGap, type PatchDisplayRow, type SplitDisplayRow } from '../../utils/patchFiles';
  import type { ReviewFile } from '../../utils/patchParser';
  import { REVIEW_LINE_HEIGHT_PX } from '../../utils/reviewRows';
  import { findLineKey } from '../../utils/reviewFindIdentity';
  import { delegatedSeenActivation } from '../../utils/seenActivation';
  import { rowInRange, type LineRange } from '../../utils/reviewRange';
  import type { LineCommentGate } from '../../stores/reviewRangeComments';

  // One review line block (≤ REVIEW_LINE_BLOCK_MAX_LINES display rows).
  //
  // Exact-height contract: with word wrap OFF every visual line renders
  // at exactly REVIEW_LINE_HEIGHT_PX (h-5 + leading-5, no vertical
  // padding or borders anywhere in the block) so the block's height is
  // rows.length × 20 and no ResizeObserver is attached. With word wrap
  // ON lines wrap freely and the virtualizer measures the block.
  //
  // Highlighting mirrors DiffFileBlock: one fire-and-forget file-level
  // span request per block (the shared cache dedupes across the file's
  // blocks); rows re-render as spans land. Until then a line shows the
  // colors its text was last painted with in this file (`painted`), or
  // plain tinted text. Gap expansion produces a new body → new content
  // key → re-request; the backend's cache absorbs the overlap.

  interface Props {
    /** The block's rows, materialized from the file's body by the
     * surface (BlockRowsCache). */
    rows: PatchDisplayRow[];
    /** Present in split view mode. */
    splitRows?: SplitDisplayRow[];
    /** The owning file — span requests are file-level. */
    file: ReviewFile;
    path: string;
    /** The thread row id, or a draft placeholder's synthetic one. It owns
     * this file's span-cache entries for eviction; the RPC subject comes
     * from `spanContext`. */
    spanOwner: string;
    /** Diff view: scope fields for parse-priming file content above
     * each hunk, and the subject the priming RPC resolves it from.
     * Absent on the conflict surface, whose pseudo-files have no file
     * behind them. */
    spanContext?: PatchScopeContext | null;
    /** The surface's last painted colors (see PaintedSpans). */
    painted?: PaintedSpans | null;
    wordWrap: boolean;
    /** Line-number gutter width in ch, per file (max line number). */
    gutterCh: number;
    /** Rows a detached comment editor suggests ("Use this line"). */
    targets?: readonly CommentAnchor[];
    /** The diff on screen these rows are of (ShownDiff.generation): a
     * pick is valid only there. */
    generation?: number;
    onAddComment?: (anchor: EditorAnchor, generation?: number) => void;
    /** Shift+click on a line's add button: span the open editor to it. */
    onExtendRange?: (anchor: EditorAnchor, generation?: number) => void;
    /** A head-ending commit run: what a line takes (no add button for
     * `none`) and why, as the add button's hover text. */
    lineComment?: LineCommentGate;
    /** A right-click (a long press on compact) on a line's number gutter:
     * the line's menu. Absent, the gutter has none. */
    onLineMenu?: (event: MouseEvent, path: string, lineIndex: number) => boolean;
    /** The line ranges of this file the diff tints (open range editors,
     * range drafts, open forge threads that span lines). */
    ranges?: readonly LineRange[];
    /** Conflict view only: expands the fold row's hidden lines. */
    onExpandFold?: (path: string, foldId: number) => void;
    /** Diff view only: fetches a hunk gap's hidden context lines. */
    onExpandGap?: (path: string, gap: DiffGap, dir: ExpandDirection) => void;
  }

  let { rows, splitRows, file, path, spanOwner, spanContext = null, painted = null, wordWrap, gutterCh, targets, generation, onAddComment, onExtendRange, lineComment, onLineMenu, ranges, onExpandFold, onExpandGap }: Props = $props();

  const ROW_TYPES: Record<PatchDisplayRow['side'], RowType> = { old: 'del', new: 'add', context: 'context' };

  // Whether `row` (or its `half` in split view) is a suggested row: the
  // row the anchor names (patchRows' rowMatchesAnchor), on its half.
  function isTarget(row: PatchDisplayRow, half: 'old' | 'new' | null): boolean {
    if (!targets || targets.length === 0) return false;
    return targets.some((target) => (target.side === 'old' ? half !== 'new' : half !== 'old')
      && rowMatchesAnchor(ROW_TYPES[row.side], row.oldLine, row.newLine, target));
  }

  // Delegated activation on the block root: 4 DOM listeners per block instead
  // of 4 per line (up to 256 per split block), preserving exact seenActivation
  // press-to-click capture semantics and computing anchors lazily on demand.
  function matchAddButton(target: EventTarget | null): HTMLElement | null {
    return target instanceof Element
      ? target.closest<HTMLElement>('[data-testid="review-add-comment"]')
      : null;
  }

  function readAddActivation(button: HTMLElement): { anchor: EditorAnchor; generation: number | undefined } | null {
    const idxStr = button.dataset.rowIdx;
    if (idxStr === undefined) return null;
    const idx = Number(idxStr);
    const half = button.dataset.half;
    if (splitRows) {
      const pair = splitRows[idx];
      if (!pair) return null;
      if (half === 'old' && pair.left) return { anchor: sideAnchor(pair.left, 'old'), generation };
      if (half === 'new' && pair.right) return { anchor: sideAnchor(pair.right, 'new'), generation };
      return null;
    }
    const row = rows[idx];
    return row ? { anchor: stackedAnchor(row), generation } : null;
  }

  const blockAddComment = delegatedSeenActivation(
    matchAddButton,
    readAddActivation,
    (at, click) => {
      if (click.shiftKey && onExtendRange) onExtendRange(at.anchor, at.generation);
      else onAddComment?.(at.anchor, at.generation);
    },
  );

  function handleBlockContextMenu(event: MouseEvent): void {
    if (!onLineMenu || !(event.target instanceof Element)) return;
    const gutterEl = event.target.closest<HTMLElement>('[data-testid="review-line-gutter"]');
    if (!gutterEl) return;
    const idxStr = gutterEl.dataset.rowIdx;
    if (idxStr === undefined) return;
    const idx = Number(idxStr);
    const row = splitRows
      ? (gutterEl.dataset.half === 'old' ? splitRows[idx]?.left : splitRows[idx]?.right) ?? null
      : (rows[idx] ?? null);
    openLineMenu(event, row);
  }

  // Whether a row (or its half in split view) is inside a tinted range.
  function inRange(row: PatchDisplayRow, half: 'old' | 'new' | null): boolean {
    if (!ranges || ranges.length === 0) return false;
    return ranges.some((range) => rowInRange(row.oldLine, row.newLine, half, range));
  }
  const RANGE_CLASS = "after:pointer-events-none after:absolute after:inset-0 after:bg-accent/15 after:content-['']";

  // Resolve the file's span array once per block instead of 32–64 times per line.
  let fileSpans = $derived(getSpansForReviewFile(file, spanContext));

  $effect(() => {
    // Steady-state highlighted blocks only subscribe to eviction/reset so
    // sibling file completions do not wake them. Missing or incomplete blocks
    // also subscribe to `diffSpanCacheGeneration()` so transient RPC failures
    // and incomplete parses retry when sibling completions land.
    diffSpanCacheEvictionGeneration();
    if (fileSpans === null || hasIncompleteSpansForReviewFile(file, spanContext)) {
      diffSpanCacheGeneration();
    }
    const fileNow = file;
    const context = spanContext;
    const owner = spanOwner;
    untrack(() => {
      void requestReviewFileSpans(fileNow, owner, context);
    });
  });

  function spansForRow(row: PatchDisplayRow) {
    return resolveReviewLineSpan(file.path, row.lineIndex ?? -1, row.line, fileSpans, painted);
  }

  // Heights come from the shared constant, not rem-based Tailwind
  // classes, so the estimate table and the rendered geometry cannot
  // drift apart via root font-size.
  let lineHeight = $derived(`${REVIEW_LINE_HEIGHT_PX}px`);
  let lineClass = $derived(wordWrap ? 'flex' : 'flex overflow-hidden');
  // Wrap mode: break at word boundaries (break-all split identifiers
  // mid-token) and hang continuations 2ch past the first line, so a
  // wrapped line reads as one line rather than a ragged new row.
  let contentClass = $derived(
    wordWrap
      ? 'whitespace-pre-wrap break-words pl-[calc(0.5rem+2ch)] indent-[-2ch]'
      : 'whitespace-pre pl-2',
  );

  function stackedAnchor(row: PatchDisplayRow): EditorAnchor {
    return {
      filePath: path,
      oldLine: row.oldLine || undefined,
      newLine: row.newLine || undefined,
      side: row.side,
      selectedText: stripPatchLinePrefix(row.line),
      lineIndex: row.lineIndex,
    };
  }

  // A half of a split row; an unchanged row's half carries both numbers.
  function sideAnchor(row: PatchDisplayRow, side: 'old' | 'new'): EditorAnchor {
    const context = row.side === 'context';
    return {
      filePath: path,
      oldLine: side === 'old' || context ? row.oldLine || undefined : undefined,
      newLine: side === 'new' || context ? row.newLine || undefined : undefined,
      side,
      selectedText: stripPatchLinePrefix(row.line),
      lineIndex: row.lineIndex,
    };
  }

  // The find's key for the row's code text (utils/reviewFindIdentity.ts).
  function findKey(row: PatchDisplayRow): string | undefined {
    return row.lineIndex === undefined ? undefined : findLineKey(path, row.lineIndex);
  }

  // Split view shows a context line on both sides; find counts it once,
  // so only its new side carries the key.
  function leftFindKey(left: PatchDisplayRow, right: PatchDisplayRow | null | undefined): string | undefined {
    return right && right.lineIndex === left.lineIndex ? undefined : findKey(left);
  }

  // A line's number gutter opens the line menu (Copy link to line) where
  // the pane offers one; elsewhere the native menu stays.
  function openLineMenu(event: MouseEvent, row: PatchDisplayRow | null): void {
    if (!onLineMenu || !row || row.lineIndex === undefined || row.pending) return;
    if (onLineMenu(event, path, row.lineIndex)) event.preventDefault();
  }

  function gapLabel(gap: DiffGap): string {
    if (gap.hidden < 0) return 'unchanged lines';
    return gap.hidden === 1 ? '1 unchanged line' : `${gap.hidden} unchanged lines`;
  }

  // A gap one fetch fully covers gets a single expand-all band;
  // larger (or unknown-size) gaps step directionally, GitHub-style.
  function isSmallGap(gap: DiffGap): boolean {
    return gap.hidden >= 0 && gap.hidden <= DIFF_CONTEXT_EXPAND_STEP;
  }
</script>

<!-- Reserved action column: a real cell at the row's left edge, so
     the hover affordance never paints over the line numbers. A row whose
     text is still being read (`pending`) has no text to quote yet. -->
{#snippet actionCell(row: PatchDisplayRow, rowIdx: number, half: 'old' | 'new' | null, sideLabel: string)}
  {@const gate = lineComment ? lineComment(half ? sideAnchor(row, half) : stackedAnchor(row)) : null}
  <span class="ao-diff-action flex w-5 shrink-0 items-center justify-center">
    {#if onAddComment && !row.pending && gate?.target !== 'none'}
      <button
        type="button"
        class="ao-diff-add-btn flex size-4 items-center justify-center rounded-[3px] bg-accent text-[0.6875rem] font-semibold leading-none text-accent-fg opacity-0 focus-visible:opacity-100 compact:opacity-100 hover:brightness-110 focus:outline-none"
        aria-label={`Add ${sideLabel} comment`}
        title={gate?.reason ?? (onExtendRange ? 'Add a comment (Shift+click to span the open comment to this line)' : undefined)}
        data-comment-target={gate?.target}
        data-testid="review-add-comment"
        data-row-idx={rowIdx}
        data-half={half ?? undefined}
      >
        +
      </button>
    {/if}
  </span>
{/snippet}

{#snippet foldRow(fold: { id: number; lines: number })}
  <button
    type="button"
    class="flex w-full cursor-pointer items-center bg-surface-2/40 px-8 text-left text-fg-muted hover:bg-surface-2/70 hover:text-fg disabled:cursor-default"
    style:height={wordWrap ? undefined : lineHeight}
    data-testid="review-conflict-fold"
    aria-label="Expand {fold.lines} unchanged lines"
    disabled={!onExpandFold}
    onclick={() => onExpandFold?.(path, fold.id)}
  >
    ⋯ {fold.lines} unchanged lines
  </button>
{/snippet}

<!-- Hunk-gap band. Exact-height contract applies: the band is one
     display row at REVIEW_LINE_HEIGHT_PX, no vertical padding/borders.
     Direction semantics match GitHub: ↓ reveals the TOP of the gap
     (extends the hunk above downward), ↑ reveals the BOTTOM (extends
     the hunk below upward) — so a leading gap offers only ↑ and a
     trailing gap only ↓. -->
{#snippet gapRow(gap: DiffGap)}
  {#if isSmallGap(gap) && onExpandGap}
    <button
      type="button"
      class="flex w-full cursor-pointer items-center gap-2 bg-surface-2/50 px-8 text-left text-[0.6875rem] text-fg-muted hover:bg-surface-2/80 hover:text-fg"
      style:height={wordWrap ? undefined : lineHeight}
      data-testid="review-gap-expand-all"
      aria-label="Expand {gapLabel(gap)}"
      onclick={() => onExpandGap?.(path, gap, 'all')}
    >
      <span class="text-fg-subtle" aria-hidden="true">↕</span>
      <span>{gapLabel(gap)}</span>
    </button>
  {:else}
    <div
      class="flex w-full items-center bg-surface-2/50 text-[0.6875rem] text-fg-muted"
      style:height={wordWrap ? undefined : lineHeight}
      data-testid="review-gap"
    >
      {#if onExpandGap}
        <span class="flex shrink-0 items-center gap-0.5 pl-1.5">
          {#if gap.location !== 'trailing'}
            <button
              type="button"
              class="flex size-4 items-center justify-center rounded-[3px] text-fg-muted hover:bg-surface-2 hover:text-accent focus-visible:bg-surface-2 focus:outline-none"
              aria-label="Expand up"
              data-testid="review-gap-expand-up"
              onclick={() => onExpandGap?.(path, gap, 'up')}
            >↑</button>
          {/if}
          {#if gap.location !== 'leading'}
            <button
              type="button"
              class="flex size-4 items-center justify-center rounded-[3px] text-fg-muted hover:bg-surface-2 hover:text-accent focus-visible:bg-surface-2 focus:outline-none"
              aria-label="Expand down"
              data-testid="review-gap-expand-down"
              onclick={() => onExpandGap?.(path, gap, 'down')}
            >↓</button>
          {/if}
        </span>
      {/if}
      <span class="pl-3">⋯ {gapLabel(gap)}</span>
    </div>
  {/if}
{/snippet}

{#snippet gutter(lineNo: number)}
  <span
    class="shrink-0 select-none pr-1 text-right tabular-nums"
    style:width="{gutterCh + 1}ch"
    aria-hidden="true"
  >{lineNo > 0 ? lineNo : ''}</span>
{/snippet}

<!-- `hoverRow` composes with the tint backgrounds via a pointer-inert
     ::before overlay — a hover bg class would just replace the add/del
     wash instead of layering on it. -->
<!-- Slab inset + border-x: each file renders as an inset card slab on the
     darker page background. Horizontal only — the exact-height contract
     allows no vertical borders here. -->
<!-- svelte-ignore a11y_no_static_element_interactions (delegated contextmenu for line gutters; line has no keyboard focus to open it from) -->
<div
  class="mx-[var(--review-slab-inset)] border-x border-border-subtle bg-surface-1 font-mono text-xs text-fg"
  style:line-height={lineHeight}
  data-testid="review-line-block"
  data-path={path}
  {@attach blockAddComment}
  oncontextmenu={handleBlockContextMenu}
>
  {#if splitRows}
    {#each splitRows as pair, pairIndex (pairIndex)}
      {#if pair.left?.line.fold}
        <!-- Fold rows span both sides (buildSplitDisplayRows mirrors
             context-like rows, so left === right here). -->
        {@render foldRow(pair.left.line.fold)}
      {:else if pair.left?.gap}
        <!-- Gap bands span both sides too (context-like mirror row). -->
        {@render gapRow(pair.left.gap)}
      {:else}
      {@const leftTarget = pair.left ? isTarget(pair.left, 'old') : false}
      {@const leftRange = pair.left ? inRange(pair.left, 'old') : false}
      {@const rightTarget = pair.right ? isTarget(pair.right, 'new') : false}
      {@const rightRange = pair.right ? inRange(pair.right, 'new') : false}
      <div class={lineClass} style:height={wordWrap ? undefined : lineHeight}>
        <div class="ao-diff-line relative flex w-1/2 min-w-0 {pair.left ? lineTintClass(pair.left.line.type) : 'bg-surface-0/40'} {leftRange ? RANGE_CLASS : ''}" data-review-range={leftRange ? '' : undefined} data-review-target={leftTarget ? '' : undefined}>
          {#if pair.left}
            {@render actionCell(pair.left, pairIndex, 'old', 'old-line')}
            <span class="flex shrink-0 {gutterTintClass(pair.left.line.type)}" data-testid="review-line-gutter" data-row-idx={pairIndex} data-half="old">{@render gutter(pair.left.oldLine)}</span>
            <span class="min-w-0 flex-1 {contentClass} pr-2" data-find-key={leftFindKey(pair.left, pair.right)}><DiffLineContent line={pair.left.line} spans={spansForRow(pair.left)} intraline={pair.left.intraline ?? null} /></span>
          {/if}
        </div>
        <div class="ao-diff-line relative flex w-1/2 min-w-0 border-l border-border-subtle {pair.right ? lineTintClass(pair.right.line.type) : 'bg-surface-0/40'} {rightRange ? RANGE_CLASS : ''}" data-review-range={rightRange ? '' : undefined} data-review-target={rightTarget ? '' : undefined}>
          {#if pair.right}
            {@render actionCell(pair.right, pairIndex, 'new', 'new-line')}
            <span class="flex shrink-0 {gutterTintClass(pair.right.line.type)}" data-testid="review-line-gutter" data-row-idx={pairIndex} data-half="new">{@render gutter(pair.right.newLine)}</span>
            <span class="min-w-0 flex-1 {contentClass} pr-2" data-find-key={findKey(pair.right)}><DiffLineContent line={pair.right.line} spans={spansForRow(pair.right)} intraline={pair.right.intraline ?? null} /></span>
          {/if}
        </div>
      </div>
      {/if}
    {/each}
  {:else}
    {#each rows as row, rowIndex (row.id)}
      {#if row.line.fold}
        {@render foldRow(row.line.fold)}
      {:else if row.gap}
        {@render gapRow(row.gap)}
      {:else}
        {@const target = isTarget(row, null)}
        {@const ranged = inRange(row, null)}
        <div
          class="ao-diff-line relative {lineClass} {lineTintClass(row.line.type)} {ranged ? RANGE_CLASS : ''}"
          style:height={wordWrap ? undefined : lineHeight}
          data-review-target={target ? '' : undefined}
          data-review-range={ranged ? '' : undefined}
        >
          {@render actionCell(row, rowIndex, null, 'line')}
          <span class="flex shrink-0 {gutterTintClass(row.line.type)}" data-testid="review-line-gutter" data-row-idx={rowIndex}>
            {@render gutter(row.oldLine)}
            {@render gutter(row.newLine)}
          </span>
          <span class="min-w-0 flex-1 {contentClass} pr-3" data-find-key={findKey(row)}><DiffLineContent line={row.line} spans={spansForRow(row)} intraline={row.intraline ?? null} /></span>
        </div>
      {/if}
    {/each}
  {/if}
</div>
