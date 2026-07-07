<script lang="ts">
  import { untrack, type Snippet } from 'svelte';
  import TimelineVirtualizer from '../virtual/TimelineVirtualizer.svelte';
  import ReviewFileHeaderRow from './ReviewFileHeaderRow.svelte';
  import ReviewLineBlockRow from './ReviewLineBlockRow.svelte';
  import { createReviewScrollOwner, reviewScrollKey } from './reviewScroll';
  import type { DiffReviewComment, ReviewThread } from '../../types/models';
  import type { PatchFile } from '../../utils/patchFiles';
  import {
    buildReviewRows,
    REVIEW_FILE_GAP_PX,
    reviewRowEstimate,
    type CommentAnchor,
    type ReviewRow,
  } from '../../utils/reviewRows';
  import type { RowEstimate, TimelineVirtualizerHandle } from '../../utils/virtual/types';

  // The continuous virtualized review surface: every file's header +
  // line blocks + comment rows in ONE TimelineVirtualizer, with a
  // sticky overlay copy of the current file's header (rows are
  // absolutely positioned, so CSS `position: sticky` group containers
  // can't work here — the overlay is driven by findItemIndex instead).
  //
  // Estimate coherence: the engine takes its RowEstimate once at
  // construction, so we hand it a stable wrapper that reads the CURRENT
  // build — `data` and `estimate` can never disagree mid-flush. wordWrap
  // and viewMode change per-row exactness / every block height, which
  // the constructor-once engine can't absorb; both remount the
  // virtualizer via {#key geometryKey}.

  // happy-dom reports zero geometry, so unit tests mount every row via
  // the virtualizer's renderAll seam (same gate + reasoning as
  // MessageTimeline: the browser vitest project keeps real windowing).
  const IS_TEST = import.meta.env.MODE === 'test'
    && typeof window !== 'undefined' && 'happyDOM' in window;

  interface Props {
    threadId: string;
    /** Review scope key segment for scroll-position memory. */
    scope: string;
    files: PatchFile[];
    viewMode: 'stacked' | 'split';
    wordWrap: boolean;
    collapsedPaths: ReadonlySet<string>;
    onToggleCollapsed: (path: string) => void;
    drafts?: readonly DiffReviewComment[];
    openEditors?: readonly CommentAnchor[];
    prThreads?: readonly ReviewThread[];
    expandedPRThreadIds?: ReadonlySet<string>;
    /** Renders draft-editor rows. Required whenever `openEditors` is
     * non-empty; absent, those rows render nothing. */
    draftEditor?: Snippet<[anchor: CommentAnchor]>;
    /** Renders comment-thread rows. Required whenever `drafts` is
     * non-empty; absent, those rows render nothing. */
    commentThread?: Snippet<[threadKey: string, anchor: CommentAnchor]>;
    prThread?: Snippet<[thread: ReviewThread, anchor: CommentAnchor, collapsed: boolean, orphaned: boolean]>;
    onAddComment?: (anchor: CommentAnchor) => void;
    /** Conflict view only: expands a fold row's hidden lines. */
    onExpandFold?: (path: string, foldId: number) => void;
    jumpToFilePath?: string | null;
    onJumpConsumed?: () => void;
    /** Row-key jump (comments list): scrolls to the exact row and
     * flash-highlights it. Keys are buildReviewRows' rowKeys. */
    jumpToRowKey?: string | null;
    onJumpRowConsumed?: () => void;
    onTopFileChange?: (fileIndex: number) => void;
  }

  let {
    threadId,
    scope,
    files,
    viewMode,
    wordWrap,
    collapsedPaths,
    onToggleCollapsed,
    drafts = [],
    openEditors = [],
    prThreads = [],
    expandedPRThreadIds = new Set(),
    draftEditor,
    commentThread,
    prThread,
    onAddComment,
    onExpandFold,
    jumpToFilePath = null,
    onJumpConsumed,
    jumpToRowKey = null,
    onJumpRowConsumed,
    onTopFileChange,
  }: Props = $props();

  const built = $derived(
    buildReviewRows({ files, viewMode, collapsedPaths, drafts, openEditors, prThreads, expandedPRThreadIds }),
  );
  const builtEstimate = $derived(reviewRowEstimate(built, wordWrap));
  // Stable identity for the engine's constructor; reads live through
  // the deriveds above (see the coherence note in the header block).
  const estimate: RowEstimate = {
    at: (index) => builtEstimate.at(index),
    isExact: (index) => builtEstimate.isExact?.(index) ?? false,
  };
  const getKey = (_row: ReviewRow, index: number) => built.rowKeys[index];

  const geometryKey = $derived(`${viewMode}:${wordWrap}`);
  const scrollKey = $derived(reviewScrollKey(threadId, scope, viewMode, wordWrap));

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: TimelineVirtualizerHandle | undefined = $state();
  const scroll = createReviewScrollOwner(() => scrollEl);

  // Save under the OUTGOING key while its geometry is still in the DOM
  // ($effect.pre cleanup runs before the flush); also fires at unmount.
  $effect.pre(() => {
    const key = scrollKey;
    return () => scroll.savePosition(key);
  });

  // Restore once per key, and only once content exists — a scope switch
  // lands its rows a load later, and restoring against the empty state
  // would just clamp to 0. Unrelated rebuilds (collapse toggles) must
  // NOT re-restore, hence the once-per-key gate.
  let restoredKey = '';
  $effect(() => {
    const key = scrollKey;
    const hasRows = built.rows.length > 0;
    if (!hasRows || restoredKey === key) return;
    restoredKey = key;
    untrack(() => {
      scroll.restorePosition(key);
      // The engine tail-seeds its window until its first scroll input
      // (chat's bottom-anchored mount seeding). This surface is
      // TOP-anchored, and neither a restore that lands on the current
      // scrollTop (reopen at offset 0) nor a fresh geometry key with no
      // saved position fires a scroll event — the window would sit at the
      // diff's tail while the viewport shows the top (blank until the
      // user scrolls). Feed the engine its real offset explicitly.
      listRef?.revalidate();
    });
  });

  // ------------------------------------------------------------------
  // Sticky overlay header
  // ------------------------------------------------------------------
  let stickyFileIndex = $state(-1);
  let topFileIndex = $state(-1);

  function setTopFileIndex(fileIndex: number): void {
    if (topFileIndex === fileIndex) return;
    topFileIndex = fileIndex;
    onTopFileChange?.(fileIndex);
  }

  function updateSticky(offset: number): void {
    const ref = listRef;
    if (!ref || built.rows.length === 0) {
      stickyFileIndex = -1;
      setTopFileIndex(-1);
      return;
    }
    const rowIndex = ref.findItemIndex(offset);
    const fileIndex = built.fileOfRow[rowIndex] ?? -1;
    setTopFileIndex(fileIndex);
    const headerRow = fileIndex >= 0 ? (built.firstRowOfFile[fileIndex] ?? -1) : -1;
    // Overlay only once the file's own header BAR has started scrolling
    // out under the viewport top — never a duplicate of a fully visible
    // one. The header row's top includes the separation gap, so the bar
    // starts REVIEW_FILE_GAP_PX below the row offset.
    const passedHeader =
      rowIndex > headerRow ||
      (rowIndex === headerRow && offset > ref.getItemOffset(rowIndex) + REVIEW_FILE_GAP_PX);
    stickyFileIndex = fileIndex >= 0 && passedHeader ? fileIndex : -1;
  }

  // Geometry can change without a scroll event (collapse toggle above
  // the viewport, reload): re-read against the current offset.
  $effect(() => {
    void built;
    untrack(() => updateSticky(scrollEl?.scrollTop ?? 0));
  });

  $effect(() => {
    const path = jumpToFilePath;
    const ref = listRef;
    if (!path || !ref || built.rows.length === 0) return;
    const fileIndex = files.findIndex((file) => file.path === path);
    const headerRow = fileIndex >= 0 ? (built.firstRowOfFile[fileIndex] ?? -1) : -1;
    if (headerRow < 0) return;
    untrack(() => {
      ref.scrollToIndex(headerRow);
      onJumpConsumed?.();
    });
  });

  // Row-key jump from the comments list. The store expands the file
  // (and thread) BEFORE setting the key, so the same flush's row model
  // already contains the target row; the key is consumed either way so
  // a stale request can't re-fire on a later rebuild.
  let flashRowKey: string | null = $state(null);
  let flashTimer: ReturnType<typeof setTimeout> | undefined;
  $effect(() => {
    const key = jumpToRowKey;
    const ref = listRef;
    if (!key || !ref || built.rows.length === 0) return;
    const rowIndex = built.rowKeys.indexOf(key);
    untrack(() => {
      onJumpRowConsumed?.();
      if (rowIndex < 0) return;
      ref.scrollToIndex(rowIndex);
      flashRowKey = key;
      clearTimeout(flashTimer);
      flashTimer = setTimeout(() => { flashRowKey = null; }, 1600);
    });
  });

  const stickyFile = $derived(stickyFileIndex >= 0 ? (files[stickyFileIndex] ?? null) : null);

  function jumpToStickyFile(): void {
    const headerRow = built.firstRowOfFile[stickyFileIndex] ?? -1;
    if (headerRow >= 0) listRef?.scrollToIndex(headerRow);
  }

  function currentTopRowIndex(): number {
    const ref = listRef;
    if (!ref || built.rows.length === 0) return -1;
    return ref.findItemIndex(scrollEl?.scrollTop ?? 0);
  }

  function currentTopFileIndex(): number {
    const topRow = currentTopRowIndex();
    return topRow >= 0 ? (built.fileOfRow[topRow] ?? -1) : -1;
  }

  function jumpFile(delta: 1 | -1): void {
    const fileIndex = currentTopFileIndex();
    if (fileIndex < 0) return;
    const targetFile = fileIndex + delta;
    const targetRow = built.firstRowOfFile[targetFile] ?? -1;
    if (targetRow >= 0) listRef?.scrollToIndex(targetRow);
  }

  function jumpComment(delta: 1 | -1): void {
    const topRow = currentTopRowIndex();
    if (topRow < 0) return;
    for (let index = topRow + delta; index >= 0 && index < built.rows.length; index += delta) {
      const row = built.rows[index];
      if (row?.kind === 'draft-editor' || row?.kind === 'comment-thread' || row?.kind === 'pr-thread') {
        listRef?.scrollToIndex(index);
        return;
      }
    }
  }

  function openFileLevelComment(): void {
    if (!onAddComment) return;
    const fileIndex = currentTopFileIndex();
    const file = fileIndex >= 0 ? files[fileIndex] : null;
    if (!file) return;
    onAddComment({ filePath: file.path, side: 'file', selectedText: '' });
  }

  function isInputTarget(target: EventTarget | null): boolean {
    if (!(target instanceof HTMLElement)) return false;
    const tag = target.tagName.toLowerCase();
    return tag === 'input' || tag === 'textarea' || tag === 'select' || target.isContentEditable;
  }

  function handleKeydown(event: KeyboardEvent): void {
    if (isInputTarget(event.target) || event.altKey || event.ctrlKey || event.metaKey || event.shiftKey) return;
    switch (event.key) {
      case 'j':
        event.preventDefault();
        jumpFile(1);
        break;
      case 'k':
        event.preventDefault();
        jumpFile(-1);
        break;
      case 'n':
        event.preventDefault();
        jumpComment(1);
        break;
      case 'p':
        event.preventDefault();
        jumpComment(-1);
        break;
      case 'c':
        event.preventDefault();
        openFileLevelComment();
        break;
    }
  }

  // Per-file gutter width (ch) from the file's max line number. Line
  // numbers are monotonic within a file, so the file's LAST numbered
  // row carries the max; the backwards scan hits it first. Conflict
  // files can end on unnumbered marker/fold rows, so keep scanning past
  // rows without a line number.
  const gutterChars = $derived.by(() => {
    const byFile = new Map<number, number>();
    for (let i = built.rows.length - 1; i >= 0; i -= 1) {
      const row = built.rows[i];
      if (row.kind !== 'line-block' || byFile.has(row.fileIndex)) continue;
      let maxLine = 0;
      for (let j = row.rows.length - 1; j >= 0 && maxLine === 0; j -= 1) {
        const displayRow = row.rows[j];
        maxLine = Math.max(displayRow?.oldLine ?? 0, displayRow?.newLine ?? 0);
      }
      if (maxLine === 0) continue;
      byFile.set(row.fileIndex, Math.max(2, String(maxLine).length));
    }
    return byFile;
  });
</script>

<div class="relative h-full min-h-0 min-w-0 flex-1" data-testid="review-diff-body">
  {#if stickyFile}
    <div
      class="absolute inset-x-0 top-0 z-10 shadow-[0_1px_4px_rgba(0,0,0,0.25)]"
      data-testid="review-sticky-header"
    >
      <ReviewFileHeaderRow
        file={stickyFile}
        collapsed={collapsedPaths.has(stickyFile.path)}
        onToggle={() => onToggleCollapsed(stickyFile.path)}
        overlay
        onJump={jumpToStickyFile}
      />
    </div>
  {/if}
  <div
    bind:this={scrollEl}
    class="h-full overflow-y-auto focus:outline-none"
    style:overflow-anchor="none"
    tabindex="0"
    role="grid"
    aria-label="Review diff"
    data-testid="review-scroll"
    onkeydown={handleKeydown}
  >
    {#key geometryKey}
      <TimelineVirtualizer
        bind:this={listRef}
        data={built.rows}
        {getKey}
        scrollRef={scrollEl}
        {estimate}
        renderAll={IS_TEST}
        applyScrollTarget={scroll.applyScrollTarget}
        onCompensation={scroll.applyCompensation}
        onscroll={updateSticky}
        onscrollend={() => scroll.savePosition(scrollKey)}
      >
        {#snippet children(row: ReviewRow, rowIndex: number)}
          {@const file = files[row.fileIndex]}
          {@const flashing = flashRowKey !== null && built.rowKeys[rowIndex] === flashRowKey}
          {#if !file}
            <!-- Build/props raced; the next flush re-renders coherent rows. -->
          {:else if row.kind === 'file-header'}
            <ReviewFileHeaderRow
              {file}
              collapsed={collapsedPaths.has(file.path)}
              onToggle={() => onToggleCollapsed(file.path)}
            />
          {:else if row.kind === 'line-block'}
            <ReviewLineBlockRow
              rows={row.rows}
              splitRows={row.splitRows}
              path={file.path}
              {threadId}
              {wordWrap}
              gutterCh={gutterChars.get(row.fileIndex) ?? 2}
              {onAddComment}
              {onExpandFold}
            />
          {:else if row.kind === 'draft-editor'}
            {@render draftEditor?.(row.anchor)}
          {:else if row.kind === 'comment-thread'}
            <div class="transition-colors duration-700 {flashing ? 'bg-accent/15' : ''}">
              {@render commentThread?.(row.threadKey, row.anchor)}
            </div>
          {:else}
            <div class="transition-colors duration-700 {flashing ? 'bg-accent/15' : ''}">
              {@render prThread?.(row.thread, row.anchor, row.collapsed, row.orphaned)}
            </div>
          {/if}
        {/snippet}
      </TimelineVirtualizer>
    {/key}
  </div>
</div>
