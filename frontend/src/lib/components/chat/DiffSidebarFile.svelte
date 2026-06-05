<script lang="ts">
  /*
   * Pure file renderer for the diff sidebar. No singleton access:
   * tokenization is dispatched by DiffSidebarBody, theme arrives as
   * a prop, the file just reads tokens out of the shared cache.
   *
   * Two visibility tiers:
   *   - File-level: registered with the parent's IntersectionObserver
   *     virtualizer. Out-of-viewport (+ overscan) files render a
   *     placeholder sized via the last measured height.
   *   - Line-level: lines render with their line-tint background as
   *     soon as the file becomes visible; once the body's dispatch
   *     coordinator has populated the cache, tokens fade in (a
   *     module-level cache-generation counter makes the cache reads
   *     reactive).
   */
  import ChevronRight from 'lucide-svelte/icons/chevron-right';
  import EditorLink from '../common/EditorLink.svelte';
  import Icon from '../primitives/Icon.svelte';
  import ToolKindIcon from './ToolKindIcon.svelte';
  import DiffLineContent from './DiffLineContent.svelte';
  import type {
    PatchDisplayRow,
    PatchFile,
    PatchLine,
    SplitDisplayRow,
  } from '../../utils/patchFiles';
  import {
    buildPatchDisplayRows,
    buildSplitDisplayRows,
  } from '../../utils/patchFiles';
  import { lineTintClass } from '../../utils/diffLineTint';
  import {
    diffFlexLineContentClass,
    diffScrollContentClass,
    diffSplitGridColumnsClass,
  } from '../../utils/diffLineLayout';
  import type { FileVirtualizerHandle } from '../../utils/diffSidebarVirtualizer.svelte';
  import { languageFromPath } from '../../utils/diffLanguage';
  import type { DiffTheme } from '../../utils/diffHighlighterPool';
  import type { DiffViewMode } from '../../stores/diffPanel.svelte';
  import type { LineToken } from '../../utils/tokenCache';
  import { getCachedTokensForLine } from '../../utils/tokenCacheReactive.svelte';

  interface Props {
    file: PatchFile;
    rowId: string;
    expanded: boolean;
    threadId: string;
    /** Absolute workspace dir used to resolve `file.path` (which is
     *  repo-relative from git diff output) when the user clicks the
     *  open-in-editor affordance. */
    workspacePath: string;
    viewMode: DiffViewMode;
    wordWrap: boolean;
    /** Resolved Shiki theme name from the parent. */
    theme: DiffTheme;
    virtualizer: FileVirtualizerHandle;
    onToggle: (rowId: string) => void;
  }

  let { file, rowId, expanded, threadId, workspacePath, viewMode, wordWrap, theme, virtualizer, onToggle }: Props = $props();

  let containerEl: HTMLElement | undefined = $state(undefined);

  $effect(() => {
    if (!containerEl) return;
    virtualizer.register(rowId, containerEl);
    return () => virtualizer.unregister(rowId);
  });

  // Render the body whenever the file is expanded. The
  // IntersectionObserver-based virtualization gate previously gated
  // body rendering on `expanded && inViewport`, but the observer
  // hadn't reliably fired before the user looked at the sidebar in
  // some cases — the body would stay stuck on the empty placeholder
  // until the first scroll-triggered tick. The body's tokenizer
  // dispatcher had the same hole (the lower portion of a long diff
  // would stay plain even after scrolling, because a fully-visible
  // file never re-fires the observer); dispatch is now gated on
  // expand, not visibility. The IO is still observed for `cachedHeight`
  // — kept so future per-file priority ordering has a viewport signal
  // to read — but the dispatch path no longer depends on it.
  let cachedHeight = $derived(virtualizer.height(rowId));
  let shouldRender = $derived(expanded);

  let scrollContentClass = $derived(diffScrollContentClass(wordWrap));
  let splitGridColumnsClass = $derived(diffSplitGridColumnsClass(wordWrap));
  let lineContentClass = $derived(diffFlexLineContentClass(wordWrap));

  // Renamed files show `old → new` so the change kind reads from the
  // path itself; +/- counts convey added (all-add) / deleted
  // (all-del) / modified (mix). No separate kind chip pill.
  let displayPath = $derived.by(() => {
    if (file.kind !== 'renamed') return file.path;
    const renameLine = file.lines.find(
      (l) => l.type === 'meta' && l.content.startsWith('rename from '),
    );
    if (!renameLine) return file.path;
    const previousPath = renameLine.content.slice('rename from '.length).trim();
    if (!previousPath) return file.path;
    return `${previousPath} → ${file.path}`;
  });

  // Build the visible row stream: `buildPatchDisplayRows` drops meta
  // lines (`diff --git`, `---`, `+++`, `@@` hunk headers) and tracks
  // old/new line numbers across hunks. The header above already
  // identifies the file; hunk headers are noisy as raw text and
  // don't help a reader scanning a rendered diff. Hunk separators
  // (`⋮`) below mark gaps between hunks visually instead.
  let displayRows: PatchDisplayRow[] = $derived(buildPatchDisplayRows(file.lines));

  // Track hunk boundaries so the body can interleave separator rows
  // between hunks (gives the reader a visual cue when line numbers
  // jump). Each entry is the index in `displayRows` AFTER which a
  // separator should appear; the first hunk doesn't get a leading
  // separator. `buildPatchDisplayRows` skips meta lines in the same
  // order this loop does, so the resulting indices align row-for-row.
  let separatorAfter = $derived.by((): Set<number> => {
    const out = new Set<number>();
    let displayIdx = -1;
    let seenFirstHunk = false;
    let pendingSeparator = false;
    for (const line of file.lines) {
      if (line.type === 'meta') {
        if (line.content.startsWith('@@')) {
          if (seenFirstHunk) pendingSeparator = true;
          seenFirstHunk = true;
        }
        continue;
      }
      displayIdx += 1;
      if (pendingSeparator) {
        // The separator visually sits BEFORE the new hunk's first
        // line — i.e. AFTER the previous displayed line.
        out.add(displayIdx - 1);
        pendingSeparator = false;
      }
    }
    return out;
  });

  let splitRows: SplitDisplayRow[] = $derived(
    viewMode === 'split' ? buildSplitDisplayRows(displayRows) : [],
  );

  // Gutter width tracks the longest line number in the file so the
  // column never reflows mid-scroll. Match `DiffFileBlock`'s minimum
  // of 2 chars so single-digit hunks still leave room for a separator
  // tick.
  let maxLineNo = $derived.by(() => {
    let max = 0;
    for (const row of displayRows) {
      if (row.newLine > max) max = row.newLine;
      if (row.oldLine > max) max = row.oldLine;
    }
    return max;
  });
  let gutterChars = $derived(Math.max(2, String(maxLineNo).length));

  let lang = $derived(languageFromPath(file.path));

  function getTokens(line: PatchLine): LineToken[] | null {
    return getCachedTokensForLine(line, threadId, theme, lang);
  }

  // HTML5 forbids whitespace in id values, and file paths can carry
  // spaces or other awkward characters. Normalize to a stable
  // identifier the aria-controls attribute (and Svelte test queries)
  // can rely on.
  let safeId = $derived(rowId.replace(/[^A-Za-z0-9_-]/g, '_'));
</script>

<!--
  Per-file row in the diff sidebar. Visual style mirrors the inline
  DiffFileBlock: chevron + ToolKindIcon + path + +/- counts +
  EditorLink. No "modified"/"added" kind chip pill (the +/- counts
  and the rename-aware path display convey the kind without an
  explicit badge), no card border around the file row. Body indents
  under the chevron column with a left rule so a multi-file sidebar
  still reads as a list while staying lighter visually.
-->
<section
  bind:this={containerEl}
  data-file-path={file.path}
  data-testid="diff-sidebar-file"
  class="group/diff-sidebar-file mb-2"
>
  <header class="flex items-center gap-2 px-1 py-1 text-[0.8125rem] hover:bg-surface-2/20 rounded-[var(--radius-control)] transition-colors">
    <button
      type="button"
      onclick={() => onToggle(rowId)}
      aria-expanded={expanded}
      aria-controls="diff-sidebar-file-{safeId}"
      class="flex flex-1 min-w-0 items-center gap-2 text-left cursor-pointer bg-transparent border-0 p-0 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent/40 rounded"
    >
      <span class="flex size-3 shrink-0 items-center justify-center text-fg-subtle select-none transition-transform duration-150" class:rotate-90={expanded}>
        <Icon icon={ChevronRight} size={12} strokeWidth={2} class="opacity-70" />
      </span>
      <ToolKindIcon kind="file" ariaLabel="File" />
      <span class="min-w-0 flex-1 truncate font-mono text-[0.75rem] text-fg-muted/85">{displayPath}</span>
      <span class="ml-auto flex gap-2 text-[0.6875rem] shrink-0 tabular-nums">
        {#if file.additions > 0}<span class="text-success">+{file.additions}</span>{/if}
        {#if file.deletions > 0}<span class="text-error">-{file.deletions}</span>{/if}
      </span>
    </button>
    <EditorLink
      path={file.path}
      {workspacePath}
      asIcon
      stopPropagation
      class="opacity-0 group-hover/diff-sidebar-file:opacity-100 focus-visible:opacity-100"
    />
  </header>

  {#if expanded}
    <div id="diff-sidebar-file-{safeId}" class="ml-5 overflow-x-auto border-l border-border-subtle bg-surface-0/35" data-testid="diff-sidebar-file-body">
      {#if shouldRender}
        {#if viewMode === 'split'}
          <div
            class="grid {scrollContentClass} {splitGridColumnsClass} gap-px bg-border-subtle font-mono text-[0.6875rem] leading-tight"
            style="--gutter-w: {gutterChars + 1}ch"
            data-testid="diff-sidebar-split-body"
          >
            {#each splitRows as row, i (i)}
              {@render splitSide(row.left, row.left?.oldLine ?? 0)}
              {@render splitSide(row.right, row.right?.newLine ?? 0)}
            {/each}
          </div>
        {:else}
          <div
            class="{scrollContentClass} py-2 font-mono text-[0.6875rem] leading-tight"
            style="--gutter-w: {gutterChars + 1}ch"
            data-testid="diff-sidebar-stacked-body"
          >
            {#each displayRows as row, i (row.id)}
              <div class="flex min-w-full {lineTintClass(row.line.type)}">
                <span
                  class="select-none tabular-nums text-fg-subtle px-3 text-right shrink-0"
                  style="width: var(--gutter-w)"
                  aria-hidden="true"
                  data-testid="diff-sidebar-line-gutter"
                >{row.newLine || row.oldLine || ''}</span><span
                  class="pl-1 pr-3 {lineContentClass}"
                  data-testid="diff-sidebar-line-content"
                ><DiffLineContent line={row.line} tokens={getTokens(row.line)} /></span>
              </div>
              {#if separatorAfter.has(i)}
                <div
                  class="my-1 flex min-w-full items-center gap-2 px-3 select-none"
                  aria-hidden="true"
                  data-testid="diff-sidebar-hunk-separator"
                >
                  <span class="flex-1 border-t border-border-subtle"></span>
                  <span class="text-[0.625rem] text-fg-subtle">⋮</span>
                  <span class="flex-1 border-t border-border-subtle"></span>
                </div>
              {/if}
            {/each}
          </div>
        {/if}
      {:else if cachedHeight !== undefined}
        <!-- Out-of-viewport placeholder preserves layout via measured height. -->
        <div aria-hidden="true" style="height: {cachedHeight}px" data-testid="diff-sidebar-file-placeholder"></div>
      {:else}
        <!-- First render before the IntersectionObserver tick. Use a
             modest skeleton so the layout doesn't pop. -->
        <div aria-hidden="true" class="h-12" data-testid="diff-sidebar-file-skeleton"></div>
      {/if}
    </div>
  {/if}
</section>

{#snippet splitSide(side: PatchDisplayRow | null, lineNo: number)}
  <div class="flex min-w-full py-px {side ? lineTintClass(side.line.type) : 'bg-surface-0/40'} {side?.line.type === 'context' ? 'bg-surface-0' : ''}">
    <span
      class="select-none tabular-nums text-fg-subtle px-2 text-right shrink-0"
      style="width: var(--gutter-w)"
      aria-hidden="true"
      data-testid="diff-sidebar-line-gutter"
    >{lineNo || ''}</span><span
      class="pl-1 pr-2 {lineContentClass}"
      data-testid="diff-sidebar-line-content"
    >{#if side}<DiffLineContent line={side.line} tokens={getTokens(side.line)} />{:else}{' '}{/if}</span>
  </div>
{/snippet}
