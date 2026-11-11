<script lang="ts">
  import { untrack, type Snippet } from 'svelte';
  import LongListVirtualizer, { type LongListHandle } from '../virtual/LongListVirtualizer.svelte';
  import ReviewFileHeaderRow from './ReviewFileHeaderRow.svelte';
  import ReviewDiffRow from './ReviewDiffRow.svelte';
  import {
    createReviewScrollOwner,
    reviewScrollKey,
    saveReadingPosition,
    savedReadingPosition,
  } from './reviewScroll';
  import type { DiffReviewComment, ReviewThread } from '../../types/models';
  import type { ExpandDirection } from '../../utils/diffContextExpansion';
  import type { PaintedSpans, PatchScopeContext } from '../../utils/diffSpanCache.svelte';
  import type { DiffGap } from '../../utils/patchFiles';
  import type { ReviewFile } from '../../utils/patchStore';
  import {
    captureReadingAnchor,
    resolveReadingAnchor,
    type ReadingAnchor,
  } from '../../utils/reviewAnchor';
  import { REVIEW_JUMP_PULSE_MS } from '../../utils/reviewJumpPulse';
  import { findLineKeyIndex, findLineKeyPath, type ReviewFindPlace } from '../../utils/reviewFindScan';
  import { findHitRow, findRowPosition } from '../../utils/reviewFindRows';
  import { reviewDiffKeydown, type ReviewDiffKeysHost } from './reviewDiffKeys';
  import { elementAtEdge } from '../../utils/findReadingEdge';
  import {
    BlockRowsCache,
    buildReviewRows,
    REVIEW_FILE_GAP_PX,
    REVIEW_FILE_HEADER_BAR_PX,
    REVIEW_OVERVIEW_ROW_KEY,
    reviewRowEstimate,
    type CommentAnchor,
    type ReviewLeadRow,
    type ReviewRow,
  } from '../../utils/reviewRows';
  import type { RowEstimate } from '../../utils/virtual/types';

  // The continuous virtualized review surface: every file's header +
  // line blocks + comment rows in ONE LongListVirtualizer, with a
  // sticky overlay copy of the current file's header (rows are
  // absolutely positioned, so CSS `position: sticky` group containers
  // can't work here — the overlay is driven by findItemIndex instead).
  // A diff taller than a browser can lay out holds a range of its rows
  // at a time; row indices here are always indices of `built.rows`.
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
    /** The review subject's identity (conversation, owner epoch and
     * checkout). Keys scroll-position memory. */
    subjectId: string;
    /** The thread row id, or a draft placeholder's synthetic one. Owns the
     * span-cache entries this body requests, which thread switch, delete
     * and draft materialization address by row id. */
    spanOwner: string;
    /** Review scope key segment for scroll-position memory. */
    scope: string;
    files: readonly ReviewFile[];
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
     * non-empty; absent, those rows render nothing. `pulse` is the
     * row-key jump that landed on the row (utils/reviewJumpPulse), 0
     * for none. */
    commentThread?: Snippet<[threadKey: string, anchor: CommentAnchor, pulse: number]>;
    prThread?: Snippet<[thread: ReviewThread, anchor: CommentAnchor, collapsed: boolean, orphaned: boolean, pulse: number]>;
    /** The lead row (the PR overview, or the picked commit's card) as the
     * list's first row, so it scrolls off with the diff. Absent when
     * neither applies. */
    overview?: Snippet;
    /** Rendered under the lead row when `files` is empty (an empty diff,
     * or every file filtered out), so the lead row still has the list to
     * sit in. */
    empty?: Snippet;
    /** Which lead row `overview` renders; keys its measured height. */
    overviewLead?: ReviewLeadRow;
    /** Fires when the PR overview row leaves or re-enters the viewport
     * top, for the title bar's peek controls. A commit card has no peek
     * controls and never reports. */
    onOverviewOffChange?: (off: boolean) => void;
    onAddComment?: (anchor: CommentAnchor) => void;
    /** Conflict view only: expands a fold row's hidden lines. */
    onExpandFold?: (path: string, foldId: number) => void;
    /** Diff view only: fetches a hunk gap's hidden context lines. */
    onExpandGap?: (path: string, gap: DiffGap, dir: ExpandDirection) => void;
    /** Diff view only: scope fields for parse-priming span requests
     * (HighlightPatchWithContext). Omitted on the conflict surface. */
    spanContext?: PatchScopeContext | null;
    /** The colors this surface last painted, which lines keep while
     * their file's exact spans are in flight. Owned by the review state. */
    painted?: PaintedSpans | null;
    jumpToFilePath?: string | null;
    onJumpConsumed?: () => void;
    /** Row-key jump (a thread's card, the unresolved stepper): scrolls
     * to the exact row and pulses its card. Keys are buildReviewRows'
     * rowKeys. */
    jumpToRowKey?: string | null;
    onJumpRowConsumed?: () => void;
    onTopFileChange?: (fileIndex: number) => void;
  }

  let {
    subjectId,
    spanOwner,
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
    overview,
    overviewLead = 'overview',
    empty,
    onOverviewOffChange,
    onAddComment,
    onExpandFold,
    onExpandGap,
    spanContext = null,
    painted = null,
    jumpToFilePath = null,
    onJumpConsumed,
    jumpToRowKey = null,
    onJumpRowConsumed,
    onTopFileChange,
  }: Props = $props();

  const built = $derived(
    buildReviewRows({ files, viewMode, collapsedPaths, drafts, openEditors, prThreads, expandedPRThreadIds, overview: overview === undefined ? undefined : overviewLead }),
  );
  const builtEstimate = $derived(reviewRowEstimate(built, wordWrap));
  // The rows of the blocks this surface renders, for the files it shows.
  const blockRows = new BlockRowsCache();
  $effect.pre(() => {
    blockRows.retain(files);
  });
  // Stable identity for the engine's constructor; reads live through
  // the deriveds above (see the coherence note in the header block).
  const estimate: RowEstimate = {
    at: (index) => builtEstimate.at(index),
    isExact: (index) => builtEstimate.isExact?.(index) ?? false,
  };
  const getKey = (_row: ReviewRow, index: number) => built.rowKeys[index];

  const geometryKey = $derived(`${viewMode}:${wordWrap}`);
  const scrollKey = $derived(reviewScrollKey(subjectId, scope, viewMode, wordWrap));

  let scrollEl: HTMLElement | undefined = $state();
  let listRef: LongListHandle | undefined = $state();
  const scroll = createReviewScrollOwner(() => scrollEl);

  // Save under the OUTGOING key ($effect.pre cleanup runs before the
  // flush); also fires at unmount.
  $effect.pre(() => {
    const key = scrollKey;
    return () => saveReadingPosition(key, readingAnchor);
  });

  // Restore once per key, and only once content exists — a scope switch
  // lands its rows a load later, and restoring against the empty state
  // would just clamp to 0. Unrelated rebuilds (collapse toggles) must
  // NOT re-restore, hence the once-per-key gate. A key with nothing saved
  // keeps the line being read (a view mode or wrap change); the reading
  // anchor is still the one captured under the outgoing key, because the
  // rebuild effect below runs after this one.
  let restoredKey = '';
  $effect(() => {
    const key = scrollKey;
    const hasRows = built.rows.length > 0;
    if (!hasRows || restoredKey === key) return;
    restoredKey = key;
    untrack(() => {
      const saved = savedReadingPosition(key);
      const target = saved === undefined ? readingAnchor : saved;
      if (target) restoreAnchor(target);
      else if (saved === null) scroll.applyScrollTarget(0);
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
  let overviewOff = false;
  // The overlay must stop short of the scroll container's scrollbar —
  // `inset-x-0` on the outer wrapper would paint over it. Classic
  // scrollbars report their width via offsetWidth − clientWidth;
  // overlay scrollbars report 0 and need no inset.
  let scrollbarInset = $state(0);

  function setTopFileIndex(fileIndex: number): void {
    if (topFileIndex === fileIndex) return;
    topFileIndex = fileIndex;
    onTopFileChange?.(fileIndex);
  }

  function setOverviewOff(off: boolean): void {
    if (overviewOff === off) return;
    overviewOff = off;
    onOverviewOffChange?.(off);
  }

  function updateSticky(offset: number): void {
    const el = scrollEl;
    if (el) scrollbarInset = el.offsetWidth - el.clientWidth;
    const ref = listRef;
    if (!ref || built.rows.length === 0) {
      stickyFileIndex = -1;
      setTopFileIndex(-1);
      setOverviewOff(false);
      return;
    }
    const rowIndex = ref.findItemIndex(offset);
    const lead = built.rows[0];
    setOverviewOff(lead?.kind === 'overview' && lead.lead === 'overview' && rowIndex > 0);
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

  // ------------------------------------------------------------------
  // Reading anchor: never move the reader unintentionally
  // ------------------------------------------------------------------
  // The top-of-viewport position is tracked as (file, line, pixel
  // delta) on every scroll (math in utils/reviewAnchor.ts). When the
  // row model rebuilds because the CONTENT changed under the same
  // scroll key — a diff reload, a gap expansion, a PR poll replacing
  // thread rows — the anchor is re-located in the new geometry and
  // scrollTop restored to it, so the line being read stays put however
  // much content above it grew or shrank. scrollTop 0 is deliberately
  // unanchored: at the top, the top stays the top. User-driven
  // rebuilds (collapse toggles, opening editors) do NOT re-anchor —
  // files/prThreads identity is unchanged there.
  let readingAnchor: ReadingAnchor | null = null;

  function captureAnchor(offset: number): void {
    const ref = listRef;
    readingAnchor = ref ? captureReadingAnchor(built, files, blockRows, ref, offset) : null;
  }

  function restoreAnchor(target: ReadingAnchor): void {
    const ref = listRef;
    if (!ref) return;
    const position = resolveReadingAnchor(built, files, blockRows, target);
    if (!position) return;
    if (ref.holds(position.index)) {
      scroll.applyScrollTarget(Math.max(0, ref.getItemOffset(position.index) + position.offset));
    } else {
      ref.scrollToIndex(position.index, { offset: position.offset });
    }
  }

  // Geometry can change without a scroll event (collapse toggle above
  // the viewport, reload): re-read against the current offset. Content
  // identity changes additionally restore the reading anchor first.
  // Initial-value captures are deliberate: these are change trackers,
  // re-assigned on every effect run.
  // svelte-ignore state_referenced_locally
  let prevFiles = files;
  // svelte-ignore state_referenced_locally
  let prevPrThreads: readonly ReviewThread[] = prThreads;
  // svelte-ignore state_referenced_locally
  let prevScrollKey = scrollKey;
  $effect(() => {
    void built;
    untrack(() => {
      const contentChanged = files !== prevFiles || prThreads !== prevPrThreads;
      const keyChanged = scrollKey !== prevScrollKey;
      prevFiles = files;
      prevPrThreads = prThreads;
      prevScrollKey = scrollKey;
      if (contentChanged && !keyChanged && readingAnchor) restoreAnchor(readingAnchor);
      const offset = scrollEl?.scrollTop ?? 0;
      updateSticky(offset);
      captureAnchor(offset);
    });
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

  // Row-key jump. The store expands the file (and thread) BEFORE setting
  // the key, so the same flush's row model already contains the target
  // row; the key is consumed either way so a stale request can't re-fire
  // on a later rebuild. The landed-on row's card pulses: the row is held
  // here, with a generation so the same row pulses again on a repeat
  // jump, for the pulse's lifetime and then forgotten, so a later
  // remount of the row (it is virtualized) does not replay the pulse.
  let pulse: { rowKey: string; generation: number } | null = $state(null);
  let pulseTimer: ReturnType<typeof setTimeout> | undefined;
  const pulseRow = $derived.by(() => {
    const current = pulse;
    return current ? (built.rows[built.rowKeys.indexOf(current.rowKey)] ?? null) : null;
  });
  $effect(() => {
    const key = jumpToRowKey;
    const ref = listRef;
    if (!key || !ref || built.rows.length === 0) return;
    const rowIndex = built.rowKeys.indexOf(key);
    untrack(() => {
      onJumpRowConsumed?.();
      if (rowIndex < 0) return;
      // The overview is a destination, not a find: no pulse, and it
      // sits above every file so nothing overlays its top.
      if (key === REVIEW_OVERVIEW_ROW_KEY) {
        ref.scrollToIndex(rowIndex);
        return;
      }
      ref.scrollToIndex(rowIndex, { offset: -REVIEW_FILE_HEADER_BAR_PX });
      pulse = { rowKey: key, generation: (pulse?.generation ?? 0) + 1 };
      clearTimeout(pulseTimer);
      pulseTimer = setTimeout(() => { pulse = null; }, REVIEW_JUMP_PULSE_MS);
    });
  });
  $effect(() => () => clearTimeout(pulseTimer));

  const stickyFile = $derived(stickyFileIndex >= 0 ? (files[stickyFileIndex] ?? null) : null);

  // The overlay's chevron. A script function rather than an inline closure
  // so the sticky file is read once, under a guard: the header's other
  // expressions carry the `collapsedPaths` prop as a second dep, and a
  // template deref of `stickyFile` beside it can be asked to render after
  // the sticky file cleared and before the branch tears down.
  function toggleStickyFile(): void {
    const file = stickyFile;
    if (file) onToggleCollapsed(file.path);
  }

  function jumpToStickyFile(): void {
    const headerRow = built.firstRowOfFile[stickyFileIndex] ?? -1;
    if (headerRow >= 0) listRef?.scrollToIndex(headerRow);
  }

  const keysHost: ReviewDiffKeysHost = {
    built: () => built,
    files: () => files,
    list: () => listRef,
    scrollTop: () => scrollEl?.scrollTop ?? 0,
    onAddComment: () => onAddComment,
  };
  const handleKeydown = reviewDiffKeydown(keysHost);

  /** Find: the (file, line) at the reading edge (the viewport top, below
   * the sticky file bar), in find-hit order. Inside a block of lines, the
   * line laid out across the edge (lines wrap); its block's first line
   * when none is (a gap or fold row). */
  export function findReadingPosition(): { fileIndex: number; line: number } | null {
    const el = scrollEl;
    const list = listRef;
    if (!el || !list || built.rows.length === 0) return null;
    const inset = stickyFile ? REVIEW_FILE_HEADER_BAR_PX : 0;
    const at = list.findItemIndex(el.scrollTop + inset);
    const position = findRowPosition(built, at);
    if (!position || built.rows[at]?.kind !== 'line-block') return position;
    const path = files[position.fileIndex]?.path;
    const lines = [...el.querySelectorAll<HTMLElement>('[data-find-key^="l:"]')].filter((line) => findLineKeyPath(line.dataset.findKey!) === path);
    const line = findLineKeyIndex(elementAtEdge(lines, el.getBoundingClientRect().top + inset)?.dataset.findKey ?? '');
    return line !== null && line >= position.line ? { fileIndex: position.fileIndex, line } : position;
  }

  /** Find: scrolls the row showing a match at `at` (resolved against the
   * files shown, reviewFindRows.ts) so its line sits a third down the
   * viewport. False when the row model does not show it. */
  export function revealFindPlace(at: { fileIndex: number; place: ReviewFindPlace }): boolean {
    const ref = listRef;
    const target = ref ? findHitRow(built, files, at) : null;
    if (!ref || !target) return false;
    ref.scrollToIndex(target.index, { offset: target.within - Math.round(ref.getViewportSize() / 3) });
    return true;
  }

  /** Find: moves the view by `delta` pixels through the scroll owner. */
  export function scrollFindBy(delta: number): void {
    const el = scrollEl;
    if (el && delta !== 0) scroll.applyScrollTarget(Math.max(0, el.scrollTop + delta));
  }

</script>

<div class="relative h-full min-h-0 min-w-0 flex-1" data-testid="review-diff-body">
  {#if stickyFile}
    <!-- left/right match the row slabs' inset (--review-slab-inset). -->
    <div
      class="absolute left-[var(--review-slab-inset)] top-0 z-10 shadow-sheet"
      style:right="calc({scrollbarInset}px + var(--review-slab-inset))"
      data-testid="review-sticky-header"
    >
      <ReviewFileHeaderRow
        file={stickyFile}
        collapsed={collapsedPaths.has(stickyFile?.path ?? '')}
        onToggle={toggleStickyFile}
        overlay
        onJump={jumpToStickyFile}
      />
    </div>
  {/if}
  <div
    bind:this={scrollEl}
    class="h-full overflow-y-auto bg-surface-0 focus:outline-none"
    style:overflow-anchor="none"
    tabindex="0"
    role="grid"
    aria-label="Review diff"
    data-testid="review-scroll"
    onkeydown={handleKeydown}
  >
    {#key geometryKey}
      <LongListVirtualizer
        bind:this={listRef}
        data={built.rows}
        {getKey}
        scrollRef={scrollEl}
        {estimate}
        renderAll={IS_TEST}
        applyScrollTarget={scroll.applyScrollTarget}
        onCompensation={scroll.applyCompensation}
        onscroll={(offset) => { updateSticky(offset); captureAnchor(offset); }}
        onscrollend={() => saveReadingPosition(scrollKey, readingAnchor)}
      >
        {#snippet children(row: ReviewRow)}
          <ReviewDiffRow
            {row}
            file={files[row.fileIndex]}
            {blockRows}
            collapsed={collapsedPaths.has(files[row.fileIndex]?.path ?? '')}
            {onToggleCollapsed}
            {spanOwner}
            {spanContext}
            {painted}
            {wordWrap}
            pulse={row === pulseRow && pulse ? pulse.generation : 0}
            {overview}
            {draftEditor}
            {commentThread}
            {prThread}
            {onAddComment}
            {onExpandFold}
            {onExpandGap}
          />
        {/snippet}
      </LongListVirtualizer>
    {/key}
    {#if files.length === 0}
      {@render empty?.()}
    {/if}
  </div>
</div>
