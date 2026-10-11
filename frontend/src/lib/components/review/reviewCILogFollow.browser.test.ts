// The CI log view's tail follow, in real Chromium: a followed log grows
// in its last chunk, which keeps its key and re-measures through
// ResizeObserver after the data change. The view must land at the true
// bottom after that re-measure, follow further growth for a reader at
// the bottom, and leave a reader who wheeled away alone. Expanding a
// section holds its row where it was instead of following to the end.

import { afterEach, describe, expect, it } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { SvelteSet } from 'svelte/reactivity';
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

function mountLog(log = logOf(FIRST_LINES)) {
  const host = document.createElement('div');
  host.style.cssText = `position:fixed;top:0;left:0;width:800px;height:${VIEWPORT_PX}px;display:flex`;
  document.body.appendChild(host);
  // A job without steps or sections is one row, the job's, kept open.
  const openSections = new SvelteSet<string>(['20/job', '21/job']);
  const p = rawProps({
    view: { stageName: 'test', jobId: '20', job: JOB },
    log,
    loading: false,
    error: null as string | null,
    available: true,
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
    onSendSection: () => {},
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

  it('holds an expanded section at its row instead of following to the end', async () => {
    // Three collapsed sections fit the viewport, so the reader is at the
    // bottom; the first one opens onto more lines than the viewport holds.
    let text = '';
    for (const [index, name] of ['prepare', 'script', 'cleanup'].entries()) {
      text += `section_start:${1700000000 + index * 10}:${name}\n${name} header\n`;
      for (let line = 1; line <= 300; line += 1) text += `${name} line ${line}\n`;
      text += `section_end:${1700000005 + index * 10}:${name}\n`;
    }
    const { scroll } = mountLog({ text, truncated: false, totalBytes: text.length });
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
    let text = '';
    for (let index = 0; index < 30; index += 1) {
      text += `section_start:${1700000000 + index * 10}:s${index}\ns${index} header\n`;
      for (let line = 1; line <= 300; line += 1) text += `s${index} line ${line}\n`;
      text += `section_end:${1700000005 + index * 10}:s${index}\n`;
    }
    const { scroll } = mountLog({ text, truncated: false, totalBytes: text.length });
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
