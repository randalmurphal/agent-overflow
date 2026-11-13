// ReviewPaneFind's CI log source: an answer applies only to the job and
// log version it was read from, and another job's log starts the query
// over at once. Its diff reveal asks the list for a match's row as the
// content shown at that moment places it. Painting is covered by
// findHighlights.browser.test.ts.
import { render } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import ReviewPaneFind from './ReviewPaneFind.svelte';
import { getPaneFindProvider, openPaneFind, resetPaneFindForTest, setPaneFindQuery } from '../../stores/paneFind.svelte';
import { REVIEW_FIND_REMOTE_DEBOUNCE_MS, type ReviewFind } from '../../stores/reviewFind.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import { placeCILogLine, type CILogFindHit } from '../../utils/ciLogFind';
import type { CILogSearch, CILogSection } from '../../types/models';
import { parseReviewFiles } from '../../utils/patchStore';
import type { ReviewFindHit, ReviewFindPlace } from '../../utils/reviewFindScan';

// The diff shows every draft as drafted and no file as an image.
const NO_MEDIA_HOST = { renderedPaths: new Set<string>() };

const KEY = 'pane-find-log';

function section(key: string, start: number, end: number): CILogSection {
  return { key, name: key, status: 'success', start, end, lines: end - start, bytes: 0 };
}

interface Asked {
  job: string;
  answer(search: Omit<CILogSearch, 'totalLines' | 'matches' | 'capped'>): void;
}

function mount() {
  const state = $state({ job: 'A', version: 1, sections: [section('a', 0, 10)] });
  const asked: Asked[] = [];
  const review = {
    get ciLogView() { return { stageName: 's', jobId: state.job, job: { id: state.job, name: state.job } }; },
    get ciLog() { return { version: state.version, sections: state.sections }; },
    searchCILog: vi.fn(() => new Promise<CILogSearch>((resolve) => {
      asked.push({ job: state.job, answer: (search) => resolve({ totalLines: 10, matches: 0, capped: false, ...search }) });
    })),
  } as unknown as ReviewPaneState;
  const root = document.createElement('div');
  document.body.append(root);
  render(ReviewPaneFind, {
    props: {
      findKey: KEY, title: 'Review', review, view: 'log', files: [], root,
      diff: () => undefined,
      log: () => ({ findReadingLine: () => 0, findLineText: () => null, findLineLoad: () => ({ loading: false, error: null }), openFindSection: () => false, revealFindLine: () => {}, scrollFindBy: () => {} }),
    },
  });
  const find = getPaneFindProvider(KEY) as ReviewFind<CILogFindHit>;
  const lines = () => find.hits.map((hit) => `${placeCILogLine(state.sections, hit.line)?.sectionKey}:${hit.line}`);
  return { state, asked, find, lines };
}

afterEach(() => {
  resetPaneFindForTest();
  vi.useRealTimers();
  document.body.innerHTML = '';
});

describe('review find over a CI job log', () => {
  it('another job opened while a read is out starts over at once; the old answer never lands', async () => {
    vi.useFakeTimers();
    const { state, asked, find, lines } = mount();
    openPaneFind(KEY);
    setPaneFindQuery(KEY, 'needle');
    await vi.advanceTimersByTimeAsync(REVIEW_FIND_REMOTE_DEBOUNCE_MS);
    expect(asked.map((a) => a.job)).toEqual(['A']);
    state.job = 'B';
    state.sections = [section('b', 0, 10)];
    flushSync();
    expect([find.result.count, find.result.pending]).toEqual([0, true]);
    // One read per pane at a time: B's waits for A's, under the refresh
    // scheduler's half second.
    await vi.advanceTimersByTimeAsync(REVIEW_FIND_REMOTE_DEBOUNCE_MS);
    asked[0].answer({ version: 1, hits: [{ line: 2, count: 1 }] });
    await vi.advanceTimersByTimeAsync(0);
    expect([find.result.count, find.result.pending]).toEqual([0, true]);
    expect(asked.map((a) => a.job)).toEqual(['A', 'B']);
    asked[1].answer({ version: 1, hits: [{ line: 5, count: 1 }] });
    await vi.advanceTimersByTimeAsync(0);
    expect(lines()).toEqual(['b:5']);
    expect(find.result.pending).toBe(false);
  });

  it('an answer read from another version of the log is not mapped onto the one shown', async () => {
    vi.useFakeTimers();
    const { state, asked, find, lines } = mount();
    openPaneFind(KEY);
    setPaneFindQuery(KEY, 'needle');
    await vi.advanceTimersByTimeAsync(REVIEW_FIND_REMOTE_DEBOUNCE_MS);
    // The log was rewritten while the read was out.
    state.version = 2;
    state.sections = [section('a2', 0, 10)];
    flushSync();
    asked[0].answer({ version: 1, hits: [{ line: 2, count: 1 }] });
    await vi.advanceTimersByTimeAsync(0);
    expect([find.result.count, find.result.pending]).toEqual([0, true]);
    // The read again answers for the version shown.
    await vi.advanceTimersByTimeAsync(2000);
    expect(asked.length).toBe(2);
    asked[1].answer({ version: 2, hits: [{ line: 3, count: 1 }] });
    await vi.advanceTimersByTimeAsync(0);
    expect(lines()).toEqual(['a2:3']);
    expect(find.result.pending).toBe(false);
  });
});

describe('review find over the diff', () => {
  const patch = (path: string, lines: string[]) => [`diff --git a/${path} b/${path}`, `--- a/${path}`, `+++ b/${path}`, `@@ -1,${lines.length} +1,${lines.length} @@`, ...lines.map((line) => ` ${line}`), ''].join('\n');

  it('a reveal whose row is not mounted asks the list for it where the content shown now places it, never where it was', async () => {
    const a = () => parseReviewFiles(patch('src/a.test.ts', ['alpha', 'zq in a']));
    const b = () => parseReviewFiles(patch('src/b.ts', ['beta', 'gamma', 'zq in b']));
    const asks: { fileIndex: number; place: ReviewFindPlace }[] = [];
    const list = {
      findReadingPosition: () => null,
      revealFindPlace: (at: { fileIndex: number; place: ReviewFindPlace }) => {
        asks.push(at);
        return true;
      },
      scrollFindBy: () => {},
    };
    const review = { shownDrafts: [], mediaHost: NO_MEDIA_HOST, drafts: [], prThreads: [], shownPRThreads: [], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>() } as unknown as ReviewPaneState;
    const root = document.createElement('div');
    document.body.append(root);
    const { rerender } = render(ReviewPaneFind, {
      props: { findKey: 'pane-find-diff', title: 'Review', review, view: 'diff', files: [...a(), ...b()], root, diff: () => list, log: () => undefined },
    });
    const find = getPaneFindProvider('pane-find-diff') as ReviewFind<ReviewFindHit>;
    openPaneFind('pane-find-diff');
    setPaneFindQuery('pane-find-diff', 'zq');
    await vi.waitFor(() => expect([find.result.count, find.result.pending]).toEqual([2, false]));
    find.next();
    // No row is mounted here: the list was asked for B, the second file.
    expect(find.hits[find.result.current].path).toBe('src/b.ts');
    const asked = asks.at(-1)!;
    expect(asked.fileIndex).toBe(1);
    // A is hidden: the content change asks again, of B as the first file.
    const before = asks.length;
    await rerender({ files: b() });
    await vi.waitFor(() => expect(asks.length).toBeGreaterThan(before));
    expect(find.wants(find.selected!)).toBe(true);
    expect(asks.slice(before).map((at) => [at.fileIndex, at.place])).toEqual([[0, asked.place]]);
    root.remove();
  });

  // A held reply shows at its thread's foot: its hit asks for the thread's
  // row, and opens the thread when it is settled, as a thread comment's does.
  it.each([
    ['unmounted', false],
    ['collapsed as resolved', true],
  ])('a held reply in a thread that is %s asks for the thread\'s row and opens it', async (_name, resolved) => {
    const files = parseReviewFiles(patch('src/a.ts', ['alpha', 'beta']));
    const thread = {
      id: 'p1', path: 'src/a.ts', line: 2, side: 'right', isResolvable: true, isResolved: resolved, isOutdated: false,
      comments: [{ id: 'n1', authorLogin: 'priya', body: 'Why beta?', createdAt: '', databaseID: 1, viewerCanEdit: false, viewerCanDelete: false }],
    };
    const reply = {
      id: 'd1', threadId: 't', scope: 'pr', sourceKey: 'k', filePath: 'src/a.ts', status: 'draft', side: 'reply',
      replyThreadId: 'p1', replyRootId: 1, selectedText: '', body: 'held zq reply', createdAt: 0, updatedAt: 0,
    };
    const expanded = new Set<string>();
    const togglePRThread = vi.fn((id: string) => { expanded.add(id); });
    const asks: { fileIndex: number; place: ReviewFindPlace }[] = [];
    const list = {
      findReadingPosition: () => null,
      revealFindPlace: (at: { fileIndex: number; place: ReviewFindPlace }) => {
        asks.push(at);
        return true;
      },
      scrollFindBy: () => {},
    };
    const review = {
      shownDrafts: [reply], mediaHost: NO_MEDIA_HOST, drafts: [reply], prThreads: [thread], shownPRThreads: [thread], collapsedPaths: new Set<string>(), expandedPRThreadIds: expanded, togglePRThread,
      draftEditBodyFor: () => null, sentComments: { editing: new Set<string>(), editBodyFor: () => null },
    } as unknown as ReviewPaneState;
    const root = document.createElement('div');
    document.body.append(root);
    render(ReviewPaneFind, {
      props: { findKey: 'pane-find-held', title: 'Review', review, view: 'diff', files, root, diff: () => list, log: () => undefined },
    });
    const find = getPaneFindProvider('pane-find-held') as ReviewFind<ReviewFindHit>;
    openPaneFind('pane-find-held');
    setPaneFindQuery('pane-find-held', 'zq');
    await vi.waitFor(() => expect([find.result.count, find.result.pending]).toEqual([1, false]));
    find.next();
    await vi.waitFor(() => expect(asks.length).toBeGreaterThan(0));
    expect(asks.at(-1)).toMatchObject({ fileIndex: 0, rowKey: 'dc:d1', place: { kind: 'thread', threadId: 'p1' } });
    expect(togglePRThread.mock.calls).toEqual(resolved ? [['p1']] : []);
    root.remove();
  });

  // Hide resolved threads: find searches the threads the view shows, so
  // a hidden thread's comments are not matched.
  it('does not match a thread Hide resolved hides', async () => {
    const files = parseReviewFiles(patch('src/a.ts', ['alpha', 'beta']));
    const thread = (id: string, body: string, resolved: boolean) => ({
      id, path: 'src/a.ts', line: 2, side: 'right', isResolvable: true, isResolved: resolved, isOutdated: false,
      comments: [{ id: `n-${id}`, authorLogin: 'priya', body, createdAt: '', databaseID: 1, viewerCanEdit: false, viewerCanDelete: false }],
    });
    const shown = thread('p1', 'open zq here', false);
    // Its suggestion is not matched either.
    const hidden = thread('p2', 'resolved zq here\n\n```suggestion\nconst zq = 1;\n```', true);
    const review = {
      shownDrafts: [], mediaHost: NO_MEDIA_HOST, drafts: [], prThreads: [shown, hidden], shownPRThreads: [shown], collapsedPaths: new Set<string>(), expandedPRThreadIds: new Set<string>(['p2']),
      draftEditBodyFor: () => null, sentComments: { editing: new Set<string>(), editBodyFor: () => null },
    } as unknown as ReviewPaneState;
    const list = { findReadingPosition: () => null, revealFindPlace: () => true, scrollFindBy: () => {} };
    const root = document.createElement('div');
    document.body.append(root);
    render(ReviewPaneFind, {
      props: { findKey: 'pane-find-hidden', title: 'Review', review, view: 'diff', files, root, diff: () => list, log: () => undefined },
    });
    const find = getPaneFindProvider('pane-find-hidden') as ReviewFind<ReviewFindHit>;
    openPaneFind('pane-find-hidden');
    setPaneFindQuery('pane-find-hidden', 'zq');
    await vi.waitFor(() => expect([find.result.count, find.result.pending]).toEqual([1, false]));
    root.remove();
  });
});
