<script lang="ts">
  import { untrack } from 'svelte';
  import DiffLineContent from '../chat/DiffLineContent.svelte';
  import type { CommentAnchor } from '../../stores/reviewPane.svelte';
  import { DIFF_CONTEXT_EXPAND_STEP, type ExpandDirection } from '../../utils/diffContextExpansion';
  import { gutterTintClass, lineTintClass } from '../../utils/diffLineTint';
  import { diffSpanCacheGeneration, getSpansForLine, requestFileSpans, type PatchScopeContext } from '../../utils/diffSpanCache.svelte';
  import { stripPatchLinePrefix, type DiffGap, type PatchDisplayRow, type PatchFile, type SplitDisplayRow } from '../../utils/patchFiles';
  import { REVIEW_LINE_HEIGHT_PX } from '../../utils/reviewRows';

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
  // blocks); rows re-render as spans land, plain tinted text until
  // then. Gap expansion produces a new lines-array identity → new
  // content key → re-request; the backend's cache absorbs the overlap.

  interface Props {
    rows: PatchDisplayRow[];
    /** Present in split view mode (precomputed by buildReviewRows). */
    splitRows?: SplitDisplayRow[];
    /** The owning file — span requests are file-level. */
    file: PatchFile;
    path: string;
    /** The review SUBJECT's identity — the thread row id, or a draft
     * placeholder's synthetic one. It owns this file's span-cache
     * entries for eviction; the RPC subject comes from `spanContext`. */
    subjectId: string;
    /** Diff view: scope fields for parse-priming file content above
     * each hunk, and the subject the priming RPC resolves it from.
     * Absent on the conflict surface, whose pseudo-files have no file
     * behind them. */
    spanContext?: PatchScopeContext | null;
    wordWrap: boolean;
    /** Line-number gutter width in ch, per file (max line number). */
    gutterCh: number;
    onAddComment?: (anchor: CommentAnchor) => void;
    /** Conflict view only: expands the fold row's hidden lines. */
    onExpandFold?: (path: string, foldId: number) => void;
    /** Diff view only: fetches a hunk gap's hidden context lines. */
    onExpandGap?: (path: string, gap: DiffGap, dir: ExpandDirection) => void;
  }

  let { rows, splitRows, file, path, subjectId, spanContext = null, wordWrap, gutterCh, onAddComment, onExpandFold, onExpandGap }: Props = $props();

  $effect(() => {
    // Generation dependency: an eviction (LRU pressure, same-thread
    // reload) re-runs this effect so mounted rows re-request instead
    // of staying plain; on a cache hit the re-run is a cheap Map
    // lookup.
    diffSpanCacheGeneration();
    const fileNow = file;
    const context = spanContext;
    const owner = subjectId;
    untrack(() => {
      void requestFileSpans(fileNow, owner, context);
    });
  });

  // Heights come from the shared constant, not rem-based Tailwind
  // classes, so the estimate table and the rendered geometry cannot
  // drift apart via root font-size.
  let lineHeight = $derived(`${REVIEW_LINE_HEIGHT_PX}px`);
  let lineClass = $derived(wordWrap ? 'flex' : 'flex overflow-hidden');
  let contentClass = $derived(
    wordWrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre',
  );

  function stackedAnchor(row: PatchDisplayRow): CommentAnchor {
    return {
      filePath: path,
      oldLine: row.oldLine || undefined,
      newLine: row.newLine || undefined,
      side: row.side,
      selectedText: stripPatchLinePrefix(row.line),
    };
  }

  function sideAnchor(row: PatchDisplayRow, side: 'old' | 'new'): CommentAnchor {
    return {
      filePath: path,
      oldLine: side === 'old' ? row.oldLine || undefined : undefined,
      newLine: side === 'new' ? row.newLine || undefined : undefined,
      side,
      selectedText: stripPatchLinePrefix(row.line),
    };
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
     the hover affordance never paints over the line numbers. -->
{#snippet actionCell(anchor: CommentAnchor, side: string)}
  <span class="flex w-5 shrink-0 items-center justify-center">
    {#if onAddComment}
      <button
        type="button"
        class="flex size-4 items-center justify-center rounded-[3px] bg-accent text-[0.6875rem] font-semibold leading-none text-accent-fg opacity-0 transition-opacity duration-75 group-hover:opacity-100 focus-visible:opacity-100 hover:brightness-110 focus:outline-none"
        aria-label={`Add ${side} comment`}
        data-testid="review-add-comment"
        onclick={() => onAddComment?.(anchor)}
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
      class="flex w-full cursor-pointer items-center gap-2 bg-accent/[0.07] px-8 text-left text-[0.6875rem] text-fg-muted hover:bg-accent/15 hover:text-fg"
      style:height={wordWrap ? undefined : lineHeight}
      data-testid="review-gap-expand-all"
      aria-label="Expand {gapLabel(gap)}"
      onclick={() => onExpandGap?.(path, gap, 'all')}
    >
      <span class="text-accent" aria-hidden="true">↕</span>
      <span>{gapLabel(gap)}</span>
    </button>
  {:else}
    <div
      class="flex w-full items-center bg-accent/[0.07] text-[0.6875rem] text-fg-muted"
      style:height={wordWrap ? undefined : lineHeight}
      data-testid="review-gap"
    >
      {#if onExpandGap}
        <span class="flex shrink-0 items-center gap-0.5 pl-1.5">
          {#if gap.location !== 'trailing'}
            <button
              type="button"
              class="flex size-4 items-center justify-center rounded-[3px] text-accent hover:bg-accent/20 focus-visible:bg-accent/20 focus:outline-none"
              aria-label="Expand up"
              data-testid="review-gap-expand-up"
              onclick={() => onExpandGap?.(path, gap, 'up')}
            >↑</button>
          {/if}
          {#if gap.location !== 'leading'}
            <button
              type="button"
              class="flex size-4 items-center justify-center rounded-[3px] text-accent hover:bg-accent/20 focus-visible:bg-accent/20 focus:outline-none"
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
<div
  class="bg-surface-1 font-mono text-xs text-fg"
  style:line-height={lineHeight}
  data-testid="review-line-block"
  data-path={path}
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
      <div class={lineClass} style:height={wordWrap ? undefined : lineHeight}>
        <div class="group relative flex w-1/2 min-w-0 before:pointer-events-none before:absolute before:inset-0 before:content-[''] hover:before:bg-fg/[0.04] {pair.left ? lineTintClass(pair.left.line.type) : 'bg-surface-0/40'}">
          {#if pair.left}
            {@render actionCell(sideAnchor(pair.left, 'old'), 'old-line')}
            <span class="flex shrink-0 {gutterTintClass(pair.left.line.type)}">{@render gutter(pair.left.oldLine)}</span>
            <span class="min-w-0 flex-1 {contentClass} pl-2 pr-2"><DiffLineContent line={pair.left.line} spans={getSpansForLine(file, pair.left.line, spanContext)} intraline={pair.left.intraline ?? null} /></span>
          {/if}
        </div>
        <div class="group relative flex w-1/2 min-w-0 border-l border-border-subtle before:pointer-events-none before:absolute before:inset-0 before:content-[''] hover:before:bg-fg/[0.04] {pair.right ? lineTintClass(pair.right.line.type) : 'bg-surface-0/40'}">
          {#if pair.right}
            {@render actionCell(sideAnchor(pair.right, 'new'), 'new-line')}
            <span class="flex shrink-0 {gutterTintClass(pair.right.line.type)}">{@render gutter(pair.right.newLine)}</span>
            <span class="min-w-0 flex-1 {contentClass} pl-2 pr-2"><DiffLineContent line={pair.right.line} spans={getSpansForLine(file, pair.right.line, spanContext)} intraline={pair.right.intraline ?? null} /></span>
          {/if}
        </div>
      </div>
      {/if}
    {/each}
  {:else}
    {#each rows as row (row.id)}
      {#if row.line.fold}
        {@render foldRow(row.line.fold)}
      {:else if row.gap}
        {@render gapRow(row.gap)}
      {:else}
        <div
          class="group relative {lineClass} {lineTintClass(row.line.type)} before:pointer-events-none before:absolute before:inset-0 before:content-[''] hover:before:bg-fg/[0.04]"
          style:height={wordWrap ? undefined : lineHeight}
        >
          {@render actionCell(stackedAnchor(row), 'line')}
          <span class="flex shrink-0 {gutterTintClass(row.line.type)}">
            {@render gutter(row.oldLine)}
            {@render gutter(row.newLine)}
          </span>
          <span class="min-w-0 flex-1 {contentClass} pl-2 pr-3"><DiffLineContent line={row.line} spans={getSpansForLine(file, row.line, spanContext)} intraline={row.intraline ?? null} /></span>
        </div>
      {/if}
    {/each}
  {/if}
</div>
