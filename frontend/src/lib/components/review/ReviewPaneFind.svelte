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
  // opens it; one in a collapsed conflict fold opens the fold first. After
  // the list scrolls to a match's row, the painted range corrects the
  // landing (wrapped lines). A match is an identity: the painter, the shift
  // and the reveal each ask whether it is present in the content shown at
  // that moment, and where (`resolveDiff`, `resolveLog`); one not present
  // now is neither painted, shifted nor shown, and its reveal stays owed.
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
  import { editingDrafts, readCommentDetails, sameCommentDetails, sameKeys, type MountedCommentDetails } from '../../utils/reviewFindDetails';
  import { findThreadCommentOfKey, reviewCommentKey, scanReviewFind, type ReviewFindHit, type ReviewFindPlace } from '../../utils/reviewFindScan';
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

  type Hit = ReviewFindHit | CILogFindHit;

  interface DiffSurface {
    findReadingPosition(): { fileIndex: number; line: number } | null;
    revealFindPlace(at: { fileIndex: number; place: ReviewFindPlace }): boolean;
    scrollFindBy(delta: number): void;
  }

  interface LogSurface {
    findReadingLine(): number;
    findShownLine(line: number): string | null;
    revealFindLine(line: number, sectionKey: string): Promise<void>;
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
  // The match a scroll just aimed at; its painted range corrects the landing.
  let landing: { identity: string; until: number } | null = null;

  // PR comments are searched as they render (ReviewThreadComments), with
  // the <details> the reader toggled in mounted rows as they left them.
  // Held while find is open.
  const readForgeSource = getContext<ForgeAttachmentSourceReader | undefined>(FORGE_ATTACHMENT_SOURCE_CONTEXT);
  let texts: ((source: string, open?: readonly boolean[]) => MarkdownFindText) | null = null;
  const NO_DETAILS: MountedCommentDetails = { toggled: new Map(), recounted: new Map() };
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

  function readEditing(): Set<string> {
    return root && untrack(() => view) === 'diff' ? editingDrafts(root) : new Set();
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
      commentRuns: (body, rowKey) => details.recounted.get(rowKey)?.map((text) => ({ text, shown: true })) ?? commentTexts()(body, details.toggled.get(rowKey)).all,
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
    return resolveCILogHit(sections, (line) => surface?.findShownLine(line) ?? null, findNeedle(paintedQuery), hit);
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
    landing = null;
    restoreShifted(null);
  }

  interface Placement {
    /** Pixels that put the match a third down; 0 when it is in view. */
    dy: number;
    /** Pixels that put it a third across `across`; 0 when it is in view. */
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

  // Where the current match's range sits against what the scroller shows,
  // on both axes; null when it is not mounted.
  function placement(): Placement | null {
    const range = highlighter?.currentRange();
    const el = scroller();
    if (!range || !el) return null;
    const rect = range.getBoundingClientRect();
    if (rect.height === 0 && rect.width === 0) return null;
    const view = el.getBoundingClientRect();
    const top = view.top + (isLogView() ? 0 : REVIEW_FILE_HEADER_BAR_PX);
    const dy = rect.top >= top && rect.bottom <= view.bottom ? 0 : Math.round(rect.top - (view.top + view.height / 3));
    const across = clipAcross(range.startContainer, el);
    const box = across && across !== el ? across.getBoundingClientRect() : view;
    const left = Math.max(box.left, view.left);
    const right = Math.min(box.right, view.right - (el.offsetWidth - el.clientWidth));
    const dx = rect.left >= left && rect.right <= right ? 0 : Math.round(rect.left - (left + (right - left) / 3));
    return { dy, dx: across ? dx : 0, across };
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
    if (line && range) {
      const rect = range.getBoundingClientRect();
      const box = line.getBoundingClientRect();
      if (rect.left < box.left || rect.right > box.right) {
        line.scrollLeft = Math.max(0, line.scrollLeft + Math.round(rect.left - (box.left + box.width / 3)));
      }
      if (line.scrollLeft > 0) shiftedLines.add(line);
    }
    restoreShifted(line);
  }

  function isLogView(): boolean {
    return untrack(() => view) === 'log';
  }

  function land(): void {
    const aim = landing;
    if (!aim || find.hits[find.result.current]?.identity !== aim.identity) return;
    const at = placement();
    if (at === null) {
      if (performance.now() > aim.until) landing = null;
      return;
    }
    landing = null;
    if (at.dy !== 0) {
      if (isLogView()) log()?.scrollFindBy(at.dy);
      else diff()?.scrollFindBy(at.dy);
    }
    // The scroller itself scrolling sideways; a clipped line is syncShift's.
    const el = scroller();
    if (at.dx !== 0 && el && at.across === el) el.scrollLeft = Math.max(0, el.scrollLeft + at.dx);
    syncShift();
  }

  function aim(hit: Hit): void {
    landing = { identity: hit.identity, until: performance.now() + 1500 };
    requestAnimationFrame(() => requestAnimationFrame(land));
  }

  // Opens what hides the match (its file, its settled thread), then
  // scrolls once the rows are built, if its reveal is still wanted (a
  // rescan that restarts the query while the rows build keeps it).
  // Whether `hit` is present now, judged with its line's text read back
  // and held while it is judged when the patch budget evicted it.
  async function presentDiff(hit: ReviewFindHit): Promise<ReviewFindResolution> {
    const resolved = resolveDiff(hit);
    if (resolved.kind !== 'evicted') return resolved;
    const { file, lineIndex } = resolved;
    return file.body.whenResident(() => resolveDiff(hit), lineIndex, lineIndex + 1);
  }

  // One host call from asking to the answer: a fold it opens changes the
  // content, and the reveal finds the match in what that shows. A match
  // not present now is not shown: the answer is false, and it stays owed.
  async function showDiffHit(hit: ReviewFindHit): Promise<boolean> {
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
    let resolved = await presentDiff(hit);
    if (resolved.kind === 'present' && resolved.at.place.kind === 'fold') {
      if (!find.wants(hit.identity)) return false;
      review.expandConflictFold(hit.path, resolved.at.place.foldId);
      await tick();
      resolved = await presentDiff(hit);
    }
    if (resolved.kind !== 'present' || !find.wants(hit.identity)) return false;
    const shown = diff()?.revealFindPlace(resolved.at) ?? false;
    if (shown) aim(hit);
    return shown;
  }

  // Frames a reveal waits for a scrolled-to log line's block to load.
  const LOG_TEXT_FRAMES = 120;

  // Opens and scrolls to a log match present now. A line whose block is not
  // loaded is scrolled to first and judged once its text shows; one still
  // not loaded after that is shown as its line, and painted when it loads.
  async function showLogHit(hit: CILogFindHit, surface: LogSurface): Promise<boolean> {
    const resolved = resolveLog(hit);
    if (resolved.kind === 'absent') return false;
    const sectionKey = resolved.kind === 'present' ? resolved.at.sectionKey : resolved.sectionKey;
    await surface.revealFindLine(hit.line, sectionKey);
    if (resolved.kind === 'unknown') {
      for (let frame = 0; frame < LOG_TEXT_FRAMES && resolveLog(hit).kind === 'unknown'; frame += 1) {
        await new Promise((resolve) => requestAnimationFrame(resolve));
      }
      if (resolveLog(hit).kind === 'absent') return false;
    }
    aim(hit);
    return true;
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
        commentText: (body: string, rowKey: string) => details.recounted.get(rowKey)?.map((text) => ({ text, shown: true })) ?? commentTexts()(body, details.toggled.get(rowKey)).all,
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
        return surface ? showLogHit(hit, surface) : false;
      }
      return showDiffHit(hit);
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
            if (reconcileMounted()) highlighter?.setCurrent(paintedCurrent);
            land();
            syncShift();
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
