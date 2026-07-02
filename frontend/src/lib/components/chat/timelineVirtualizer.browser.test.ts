// Real-Chromium suite for TimelineVirtualizer + VirtualRow: real
// ResizeObserver timing, real scroll events, real layout. Exercises the
// adapter against a synthetic fixture (TimelineVirtualizerHarness) — the
// MessageTimeline integration is covered by the engine-agnostic outcome
// suites at cutover (plan §4 V1/V2).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import TimelineVirtualizerHarness, { type HarnessRow } from './TimelineVirtualizerHarness.svelte';
import type { EngineCompensation } from '../../utils/virtual/types';
import { raf, waitFor } from '../../../test/helpers/browserFrames';

const VIEWPORT_PX = 600;
const BUFFER_PX = 400;
const ROW_PX = 100;
const ESTIMATE_PX = 56;
const ROW_COUNT = 60;

const mounted: { app: object; host: HTMLElement }[] = [];

afterEach(() => {
  for (const { app, host } of mounted.splice(0)) {
    unmount(app);
    host.remove();
  }
});

function makeRows(count: number, heightPx = ROW_PX): HarnessRow[] {
  return Array.from({ length: count }, (_, i) => ({
    id: `row-${i}`,
    heightPx,
    label: `Row ${i}`,
  }));
}

function mountHarness(
  props: Partial<{
    initialRows: HarnessRow[];
    bufferSize: number;
    renderAll: boolean;
    onscroll: (offset: number) => void;
    onscrollend: () => void;
    onCompensation: (compensation: EngineCompensation) => void;
  }> = {},
) {
  const host = document.createElement('div');
  document.body.appendChild(host);
  const harness = mount(TimelineVirtualizerHarness, {
    target: host,
    props: {
      initialRows: makeRows(ROW_COUNT),
      bufferSize: BUFFER_PX,
      viewportPx: VIEWPORT_PX,
      ...props,
    },
  });
  mounted.push({ app: harness, host });
  const scrollEl = host.querySelector('[data-testid="virt-scroll"]') as HTMLElement;
  return { harness, host, scrollEl };
}

function mountedRowIndexes(scrollEl: HTMLElement): number[] {
  return [...scrollEl.querySelectorAll('[data-row-index]')].map((el) =>
    Number(el.getAttribute('data-row-index')),
  );
}

function rowEl(scrollEl: HTMLElement, id: string): HTMLElement | null {
  return scrollEl.querySelector(`[data-row-id="${id}"]`);
}

// Settle: geometry stable for `stableFrames` consecutive frames.
async function waitForStableGeometry(scrollEl: HTMLElement, label: string): Promise<void> {
  let lastHeight = -1;
  let stable = 0;
  await waitFor(() => {
    const height = scrollEl.scrollHeight;
    if (height === lastHeight) {
      stable++;
    } else {
      stable = 0;
      lastHeight = height;
    }
    return stable >= 5;
  }, label);
}

// Emulates the scroll controller's pinned-follow: the engine deliberately
// never self-pins, so as newly mounted estimate-sized rows measure larger,
// "bottom" moves and must be re-written each beat until geometry settles.
async function pinToBottomAndSettle(scrollEl: HTMLElement, label: string): Promise<void> {
  let lastHeight = -1;
  let stable = 0;
  await waitFor(() => {
    const max = scrollEl.scrollHeight - scrollEl.clientHeight;
    if (scrollEl.scrollTop !== max) scrollEl.scrollTop = max;
    if (scrollEl.scrollHeight === lastHeight && scrollEl.scrollTop === max) {
      stable++;
    } else {
      stable = 0;
      lastHeight = scrollEl.scrollHeight;
    }
    return stable >= 5;
  }, label);
}

describe('mount + tail seeding', () => {
  it('mounts a tail-anchored subset and keeps totalSize === scrollHeight', async () => {
    const { harness, scrollEl } = mountHarness();
    await waitFor(() => mountedRowIndexes(scrollEl).length > 0, 'rows to mount');

    const indexes = mountedRowIndexes(scrollEl);
    // Real windowing: a subset, anchored at the tail.
    expect(indexes.length).toBeGreaterThan(0);
    expect(indexes.length).toBeLessThan(ROW_COUNT);
    expect(indexes).toContain(ROW_COUNT - 1);
    expect(indexes).not.toContain(0);

    await waitForStableGeometry(scrollEl, 'mount measurements');
    const handle = harness.handle()!;
    expect(scrollEl.scrollHeight).toBe(Math.round(handle.getTotalSize()));
    expect(handle.getViewportSize()).toBe(VIEWPORT_PX);

    // Mounted rows are measured and revealed.
    const firstMounted = scrollEl.querySelector('[data-row-index]') as HTMLElement;
    expect(getComputedStyle(firstMounted.parentElement as HTMLElement).visibility).toBe('visible');
  });

  it('renderAll mounts every row', async () => {
    const { scrollEl } = mountHarness({ renderAll: true });
    await waitFor(
      () => mountedRowIndexes(scrollEl).length === ROW_COUNT,
      'renderAll to mount everything',
    );
  });
});

describe('windowing', () => {
  it('evicts on scroll away and remounts on return', async () => {
    const { scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');

    // In production the controller's pin write provides the first scroll
    // input; the harness has no controller, so emulate it — assigning an
    // unchanged scrollTop (0 → 0) fires no scroll event at all.
    await pinToBottomAndSettle(scrollEl, 'bottom settle');

    scrollEl.scrollTop = 0;
    await waitFor(
      () =>
        rowEl(scrollEl, `row-${ROW_COUNT - 1}`) === null && rowEl(scrollEl, 'row-0') !== null,
      'tail to unmount at top',
    );
    // Window stays bounded: viewport + 2×buffer of ~100px rows + margin.
    expect(mountedRowIndexes(scrollEl).length).toBeLessThan(30);

    await pinToBottomAndSettle(scrollEl, 'return to bottom');
    await waitFor(
      () =>
        rowEl(scrollEl, `row-${ROW_COUNT - 1}`) !== null && rowEl(scrollEl, 'row-0') === null,
      'tail to remount at bottom',
    );
    expect(mountedRowIndexes(scrollEl).length).toBeLessThan(30);
  });
});

describe('ownership: the adapter never writes scrollTop', () => {
  it('reports an above-viewport remeasure as compensation and leaves scrollTop alone', async () => {
    const compensations: EngineCompensation[] = [];
    const { harness, scrollEl } = mountHarness({
      onCompensation: (c) => compensations.push(c),
    });
    await waitForStableGeometry(scrollEl, 'mount');

    await pinToBottomAndSettle(scrollEl, 'bottom settle');
    const scrollTopBefore = scrollEl.scrollTop;
    compensations.length = 0;

    // A mounted row entirely above the viewport grows by 200px.
    const grownIndex = mountedRowIndexes(scrollEl)[0];
    harness.resizeRow(`row-${grownIndex}`, ROW_PX + 200);
    await waitFor(() => compensations.length > 0, 'remeasure compensation');

    expect(compensations[0].kind).toBe('remeasure-above');
    expect(compensations[0].delta).toBe(200);
    expect(compensations[0].target).toBe(scrollTopBefore + 200);
    // Single-writer: with no controller attached, nothing wrote scrollTop.
    expect(scrollEl.scrollTop).toBe(scrollTopBefore);
  });

  it('reports a head splice as compensation and keeps row DOM identity', async () => {
    const compensations: EngineCompensation[] = [];
    const { harness, scrollEl } = mountHarness({
      onCompensation: (c) => compensations.push(c),
    });
    await waitForStableGeometry(scrollEl, 'mount');

    // Kick a real scroll input first (see the eviction test note), then
    // return to the top where the head splice is observable.
    await pinToBottomAndSettle(scrollEl, 'bottom settle');
    scrollEl.scrollTop = 0;
    await waitFor(() => rowEl(scrollEl, 'row-0') !== null, 'top rows to mount');
    await waitForStableGeometry(scrollEl, 'top settle');
    const row0Before = rowEl(scrollEl, 'row-0');
    compensations.length = 0;

    harness.setRows(
      [{ id: 'prepended', heightPx: ROW_PX, label: 'Prepended' }, ...harness.getRows()],
      { shift: true },
    );
    await waitFor(() => compensations.length > 0, 'head-splice compensation');

    expect(compensations[0].kind).toBe('head-splice');
    // The new head row enters at the flat estimate until measured.
    expect(compensations[0].delta).toBe(ESTIMATE_PX);
    expect(compensations[0].target).toBe(ESTIMATE_PX);
    expect(scrollEl.scrollTop).toBe(0);

    // Keyed identity: the old first row keeps its DOM node and re-registers
    // under its shifted index.
    await waitFor(
      () => rowEl(scrollEl, 'row-0')?.getAttribute('data-row-index') === '1',
      'row-0 to re-index',
    );
    expect(rowEl(scrollEl, 'row-0')).toBe(row0Before);
    expect(rowEl(scrollEl, 'prepended')).not.toBeNull();
  });
});

describe('write timing: compensation is delivered post-flush', () => {
  // The scroll controller's resolver reads live DOM geometry (bottom
  // target = scrollHeight − clientHeight) and may write a target beyond
  // the PRE-update scrollHeight. Delivered before the template flush,
  // that write clamps against the stale spacer and the pinned tail
  // visibly shifts by the growth delta (the compensationOutcome failure
  // shapes). Pin the contract at the adapter level: at onCompensation
  // time the DOM must already reflect the engine's new totalSize.
  it('the DOM reflects the new totalSize at onCompensation time', async () => {
    const samples: { kind: string; scrollHeight: number; totalSize: number }[] = [];
    const ctx = mountHarness({
      onCompensation: (c) => {
        samples.push({
          kind: c.kind,
          scrollHeight: ctx.scrollEl.scrollHeight,
          totalSize: ctx.harness.handle()!.getTotalSize(),
        });
      },
    });
    const { harness, scrollEl } = ctx;
    await waitForStableGeometry(scrollEl, 'mount');
    await pinToBottomAndSettle(scrollEl, 'bottom settle');

    // Above-viewport remeasure.
    const grownIndex = mountedRowIndexes(scrollEl)[0];
    harness.resizeRow(`row-${grownIndex}`, ROW_PX + 200);
    await waitFor(() => samples.some((s) => s.kind === 'remeasure-above'), 'remeasure delivery');

    // Head splice observed from the top.
    scrollEl.scrollTop = 0;
    await waitFor(() => rowEl(scrollEl, 'row-0') !== null, 'top rows to mount');
    await waitForStableGeometry(scrollEl, 'top settle');
    harness.setRows(
      [{ id: 'prepended-timing', heightPx: ROW_PX, label: 'Prepended' }, ...harness.getRows()],
      { shift: true },
    );
    await waitFor(() => samples.some((s) => s.kind === 'head-splice'), 'head-splice delivery');

    for (const sample of samples) {
      expect(
        sample.scrollHeight,
        `${sample.kind} compensation delivered before the spacer updated`,
      ).toBe(Math.round(sample.totalSize));
    }
  });
});

describe('scrollToIndex', () => {
  it('converges onto an unmeasured destination', async () => {
    const { harness, scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');
    const handle = harness.handle()!;

    handle.scrollToIndex(30, { align: 'start' });
    await waitForStableGeometry(scrollEl, 'index scroll settle');

    // Self-consistent landing: scrollTop sits on the row's final offset
    // and the row's box starts at the viewport top.
    expect(Math.abs(scrollEl.scrollTop - handle.getItemOffset(30))).toBeLessThanOrEqual(1);
    const row = rowEl(scrollEl, 'row-30')!;
    const scrollRect = scrollEl.getBoundingClientRect();
    const rowRect = row.getBoundingClientRect();
    expect(Math.abs(rowRect.top - scrollRect.top)).toBeLessThanOrEqual(1);
  });

  it('aligns end onto the last row', async () => {
    const { harness, scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');
    const handle = harness.handle()!;

    handle.scrollToIndex(ROW_COUNT - 1, { align: 'end' });
    await waitForStableGeometry(scrollEl, 'align-end settle');

    const row = rowEl(scrollEl, `row-${ROW_COUNT - 1}`)!;
    const scrollRect = scrollEl.getBoundingClientRect();
    expect(Math.abs(row.getBoundingClientRect().bottom - scrollRect.bottom)).toBeLessThanOrEqual(1);
  });
});

describe('scrollend synthesis', () => {
  it('fires once ~150ms after the last scroll event', async () => {
    const onscrollend = vi.fn();
    const { scrollEl } = mountHarness({ onscrollend });
    await waitForStableGeometry(scrollEl, 'mount');
    onscrollend.mockClear();

    scrollEl.scrollTop = 1000;
    await raf();
    expect(onscrollend).not.toHaveBeenCalled();
    await waitFor(() => onscrollend.mock.calls.length > 0, 'synthetic scrollend');
    expect(onscrollend).toHaveBeenCalledTimes(1);
  });
});

describe('teardown', () => {
  it('survives mount/unmount churn, including unmount before first frame', async () => {
    for (let i = 0; i < 4; i++) {
      const host = document.createElement('div');
      document.body.appendChild(host);
      const app = mount(TimelineVirtualizerHarness, {
        target: host,
        props: { initialRows: makeRows(20), bufferSize: BUFFER_PX, viewportPx: VIEWPORT_PX },
      });
      if (i % 2 === 0) {
        // Tear down immediately — no rAF, no RO delivery yet (the
        // upstream tick().then teardown race).
        unmount(app);
        host.remove();
        continue;
      }
      const scrollEl = host.querySelector('[data-testid="virt-scroll"]') as HTMLElement;
      await waitFor(() => scrollEl.querySelectorAll('[data-row-index]').length > 0, 'rows');
      scrollEl.scrollTop = 0;
      app.resizeRow('row-19', 300);
      unmount(app);
      host.remove();
    }
    // Reaching here without an unhandled error is the assertion; give any
    // stray timers/observers a beat to surface.
    await raf();
    await raf();
  });
});
