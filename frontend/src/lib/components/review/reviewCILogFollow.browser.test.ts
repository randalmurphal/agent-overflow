// The CI log view's tail follow, in real Chromium: a followed log grows
// in its last block's row, which keeps its key and re-measures through
// ResizeObserver once the block reads its new lines. The view must land at the true
// bottom after that re-measure, follow further growth for a reader at
// the bottom, including one who wheeled back there however late the
// scroll landed, and leave a reader who wheeled away alone. Expanding a
// section holds its row where it was instead of following to the end.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
import '../../../app.css';
import ReviewCILogView from './ReviewCILogView.svelte';
import type { CIJob } from '../../types/models';
import { FakeCILog, numberedLines, section } from '../../../test/helpers/ciLog';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';

const VIEWPORT_PX = 400;
// A log of two blocks whose last block is partial, so growth re-measures
// a mounted row rather than adding one.
const FIRST_LINES = 350;
const GROWTH_LINES = 31;
// The controller's near-bottom band is wider; the pin itself lands exact.
const AT_BOTTOM_PX = 2;

const JOB: CIJob = { id: '20', name: 'unit', status: 'running', logsAvailable: true };

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
  resetScrollIntentModuleStateForTest();
});

function mountLog(fake = new FakeCILog(numberedLines(FIRST_LINES))) {
  const host = document.createElement('div');
  host.style.cssText = `position:fixed;top:0;left:0;width:800px;height:${VIEWPORT_PX}px;display:flex`;
  document.body.appendChild(host);
  // A job without steps or sections is one row, the job's, kept open.
  const openSections = new SvelteSet<string>(['20/job', '21/job']);
  const p = rawProps({
    view: { stageName: 'test', jobId: '20', job: JOB },
    log: fake.state,
    fetchLines: fake.fetch,
    loading: false,
    error: null as string | null,
    savedPath: null as string | null,
    openSections,
    onBack: () => {},
    onRefresh: () => {},
    onSave: () => {},
    onSend: () => {},
    onToggleSection: (key: string) => {
      const full = `20/${key}`;
      if (openSections.has(full)) openSections.delete(full);
      else openSections.add(full);
    },
    onSetSectionsOpen: () => {},
    onCopySection: async () => true,
    onSendSection: () => {},
  });
  const app = mount(ReviewCILogView, { target: host, props: p });
  mounted.push({ app, host });
  const scroll = host.querySelector<HTMLElement>('[data-testid="review-ci-log-scroll"]');
  if (!scroll) throw new Error('log scroller did not mount');
  return { p, scroll, fake };
}

function distanceFromBottom(el: HTMLElement): number {
  return el.scrollHeight - el.scrollTop - el.clientHeight;
}

async function settledAtBottom(scroll: HTMLElement, label: string): Promise<void> {
  await waitFor(() => distanceFromBottom(scroll) <= AT_BOTTOM_PX, label);
  // Still there once the engine's measurement passes have all landed.
  for (let i = 0; i < 12; i += 1) await raf();
  expect(distanceFromBottom(scroll)).toBeLessThanOrEqual(AT_BOTTOM_PX);
}

/** GitLab sections of a header and 300 lines each, as the backend lists
 * them. */
function sectionedLog(names: string[]): FakeCILog {
  const lines: string[] = [];
  const sections = names.map((name, index) => {
    const start = lines.length;
    lines.push(`${name} header`, ...numberedLines(300, 1, `${name} line`));
    return section(`section:${name}:${1700000000 + index * 10}`, name, 'done', start, lines.length, { durationSeconds: 5 });
  });
  return new FakeCILog(lines, sections);
}

describe('ReviewCILogView tail follow', () => {
  it('opens at the tail and follows growth of the last chunk', async () => {
    const { p, scroll, fake } = mountLog();
    // The pane bounds the view: the tail is a scroll position.
    await waitFor(() => scroll.scrollHeight - scroll.clientHeight > 1000, 'the log to lay out');
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES}`) === true, 'the last block');
    await settledAtBottom(scroll, 'the opened log at its tail');

    fake.grow(numberedLines(GROWTH_LINES, FIRST_LINES + 1));
    p.log = fake.state;
    flushSync();
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES + GROWTH_LINES}`) === true, 'the grown log');
    await settledAtBottom(scroll, 'the tail after growth');
    // The grown block read only its new lines.
    expect(fake.reads.at(-1)).toEqual([FIRST_LINES, GROWTH_LINES]);
  });

  it('leaves a reader who wheeled up alone and re-sticks on a new job', async () => {
    const { p, scroll, fake } = mountLog();
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES}`) === true, 'the last block');
    await settledAtBottom(scroll, 'the opened log at its tail');

    // The reader wheels up, past the re-stick band but with the last
    // chunk still mounted, so its growth is measured and would re-pin a
    // reader the controller still took for one at the bottom.
    scroll.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
    const readingAt = scroll.scrollTop - 600;
    scroll.scrollTop = readingAt;
    await raf();
    await raf();
    expect(scroll.scrollTop).toBe(readingAt);

    fake.grow(numberedLines(GROWTH_LINES, FIRST_LINES + 1));
    p.log = fake.state;
    flushSync();
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES + GROWTH_LINES}`) === true, 'the grown log');
    for (let i = 0; i < 12; i += 1) await raf();
    expect(scroll.scrollTop).toBe(readingAt);

    // Opening another job's log lands at its tail.
    const lint = new FakeCILog(numberedLines(FIRST_LINES));
    p.view = { stageName: 'test', jobId: '21', job: { ...JOB, id: '21', name: 'lint' } };
    p.fetchLines = lint.fetch;
    p.log = lint.state;
    flushSync();
    await settledAtBottom(scroll, 'the new job at its tail');
  });

  it('follows again once a reader\'s wheel to the end lands, however late its scroll event', async () => {
    const { p, scroll, fake } = mountLog();
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES}`) === true, 'the last block');
    await settledAtBottom(scroll, 'the opened log at its tail');
    // The landing below returns to the pin's own scrollTop: let the pin's
    // write tokens expire so it reads as the reader's scroll, as it does
    // after any real reading.
    await new Promise((resolve) => setTimeout(resolve, 600));

    scroll.dispatchEvent(new WheelEvent('wheel', { deltaY: -120, bubbles: true }));
    scroll.scrollTop -= 600;
    await raf();
    await raf();

    // The wheel back down is the reader's; a loaded host delivers its
    // scroll to the end well past the 250 ms down-intent window.
    scroll.dispatchEvent(new WheelEvent('wheel', { deltaY: 120, bubbles: true }));
    await new Promise((resolve) => setTimeout(resolve, 400));
    scroll.scrollTop = scroll.scrollHeight;
    await settledAtBottom(scroll, 'the reader at the end');

    fake.grow(numberedLines(GROWTH_LINES, FIRST_LINES + 1));
    p.log = fake.state;
    flushSync();
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES + GROWTH_LINES}`) === true, 'the grown log');
    await settledAtBottom(scroll, 'the tail after growth');
  });

  it('holds an expanded section at its row instead of following to the end', async () => {
    // Three collapsed sections fit the viewport, so the reader is at the
    // bottom; the first one opens onto more lines than the viewport holds.
    const { scroll } = mountLog(sectionedLog(['prepare', 'script', 'cleanup']));
    await waitFor(() => scroll.querySelectorAll('[data-testid="review-ci-section"]').length === 3, 'the section rows');
    expect(scroll.scrollHeight).toBeLessThanOrEqual(scroll.clientHeight);

    const rowOf = () => scroll.querySelector<HTMLElement>('[data-key="section:prepare:1700000000"]');
    const toggle = rowOf()?.querySelector<HTMLElement>('[data-testid="review-ci-section-toggle"]');
    if (!toggle) throw new Error('no toggle');
    const rowTop = rowOf()!.getBoundingClientRect().top;
    toggle.click();
    await waitFor(() => scroll.scrollHeight - scroll.clientHeight > 1000, 'the expanded section');
    for (let i = 0; i < 12; i += 1) await raf();
    expect(scroll.scrollTop).toBe(0);
    // The row stays put, open, with its lines under it on screen.
    const row = rowOf();
    expect(row?.dataset.open).toBe('true');
    expect(row?.getBoundingClientRect().top).toBe(rowTop);
    expect(scroll.textContent).toContain('prepare line 1\n');
  });

  it('holds an expanded section of a list that opened scrolled to its end', async () => {
    // More collapsed sections than the viewport holds: the open places
    // the list at its end, and the reader expands a row on screen there.
    const { scroll } = mountLog(sectionedLog(Array.from({ length: 30 }, (_, index) => `s${index}`)));
    await settledAtBottom(scroll, 'the list at its end');

    const rowOf = () => scroll.querySelector<HTMLElement>('[data-key="section:s27:1700000270"]');
    const rowTop = rowOf()!.getBoundingClientRect().top;
    rowOf()!.querySelector<HTMLElement>('[data-testid="review-ci-section-toggle"]')!.click();
    await waitFor(() => rowOf()?.dataset.open === 'true', 'the expanded section');
    for (let i = 0; i < 12; i += 1) await raf();
    await new Promise((resolve) => setTimeout(resolve, 300));
    expect(rowOf()?.getBoundingClientRect().top).toBe(rowTop);
    expect(scroll.textContent).toContain('s27 line 1\n');
  });
});
