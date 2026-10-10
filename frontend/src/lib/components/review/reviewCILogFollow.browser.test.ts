// The CI log view's tail follow, in real Chromium: a followed log grows
// in its last chunk, which keeps its key and re-measures through
// ResizeObserver after the data change. The view must land at the true
// bottom after that re-measure, follow further growth for a reader at
// the bottom, and leave a reader who wheeled away alone.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import ReviewCILogView from './ReviewCILogView.svelte';
import type { CIJob } from '../../types/models';
import { raf, waitFor } from '../../../test/helpers/browserFrames';
import { rawProps } from '../../../test/helpers/rawProps.svelte';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';

const VIEWPORT_PX = 400;
// A log of two chunks whose last chunk is partial, so growth re-measures
// a mounted row rather than adding one.
const FIRST_LINES = 390;
const GROWTH_LINES = 31;
// The controller's near-bottom band is wider; the pin itself lands exact.
const AT_BOTTOM_PX = 2;

const JOB: CIJob = { id: '20', name: 'unit', status: 'running', logsAvailable: true };

function logOf(lines: number) {
  const text = Array.from({ length: lines }, (_, index) => `line ${index + 1}`).join('\n') + '\n';
  return { text, truncated: false, totalBytes: text.length };
}

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
  resetScrollIntentModuleStateForTest();
});

function mountLog() {
  const host = document.createElement('div');
  host.style.cssText = `position:fixed;top:0;left:0;width:800px;height:${VIEWPORT_PX}px;display:flex`;
  document.body.appendChild(host);
  const p = rawProps({
    view: { stageName: 'test', jobId: '20', job: JOB },
    log: logOf(FIRST_LINES),
    loading: false,
    error: null as string | null,
    available: true,
    savedPath: null as string | null,
    onBack: () => {},
    onRefresh: () => {},
    onSave: () => {},
    onSend: () => {},
  });
  const app = mount(ReviewCILogView, { target: host, props: p });
  mounted.push({ app, host });
  const scroll = host.querySelector<HTMLElement>('[data-testid="review-ci-log-scroll"]');
  if (!scroll) throw new Error('log scroller did not mount');
  return { p, scroll };
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

describe('ReviewCILogView tail follow', () => {
  it('opens at the tail and follows growth of the last chunk', async () => {
    const { p, scroll } = mountLog();
    // The pane bounds the view: the tail is a scroll position.
    await waitFor(() => scroll.scrollHeight - scroll.clientHeight > 1000, 'the log to lay out');
    await settledAtBottom(scroll, 'the opened log at its tail');

    p.log = logOf(FIRST_LINES + GROWTH_LINES);
    flushSync();
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES + GROWTH_LINES}`) === true, 'the grown log');
    await settledAtBottom(scroll, 'the tail after growth');
  });

  it('leaves a reader who wheeled up alone and re-sticks on a new job', async () => {
    const { p, scroll } = mountLog();
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

    p.log = logOf(FIRST_LINES + GROWTH_LINES);
    flushSync();
    await waitFor(() => scroll.textContent?.includes(`line ${FIRST_LINES + GROWTH_LINES}`) === true, 'the grown log');
    for (let i = 0; i < 12; i += 1) await raf();
    expect(scroll.scrollTop).toBe(readingAt);

    // Opening another job's log lands at its tail.
    p.view = { stageName: 'test', jobId: '21', job: { ...JOB, id: '21', name: 'lint' } };
    p.log = logOf(FIRST_LINES);
    flushSync();
    await settledAtBottom(scroll, 'the new job at its tail');
  });
});
