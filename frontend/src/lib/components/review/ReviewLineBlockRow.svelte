<script lang="ts">
  import { untrack } from 'svelte';
  import DiffLineContent from '../chat/DiffLineContent.svelte';
  import { dispatchInlineFileTokens } from '../chat/diffInlineTokenize';
  import { getDiffTheme } from '../../stores/diffTheme.svelte';
  import type { CommentAnchor } from '../../stores/reviewPane.svelte';
  import type { DiffTheme } from '../../utils/diffHighlighterPool';
  import { languageFromPath } from '../../utils/diffLanguage';
  import { lineTintClass } from '../../utils/diffLineTint';
  import { stripPatchLinePrefix, type PatchDisplayRow, type PatchLine, type SplitDisplayRow } from '../../utils/patchFiles';
  import { REVIEW_LINE_HEIGHT_PX } from '../../utils/reviewRows';
  import type { LineToken } from '../../utils/tokenCache';
  import { getCachedTokensForLine } from '../../utils/tokenCacheReactive.svelte';

  // One review line block (≤ REVIEW_LINE_BLOCK_MAX_LINES display rows).
  //
  // Exact-height contract: with word wrap OFF every visual line renders
  // at exactly REVIEW_LINE_HEIGHT_PX (h-5 + leading-5, no vertical
  // padding or borders anywhere in the block) so the block's height is
  // rows.length × 20 and no ResizeObserver is attached. With word wrap
  // ON lines wrap freely and the virtualizer measures the block.
  //
  // Tokenization mirrors DiffFileBlock: one fire-and-forget Shiki batch
  // per block; the shared reactive token cache re-renders lines as
  // tokens land, plain tinted text until then.

  interface Props {
    rows: PatchDisplayRow[];
    /** Present in split view mode (precomputed by buildReviewRows). */
    splitRows?: SplitDisplayRow[];
    path: string;
    threadId: string;
    wordWrap: boolean;
    /** Line-number gutter width in ch, per file (max line number). */
    gutterCh: number;
    onAddComment?: (anchor: CommentAnchor) => void;
  }

  let { rows, splitRows, path, threadId, wordWrap, gutterCh, onAddComment }: Props = $props();

  let theme: DiffTheme = $derived(getDiffTheme());
  let lang = $derived(languageFromPath(path));

  $effect(() => {
    const t = theme;
    const l = lang;
    const id = threadId;
    const lines = rows.map((row) => row.line);
    untrack(() => {
      void dispatchInlineFileTokens(lines, id, l, t);
    });
  });

  function getTokens(line: PatchLine): LineToken[] | null {
    return getCachedTokensForLine(line, threadId, theme, lang);
  }

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
</script>

{#snippet addButton(anchor: CommentAnchor, side: string)}
  {#if onAddComment}
    <button
      type="button"
      class="absolute left-0 top-0 z-[1] flex h-full w-5 items-center justify-center bg-surface-1/95 text-[0.6875rem] font-semibold text-fg-muted opacity-0 shadow-sm hover:text-fg group-hover:opacity-100 focus:opacity-100 focus:outline-none"
      aria-label={`Add ${side} comment`}
      data-testid="review-add-comment"
      onclick={() => onAddComment?.(anchor)}
    >
      +
    </button>
  {/if}
{/snippet}

{#snippet gutter(lineNo: number)}
  <span
    class="shrink-0 select-none pr-1 text-right tabular-nums text-fg-subtle"
    style:width="{gutterCh + 1}ch"
    aria-hidden="true"
  >{lineNo > 0 ? lineNo : ''}</span>
{/snippet}

<div
  class="font-mono text-xs"
  style:line-height={lineHeight}
  data-testid="review-line-block"
  data-path={path}
>
  {#if splitRows}
    {#each splitRows as pair, pairIndex (pairIndex)}
      <div class={lineClass} style:height={wordWrap ? undefined : lineHeight}>
        <div class="group relative flex w-1/2 min-w-0 {pair.left ? lineTintClass(pair.left.line.type) : 'bg-surface-0/30'}">
          {#if pair.left}
            {@render addButton(sideAnchor(pair.left, 'old'), 'old-line')}
            {@render gutter(pair.left.oldLine)}
            <span class="min-w-0 flex-1 {contentClass} pl-1 pr-2"><DiffLineContent line={pair.left.line} tokens={getTokens(pair.left.line)} /></span>
          {/if}
        </div>
        <div class="group relative flex w-1/2 min-w-0 border-l border-border-subtle {pair.right ? lineTintClass(pair.right.line.type) : 'bg-surface-0/30'}">
          {#if pair.right}
            {@render addButton(sideAnchor(pair.right, 'new'), 'new-line')}
            {@render gutter(pair.right.newLine)}
            <span class="min-w-0 flex-1 {contentClass} pl-1 pr-2"><DiffLineContent line={pair.right.line} tokens={getTokens(pair.right.line)} /></span>
          {/if}
        </div>
      </div>
    {/each}
  {:else}
    {#each rows as row (row.id)}
      <div
        class="group relative {lineClass} {lineTintClass(row.line.type)}"
        style:height={wordWrap ? undefined : lineHeight}
      >
        {@render addButton(stackedAnchor(row), 'line')}
        {@render gutter(row.oldLine)}
        {@render gutter(row.newLine)}
        <span class="min-w-0 flex-1 {contentClass} pl-2 pr-3"><DiffLineContent line={row.line} tokens={getTokens(row.line)} /></span>
      </div>
    {/each}
  {/if}
</div>
