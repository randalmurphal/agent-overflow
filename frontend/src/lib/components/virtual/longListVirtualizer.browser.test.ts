// Real-Chromium suite for LongListVirtualizer: a list taller than the
// browser can lay out is held a range at a time, and reading across a
// move of the range, jumping outside it, and changing the data under it
// all keep the reader exactly where they were.

import { afterEach, describe, expect, it } from 'vitest';
import { mount, unmount } from 'svelte';
import LongListVirtualizerHarness, { type LongListRow } from './LongListVirtualizerHarness.svelte';
import { HELD_LIMIT_PX, type HeldLimits } from '../../utils/virtual/heldRows';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

const VIEWPORT_PX = 600;
const LIMITS: HeldLimits = { span: 20_000, limit: 30_000, edge: 5_000 };

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function makeRows(count: number, heightPx = 100, estimatePx = heightPx, prefix = 'r'): LongListRow[] {
  return Array.from({ length: count }, (_, index) => ({ id: `${prefix}${index}`, index, heightPx, estimatePx }));
}

async function mountHarness(rows: LongListRow[], limits: HeldLimits | undefined = LIMITS) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const harness = mount(LongListVirtualizerHarness, {
    target: host,
    props: { initialRows: rows, limits, viewportPx: VIEWPORT_PX },
  });
  mounted.push({ app: harness, host });
  const scrollEl = host.querySelector('[data-testid="long-scroll"]') as HTMLElement;
  // The engine tail-seeds until its first scroll input; a top-anchored
  // consumer feeds it the real offset (ReviewDiffBody's restore).
  await settle(scrollEl);
  harness.handle()!.revalidate();
  await settle(scrollEl);
  return { harness, scrollEl };
}

/** Geometry stable for three frames: moves, compensations and their
 * scroll events have all landed. */
async function settle(scrollEl: HTMLElement): Promise<void> {
  let last = '';
  let stable = 0;
  await waitFor(() => {
    const now = `${scrollEl.scrollTop}:${scrollEl.scrollHeight}`;
    stable = now === last ? stable + 1 : 0;
    last = now;
    return stable >= 3;
  }, 'stable geometry');
}

/** The row under the viewport top and the pixels from its top. */
function topRow(scrollEl: HTMLElement): { id: string; index: number; into: number } {
  const top = scrollEl.getBoundingClientRect().top;
  for (const el of scrollEl.querySelectorAll<HTMLElement>('[data-row-id]')) {
    const rect = el.getBoundingClientRect();
    if (rect.top <= top && rect.bottom > top) {
      return { id: el.dataset.rowId!, index: Number(el.dataset.rowIndex), into: top - rect.top };
    }
  }
  throw new Error('no row under the viewport top');
}

/** The reader's position in the whole list, from rendered rows alone. */
function contentPosition(scrollEl: HTMLElement, rowPx: number): number {
  const row = topRow(scrollEl);
  return row.index * rowPx + row.into;
}

describe('LongListVirtualizer', () => {
  it('holds a list that fits whole', async () => {
    const { scrollEl } = await mountHarness(makeRows(100));
    expect(scrollEl.scrollHeight).toBe(10_000);
    scrollEl.scrollTop = 10_000;
    await settle(scrollEl);
    expect(topRow(scrollEl).index).toBe(94);
  });

  it('scrolls a list past the limit one pixel for one pixel across every move, down and back up', async () => {
    const { harness, scrollEl } = await mountHarness(makeRows(1000));
    expect(scrollEl.scrollHeight).toBeLessThanOrEqual(LIMITS.limit);
    const firstHeld = new Set<number>();
    let position = contentPosition(scrollEl, 100);
    let deepest = 0;
    for (const step of [1690, -1690]) {
      for (let i = 0; i < 60; i += 1) {
        scrollEl.scrollTop += step;
        await settle(scrollEl);
        const next = contentPosition(scrollEl, 100);
        const expected = Math.max(0, Math.min(100_000 - VIEWPORT_PX, position + step));
        expect(next).toBe(expected);
        expect(scrollEl.scrollHeight).toBeLessThanOrEqual(LIMITS.limit);
        firstHeld.add(harness.handle()!.findItemIndex(0));
        position = next;
        deepest = Math.max(deepest, position);
      }
    }
    // The held range moved, and the reader reached both ends of the list.
    expect(deepest).toBe(100_000 - VIEWPORT_PX);
    expect(position).toBe(0);
    expect(firstHeld.size).toBeGreaterThan(4);
  }, 60_000);

  it('jumps to rows outside the held range and back', async () => {
    const { harness, scrollEl } = await mountHarness(makeRows(1000));
    for (const target of [900, 5, 640, 999]) {
      harness.handle()!.scrollToIndex(target);
      await settle(scrollEl);
      if (target === 999) {
        const last = scrollEl.querySelector('[data-row-id="r999"]')!.getBoundingClientRect();
        expect(last.bottom).toBe(scrollEl.getBoundingClientRect().bottom);
      } else {
        expect(topRow(scrollEl)).toEqual({ id: `r${target}`, index: target, into: 0 });
      }
      expect(harness.handle()!.holds(target)).toBe(true);
    }
  });

  it('keeps the row being read in place when rows change around the held range', async () => {
    const { harness, scrollEl } = await mountHarness(makeRows(1000));
    harness.handle()!.scrollToIndex(500);
    await settle(scrollEl);
    scrollEl.scrollTop += 37;
    await settle(scrollEl);
    const before = topRow(scrollEl);

    // Rows arrive above everything, some go below, and more arrive at the end.
    const rows = [...makeRows(50, 100, 100, 'new'), ...makeRows(1000).filter((row) => row.index < 700 || row.index > 710), ...makeRows(80, 100, 100, 'tail')];
    harness.setRows(rows);
    await settle(scrollEl);
    expect(topRow(scrollEl)).toEqual(before);
    expect(scrollEl.scrollHeight).toBeLessThanOrEqual(LIMITS.limit);

    // Every row of the list stays reachable.
    harness.handle()!.scrollToIndex(rows.length - 1);
    await settle(scrollEl);
    expect(scrollEl.querySelector('[data-row-id="tail79"]')).not.toBeNull();
    harness.handle()!.scrollToIndex(0);
    await settle(scrollEl);
    expect(topRow(scrollEl).id).toBe('new0');
  });

  it('keeps the row being read when a list that fit grows past the limit', async () => {
    // A diff still arriving: held whole, then too tall to hold.
    const { harness, scrollEl } = await mountHarness(makeRows(200));
    scrollEl.scrollTop = 150 * 100 + 37;
    await settle(scrollEl);
    const before = topRow(scrollEl);
    harness.setRows(makeRows(1000));
    await settle(scrollEl);
    expect(topRow(scrollEl)).toEqual(before);
    expect(scrollEl.scrollHeight).toBeLessThanOrEqual(LIMITS.limit);
    expect(harness.handle()!.holds(0)).toBe(false);
  });

  it('moves the held range when rows measure taller than estimated', async () => {
    // Estimated at 100 px, rendered at 400: the held rows pass the limit
    // as they are measured, and the range shrinks to fit.
    const { scrollEl } = await mountHarness(makeRows(1000, 400, 100));
    let position = contentPosition(scrollEl, 400);
    let largest = 0;
    for (let i = 0; i < 100; i += 1) {
      scrollEl.scrollTop += 2000;
      await settle(scrollEl);
      const next = contentPosition(scrollEl, 400);
      expect(next).toBe(position + 2000);
      position = next;
      largest = Math.max(largest, scrollEl.scrollHeight);
    }
    // Mounted rows can grow the range past the limit until the next
    // scroll moves it: at most the rows a scroll mounts, at 4x.
    expect(largest).toBeLessThanOrEqual(LIMITS.limit + 4 * (VIEWPORT_PX + 2 * 1200 + 2000));
    expect(scrollEl.scrollHeight).toBeLessThanOrEqual(LIMITS.limit);
  }, 60_000);

  it('reaches every row of a list taller than the browser can lay out', async () => {
    // 40M px, past Chromium's ~33.5M px element height limit.
    const count = 400_000;
    const { harness, scrollEl } = await mountHarness(makeRows(count), undefined);
    expect(scrollEl.scrollHeight).toBeLessThanOrEqual(HELD_LIMIT_PX);

    harness.handle()!.scrollToIndex(count - 1);
    await settle(scrollEl);
    const last = scrollEl.querySelector(`[data-row-id="r${count - 1}"]`)!.getBoundingClientRect();
    expect(last.bottom).toBe(scrollEl.getBoundingClientRect().bottom);

    harness.handle()!.scrollToIndex(333_333);
    await settle(scrollEl);
    expect(topRow(scrollEl)).toEqual({ id: 'r333333', index: 333_333, into: 0 });
    const position = contentPosition(scrollEl, 100);
    scrollEl.scrollTop += 1234;
    await settle(scrollEl);
    expect(contentPosition(scrollEl, 100)).toBe(position + 1234);
    await raf();
  });
});
