// Review find over a CI log in real Chromium: a step to a match whose
// block is not loaded stays owed until the block arrives and its painted
// match is in view, however long that takes, and schedules no frame or
// timer of its own while it waits; the reader scrolling meanwhile ends
// it; a block that fails to load puts its error in the bar.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import '../../../app.css';
import ReviewCILogView from './ReviewCILogView.svelte';
import ReviewPaneFind from './ReviewPaneFind.svelte';
import { getPaneFindProvider, openPaneFind, resetPaneFindForTest, setPaneFindQuery } from '../../stores/paneFind.svelte';
import type { ReviewFind } from '../../stores/reviewFind.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import type { CILogFindHit } from '../../utils/ciLogFind';
import { findHighlightName } from '../../utils/findHighlights';
import type { CIJob, CILogSearch } from '../../types/models';
import { FakeCILog, numberedLines } from '../../../test/helpers/ciLog';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { spyScheduling } from '../../../test/helpers/schedulingSpy';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';

const JOB: CIJob = { id: '20', name: 'unit', status: 'success', logsAvailable: true };
const MATCH_LINE = 3000;
const SECOND_LINE = 7000;

const mounted: (() => void)[] = [];

afterEach(() => {
  for (const release of mounted.splice(0)) release();
  resetPaneFindForTest();
  resetScrollIntentModuleStateForTest();
});

function setup(key: string, second = false) {
  const lines = numberedLines(10_000);
  // Wrapped lines above the match: the per-line estimate lands far off.
  for (let at = MATCH_LINE - 199; at < MATCH_LINE; at += 1) lines[at] = `${'w'.repeat(400)} ${at}`;
  lines[MATCH_LINE] = '\x1b[31mzq\x1b[0m at the match';
  if (second) lines[SECOND_LINE] = 'zq at the second match';
  const fake = new FakeCILog(lines);
  // Reads of one block wait on its gate.
  const blockGates = new Map<number, Promise<void>>();
  const asked: number[] = [];
  const fetchLines: typeof fake.fetch = async (from, count) => {
    asked.push(from);
    const gate = blockGates.get(from);
    if (gate) await gate;
    return fake.fetch(from, count);
  };
  const host = document.createElement('div');
  host.style.cssText = 'position:fixed;top:0;left:0;width:700px;height:400px;display:flex';
  document.body.appendChild(host);
  const openSections = new SvelteSet<string>(['20/job']);
  const logProps = rawProps({
    view: { stageName: 'test', jobId: '20', job: JOB },
    log: fake.state,
    fetchLines,
    loading: false,
    error: null as string | null,
    savedPath: null as string | null,
    openSections,
    onBack: () => {},
    onRefresh: () => {},
    onSave: () => {},
    onSend: () => {},
    onToggleSection: () => {},
    onSetSectionsOpen: (keys: readonly string[]) => { for (const each of keys) openSections.add(`20/${each}`); },
    onCopySection: async () => true,
    onSendSection: () => {},
  });
  const logView = mount(ReviewCILogView, { target: host, props: logProps });
  const hits = second ? [{ line: MATCH_LINE, count: 1 }, { line: SECOND_LINE, count: 1 }] : [{ line: MATCH_LINE, count: 1 }];
  const search: CILogSearch = { version: fake.version, totalLines: lines.length, matches: hits.length, capped: false, hits };
  const review = {
    ciLog: fake.state,
    ciLogView: { jobId: '20' },
    searchCILog: async () => search,
    drafts: [], prThreads: [], wordWrap: false,
  } as unknown as ReviewPaneState;
  const finder = mount(ReviewPaneFind, {
    target: document.body,
    props: { findKey: key, title: 'Review', review, view: 'log', files: [], root: host, diff: () => undefined, log: () => logView as never },
  });
  flushSync();
  mounted.push(() => {
    unmount(finder);
    unmount(logView);
    host.remove();
  });
  const scroll = host.querySelector<HTMLElement>('[data-testid="review-ci-log-scroll"]')!;
  const find = getPaneFindProvider(key) as ReviewFind<CILogFindHit>;
  const painted = () => {
    const [range] = [...(CSS.highlights.get(`${findHighlightName(key)}-current`) ?? [])] as Range[];
    return range ?? null;
  };
  const inView = () => {
    const range = painted();
    if (!range) return false;
    const box = range.getBoundingClientRect();
    const view = scroll.getBoundingClientRect();
    return box.top >= view.top && box.bottom <= view.bottom;
  };
  return { fake, scroll, find, painted, inView, blockGates, asked };
}

function deferred() {
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  return { gate, release };
}

describe('review find over a CI log', () => {
  it('a reveal whose block loads late stays owed, and brings the painted match into view once it loads', async () => {
    const { fake, scroll, find, painted, inView } = setup('pane-log-late');
    await waitFor(() => scroll.textContent?.includes('line 10000') === true, 'the tail');
    const block = deferred();
    fake.gate = block.gate;
    openPaneFind('pane-log-late');
    setPaneFindQuery('pane-log-late', 'zq');
    await waitFor(() => find.result.count === 1 && find.selected !== null, 'the answer');
    const spy = spyScheduling('ReviewPaneFind.svelte', ['pause']);
    try {
      // Far longer than any frame allowance: still owed, nothing claimed,
      // and nothing of find's own scheduled to ask again.
      for (let i = 0; i < 200; i += 1) await raf();
      expect(find.wants(find.selected!)).toBe(true);
      expect(painted()).toBeNull();
      expect(spy.calls).toEqual([]);
      fake.gate = null;
      block.release();
      await waitFor(() => !find.wants(find.selected!), 'the reveal answered', 600);
      expect(painted()?.toString()).toBe('zq');
      expect(inView()).toBe(true);
      expect(find.result.error).toBeNull();
      for (let i = 0; i < 30; i += 1) await raf();
      expect(spy.calls).toEqual([]);
    } finally {
      spy.restore();
    }
  });

  it('the reader scrolling while a reveal waits on its block ends the reveal: the block arrives and the view stays where the reader left it', async () => {
    const { fake, scroll, find, painted, inView } = setup('pane-log-away');
    await waitFor(() => scroll.textContent?.includes('line 10000') === true, 'the tail');
    const tail = scroll.scrollTop;
    const block = deferred();
    fake.gate = block.gate;
    openPaneFind('pane-log-away');
    setPaneFindQuery('pane-log-away', 'zq');
    await waitFor(() => find.result.count === 1 && find.selected !== null, 'the answer');
    // The list was asked for the match's row: it is mounted, not loaded.
    await waitFor(() => scroll.scrollTop < tail - 10_000 && scroll.querySelector('[data-testid="review-ci-log-block-placeholder"]') !== null, 'the row asked', 600);
    for (let i = 0; i < 10; i += 1) await raf();
    expect(find.wants(find.selected!)).toBe(true);
    const spy = spyScheduling('ReviewPaneFind.svelte', ['pause']);
    try {
      // The reader wheels down past it; its row stays mounted.
      scroll.dispatchEvent(new WheelEvent('wheel', { deltaY: 300, bubbles: true }));
      scroll.scrollTop += 300;
      for (let i = 0; i < 5; i += 1) await raf();
      expect(find.wants(find.selected!)).toBe(false);
      fake.gate = null;
      block.release();
      // The block arrives and paints: the repaint asks nothing.
      await waitFor(() => painted()?.toString() === 'zq', 'the block painted', 600);
      for (let i = 0; i < 30; i += 1) await raf();
      expect(find.wants(find.selected!)).toBe(false);
      // Still past it, as the reader left it (the rows above it grew and
      // were held in place, so scrollTop itself moved).
      expect(inView()).toBe(false);
      expect(painted()!.getBoundingClientRect().bottom).toBeLessThan(scroll.getBoundingClientRect().top);
      expect(find.result.error).toBeNull();
      expect(spy.calls).toEqual([]);
    } finally {
      spy.restore();
    }
  });

  it('a step back onto a match whose block is still being read asks for its row again, and shows it once the read lands', async () => {
    const { scroll, find, painted, inView, blockGates, asked } = setup('pane-log-again', true);
    await waitFor(() => scroll.textContent?.includes('line 10000') === true, 'the tail');
    const block = deferred();
    blockGates.set(SECOND_LINE, block.gate);
    openPaneFind('pane-log-again');
    setPaneFindQuery('pane-log-again', 'zq');
    await waitFor(() => find.result.count === 2 && find.selected !== null, 'the answer');
    // From the tail the first match is the last: its block's read starts.
    expect(find.hits[find.result.current].line).toBe(SECOND_LINE);
    await waitFor(() => asked.includes(SECOND_LINE), 'the second block asked', 600);
    // Back to the first match: shown, far from the second.
    find.previous();
    await waitFor(() => !find.wants(find.selected!) && painted()?.toString() === 'zq', 'the first match shown', 600);
    // The second match's row is not mounted, and its block's read is out.
    expect(scroll.textContent).not.toContain(`lines ${SECOND_LINE + 1} to`);
    // Forward again while the second block's first read is still out.
    find.next();
    expect(find.hits[find.result.current].line).toBe(SECOND_LINE);
    // The step asks for its row at once: it mounts on the read in flight.
    await waitFor(() => scroll.textContent?.includes(`lines ${SECOND_LINE + 1} to`) === true, 'the second row mounted', 60);
    expect(find.wants(find.selected!)).toBe(true);
    block.release();
    await waitFor(() => !find.wants(find.selected!), 'the step answered', 600);
    expect(painted()?.startContainer.parentElement?.closest('[data-find-key]')?.textContent).toContain('the second match');
    expect(inView()).toBe(true);
  });

  it('a block that fails to load puts its error in the bar', async () => {
    const { fake, scroll, find } = setup('pane-log-fail');
    await waitFor(() => scroll.textContent?.includes('line 10000') === true, 'the tail');
    const block = deferred();
    fake.gate = block.gate;
    fake.failure = new Error('read failed');
    openPaneFind('pane-log-fail');
    setPaneFindQuery('pane-log-fail', 'zq');
    await waitFor(() => find.result.count === 1 && find.selected !== null, 'the answer');
    block.release();
    await waitFor(() => find.result.error !== null, 'the error', 600);
    expect(find.result.error).toContain('read failed');
    expect(find.wants(find.selected!)).toBe(false);
  });
});
