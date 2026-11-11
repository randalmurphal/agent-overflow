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
  // opens it; one in a collapsed conflict fold opens the fold, and the
  // rescan that follows brings it on screen. After the list scrolls to a
  // match's row, the painted range corrects the landing (wrapped lines).
  import { getContext, tick, untrack } from 'svelte';
  import PaneFindBar from '../panes/PaneFindBar.svelte';
  import { registerPaneFindProvider } from '../../stores/paneFind.svelte';
  import { createReviewFind, REVIEW_FIND_LIMIT, type ReviewFindSource } from '../../stores/reviewFind.svelte';
  import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
  import { ciLogAnswerHits, type CILogFindHit, type CILogFindShown } from '../../utils/ciLogFind';
  import { createFindHighlighter, findHighlightName, type FindHighlighter } from '../../utils/findHighlights';
  import type { ReviewFile } from '../../utils/patchStore';
  import { createRefreshScheduler } from '../../utils/refreshScheduler';
  import { createMarkdownFindTextCache, type MarkdownFindText } from '../chat/markdown/markdownFindText';
  import { FORGE_ATTACHMENT_SOURCE_CONTEXT, type ForgeAttachmentSourceReader } from '../chat/markdown/forgeAttachmentContext';
  import { visibleBody } from '../../utils/reviewComments';
  import { commentOfRowKey, editingDrafts, readCommentDetails, sameCommentDetails, sameKeys, type MountedCommentDetails } from '../../utils/reviewFindDetails';
  import { scanReviewFind, type ReviewFindHit } from '../../utils/reviewFindScan';
  import { findHitAtOrBelow } from '../../utils/reviewFindRows';
  import { REVIEW_FILE_HEADER_BAR_PX } from '../../utils/reviewRows';

  type Hit = ReviewFindHit | CILogFindHit;

  interface DiffSurface {
    findReadingPosition(): { fileIndex: number; line: number } | null;
    revealFindHit(hit: ReviewFindHit): boolean;
    scrollFindBy(delta: number): void;
  }

  interface LogSurface {
    findReadingLine(): number;
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

  const isLogHit = (hit: Hit): hit is CILogFindHit => 'sectionKey' in hit;
  const pause = () => new Promise<void>((resolve) => setTimeout(resolve, 0));

  function scroller(): HTMLElement | null {
    return root?.querySelector<HTMLElement>('[data-testid="review-scroll"], [data-testid="review-ci-log-scroll"]') ?? null;
  }

  let highlighter: FindHighlighter | null = null;
  let highlighterRoot: HTMLElement | null = null;
  let paintedQuery = '';
  // The match a scroll just aimed at; its painted range corrects the landing.
  // `sideways` once the vertical move is done and only the horizontal
  // one remains.
  let landing: { identity: string; until: number; sideways?: boolean } | null = null;

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
      const at = commentOfRowKey(rowKey);
      const comment = at ? review.prThreads.find((thread) => thread.id === at.threadId)?.comments[at.index] : undefined;
      return comment ? commentTexts()(visibleBody(comment.body)).details : null;
    });
  }

  function readEditing(): Set<string> {
    return root && untrack(() => view) === 'diff' ? editingDrafts(root) : new Set();
  }

  // A <details> opened or closed by hand, a draft's editor opened or
  // closed, or a row that remounted as written: rescan in place.
  function reconcileMounted(): void {
    const nextDetails = readDetails();
    const nextEditing = readEditing();
    if (sameCommentDetails(nextDetails, details) && sameKeys(nextEditing, editing)) return;
    details = nextDetails;
    editing = nextEditing;
    find.refresh();
  }

  function releaseHighlighter(): void {
    texts = null;
    details = NO_DETAILS;
    editing = new Set();
    highlighter?.dispose();
    highlighter = null;
    highlighterRoot = null;
    paintedQuery = '';
    landing = null;
    unshift();
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

  // A code line find scrolled sideways to show a match; put back when find
  // moves to another line or closes.
  let shifted: HTMLElement | null = null;

  function unshift(): void {
    if (shifted?.isConnected) shifted.scrollLeft = 0;
    shifted = null;
  }

  function shiftBy(across: HTMLElement, dx: number): void {
    if (across !== scroller()) shifted = across;
    across.scrollLeft = Math.max(0, across.scrollLeft + dx);
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
    if (at.dy !== 0 && !aim.sideways) {
      if (isLogView()) log()?.scrollFindBy(at.dy);
      else diff()?.scrollFindBy(at.dy);
      landing = null;
      // The row may move or remount: place it sideways once it settles.
      if (at.dx !== 0) {
        landing = { ...aim, sideways: true };
        requestAnimationFrame(() => requestAnimationFrame(land));
      }
      return;
    }
    landing = null;
    if (shifted && shifted !== at.across) unshift();
    if (at.dx !== 0 && at.across) shiftBy(at.across, at.dx);
  }

  function aim(hit: Hit): void {
    landing = { identity: hit.identity, until: performance.now() + 1500 };
    requestAnimationFrame(() => requestAnimationFrame(land));
  }

  // Opens what hides the match (its file, its settled thread), then
  // scrolls once the rows are built, if its reveal is still wanted (a
  // rescan that restarts the query while the rows build keeps it).
  async function showDiffHit(hit: ReviewFindHit): Promise<boolean> {
    const conflicts = untrack(() => view) === 'conflicts';
    if (conflicts && review.conflictCollapsedPaths.has(hit.path)) await review.toggleConflictCollapsed(hit.path);
    else if (!conflicts && review.collapsedPaths.has(hit.path)) review.toggleCollapsed(hit.path);
    const place = hit.place;
    if (place.kind === 'thread') {
      const thread = review.prThreads.find((t) => t.id === place.threadId);
      if (thread?.isResolved && !review.expandedPRThreadIds.has(thread.id)) review.togglePRThread(thread.id);
    }
    await tick();
    if (!find.wants(hit.identity)) return false;
    const shown = diff()?.revealFindHit(hit) ?? false;
    if (shown) aim(hit);
    return shown;
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
        commentText: (body: string, rowKey: string) => details.recounted.get(rowKey) ?? commentTexts()(body, details.toggled.get(rowKey)).runs,
        editing,
      }));
      return scanReviewFind(content, query, { limit: REVIEW_FIND_LIMIT, live: context.live, emit: context.emit, pause });
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
      return (hit) => !isLogHit(hit) && findHitAtOrBelow(hit, position);
    },
    reveal: (hit, force) => {
      if (!force && inView()) return true;
      if (isLogHit(hit)) {
        const surface = log();
        if (!surface) return false;
        return surface.revealFindLine(hit.line, hit.sectionKey).then(() => {
          aim(hit);
          return true;
        });
      }
      const place = hit.place;
      if (place.kind === 'fold') {
        review.expandConflictFold(hit.path, place.foldId);
        return false;
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
            land();
            reconcileMounted();
          },
        });
        highlighterRoot = at;
      }
      if (query !== paintedQuery) {
        paintedQuery = query;
        highlighter!.setQuery(query);
      }
      highlighter!.setCurrent(current ? { row: current.rowKey, occurrence: current.occurrence } : null);
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
      } else if (shown === 'log') {
        logRefreshes.request();
      } else {
        find.refresh();
      }
    });
  });
</script>

<PaneFindBar paneId={findKey} {title} fallbackFocus={scroller} />
