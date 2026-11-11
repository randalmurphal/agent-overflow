// ReviewPaneFind's CI log source: an answer applies only to the job and
// log version it was read from, and another job's log starts the query
// over at once. Painting is covered by findHighlights.browser.test.ts.
import { render } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import ReviewPaneFind from './ReviewPaneFind.svelte';
import { getPaneFindProvider, openPaneFind, resetPaneFindForTest, setPaneFindQuery } from '../../stores/paneFind.svelte';
import { REVIEW_FIND_REMOTE_DEBOUNCE_MS, type ReviewFind } from '../../stores/reviewFind.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import { placeCILogLine, type CILogFindHit } from '../../utils/ciLogFind';
import type { CILogSearch, CILogSection } from '../../types/models';

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
      log: () => ({ findReadingLine: () => 0, findLineText: () => null, findLineLoad: () => ({ loading: false, error: null }), revealFindLine: async () => {}, scrollFindBy: () => {} }),
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
