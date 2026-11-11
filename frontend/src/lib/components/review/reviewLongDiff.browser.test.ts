// A review diff taller than the browser can lay out, in real Chromium:
// ReviewDiffBody holds a range of its rows (LongListVirtualizer), and
// every navigation and restore must still reach the right line.

import { afterEach, beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { createRawSnippet, flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import ReviewDiffBody from './ReviewDiffBody.svelte';
import ReviewCommentThread from './ReviewCommentThread.svelte';
import ReviewPaneFind from './ReviewPaneFind.svelte';
import { getPaneFindProvider, openPaneFind, resetPaneFindForTest, setPaneFindQuery } from '../../stores/paneFind.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import type { ReviewFind } from '../../stores/reviewFind.svelte';
import type { CommentAnchor } from '../../utils/reviewRows';
import type { DiffReviewComment } from '../../types/models';
import { REVIEW_FILE_HEADER_BAR_PX } from '../../utils/reviewRows';
import { findDraftKey, scanReviewFind, type ReviewFindHit } from '../../utils/reviewFindScan';
import { locateReviewFindHit } from '../../utils/reviewFindRows';
import { findHighlightName } from '../../utils/findHighlights';
import { parseReviewFiles, reviewFileFromPatchFile, type ReviewFile } from '../../utils/patchStore';
import { conflictPatchFile } from '../../utils/conflictFile';
import { HELD_EDGE_PX, HELD_LIMIT_PX } from '../../utils/virtual/heldRows';
import { waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { reviewThreadRowSnippet } from '../../../test/helpers/reviewThreadRowSnippet.svelte';
import { setBindingMock } from '../../../test/mocks/bindings-app';

// 700 files of 1000 added lines: 14M px, past the held limit.
const FILE_COUNT = 700;
const LINES = 1000;
const VIEWPORT_PX = 600;

let patch = '';
beforeAll(() => {
  const out: string[] = [];
  for (let file = 0; file < FILE_COUNT; file += 1) {
    const path = `src/f${String(file).padStart(3, '0')}.ts`;
    out.push(`diff --git a/${path} b/${path}`, 'new file mode 100644', '--- /dev/null', `+++ b/${path}`, `@@ -0,0 +1,${LINES} @@`);
    for (let line = 1; line <= LINES; line += 1) out.push(`+f${file}:${line}`);
  }
  patch = out.join('\n') + '\n';
});

beforeEach(() => {
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
});

const mounted: { app: object; host: HTMLElement; files: ReviewFile[][] }[] = [];

afterEach(() => {
  resetPaneFindForTest();
  for (const { app, host, files } of mounted.splice(0)) {
    unmount(app);
    host.remove();
    for (const set of files) set[0]?.body.segments[0]?.store.dispose();
  }
});

// The `data-find-key` of the element showing `hit` in `files`.
function keyIn(files: readonly ReviewFile[], hit: ReviewFindHit): string {
  return locateReviewFindHit({ files, drafts: [], prThreads: [] }, hit)?.rowKey ?? '';
}

function readFiles(source = patch): ReviewFile[] {
  const files = parseReviewFiles(source);
  mounted.at(-1)?.files.push(files);
  return files;
}

async function mountBody(source = patch, wordWrap = false) {
  const host = document.createElement('div');
  host.style.cssText = `position:fixed;top:0;left:0;width:900px;height:${VIEWPORT_PX}px;display:flex`;
  document.body.appendChild(host);
  const entry = { app: {} as object, host, files: [] as ReviewFile[][] };
  mounted.push(entry);
  const topFiles: number[] = [];
  const files = readFiles(source);
  const props = rawProps({
    subjectId: 'subject',
    spanOwner: 'thread-1',
    scope: 'workspace',
    files,
    viewMode: 'stacked' as 'stacked' | 'split',
    wordWrap,
    collapsedPaths: new Set<string>() as ReadonlySet<string>,
    onToggleCollapsed: () => {},
    jumpToFilePath: null as string | null,
    onJumpConsumed: () => { props.jumpToFilePath = null; },
    drafts: [] as DiffReviewComment[],
    commentThread: reviewThreadRowSnippet(),
    jumpToRowKey: null as string | null,
    onJumpRowConsumed: () => { props.jumpToRowKey = null; },
    onTopFileChange: (fileIndex: number) => { topFiles.push(fileIndex); },
  });
  entry.app = mount(ReviewDiffBody, { target: host, props });
  const scrollEl = host.querySelector('[data-testid="review-scroll"]') as HTMLElement;
  await settle(scrollEl);
  return { props, scrollEl, topFiles, body: entry.app as { revealFindHit(hit: ReviewFindHit): boolean; findReadingPosition(): { fileIndex: number; line: number } | null }, files };
}

async function settle(scrollEl: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${scrollEl.scrollTop}:${scrollEl.scrollHeight}:${scrollEl.querySelectorAll('[data-testid="review-line-block"]').length}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable review geometry');
}

/** The diff line at a viewport y: its text and its top. */
function lineAt(scrollEl: HTMLElement, y: number): { text: string; top: number } {
  const rect = scrollEl.getBoundingClientRect();
  let el = document.elementFromPoint(rect.left + 200, y) as HTMLElement | null;
  while (el && el.parentElement?.dataset.testid !== 'review-line-block') el = el.parentElement;
  if (!el) throw new Error(`no diff line at y=${y}`);
  return { text: el.textContent!.replace(/\s+/g, ' ').trim(), top: el.getBoundingClientRect().top };
}

function lineAtTop(scrollEl: HTMLElement): { text: string; top: number } {
  // Below the sticky header overlay.
  return lineAt(scrollEl, scrollEl.getBoundingClientRect().top + 60);
}

describe('a review diff past the held limit', () => {
  it('holds a range of rows and jumps to any file', async () => {
    const { props, scrollEl, topFiles } = await mountBody();
    expect(scrollEl.scrollHeight).toBeLessThanOrEqual(HELD_LIMIT_PX);
    for (const target of [650, 3, 699, 350]) {
      props.jumpToFilePath = `src/f${String(target).padStart(3, '0')}.ts`;
      await settle(scrollEl);
      expect(topFiles.at(-1)).toBe(target);
      const header = scrollEl.querySelector(`[data-testid="review-file-header"][data-path="src/f${String(target).padStart(3, '0')}.ts"]`)
        ?? [...scrollEl.querySelectorAll('[data-testid="review-file-header-path"]')].find((el) => el.textContent?.includes(`f${String(target).padStart(3, '0')}.ts`));
      expect(header, `header of file ${target}`).toBeTruthy();
      expect(lineAtTop(scrollEl).text).toContain(`f${target}:`);
    }
  });

  it('reads across a move of the held rows without moving the line being read', async () => {
    const { props, scrollEl } = await mountBody();
    props.jumpToFilePath = 'src/f350.ts';
    await settle(scrollEl);
    // Short of the end of the held rows by 100 px, then 300 px on.
    const total = scrollEl.scrollHeight;
    scrollEl.scrollTop = total - HELD_EDGE_PX - VIEWPORT_PX - 100;
    await settle(scrollEl);
    const before = scrollEl.scrollTop;
    const probe = lineAt(scrollEl, scrollEl.getBoundingClientRect().top + 360);
    scrollEl.scrollTop = before + 300;
    await settle(scrollEl);
    expect(scrollEl.scrollTop).toBeLessThan(before - 1_000_000);
    const now = lineAt(scrollEl, scrollEl.getBoundingClientRect().top + 60);
    expect(now).toEqual({ text: probe.text, top: probe.top - 300 });
  });

  it('jumps to a comment row outside the held rows and hands it the pulse', async () => {
    const { props, scrollEl } = await mountBody();
    props.drafts = [{
      id: 'far', threadId: 'thread-1', scope: 'workspace', sourceKey: 'source', filePath: 'src/f650.ts',
      status: 'draft', newLine: 10, side: 'new', selectedText: '', body: 'far', createdAt: 1, updatedAt: 1,
    }];
    await settle(scrollEl);
    props.jumpToRowKey = 't:far';
    await settle(scrollEl);
    const thread = scrollEl.querySelector<HTMLElement>('[data-thread="far"]');
    expect(thread).not.toBeNull();
    // The row lands below the sticky file bar, not under it.
    expect(thread!.getBoundingClientRect().top).toBe(scrollEl.getBoundingClientRect().top + REVIEW_FILE_HEADER_BAR_PX);
    expect(scrollEl.parentElement!.querySelector('[data-testid="review-sticky-header"]')).not.toBeNull();
    expect(thread!.dataset.pulse).toBe('1');
    // The same jump again is a new generation: the row pulses again.
    props.jumpToRowKey = 't:far';
    await settle(scrollEl);
    expect(scrollEl.querySelector<HTMLElement>('[data-thread="far"]')!.dataset.pulse).toBe('2');
    // Held for the pulse's lifetime (1.6s), then forgotten.
    await waitFor(() => scrollEl.querySelector<HTMLElement>('[data-thread="far"]')!.dataset.pulse === '0', 'pulse released', 240);
  });

  it('keeps the line being read when the files are read again', async () => {
    const { props, scrollEl } = await mountBody();
    props.jumpToFilePath = 'src/f600.ts';
    await settle(scrollEl);
    scrollEl.scrollTop += 4567;
    await settle(scrollEl);
    const before = lineAtTop(scrollEl);
    props.files = readFiles();
    await settle(scrollEl);
    expect(lineAtTop(scrollEl)).toEqual(before);
  });

  it('keeps the line being read across word wrap and back', async () => {
    const { props, scrollEl } = await mountBody();
    props.jumpToFilePath = 'src/f520.ts';
    await settle(scrollEl);
    scrollEl.scrollTop += 2345;
    await settle(scrollEl);
    const before = lineAtTop(scrollEl);
    props.wordWrap = true;
    flushSync();
    await settle(scrollEl);
    expect(lineAtTop(scrollEl).text).toBe(before.text);
    props.wordWrap = false;
    flushSync();
    await settle(scrollEl);
    expect(lineAtTop(scrollEl)).toEqual(before);
  });

  it('find reveals a scanned match outside the held rows a third down the view', async () => {
    const { scrollEl, body, files } = await mountBody();
    const hits: ReviewFindHit[] = [];
    await scanReviewFind({ files, drafts: [], prThreads: [], commentText: (body) => [body] }, 'F650:700', {
      limit: 10, live: () => true, emit: (hit) => hits.push(hit), pause: async () => {},
    });
    expect(hits.map((hit) => [hit.path, keyIn(files, hit)])).toEqual([['src/f650.ts', expect.stringMatching(/^l:/)]]);
    expect(body.revealFindHit(hits[0])).toBe(true);
    await settle(scrollEl);
    const target = scrollEl.querySelector<HTMLElement>(`[data-find-key="${keyIn(files, hits[0])}"]`);
    expect(target?.textContent).toContain('f650:700');
    const view = scrollEl.getBoundingClientRect();
    expect(Math.abs(target!.getBoundingClientRect().top - view.top - Math.round(view.height / 3))).toBeLessThanOrEqual(18);
    expect(body.findReadingPosition()?.fileIndex).toBe(650);
  });
});

describe('review find reading position', () => {
  // The line under the reading edge (below the sticky file bar), by
  // hit-testing the laid-out page.
  function keyAcross(scrollEl: HTMLElement): number {
    const view = scrollEl.getBoundingClientRect();
    const hit = document.elementFromPoint(view.left + 300, view.top + REVIEW_FILE_HEADER_BAR_PX + 1);
    const key = hit?.closest<HTMLElement>('[data-find-key^="l:"]')?.dataset.findKey;
    if (!key) throw new Error('no line at the reading edge');
    return Number(key.split(':')[1]);
  }

  it.each([false, true])('is the line at the reading edge inside a block (wrap %s)', async (wrap) => {
    const out: string[] = [];
    for (let file = 0; file < 3; file += 1) {
      const path = `src/w${file}.ts`;
      out.push(`diff --git a/${path} b/${path}`, 'new file mode 100644', '--- /dev/null', `+++ b/${path}`, '@@ -0,0 +1,300 @@');
      // Every fifth line wraps over several rows when wrap is on.
      for (let line = 1; line <= 300; line += 1) out.push(`+w${file}:${line}${line % 5 === 2 ? ` ${'wrapping words '.repeat(30)}` : ''}`);
    }
    const { props, scrollEl, body } = await mountBody(out.join('\n') + '\n', wrap);
    props.jumpToFilePath = 'src/w1.ts';
    await settle(scrollEl);
    const starts = new Set<number>();
    for (const by of [413, 777, 1290]) {
      scrollEl.scrollTop += by;
      await settle(scrollEl);
      const expected = keyAcross(scrollEl);
      const position = body.findReadingPosition();
      expect(position?.fileIndex).toBe(1);
      expect(position?.line, `scrolled ${by}`).toBe(expected);
      starts.add(expected);
    }
    expect(starts.size).toBe(3);
  });
});

describe('review find over the real list', () => {
  // ReviewPane's comment-thread row: the real draft card.
  function draftRows(drafts: () => readonly DiffReviewComment[]) {
    return createRawSnippet((threadKey: () => string, _anchor: () => CommentAnchor, pulse: () => number) => ({
      render: () => '<div></div>',
      setup: (node: Element) => {
        const comment = drafts().find((draft) => draft.id === threadKey())!;
        const app = mount(ReviewCommentThread, { target: node, props: { comment, pulse: pulse(), onUpdate: () => {}, onDelete: () => {} } });
        return () => { void unmount(app); };
      },
    }));
  }

  it('a recount from opening the current match\'s draft editor neither scrolls nor unmounts the editor', async () => {
    // The only other match is far below, outside the held rows.
    const { props, scrollEl, body } = await mountBody(patch.replace('+f650:500\n', '+zq f650:500\n'));
    const draft: DiffReviewComment = {
      id: 'near', threadId: 'thread-1', scope: 'workspace', sourceKey: 'source', filePath: 'src/f000.ts',
      status: 'draft', newLine: 5, side: 'new', selectedText: '', body: 'zq note', createdAt: 1, updatedAt: 1,
    };
    props.drafts = [draft];
    props.commentThread = draftRows(() => props.drafts) as typeof props.commentThread;
    await settle(scrollEl);
    const review = {
      drafts: [draft], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(),
      toggleCollapsed: () => {}, togglePRThread: () => {},
    } as unknown as ReviewPaneState;
    const host = mounted.at(-1)!.host;
    const finder = mount(ReviewPaneFind, {
      target: document.body,
      props: {
        findKey: 'pane-real', title: 'Review', review, view: 'diff', files: props.files, root: host,
        diff: () => body as never, log: () => undefined,
      },
    });
    flushSync();
    try {
      const find = getPaneFindProvider('pane-real')!;
      openPaneFind('pane-real');
      setPaneFindQuery('pane-real', 'zq');
      await waitFor(() => find.result.count === 2 && !find.result.pending, 'both matches');
      expect(find.result.current).toBe(0);
      await settle(scrollEl);
      const top = scrollEl.scrollTop;
      const card = scrollEl.querySelector<HTMLElement>('[data-testid="review-comment-thread"]')!;
      card.querySelector<HTMLButtonElement>('button[aria-label="Edit"]')!.click();
      flushSync();
      const box = card.querySelector('textarea')!;
      box.value = 'zq typed and unsaved';
      box.dispatchEvent(new Event('input', { bubbles: true }));
      // The draft's match is gone: the far one is current, not shown.
      await waitFor(() => find.result.count === 1 && !find.result.pending, 'the recount');
      await settle(scrollEl);
      expect(find.result.current).toBe(0);
      expect(scrollEl.scrollTop).toBe(top);
      expect(box.isConnected).toBe(true);
      expect(scrollEl.querySelector('textarea')?.value).toBe('zq typed and unsaved');
    } finally {
      unmount(finder);
    }
  });

  it('a fold opened by a query still scanning is brought on screen after the rescan it causes', async () => {
    const { props, scrollEl, body } = await mountBody('diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n');
    // The match is deep inside a long fold at the top; a second, large
    // file keeps the scan going past it.
    const lead = Array.from({ length: 1000 }, (_, i) => (i === 600 ? 'zq deep in the fold' : `lead ${i}`));
    const contents: Record<string, string> = {
      'a.go': [...lead, '<<<<<<< ours', 'left', '=======', 'right', '>>>>>>> theirs'].join('\n'),
      'z.go': [...Array.from({ length: 200_000 }, (_, i) => `filler ${i}`), '<<<<<<< ours', 'l', '=======', 'r', '>>>>>>> theirs'].join('\n'),
    };
    const expanded = new Map<string, Set<number>>();
    const folds = new Map<string, Map<number, readonly string[]>>();
    const build = () => Object.entries(contents).map(([path, content]) => {
      const fileFolds = new Map<number, readonly string[]>();
      folds.set(path, fileFolds);
      return reviewFileFromPatchFile(conflictPatchFile(path, content, {
        expandedFolds: expanded.get(path), onFold: (id, lines) => fileFolds.set(id, lines),
      }));
    });
    let files = build();
    mounted.at(-1)!.files.push(files);
    props.files = files;
    await settle(scrollEl);
    let pendingAtExpand: boolean | null = null;
    const findProps = rawProps({
      findKey: 'pane-fold', title: 'Review', view: 'conflicts' as const, files, root: mounted.at(-1)!.host as HTMLElement | undefined,
      review: {
        drafts: [], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(),
        conflictCollapsedPaths: new Set<string>(),
        conflictFoldLines: (path: string) => folds.get(path) ?? null,
        expandConflictFold: (path: string, foldId: number) => {
          pendingAtExpand = getPaneFindProvider('pane-fold')!.result.pending;
          expanded.set(path, new Set([...(expanded.get(path) ?? []), foldId]));
          files = build();
          mounted.at(-1)!.files.push(files);
          props.files = files;
          findProps.files = files;
        },
      } as unknown as ReviewPaneState,
      diff: () => body as never,
      log: () => undefined,
    });
    const finder = mount(ReviewPaneFind, { target: document.body, props: findProps });
    flushSync();
    try {
      const find = getPaneFindProvider('pane-fold') as ReviewFind<ReviewFindHit>;
      openPaneFind('pane-fold');
      setPaneFindQuery('pane-fold', 'zq');
      await waitFor(() => find.result.count === 1 && !find.result.pending, 'the scan', 600);
      await settle(scrollEl);
      // The fold opened while the query was still scanning.
      expect(pendingAtExpand).toBe(true);
      const row = scrollEl.querySelector<HTMLElement>(`[data-find-key="${keyIn(files, find.hits[0])}"]`);
      expect(row?.textContent).toContain('zq deep in the fold');
      const view = scrollEl.getBoundingClientRect();
      const box = row!.getBoundingClientRect();
      expect(box.top).toBeGreaterThanOrEqual(view.top);
      expect(box.bottom).toBeLessThanOrEqual(view.bottom);
    } finally {
      unmount(finder);
    }
  });

  it('a collapsed file opened by a query still scanning is brought on screen though its load restarts the scan', async () => {
    const { props, scrollEl, body, files } = await mountBody();
    const path = 'src/f003.ts';
    let collapsed = new Set([path]);
    props.collapsedPaths = collapsed;
    await settle(scrollEl);
    let pendingAtOpen: boolean | null = null;
    const findProps = rawProps({
      findKey: 'pane-load', title: 'Review', view: 'diff' as const, files, root: mounted.at(-1)!.host as HTMLElement | undefined,
      review: {
        drafts: [], prThreads: [], expandedPRThreadIds: new Set<string>(),
        get collapsedPaths() { return collapsed; },
        // Opening the file loads it: the files are read again.
        toggleCollapsed: (at: string) => {
          pendingAtOpen = getPaneFindProvider('pane-load')!.result.pending;
          collapsed = new Set([...collapsed].filter((p) => p !== at));
          props.collapsedPaths = collapsed;
          const next = readFiles();
          props.files = next;
          findProps.files = next;
        },
      } as unknown as ReviewPaneState,
      diff: () => body as never,
      log: () => undefined,
    });
    const finder = mount(ReviewPaneFind, { target: document.body, props: findProps });
    flushSync();
    try {
      const find = getPaneFindProvider('pane-load') as ReviewFind<ReviewFindHit>;
      openPaneFind('pane-load');
      setPaneFindQuery('pane-load', 'f3:500');
      await waitFor(() => find.result.count === 1 && !find.result.pending, 'the scan', 1200);
      await settle(scrollEl);
      expect(pendingAtOpen).toBe(true);
      const row = scrollEl.querySelector<HTMLElement>(`[data-find-key="${keyIn(findProps.files, find.hits[0])}"]`);
      expect(row?.textContent).toContain('f3:500');
      const view = scrollEl.getBoundingClientRect();
      const box = row!.getBoundingClientRect();
      expect(box.top).toBeGreaterThanOrEqual(view.top);
      expect(box.bottom).toBeLessThanOrEqual(view.bottom);
    } finally {
      unmount(finder);
    }
  });

  // The viewport rect of the first `needle` in the row keyed `rowKey`.
  function matchRect(scrollEl: HTMLElement, rowKey: string, needle: string): DOMRect {
    const row = scrollEl.querySelector<HTMLElement>(`[data-find-key="${rowKey}"]`)!;
    const walker = document.createTreeWalker(row, NodeFilter.SHOW_TEXT);
    for (let node = walker.nextNode(); node; node = walker.nextNode()) {
      const at = node.textContent!.indexOf(needle);
      if (at < 0) continue;
      const range = document.createRange();
      range.setStart(node, at);
      range.setEnd(node, at + needle.length);
      return range.getBoundingClientRect();
    }
    throw new Error(`no ${needle} in ${rowKey}`);
  }

  // Inside the scroller and every box between it and the match that clips.
  function expectShown(scrollEl: HTMLElement, rowKey: string, needle: string): void {
    const rect = matchRect(scrollEl, rowKey, needle);
    const view = scrollEl.getBoundingClientRect();
    expect(rect.left, `${rowKey} left`).toBeGreaterThanOrEqual(view.left);
    expect(rect.right, `${rowKey} right`).toBeLessThanOrEqual(view.left + scrollEl.clientWidth);
    expect(rect.top, `${rowKey} top`).toBeGreaterThanOrEqual(view.top);
    expect(rect.bottom, `${rowKey} bottom`).toBeLessThanOrEqual(view.bottom);
    let at = scrollEl.querySelector<HTMLElement>(`[data-find-key="${rowKey}"]`)!.parentElement;
    for (; at && at !== scrollEl; at = at.parentElement) {
      if (getComputedStyle(at).overflowX === 'visible') continue;
      const clip = at.getBoundingClientRect();
      expect(rect.left, `${rowKey} inside its line`).toBeGreaterThanOrEqual(clip.left);
      expect(rect.right, `${rowKey} inside its line`).toBeLessThanOrEqual(clip.right);
    }
  }

  it.each(['diff', 'conflicts'] as const)('brings a match at the end of a long unwrapped line into view on both axes (%s)', async (view) => {
    const long = (fill: string, tail: string) => `${fill.repeat(300)} ${tail}`;
    const { props, scrollEl, body } = await mountBody('diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n');
    const files = view === 'diff'
      ? readFiles(['diff --git a/src/a.ts b/src/a.ts', '--- a/src/a.ts', '+++ b/src/a.ts', '@@ -1,3 +1,3 @@', ' short', `-${long('w', 'zq one')}`, `+${long('v', 'zq two')}`, ''].join('\n'))
      : [reviewFileFromPatchFile(conflictPatchFile('c.go', ['short', '<<<<<<< ours', long('w', 'zq one'), '=======', long('v', 'zq two'), '>>>>>>> theirs'].join('\n')))];
    if (view === 'conflicts') mounted.at(-1)!.files.push(files);
    const host = mounted.at(-1)!.host;
    host.style.width = '360px';
    props.files = files;
    await settle(scrollEl);
    const finder = mount(ReviewPaneFind, {
      target: document.body,
      props: {
        findKey: `pane-wide-${view}`, title: 'Review', view, files, root: host,
        review: {
          drafts: [], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(),
          conflictCollapsedPaths: new Set<string>(), conflictFoldLines: () => null,
        } as unknown as ReviewPaneState,
        diff: () => body as never, log: () => undefined,
      },
    });
    flushSync();
    try {
      const find = getPaneFindProvider(`pane-wide-${view}`) as ReviewFind<ReviewFindHit>;
      openPaneFind(`pane-wide-${view}`);
      setPaneFindQuery(`pane-wide-${view}`, 'zq');
      await waitFor(() => find.result.count === 2 && !find.result.pending, 'the scan');
      const [one, two] = find.hits;
      await waitFor(() => scrollEl.querySelector<HTMLElement>(`[data-find-key="${keyIn(files, one)}"]`)!.parentElement!.scrollLeft > 0, 'the line scrolled sideways');
      await settle(scrollEl);
      expectShown(scrollEl, keyIn(files, one), 'zq one');
      find.next();
      await waitFor(() => scrollEl.querySelector<HTMLElement>(`[data-find-key="${keyIn(files, two)}"]`)!.parentElement!.scrollLeft > 0, 'the next line scrolled sideways');
      await settle(scrollEl);
      expectShown(scrollEl, keyIn(files, two), 'zq two');
      // The line left behind reads from its start again.
      expect(scrollEl.querySelector<HTMLElement>(`[data-find-key="${keyIn(files, one)}"]`)!.parentElement!.scrollLeft).toBe(0);
    } finally {
      unmount(finder);
    }
  });

  it('shifts only the line that clips the current match, and no line once it is not one', async () => {
    const long = (fill: string, tail: string) => `${fill.repeat(300)} ${tail}`;
    const patchOf = (lines: string[]) => ['diff --git a/src/a.ts b/src/a.ts', '--- a/src/a.ts', '+++ b/src/a.ts', `@@ -1,${lines.length} +1,${lines.length} @@`, ...lines.map((line) => ` ${line}`), ''].join('\n');
    const { props, scrollEl, body } = await mountBody('diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n');
    const host = mounted.at(-1)!.host;
    host.style.width = '360px';
    let files = readFiles(patchOf(['short zq here', long('w', 'zq one'), long('v', 'zq two'), 'tail']));
    props.files = files;
    await settle(scrollEl);
    const review = rawProps({
      drafts: [] as DiffReviewComment[], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(),
      wordWrap: false,
    });
    const findProps = rawProps({
      findKey: 'pane-shift', title: 'Review', view: 'diff' as const, files, root: host as HTMLElement | undefined,
      review: review as unknown as ReviewPaneState, diff: () => body as never, log: () => undefined,
    });
    const finder = mount(ReviewPaneFind, { target: document.body, props: findProps });
    flushSync();
    const find = getPaneFindProvider('pane-shift') as ReviewFind<ReviewFindHit>;
    // Every line scrolled sideways under the list.
    const shifted = () => [...scrollEl.querySelectorAll<HTMLElement>('[data-testid="review-line-block"] *')].filter((el) => el.scrollLeft > 0);
    const holdsCurrent = (line: HTMLElement) => {
      const hit = find.hits[find.result.current];
      return hit !== undefined && line.querySelector(`[data-find-key="${keyIn(files, hit)}"]`) !== null;
    };
    const settleShift = async () => {
      await settle(scrollEl);
      await new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve)));
      await new Promise((resolve) => setTimeout(resolve, 120));
    };
    try {
      openPaneFind('pane-shift');
      setPaneFindQuery('pane-shift', 'zq');
      await waitFor(() => find.result.count === 3 && !find.result.pending, 'the scan');
      // The first match is on the short line: nothing shifts.
      await settleShift();
      expect(shifted()).toEqual([]);
      find.next();
      await waitFor(() => shifted().length === 1, 'the long line shifted');
      expect(holdsCurrent(shifted()[0])).toBe(true);
      find.next();
      await settleShift();
      expect(shifted().length).toBe(1);
      expect(holdsCurrent(shifted()[0])).toBe(true);
      // A new query whose match is in view on a short line.
      setPaneFindQuery('pane-shift', 'tail');
      await waitFor(() => find.result.count === 1 && !find.result.pending, 'the short match');
      await settleShift();
      expect(shifted()).toEqual([]);
      // Back to the far match, then a recount that removes it.
      setPaneFindQuery('pane-shift', 'zq two');
      await waitFor(() => shifted().length === 1, 'the far match shifted');
      files = readFiles(patchOf(['short zq here', long('w', 'zq one'), 'tail']));
      props.files = files;
      findProps.files = files;
      await waitFor(() => find.result.count === 0 && !find.result.pending, 'the recount');
      await settleShift();
      expect(shifted()).toEqual([]);
      // Wrap on: no line clips, so none stays shifted.
      setPaneFindQuery('pane-shift', 'zq one');
      await waitFor(() => shifted().length === 1, 'shifted again');
      props.wordWrap = true;
      review.wordWrap = true;
      flushSync();
      await settleShift();
      expect(shifted()).toEqual([]);
      props.wordWrap = false;
      review.wordWrap = false;
      flushSync();
      await waitFor(() => shifted().length === 1 && holdsCurrent(shifted()[0]), 'shifted after wrap off');
      // No results, then close.
      setPaneFindQuery('pane-shift', 'nothing here at all');
      await waitFor(() => find.result.count === 0 && !find.result.pending, 'no results');
      await settleShift();
      expect(shifted()).toEqual([]);
      setPaneFindQuery('pane-shift', 'zq one');
      await waitFor(() => shifted().length === 1, 'shifted before close');
      find.clear();
      await settleShift();
      expect(shifted()).toEqual([]);
    } finally {
      unmount(finder);
    }
  });

  it('a recount during a query still scanning keeps the published matches and the shifted line, and a gone match gives way to the next one published', async () => {
    const long = (fill: string, tail: string) => `${fill.repeat(300)} ${tail}`;
    // The matches sit deep in a large file, so a scan from the start takes
    // many frames to reach them and more to finish past them.
    const filler = (from: number) => Array.from({ length: 250_000 }, (_, i) => `filler ${from + i}`);
    const patchOf = (middle: string[], extra: string[] = []) => {
      const lines = [...filler(0), ...middle, ...filler(250_000), ...extra, 'zq end'];
      return ['diff --git a/src/big.ts b/src/big.ts', '--- a/src/big.ts', '+++ b/src/big.ts', `@@ -1,${lines.length} +1,${lines.length} @@`, ...lines.map((line) => ` ${line}`), ''].join('\n');
    };
    const lines = ['short', long('w', 'zq one'), long('v', 'zq two'), long('u', 'zq three'), 'tail'];
    const { props, scrollEl, body } = await mountBody('diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-a\n+b\n');
    const host = mounted.at(-1)!.host;
    host.style.width = '360px';
    let files = readFiles(patchOf(lines));
    props.files = files;
    await settle(scrollEl);
    const findProps = rawProps({
      findKey: 'pane-recount', title: 'Review', view: 'diff' as const, files, root: host as HTMLElement | undefined,
      review: {
        drafts: [], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(), wordWrap: false,
      } as unknown as ReviewPaneState,
      diff: () => body as never, log: () => undefined,
    });
    const replace = (next: ReviewFile[]) => {
      files = next;
      props.files = next;
      findProps.files = next;
      flushSync();
    };
    const finder = mount(ReviewPaneFind, { target: document.body, props: findProps });
    flushSync();
    const find = getPaneFindProvider('pane-recount') as ReviewFind<ReviewFindHit>;
    const rowText = (hit: ReviewFindHit | undefined) => (hit ? scrollEl.querySelector<HTMLElement>(`[data-find-key="${keyIn(files, hit)}"]`)?.textContent ?? '' : '');
    const shiftedLine = () => [...scrollEl.querySelectorAll<HTMLElement>('[data-testid="review-line-block"] *')].find((el) => el.scrollLeft > 0);
    let sampling = true;
    try {
      // Read from the top: the first match is zq one.
      scrollEl.scrollTop = 0;
      await settle(scrollEl);
      openPaneFind('pane-recount');
      setPaneFindQuery('pane-recount', 'zq');
      await waitFor(() => find.hits.length === 3 && shiftedLine() !== undefined, 'the middle matches published');
      expect(find.result.pending).toBe(true);
      find.next();
      await waitFor(() => rowText(find.hits[find.result.current]).includes('zq two') && shiftedLine()?.textContent?.includes('zq two') === true, 'the second match shifted');
      const line = shiftedLine()!;
      const left = line.scrollLeft;
      const published = find.hits.map((hit) => hit.identity);
      const samples: number[] = [];
      const sample = () => {
        if (!sampling) return;
        if (line.isConnected) samples.push(line.scrollLeft);
        requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
      // A content change while the query scans: a recount, with the match surviving.
      replace(readFiles(patchOf(lines, ['filler more'])));
      expect(find.result.pending).toBe(true);
      for (let i = 0; i < 6; i += 1) await new Promise((resolve) => requestAnimationFrame(resolve));
      expect(find.result.pending).toBe(true);
      expect(find.hits.map((hit) => hit.identity)).toEqual(published);
      expect(rowText(find.hits[find.result.current])).toContain('zq two');
      // The current match goes from its line (a line is its number, so the
      // line stays), and another recount runs to the end. Not present now,
      // it is neither painted nor shifted until the swap.
      sampling = false;
      expect(new Set(samples)).toEqual(new Set([left]));
      replace(readFiles(patchOf(lines.map((text) => (text.includes('zq two') ? long('v', 'no match') : text)), ['filler more'])));
      expect(find.result.pending).toBe(true);
      expect(find.hits.map((hit) => hit.identity)).toEqual(published);
      await waitFor(() => !find.result.pending, 'the recount', 1200);
      expect(find.result.count).toBe(3);
      expect(rowText(find.hits[find.result.current])).toContain('zq three');
    } finally {
      sampling = false;
      unmount(finder);
    }
  });

  // A first file on screen, then a large one that keeps a scan going for
  // many frames.
  const recountPatch = (first: string[]) => {
    const big = Array.from({ length: 400_000 }, (_, i) => `filler ${i}`);
    const file = (path: string, lines: string[]) => [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`, `@@ -1,${lines.length} +1,${lines.length} @@`, ...lines.map((line) => ` ${line}`)];
    return [...file('src/a.ts', first), ...file('src/big.ts', big), ''].join('\n');
  };
  const currentText = (key: string) => [...(CSS.highlights.get(`${findHighlightName(key)}-current`) ?? [])].map((range) => (range as Range).toString());

  it('a current match whose row goes during a recount paints nothing until the swap, then its survivor', async () => {
    const draft = (id: string, newLine: number, body: string): DiffReviewComment => ({
      id, threadId: 'thread-1', scope: 'workspace', sourceKey: 'source', filePath: 'src/a.ts',
      status: 'draft', newLine, side: 'new', selectedText: '', body, createdAt: 1, updatedAt: 1,
    });
    const { props, scrollEl, body } = await mountBody(recountPatch(['alpha', 'beta', 'gamma', 'delta zq', 'epsilon']));
    const drafts = [draft('first', 2, 'zq first note'), draft('second', 3, 'zq second note')];
    props.drafts = drafts;
    props.commentThread = draftRows(() => props.drafts) as typeof props.commentThread;
    await settle(scrollEl);
    const review = rawProps({
      drafts, prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(),
      toggleCollapsed: () => {}, togglePRThread: () => {},
    });
    const finder = mount(ReviewPaneFind, {
      target: document.body,
      props: {
        findKey: 'pane-gone', title: 'Review', review: review as unknown as ReviewPaneState, view: 'diff', files: props.files,
        root: mounted.at(-1)!.host, diff: () => body as never, log: () => undefined,
      },
    });
    flushSync();
    try {
      const find = getPaneFindProvider('pane-gone') as ReviewFind<ReviewFindHit>;
      scrollEl.scrollTop = 0;
      await settle(scrollEl);
      openPaneFind('pane-gone');
      setPaneFindQuery('pane-gone', 'zq');
      await waitFor(() => find.result.count === 3 && !find.result.pending, 'the scan', 1200);
      await settle(scrollEl);
      expect(find.hits[find.result.current].at).toEqual({ kind: 'draft', commentId: 'first' });
      expect(currentText('pane-gone')).toEqual(['zq']);
      const published = find.hits.map((hit) => hit.identity);
      // The first draft is deleted: its row goes, and the view recounts.
      const rest = drafts.slice(1);
      props.drafts = rest;
      review.drafts = rest;
      flushSync();
      await waitFor(() => scrollEl.querySelector(`[data-find-key="${findDraftKey('first')}"]`) === null, 'the row gone');
      for (let i = 0; i < 4; i += 1) await new Promise((resolve) => requestAnimationFrame(resolve));
      expect(find.result.pending).toBe(true);
      expect(find.hits.map((hit) => hit.identity)).toEqual(published);
      expect(find.hits[find.result.current].at).toEqual({ kind: 'draft', commentId: 'first' });
      expect(currentText('pane-gone')).toEqual([]);
      await waitFor(() => !find.result.pending, 'the recount', 1200);
      await settle(scrollEl);
      expect(find.result.count).toBe(2);
      expect(find.hits[find.result.current].at).toEqual({ kind: 'draft', commentId: 'second' });
      const current = CSS.highlights.get(`${findHighlightName('pane-gone')}-current`)!;
      const [range] = [...current] as Range[];
      expect(range.toString()).toBe('zq');
      expect(range.startContainer.parentElement?.closest(`[data-find-key="${findDraftKey('second')}"]`)).not.toBeNull();
    } finally {
      unmount(finder);
    }
  });

  it('a recount from an edit on screen keeps the shifted line that holds the current match', async () => {
    const long = `${'w'.repeat(300)} zq one`;
    const { props, scrollEl, body } = await mountBody(recountPatch(['short', long, 'edit me', 'tail']));
    const host = mounted.at(-1)!.host;
    host.style.width = '360px';
    await settle(scrollEl);
    const findProps = rawProps({
      findKey: 'pane-edit', title: 'Review', view: 'diff' as const, files: props.files, root: host as HTMLElement | undefined,
      review: {
        drafts: [], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(), wordWrap: false,
      } as unknown as ReviewPaneState,
      diff: () => body as never, log: () => undefined,
    });
    const finder = mount(ReviewPaneFind, { target: document.body, props: findProps });
    flushSync();
    const find = getPaneFindProvider('pane-edit') as ReviewFind<ReviewFindHit>;
    const shiftedLine = () => [...scrollEl.querySelectorAll<HTMLElement>('[data-testid="review-line-block"] *')].find((el) => el.scrollLeft > 0);
    let sampling = true;
    try {
      scrollEl.scrollTop = 0;
      await settle(scrollEl);
      openPaneFind('pane-edit');
      setPaneFindQuery('pane-edit', 'zq');
      await waitFor(() => !find.result.pending && shiftedLine() !== undefined, 'the scan and the shift', 1200);
      await settle(scrollEl);
      const line = shiftedLine()!;
      expect(line.textContent).toContain('zq one');
      const left = line.scrollLeft;
      const edited = () => scrollEl.textContent?.includes('edited now') === true;
      const samples: number[] = [];
      let editSeen = false;
      const sample = () => {
        if (!sampling) return;
        samples.push(line.scrollLeft);
        if (edited()) editSeen = true;
        requestAnimationFrame(sample);
      };
      requestAnimationFrame(sample);
      // A line on screen changes: the highlighter sees the rows mutate while
      // the recount runs.
      const files = readFiles(recountPatch(['short', long, 'edited now', 'tail']));
      props.files = files;
      findProps.files = files;
      flushSync();
      await waitFor(edited, 'the edit on screen');
      for (let i = 0; i < 6; i += 1) await new Promise((resolve) => requestAnimationFrame(resolve));
      expect(find.result.pending).toBe(true);
      expect(editSeen).toBe(true);
      expect(new Set(samples)).toEqual(new Set([left]));
      await waitFor(() => !find.result.pending, 'the recount', 1200);
      await settle(scrollEl);
      sampling = false;
      expect(line.isConnected).toBe(true);
      expect(new Set(samples)).toEqual(new Set([left]));
    } finally {
      sampling = false;
      unmount(finder);
    }
  });

  // A second file large enough that a scan over it takes many frames.
  const bigFiles = (() => {
    let files: ReviewFile[] | null = null;
    return () => (files ??= readFiles(['diff --git a/src/big.ts b/src/big.ts', '--- a/src/big.ts', '+++ b/src/big.ts', '@@ -1,400000 +1,400000 @@', ...Array.from({ length: 400_000 }, (_, i) => ` filler ${i}`), ''].join('\n')));
  })();
  const onePatch = (path: string, hunk: string, lines: string[]) => [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`, hunk, ...lines.map((line) => ` ${line}`), ''].join('\n');
  // The row holding the current match's painted range.
  const currentRow = (key: string): HTMLElement | null => {
    const [range] = [...(CSS.highlights.get(`${findHighlightName(key)}-current`) ?? [])] as Range[];
    return range ? (range.startContainer.parentElement?.closest<HTMLElement>('[data-find-key]') ?? null) : null;
  };
  const onScreen = (scrollEl: HTMLElement, text: string) => {
    const row = [...scrollEl.querySelectorAll<HTMLElement>('[data-find-key]')].find((el) => el.textContent?.includes(text));
    if (!row) return false;
    const view = scrollEl.getBoundingClientRect();
    const box = row.getBoundingClientRect();
    return box.top >= view.top && box.bottom <= view.bottom;
  };

  it('a step during the recount a filter starts reveals a match in a later file before the swap', async () => {
    const { props, scrollEl, body } = await mountBody(onePatch('src/a.test.ts', '@@ -1,3 +1,3 @@', ['alpha', 'zq in the test', 'beta']));
    // The later file's match is deep, far below the first.
    const deep = readFiles(onePatch('src/b.ts', '@@ -1,200001 +1,200001 @@', [...Array.from({ length: 200_000 }, (_, i) => `lead ${i}`), 'zq deep in b']));
    const all = [...readFiles(onePatch('src/a.test.ts', '@@ -1,3 +1,3 @@', ['alpha', 'zq in the test', 'beta'])), ...deep, ...bigFiles()];
    props.files = all;
    await settle(scrollEl);
    const findProps = rawProps({
      findKey: 'pane-hide', title: 'Review', view: 'diff' as const, files: all as readonly ReviewFile[], root: mounted.at(-1)!.host as HTMLElement | undefined,
      review: { drafts: [], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>() } as unknown as ReviewPaneState,
      diff: () => body as never, log: () => undefined,
    });
    const finder = mount(ReviewPaneFind, { target: document.body, props: findProps });
    flushSync();
    try {
      const find = getPaneFindProvider('pane-hide') as ReviewFind<ReviewFindHit>;
      scrollEl.scrollTop = 0;
      await settle(scrollEl);
      openPaneFind('pane-hide');
      setPaneFindQuery('pane-hide', 'zq');
      await waitFor(() => find.result.count === 2 && !find.result.pending, 'the scan', 1200);
      expect(find.hits[find.result.current].path).toBe('src/a.test.ts');
      // To the later match and back: both have been shown in this list.
      find.next();
      await waitFor(() => onScreen(scrollEl, 'zq deep in b'), 'the later match shown', 600);
      find.previous();
      await waitFor(() => onScreen(scrollEl, 'zq in the test'), 'the first match shown again', 600);
      // Hide tests: the test file leaves the list, and the view recounts.
      const shown = all.filter((file) => !file.path.endsWith('.test.ts'));
      props.files = shown;
      findProps.files = shown;
      flushSync();
      expect(find.result.pending).toBe(true);
      // Enter while it runs: the published next match is the later file's.
      find.next();
      expect(find.hits[find.result.current].path).toBe('src/b.ts');
      let pendingWhenShown: boolean | null = null;
      await waitFor(() => {
        if (!onScreen(scrollEl, 'zq deep in b')) return false;
        pendingWhenShown = find.result.pending;
        return true;
      }, 'the later match on screen', 600);
      expect(pendingWhenShown).toBe(true);
      await waitFor(() => !find.result.pending, 'the recount', 1200);
      await settle(scrollEl);
      expect([find.result.count, find.result.current]).toEqual([1, 0]);
      expect(onScreen(scrollEl, 'zq deep in b')).toBe(true);
    } finally {
      unmount(finder);
    }
  });

  it('a gap expanded above the selected line during a recount paints and shifts the selected line, not the one inserted above it', async () => {
    const long = (fill: string, tail: string) => `${fill.repeat(300)} ${tail}`;
    const before = readFiles(onePatch('src/c.ts', '@@ -100,2 +100,2 @@', [long('v', 'needle hundred'), 'after']));
    const expanded = readFiles(onePatch('src/c.ts', '@@ -99,3 +99,3 @@', [long('w', 'needle ninety-nine'), long('v', 'needle hundred'), 'after']));
    const { props, scrollEl, body } = await mountBody(onePatch('x', '@@ -1 +1 @@', ['x']));
    const host = mounted.at(-1)!.host;
    host.style.width = '360px';
    // The large file first: a recount reaches the changed one late.
    let files = [...bigFiles(), ...before];
    props.files = files;
    await settle(scrollEl);
    const findProps = rawProps({
      findKey: 'pane-gap', title: 'Review', view: 'diff' as const, files: files as readonly ReviewFile[], root: host as HTMLElement | undefined,
      review: { drafts: [], prThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(), wordWrap: false } as unknown as ReviewPaneState,
      diff: () => body as never, log: () => undefined,
    });
    const finder = mount(ReviewPaneFind, { target: document.body, props: findProps });
    flushSync();
    const shifted = () => [...scrollEl.querySelectorAll<HTMLElement>('[data-testid="review-line-block"] *')].filter((el) => el.scrollLeft > 0);
    try {
      const find = getPaneFindProvider('pane-gap') as ReviewFind<ReviewFindHit>;
      scrollEl.scrollTop = 0;
      await settle(scrollEl);
      openPaneFind('pane-gap');
      setPaneFindQuery('pane-gap', 'needle');
      await waitFor(() => !find.result.pending && shifted().length === 1, 'the scan and the shift', 1200);
      expect(find.hits[find.result.current].at).toEqual({ kind: 'line', tag: 'c100:100' });
      expect(currentRow('pane-gap')?.textContent).toContain('needle hundred');
      // The leading gap opens one line: line 99, also a match, above it.
      files = [...bigFiles(), ...expanded];
      props.files = files;
      findProps.files = files;
      flushSync();
      await waitFor(() => scrollEl.textContent?.includes('needle ninety-nine') === true, 'the expanded line');
      for (let i = 0; i < 8; i += 1) await new Promise((resolve) => requestAnimationFrame(resolve));
      expect(find.result.pending).toBe(true);
      expect(currentRow('pane-gap')?.textContent).toContain('needle hundred');
      expect(shifted().map((line) => line.textContent?.includes('needle hundred'))).toEqual([true]);
      await waitFor(() => !find.result.pending, 'the recount', 1200);
      await settle(scrollEl);
      expect(find.result.count).toBe(2);
      expect(find.hits[find.result.current].at).toEqual({ kind: 'line', tag: 'c100:100' });
      expect(currentRow('pane-gap')?.textContent).toContain('needle hundred');
      expect(shifted().map((line) => line.textContent?.includes('needle hundred'))).toEqual([true]);
    } finally {
      unmount(finder);
    }
  });
});
