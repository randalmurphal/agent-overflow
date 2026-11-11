<script lang="ts" module>
  /** What the pane shows; `none` while it shows nothing searchable. */
  export type ReviewFindView = 'diff' | 'conflicts' | 'log' | 'none';
</script>

<script lang="ts">
  // The review pane's find: registers the find provider
  // (stores/reviewFind.svelte.ts) under the pane's find key while mounted,
  // renders that key's bar, and does the DOM work: the highlights over the
  // mounted rows, the reading position, and bringing a match on screen.
  //
  // The view searched is the one the pane shows: the diff's listed files
  // (filter, commit and hidden tests applied) with their comments, the
  // conflict view's files, or the open CI job log. A content change in a
  // view rescans in place; a live log rescans at most every half second.
  // Another job's log starts over, and a log answer applies only to the
  // job and log version it was read from.
  // A match in a collapsed file, a settled thread or a closed log section
  // opens it; one in a collapsed conflict fold opens the fold first. A
  // reveal is one attempt that never waits on frames or timers: it asks
  // the list for the match's row when the row is not mounted, and scrolls
  // a painted match toward where it can be shown, answering shown once it
  // is in view or the scroll would change nothing. Unshown, it stays owed
  // and the store asks it again at the next publish, content change or
  // repaint (rows mounted, a block loaded). The reader's own scroll input
  // on the view, or a jump the view makes for them, ends it. A match is
  // an identity: the painter, the shift and the reveal each ask whether it
  // is present in the content shown at that moment, and where
  // (`resolveDiff`, `resolveLog`); one not present now is neither painted,
  // shifted nor shown, and its reveal stays owed.
  // The reading position places a scan's own fresh matches.
  import { getContext, tick, untrack } from 'svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import { registerPaneFindProvider } from '../../stores/paneFind.svelte';
  import { createReviewFind, REVIEW_FIND_LIMIT, type ReviewFindSource } from '../../stores/reviewFind.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { ciLogAnswerHits, resolveCILogHit, type CILogFindHit, type CILogFindShown, type CILogResolution } from '../../utils/ciLogFind';
  import { findNeedle } from '../../utils/findMatch';
  import { createFindHighlighter, findHighlightName, type FindCurrent, type FindHighlighter } from '../../utils/findHighlights';
  import type { ReviewFile } from '../../utils/patchStore';
  import { createRefreshScheduler } from '../../utils/refreshScheduler';
  import { createMarkdownFindTextCache, type MarkdownFindText } from '../chat/markdown/markdownFindText';
  import { FORGE_ATTACHMENT_SOURCE_CONTEXT, type ForgeAttachmentSourceReader } from '../chat/markdown/forgeAttachmentContext';
  import { visibleBody } from '../../utils/reviewComments';
  import { readCommentDetails, sameCommentDetails, sameKeys, type MountedCommentDetails } from '../../utils/reviewFindDetails';
  import { findDraftKey, findThreadCommentOfKey, reviewCommentKey, scanReviewFind, type ReviewFindHit, type ReviewFindPlace } from '../../utils/reviewFindScan';
  import {
    findHitAtOrBelow,
    placeReviewFindHit,
    resolveReviewFindHit,
    seedReviewFindSpot,
    type ReviewFindLocateContent,
    type ReviewFindResolution,
    type ReviewFindShown,
  } from '../../utils/reviewFindRows';
  import { REVIEW_FILE_HEADER_BAR_PX } from '../../utils/reviewRows';
  import { watchReaderScrollInput } from '../../utils/scroll/readerInput';

  type Hit = ReviewFindHit | CILogFindHit;

  interface DiffSurface {
    findReadingPosition(): { fileIndex: number; line: number } | null;
    revealFindPlace(at: { fileIndex: number; place: ReviewFindPlace }): boolean;
    scrollFindBy(delta: number): void;
  }

  interface LogSurface {
    findReadingLine(): number;
    findLineText(line: number): string | null;
    findLineLoad(line: number): { loading: boolean; error: string | null };
    openFindSection(sectionKey: string): boolean;
    revealFindLine(line: number): void;
    scrollFindBy(delta: number): void;
  }

  interface Props {
    /** The key the find state, provider and highlights are kept under. */
    findKey: string;
    title: string;
    review: ReviewPaneState;
    view: ReviewFindView;
    /** The files the diff or conflict view lists. */
    files: readonly ReviewFile[];
    root: HTMLElement | undefined;
    diff: () => DiffSurface | undefined;
    log: () => LogSurface | undefined;
  }

  let { findKey, title, review, view, files, root, diff, log }: Props = $props();

  const isLogHit = (hit: Hit): hit is CILogFindHit => !('at' in hit);
  const pause = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

  function scroller(): HTMLElement | null {
    return root?.querySelector<HTMLElement>('[data-testid="review-scroll"], [data-testid="review-ci-log-scroll"]') ?? null;
  }

  let highlighter: FindHighlighter | null = null;
  let highlighterRoot: HTMLElement | null = null;
  let paintedQuery = '';

  // PR comments are searched as they render (ReviewThreadComments), with
  // the <details> the reader toggled in mounted rows as they left them.
  // Held while find is open.
  const readForgeSource = getContext<ForgeAttachmentSourceReader | undefined>(FORGE_ATTACHMENT_SOURCE_CONTEXT);
  let texts: ((source: string, open?: readonly boolean[]) => MarkdownFindText) | null = null;
  const NO_DETAILS: MountedCommentDetails = { toggled: new Map() };
  let details = NO_DETAILS;
  let editing = new Set<string>();

  function commentTexts(): (source: string, open?: readonly boolean[]) => MarkdownFindText {
    return (texts ??= createMarkdownFindTextCache({ embeddedHtml: true, forgeSource: readForgeSource?.() ?? null }));
  }

  function readDetails(): MountedCommentDetails {
    if (!root || untrack(() => view) !== 'diff') return NO_DETAILS;
    return readCommentDetails(root, (rowKey) => {
      const at = findThreadCommentOfKey(rowKey);
      const comment = at ? review.prThreads.find((thread) => thread.id === at.threadId)?.comments.find((each) => reviewCommentKey(each) === at.comment) : undefined;
      return comment ? commentTexts()(visibleBody(comment.body)).details : null;
    });
  }

  // The drafts open in their editor, mounted or not: the store keeps an
  // open edit across a row unmount, and its saved body is not on screen.
  function readEditing(): Set<string> {
    return untrack(() => {
      if (view !== 'diff') return new Set<string>();
      return new Set(review.drafts.filter((draft) => review.draftEditBodyFor(draft.id) !== null).map((draft) => findDraftKey(draft.id)));
    });
  }

  // A <details> opened or closed by hand, a draft's editor opened or
  // closed, or a row that remounted as written: rescan in place. True when
  // it changed what shows.
  function reconcileMounted(): boolean {
    const nextDetails = readDetails();
    const nextEditing = readEditing();
    if (sameCommentDetails(nextDetails, details) && sameKeys(nextEditing, editing)) return false;
    details = nextDetails;
    editing = nextEditing;
    find.refresh();
    return true;
  }

  // The diff or conflict view's content: files and comments.
  function diffContent(): ReviewFindLocateContent {
    return untrack(() => {
      const conflicts = view === 'conflicts';
      return { files, drafts: conflicts ? [] : review.drafts, prThreads: conflicts ? [] : review.prThreads };
    });
  }

  // What the scan reads, as shown now: the reader's <details> and editors.
  function shownDiff(): ReviewFindShown {
    const conflicts = untrack(() => view) === 'conflicts';
    return {
      ...diffContent(),
      commentRuns: (body, rowKey) => commentTexts()(body, details.toggled.get(rowKey)).all,
      editing,
      foldLines: conflicts ? (path) => untrack(() => review.conflictFoldLines(path)) : undefined,
    };
  }

  // Whether a match is present in what the view shows now, and where.
  function resolveDiff(hit: ReviewFindHit): ReviewFindResolution {
    return resolveReviewFindHit(shownDiff(), hit, findNeedle(paintedQuery));
  }

  function resolveLog(hit: CILogFindHit): CILogResolution {
    const sections = untrack(() => review.ciLog?.sections) ?? [];
    const surface = log();
    return resolveCILogHit(sections, (line) => surface?.findLineText(line) ?? null, findNeedle(paintedQuery), hit);
  }

  // The element and index the highlighter paints as current for `hit`;
  // none for a match not present now, or one whose text is not loaded.
  function currentOf(hit: Hit): FindCurrent {
    if (isLogHit(hit)) {
      const resolved = resolveLog(hit);
      return resolved.kind === 'present' ? { row: resolved.at.rowKey, occurrence: resolved.at.occurrence } : null;
    }
    const resolved = resolveDiff(hit);
    return resolved.kind === 'present' ? { row: resolved.at.rowKey, occurrence: resolved.at.ordinal } : null;
  }

  // The painted match: re-found at every paint, including the recompute
  // after the rows change, so content that moved under them (a gap
  // expanded above it, a filter) paints and shifts its new row.
  let painted: Hit | null = null;
  const paintedCurrent = (): FindCurrent => (painted ? currentOf(painted) : null);

  function releaseHighlighter(): void {
    texts = null;
    details = NO_DETAILS;
    editing = new Set();
    highlighter?.dispose();
    highlighter = null;
    highlighterRoot = null;
    paintedQuery = '';
    painted = null;
    restoreShifted(null);
  }

  // The pixels that bring a span [start, end) into [lo, hi) on one axis: 0
  // when it is in view. A span larger than the room is in view while its
  // start is, and is placed with its start at `lo`, so the reader sees
  // where it begins; another is placed a third of the way in.
  function axisDelta(start: number, end: number, lo: number, hi: number): number {
    const room = hi - lo;
    if (end - start > room) return start >= lo && start < hi ? 0 : Math.round(start - lo);
    if (start >= lo && end <= hi) return 0;
    return Math.round(start - (lo + room / 3));
  }

  interface Placement {
    /** Pixels that bring the match into view vertically; 0 when it is. */
    dy: number;
    /** Pixels that bring it into view across `across`; 0 when it is. */
    dx: number;
    /** What scrolls it horizontally: a code line that clips it (wrap off),
     * else the scroller when that scrolls sideways; null for neither. */
    across: HTMLElement | null;
  }

  // The first box between the match and the scroller that clips it
  // horizontally, else the scroller when its content is wider than it.
  function clipAcross(node: Node, el: HTMLElement): HTMLElement | null {
    for (let at = node.parentElement; at && at !== el; at = at.parentElement) {
      if (at.scrollWidth > at.clientWidth && getComputedStyle(at).overflowX !== 'visible') return at;
    }
    return el.scrollWidth > el.clientWidth ? el : null;
  }

  // The visible horizontal room of `across` within the scroller.
  function roomAcross(across: HTMLElement, el: HTMLElement): { left: number; right: number } {
    const view = el.getBoundingClientRect();
    const box = across !== el ? across.getBoundingClientRect() : view;
    return { left: Math.max(box.left, view.left), right: Math.min(box.right, view.right - (el.offsetWidth - el.clientWidth)) };
  }

  // Where the current match's painted range sits against what the
  // scroller shows, on both axes; null when it is not painted.
  function placement(): Placement | null {
    const range = highlighter?.currentRange();
    const el = scroller();
    if (!range || !el) return null;
    const rect = range.getBoundingClientRect();
    if (rect.height === 0 && rect.width === 0) return null;
    const view = el.getBoundingClientRect();
    const top = view.top + (isLogView() ? 0 : REVIEW_FILE_HEADER_BAR_PX);
    const dy = axisDelta(rect.top, rect.bottom, top, view.bottom);
    const across = clipAcross(range.startContainer, el);
    if (!across) return { dy, dx: 0, across };
    const { left, right } = roomAcross(across, el);
    return { dy, dx: axisDelta(rect.left, rect.right, left, right), across };
  }

  function inView(): boolean {
    const at = placement();
    return at !== null && at.dy === 0 && at.dx === 0;
  }

  // The code lines find has scrolled sideways (wrap off). Derived from the
  // current match, never set by a reveal: only the line that clips the
  // current match is shifted, to show it; every other line goes back, so
  // none is shifted with no current match, no results or find closed.
  const shiftedLines = new Set<HTMLElement>();

  function restoreShifted(keep: HTMLElement | null): void {
    for (const line of shiftedLines) {
      if (line === keep) continue;
      if (line.isConnected) line.scrollLeft = 0;
      shiftedLines.delete(line);
    }
  }

  // Runs whenever the current match, its row or the layout may have
  // changed: a paint, a recompute of the matches, a wrap toggle.
  function syncShift(): void {
    const el = scroller();
    const range = el ? (highlighter?.currentRange() ?? null) : null;
    const across = range && el ? clipAcross(range.startContainer, el) : null;
    const line = across && across !== el ? across : null;
    if (line && range && el) {
      const rect = range.getBoundingClientRect();
      const { left, right } = roomAcross(line, el);
      const dx = axisDelta(rect.left, rect.right, left, right);
      if (dx !== 0) line.scrollLeft = Math.max(0, line.scrollLeft + dx);
      if (line.scrollLeft > 0) shiftedLines.add(line);
    }
    restoreShifted(line);
  }

  function isLogView(): boolean {
    return untrack(() => view) === 'log';
  }

  // Whether the row keyed `rowKey` is mounted.
  function mounted(rowKey: string): boolean {
    return root?.querySelector(`[data-find-key="${CSS.escape(rowKey)}"]`) != null;
  }

  // One attempt at the painted current match, with no waiting: scrolls
  // toward the placement the scroll geometry can reach. True when it is in
  // view after, or when the scroll it asks for changes nothing (clamped at
  // an end, or its start shown in a box it is larger than); false while it
  // is not painted or the scroll moved and it is not in view yet, which a
  // repaint asks again.
  function placeCurrent(scrollBy: (delta: number) => void): boolean {
    const at = placement();
    const el = scroller();
    if (!at || !el) return false;
    if (at.dy === 0 && at.dx === 0) {
      syncShift();
      return true;
    }
    const before = [el.scrollTop, el.scrollLeft, at.across?.scrollLeft ?? 0];
    if (at.dy !== 0) scrollBy(at.dy);
    // The scroller itself scrolling sideways; a clipped line is syncShift's.
    if (at.dx !== 0 && at.across === el) el.scrollLeft = Math.max(0, el.scrollLeft + at.dx);
    syncShift();
    if (inView()) return true;
    const after = [el.scrollTop, el.scrollLeft, at.across?.scrollLeft ?? 0];
    return after.every((value, index) => value === before[index]);
  }

  // Whether `hit` is present now, judged with its line's text read back
  // and held while it is judged when the patch budget evicted it.
  async function presentDiff(hit: ReviewFindHit): Promise<ReviewFindResolution> {
    const resolved = resolveDiff(hit);
    if (resolved.kind !== 'evicted') return resolved;
    const { file, lineIndex } = resolved;
    return file.body.whenResident(() => resolveDiff(hit), lineIndex, lineIndex + 1);
  }

  // One attempt at a diff match, resolved in the content shown now: its
  // row is asked of the list when it is not mounted, else the painted
  // match is placed.
  function attemptDiff(hit: ReviewFindHit): boolean {
    if (!find.wants(hit.identity)) return false;
    const resolved = resolveDiff(hit);
    if (resolved.kind !== 'present') return false;
    if (!mounted(resolved.at.rowKey)) {
      diff()?.revealFindPlace(resolved.at);
      return false;
    }
    return placeCurrent((delta) => diff()?.scrollFindBy(delta));
  }

  // Opens what hides the match (its file, its settled thread, its fold),
  // then attempts it. What it opens changes the content, so the call is in
  // flight until that content shows; a match not present now is not
  // shown: the answer is false, and it stays owed.
  async function openDiffHit(hit: ReviewFindHit): Promise<boolean> {
    if ((await presentDiff(hit)).kind !== 'present') return false;
    const conflicts = untrack(() => view) === 'conflicts';
    if (conflicts && review.conflictCollapsedPaths.has(hit.path)) await review.toggleConflictCollapsed(hit.path);
    else if (!conflicts && review.collapsedPaths.has(hit.path)) review.toggleCollapsed(hit.path);
    const at = hit.at;
    if (at.kind === 'thread') {
      const thread = review.prThreads.find((t) => t.id === at.threadId);
      if (thread?.isResolved && !review.expandedPRThreadIds.has(thread.id)) review.togglePRThread(thread.id);
    }
    await tick();
    const resolved = await presentDiff(hit);
    if (resolved.kind === 'present' && resolved.at.place.kind === 'fold') {
      if (!find.wants(hit.identity)) return false;
      review.expandConflictFold(hit.path, resolved.at.place.foldId);
      await tick();
      await presentDiff(hit);
    }
    return attemptDiff(hit);
  }

  // Whether the diff shows what the match needs open, with its text
  // resident: then one attempt answers now.
  function openInDiff(hit: ReviewFindHit): boolean {
    const resolved = resolveDiff(hit);
    if (resolved.kind !== 'present' || resolved.at.place.kind === 'fold') return false;
    const conflicts = untrack(() => view) === 'conflicts';
    if ((conflicts ? review.conflictCollapsedPaths : review.collapsedPaths).has(hit.path)) return false;
    const at = hit.at;
    if (at.kind !== 'thread') return true;
    const thread = review.prThreads.find((t) => t.id === at.threadId);
    return !thread?.isResolved || review.expandedPRThreadIds.has(thread.id);
  }

  function revealDiff(hit: ReviewFindHit): boolean | Promise<boolean> {
    return openInDiff(hit) ? attemptDiff(hit) : openDiffHit(hit);
  }

  // One attempt at a log match, resolved in the log shown now: a closed
  // section is opened (in flight until it shows), a row not mounted is
  // asked of the list, a row whose block failed rejects with its error,
  // else the painted match is placed.
  function attemptLog(hit: CILogFindHit, surface: LogSurface): boolean | Promise<boolean> {
    if (!find.wants(hit.identity)) return false;
    const resolved = resolveLog(hit);
    if (resolved.kind === 'absent') return false;
    const sectionKey = resolved.kind === 'present' ? resolved.at.sectionKey : resolved.sectionKey;
    if (surface.openFindSection(sectionKey)) return tick().then(() => attemptLog(hit, surface));
    if (resolved.kind === 'unknown') {
      const load = surface.findLineLoad(hit.line);
      if (load.error !== null) return Promise.reject(new Error(load.error));
      // Its row is asked for whatever its block's read is doing: the
      // mounted row takes the block, read or in flight, and paints it.
      surface.revealFindLine(hit.line);
      return false;
    }
    if (!mounted(resolved.at.rowKey)) {
      surface.revealFindLine(hit.line);
      return false;
    }
    return placeCurrent((delta) => surface.scrollFindBy(delta));
  }

  // A revision, commit, filter or conflict tree change replaces `files`,
  // and the effect below stops the scan that read the old ones.
  const diffSource: ReviewFindSource<Hit> = {
    remote: false,
    run: (query, context) => {
      const conflicts = untrack(() => view) === 'conflicts';
      details = readDetails();
      editing = readEditing();
      const content = untrack(() => ({
        files,
        drafts: conflicts ? [] : review.drafts,
        prThreads: conflicts ? [] : review.prThreads,
        foldLines: conflicts ? (path: string) => review.conflictFoldLines(path) : undefined,
        commentText: (body: string, rowKey: string) => commentTexts()(body, details.toggled.get(rowKey)).all,
        editing,
      }));
      return scanReviewFind(content, query, {
        limit: REVIEW_FIND_LIMIT,
        live: context.live,
        emit: (hit, file, spot) => {
          seedReviewFindSpot(file, hit, spot);
          context.emit(hit);
        },
        pause,
      });
    },
  };

  function shownLog(): CILogFindShown | null {
    return untrack(() => {
      const jobId = review.ciLogView?.jobId;
      const log = review.ciLog;
      return jobId && log ? { jobId, version: log.version, sections: log.sections } : null;
    });
  }

  const logSource: ReviewFindSource<Hit> = {
    remote: true,
    run: async (query, context) => {
      const asked = shownLog()?.jobId;
      if (!asked) return null;
      const answer = await review.searchCILog(query, REVIEW_FIND_LIMIT);
      if (!context.live()) return false;
      const hits = ciLogAnswerHits(answer, asked, shownLog(), REVIEW_FIND_LIMIT);
      if (!hits) {
        // Read from another job or version than the one shown: ask again.
        logRefreshes.request();
        return null;
      }
      for (const hit of hits) context.emit(hit);
      return answer.capped;
    },
  };

  const find = createReviewFind<Hit>({
    paneId: () => findKey,
    source: () => {
      const shown = untrack(() => view);
      if (shown === 'log') return logSource;
      return shown === 'none' ? null : diffSource;
    },
    readingPosition: () => {
      if (isLogView()) {
        const line = log()?.findReadingLine() ?? 0;
        return (hit) => isLogHit(hit) && hit.line >= line;
      }
      const position = diff()?.findReadingPosition() ?? null;
      const content = diffContent();
      const memo = new Map<string, Map<string, number>>();
      return (hit) => !isLogHit(hit) && findHitAtOrBelow(placeReviewFindHit(content, hit, memo), position);
    },
    reveal: (hit, force) => {
      if (!force && inView()) return true;
      if (isLogHit(hit)) {
        const surface = log();
        return surface ? attemptLog(hit, surface) : false;
      }
      return revealDiff(hit);
    },
    paint: (query, current) => {
      if (!query) {
        releaseHighlighter();
        return;
      }
      const at = root ?? null;
      if (!at) return;
      if (highlighterRoot !== at) {
        releaseHighlighter();
        highlighter = createFindHighlighter({
          root: at,
          name: findHighlightName(findKey),
          rowSelector: '[data-find-key]',
          rowKeyAttribute: 'data-find-key',
          rowsOnly: true,
          onMatches: () => {
            // What shows is read first, so the current match is found in it.
            const rescanned = reconcileMounted();
            if (rescanned) highlighter?.setCurrent(paintedCurrent);
            syncShift();
            // Rows appeared or changed: an owed reveal is asked again (a
            // rescan asked it already).
            if (!rescanned) find.repainted();
          },
        });
        highlighterRoot = at;
      }
      if (query !== paintedQuery) {
        paintedQuery = query;
        highlighter!.setQuery(query);
      }
      painted = current;
      highlighter!.setCurrent(paintedCurrent);
      syncShift();
    },
  }, () => (view === 'log' ? 'log' : view === 'conflicts' ? 'conflicts' : 'diff'));

  $effect(() => {
    const key = findKey;
    const release = untrack(() => registerPaneFindProvider(key, find));
    return () => {
      release();
      releaseHighlighter();
    };
  });

  // Another view starts the query over; new content in the same view
  // rescans in place. A live log moves often: its rescans are rate-bounded.
  const logRefreshes = createRefreshScheduler({
    name: 'review find log',
    delayMs: 500,
    maxWaitMs: 2000,
    run: () => find.refresh(),
  });
  $effect(() => () => logRefreshes.dispose());
  // The reader scrolling the view, or a jump made for them, ends the owed
  // reveal.
  $effect(() => {
    const at = root;
    if (!at) return;
    return watchReaderScrollInput(at, scroller, () => find.withdraw());
  });
  // A wrap toggle changes which lines clip, after its rows lay out.
  $effect(() => {
    void review.wordWrap;
    const frame = requestAnimationFrame(syncShift);
    return () => cancelAnimationFrame(frame);
  });
  // Another job's log is another view.
  const scopeOf = (shown: ReviewFindView) => `${shown}:${shown === 'log' ? (review.ciLogView?.jobId ?? '') : ''}`;
  let seenView = untrack(() => scopeOf(view));
  $effect(() => {
    const shown = view;
    const scope = scopeOf(shown);
    const content = shown === 'log'
      ? [review.ciLog?.version, review.ciLog?.sections]
      : [files, shown === 'diff' ? review.drafts : null, shown === 'diff' ? review.prThreads : null];
    void content;
    untrack(() => {
      if (scope !== seenView) {
        seenView = scope;
        logRefreshes.reset();
        find.rescope();
        return;
      }
      if (shown === 'log') logRefreshes.request();
      else find.refresh();
    });
  });
</script>

<PaneFindBar paneId={findKey} {title} fallbackFocus={scroller} />
