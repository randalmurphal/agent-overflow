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
  import { commentOfRowKey, editingDrafts, sameKeys, sameToggledDetails, toggledCommentDetails } from '../../utils/reviewFindDetails';
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
  let landing: { identity: string; until: number } | null = null;

  // PR comments are searched as they render (ReviewThreadComments), with
  // the <details> the reader toggled in mounted rows as they left them.
  // Held while find is open.
  const readForgeSource = getContext<ForgeAttachmentSourceReader | undefined>(FORGE_ATTACHMENT_SOURCE_CONTEXT);
  let texts: ((source: string, open?: readonly boolean[]) => MarkdownFindText) | null = null;
  let toggled = new Map<string, boolean[]>();
  let editing = new Set<string>();

  function commentTexts(): (source: string, open?: readonly boolean[]) => MarkdownFindText {
    return (texts ??= createMarkdownFindTextCache({ embeddedHtml: true, forgeSource: readForgeSource?.() ?? null }));
  }

  function readToggled(): Map<string, boolean[]> {
    if (!root || untrack(() => view) !== 'diff') return new Map();
    return toggledCommentDetails(root, (rowKey) => {
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
    const nextToggled = readToggled();
    const nextEditing = readEditing();
    if (sameToggledDetails(nextToggled, toggled) && sameKeys(nextEditing, editing)) return;
    toggled = nextToggled;
    editing = nextEditing;
    find.refresh();
  }

  function releaseHighlighter(): void {
    texts = null;
    toggled = new Map();
    editing = new Set();
    highlighter?.dispose();
    highlighter = null;
    highlighterRoot = null;
    paintedQuery = '';
    landing = null;
  }

  // Where the current match's range sits against the visible scroller:
  // null when it is not mounted, 0 when it is in view, else the pixels
  // that put it a third down.
  function offscreenBy(): number | null {
    const range = highlighter?.currentRange();
    const el = scroller();
    if (!range || !el) return null;
    const rect = range.getBoundingClientRect();
    const view = el.getBoundingClientRect();
    if (rect.height === 0 && rect.width === 0) return null;
    const top = view.top + (isLogView() ? 0 : REVIEW_FILE_HEADER_BAR_PX);
    if (rect.top >= top && rect.bottom <= view.bottom) return 0;
    return Math.round(rect.top - (view.top + view.height / 3));
  }

  function isLogView(): boolean {
    return untrack(() => view) === 'log';
  }

  function land(): void {
    const aim = landing;
    if (!aim || find.hits[find.result.current]?.identity !== aim.identity) return;
    const delta = offscreenBy();
    if (delta === null) {
      if (performance.now() > aim.until) landing = null;
      return;
    }
    landing = null;
    if (delta === 0) return;
    if (isLogView()) log()?.scrollFindBy(delta);
    else diff()?.scrollFindBy(delta);
  }

  function aim(hit: Hit): void {
    landing = { identity: hit.identity, until: performance.now() + 1500 };
    requestAnimationFrame(() => requestAnimationFrame(land));
  }

  // Opens what hides the match (its file, its settled thread), then
  // scrolls once the rows are built, if it is still the current match.
  async function showDiffHit(hit: ReviewFindHit): Promise<void> {
    const conflicts = untrack(() => view) === 'conflicts';
    if (conflicts && review.conflictCollapsedPaths.has(hit.path)) await review.toggleConflictCollapsed(hit.path);
    else if (!conflicts && review.collapsedPaths.has(hit.path)) review.toggleCollapsed(hit.path);
    const place = hit.place;
    if (place.kind === 'thread') {
      const thread = review.prThreads.find((t) => t.id === place.threadId);
      if (thread?.isResolved && !review.expandedPRThreadIds.has(thread.id)) review.togglePRThread(thread.id);
    }
    await tick();
    if (find.hits[find.result.current]?.identity !== hit.identity) return;
    if (diff()?.revealFindHit(hit)) aim(hit);
  }

  // A revision, commit, filter or conflict tree change replaces `files`,
  // and the effect below stops the scan that read the old ones.
  const diffSource: ReviewFindSource<Hit> = {
    remote: false,
    run: (query, context) => {
      const conflicts = untrack(() => view) === 'conflicts';
      toggled = readToggled();
      editing = readEditing();
      const content = untrack(() => ({
        files,
        drafts: conflicts ? [] : review.drafts,
        prThreads: conflicts ? [] : review.prThreads,
        foldLines: conflicts ? (path: string) => review.conflictFoldLines(path) : undefined,
        commentText: (body: string, rowKey: string) => commentTexts()(body, toggled.get(rowKey)).runs,
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
      if (!force && offscreenBy() === 0) return true;
      if (isLogHit(hit)) {
        void log()?.revealFindLine(hit.line, hit.sectionKey).then(() => aim(hit));
        return true;
      }
      const place = hit.place;
      if (place.kind === 'fold') {
        review.expandConflictFold(hit.path, place.foldId);
        return false;
      }
      void showDiffHit(hit);
      return true;
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
