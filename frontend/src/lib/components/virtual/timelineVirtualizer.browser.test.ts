// Real-Chromium suite for TimelineVirtualizer + VirtualRow: real
// ResizeObserver timing, real scroll events, real layout. Exercises the
// adapter against a synthetic fixture (TimelineVirtualizerHarness) — the
// MessageTimeline integration is covered by the engine-agnostic outcome
// suites at cutover (plan §4 V1/V2).

import { afterEach, describe, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import TimelineVirtualizerHarness, { type HarnessRow } from './TimelineVirtualizerHarness.svelte';
import type { ContentGeometrySample, EngineCompensation, RowEstimate } from '../../utils/virtual/types';
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

function waitMs(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

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
    estimate: RowEstimate;
    onscroll: (offset: number) => void;
    onscrollend: () => void;
    onCompensation: (compensation: EngineCompensation) => void;
    onContentGeometry: (sample: ContentGeometrySample) => void;
    trackReadingAnchor: () => boolean;
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

describe('straddling-row attribution (reading anchor)', () => {
  // A tall row spanning the viewport top is the one row whole-row
  // [index, height] can't classify: growth in its off-screen-above part
  // shifts everything visible down; growth below the top is ordinary
  // reflow. The adapter measures the split against a hit-tested anchor and
  // the engine folds the bounded result into the same compensation.
  //
  // Fixture: 10 short rows (1000px), then one 1200px row split into a
  // 400px head and an 800px body, then more short rows. At scrollTop 1500
  // the tall row spans [1000, 2200) and the viewport top sits 500px into
  // it — inside the BODY, 100px past the head. Growing the head therefore
  // pushes the reading position down by exactly the head's growth.
  const SHORT_PX = 100;
  const TALL_PX = 1200;
  const HEAD_PX = 400;
  const TALL_INDEX = 10;
  const STRADDLE_SCROLL_TOP = 1500;

  function straddleRows(): HarnessRow[] {
    return [
      ...Array.from({ length: TALL_INDEX }, (_, i) => ({
        id: `row-${i}`,
        heightPx: SHORT_PX,
        label: `Row ${i}`,
      })),
      { id: 'tall', heightPx: TALL_PX, headPx: HEAD_PX, label: 'Tall' },
      ...Array.from({ length: 20 }, (_, i) => ({
        id: `row-${TALL_INDEX + 1 + i}`,
        heightPx: SHORT_PX,
        label: `Row ${TALL_INDEX + 1 + i}`,
      })),
    ];
  }

  async function mountStraddled(trackReadingAnchor?: () => boolean) {
    const compensations: EngineCompensation[] = [];
    const { harness, scrollEl } = mountHarness({
      initialRows: straddleRows(),
      onCompensation: (c) => compensations.push(c),
      trackReadingAnchor,
    });
    await waitForStableGeometry(scrollEl, 'mount');
    // The engine tail-seeds until a real scroll input lands, and mount
    // leaves the DOM at scrollTop 0 — so assigning 0 fires no scroll event
    // and the window would stay at the tail. Pin to the bottom first (same
    // note as the head-splice test), then walk back up.
    await pinToBottomAndSettle(scrollEl, 'bottom settle');
    scrollEl.scrollTop = 0;
    await waitFor(() => rowEl(scrollEl, 'row-0') !== null, 'top rows');
    await waitForStableGeometry(scrollEl, 'top settle');
    scrollEl.scrollTop = STRADDLE_SCROLL_TOP;
    await waitFor(() => rowEl(scrollEl, 'tall') !== null, 'tall row to mount');
    await waitForStableGeometry(scrollEl, 'straddle settle');
    // Sanity: the fixture really does straddle the viewport top.
    const handle = harness.handle()!;
    const top = handle.getItemOffset(TALL_INDEX);
    expect(top).toBeLessThan(scrollEl.scrollTop);
    expect(top + TALL_PX).toBeGreaterThan(scrollEl.scrollTop);
    compensations.length = 0;
    return { harness, scrollEl, compensations };
  }

  it('compensates growth that landed above the reading position', async () => {
    const { harness, scrollEl, compensations } = await mountStraddled();
    const scrollTopBefore = scrollEl.scrollTop;

    harness.growRowHead('tall', 60);
    await waitFor(() => compensations.length > 0, 'straddle compensation');

    const total = compensations.reduce((sum, c) => sum + c.delta, 0);
    expect(total).toBe(60);
    // Still an observation, never a write — the adapter's ownership
    // contract is unchanged by sub-row attribution.
    expect(scrollEl.scrollTop).toBe(scrollTopBefore);
  });

  it('compensates nothing when the growth landed below the reading position', async () => {
    const { harness, scrollEl, compensations } = await mountStraddled();

    // Grow the row's TAIL instead: total height up, head unchanged, so
    // nothing above the reading position moved.
    harness.resizeRow('tall', TALL_PX + 60);
    await waitForStableGeometry(scrollEl, 'tail growth settle');

    expect(compensations.reduce((sum, c) => sum + c.delta, 0)).toBe(0);
  });

  it('reports nothing while the viewport top is not a reading position', async () => {
    // trackReadingAnchor false models the controller holding bottom-follow
    // intent: the per-beat pin write already absorbs the growth, so no
    // anchor is sampled and the engine falls back to attributing nothing.
    const { harness, compensations } = await mountStraddled(() => false);

    harness.growRowHead('tall', 60);
    await waitFor(() => compensations.length > 0 || true, 'settle');
    await raf();
    await raf();

    expect(compensations.reduce((sum, c) => sum + c.delta, 0)).toBe(0);
  });

  it('drops a live anchor the moment bottom-follow intent is regained', async () => {
    // Transition, not state: bottom-follow can be regained with no scroll
    // event and no measurement pass (markAtBottom, forceStick, the
    // resolver's setIsAtBottom), so an anchor sampled while scrolled up is
    // still armed at the flip. If it survived, its correction would land on
    // top of the pin write as a double move.
    let tracking = true;
    const { harness, compensations } = await mountStraddled(() => tracking);

    tracking = false;
    harness.growRowHead('tall', 60);
    await raf();
    await raf();
    await raf();

    expect(compensations.reduce((sum, c) => sum + c.delta, 0)).toBe(0);
  });

  it('re-anchors between passes so successive growths each compensate once', async () => {
    const { harness, scrollEl, compensations } = await mountStraddled();
    const scrollTopBefore = scrollEl.scrollTop;

    for (const by of [40, 30, 50]) {
      compensations.length = 0;
      harness.growRowHead('tall', by);
      await waitFor(() => compensations.length > 0, `growth of ${by}`);
      await waitForStableGeometry(scrollEl, `settle after ${by}`);
      expect(compensations.reduce((sum, c) => sum + c.delta, 0)).toBe(by);
    }
    // The anchor is re-sampled post-flush, so nothing accumulated across
    // passes and the adapter still never wrote.
    expect(scrollEl.scrollTop).toBe(scrollTopBefore);
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

describe('content geometry samples (engine-sourced contentRO replacement)', () => {
  // The adapter is the scroll controller's content-geometry source under
  // `externalContentGeometry`: `onContentGeometry` samples replace the
  // controller's contentEl ResizeObserver. These pin the source contract:
  // post-flush delivery (sample.height already in the DOM), change-only
  // delivery, per-row settle evidence, and width sampling from the
  // scroller's RO entry.
  interface GeometryCapture {
    sample: ContentGeometrySample;
    spacerHeight: string;
    scrollHeight: number;
  }

  function exactEstimate(): RowEstimate {
    // Models a priors-hit revisit: every row's estimate matches its real
    // rendered height, so first measurements land with zero correction.
    return { at: () => ROW_PX };
  }

  it('delivers post-flush (height already in the DOM) and only on change', async () => {
    const captures: GeometryCapture[] = [];
    const ctx = mountHarness({
      onContentGeometry: (sample) => {
        captures.push({
          sample,
          spacerHeight: (ctx.scrollEl.firstElementChild as HTMLElement).style.height,
          scrollHeight: ctx.scrollEl.scrollHeight,
        });
      },
    });
    const { scrollEl } = ctx;
    await waitForStableGeometry(scrollEl, 'mount');
    await pinToBottomAndSettle(scrollEl, 'bottom settle');
    await waitFor(() => captures.length > 0, 'geometry samples');

    // Post-flush contract: at delivery time the spacer style already
    // carries the reported height — the same timing argument as
    // compensation delivery (a pre-flush sample would make the controller
    // pin against a stale bottom target).
    for (const capture of captures) {
      expect(
        capture.spacerHeight,
        'sample delivered before the spacer updated',
      ).toBe(`${capture.sample.height}px`);
    }

    // Once the window is fully measured, the DOM's scrollHeight agrees
    // with the reported height exactly. (Mid-cascade samples are exempt:
    // an unmeasured row renders at its natural height and overflows its
    // estimate-derived offset until the engine records the measurement,
    // so scrollHeight can transiently exceed the spacer.)
    const measured = captures.filter((capture) => capture.sample.windowMeasured);
    expect(measured.length).toBeGreaterThan(0);
    for (const capture of measured) {
      expect(capture.scrollHeight).toBe(Math.round(capture.sample.height));
    }

    // Change-only contract: a scroll within the settled, fully-measured
    // window changes no geometry and must deliver nothing (the controller
    // would otherwise process a delta-0 sample per scroll event).
    const settledCount = captures.length;
    scrollEl.scrollTop -= 10;
    await raf();
    await raf();
    expect(captures.length).toBe(settledCount);
  });

  it('reports zero corrections under exact estimates and the real error under flat ones', async () => {
    // Exact estimates (priors-hit shape): the window measures fully with
    // maxFirstMeasureCorrectionPx === 0 — the evidence the controller's
    // warm gate fast-paths on.
    const exact: ContentGeometrySample[] = [];
    const exactCtx = mountHarness({
      estimate: exactEstimate(),
      onContentGeometry: (sample) => exact.push(sample),
    });
    await waitForStableGeometry(exactCtx.scrollEl, 'exact-estimate mount');
    await waitFor(() => exact.at(-1)?.windowMeasured === true, 'exact window to measure');
    expect(exact.at(-1)!.maxFirstMeasureCorrectionPx).toBe(0);

    // Flat default estimate (cold-mount shape): rows render at ROW_PX but
    // were placed at ESTIMATE_PX, so the evidence carries the real error
    // and the fast-path can never fire.
    const flat: ContentGeometrySample[] = [];
    const flatCtx = mountHarness({
      onContentGeometry: (sample) => flat.push(sample),
    });
    await waitForStableGeometry(flatCtx.scrollEl, 'flat-estimate mount');
    await waitFor(() => flat.at(-1)?.windowMeasured === true, 'flat window to measure');
    expect(flat.at(-1)!.maxFirstMeasureCorrectionPx).toBe(ROW_PX - ESTIMATE_PX);

    // Mount-lifetime max: a later first measurement landing exactly on
    // its estimate (correction 0) must not lower the recorded max — a
    // mid-mount warm re-arm would otherwise fast-path off evidence that
    // predates the cascade.
    const heightBefore = flat.at(-1)!.height;
    flatCtx.harness.setRows([
      ...flatCtx.harness.getRows(),
      { id: 'row-extra', heightPx: ESTIMATE_PX, label: 'Row extra' },
    ]);
    await waitFor(
      () => flat.at(-1)!.height > heightBefore && flat.at(-1)!.windowMeasured,
      'appended row to measure',
    );
    expect(flat.at(-1)!.maxFirstMeasureCorrectionPx).toBe(ROW_PX - ESTIMATE_PX);
  });

  it('a scroller width change delivers a width-only sample at unchanged height', async () => {
    const captures: ContentGeometrySample[] = [];
    const ctx = mountHarness({
      onContentGeometry: (sample) => captures.push(sample),
    });
    const { host, scrollEl } = ctx;
    await waitForStableGeometry(scrollEl, 'mount');
    await pinToBottomAndSettle(scrollEl, 'bottom settle');
    await waitFor(() => captures.length > 0, 'geometry samples');

    const before = captures.at(-1)!;
    // Narrow the fixed host; the scroller (width: 100%) follows and its
    // RO entry carries the new content-box width. Rows are fixed-height,
    // so this is the width-only reflow shape the controller classifies
    // with (delta 0 + widthChanged → width-reflow settle window).
    (host.firstElementChild as HTMLElement).style.width = '640px';
    await waitFor(
      () => captures.length > 0 && captures.at(-1)!.width < before.width,
      'width sample',
    );
    const after = captures.at(-1)!;
    expect(after.height).toBe(before.height);
    expect(after.width).toBeLessThan(before.width);
  });
});

describe('row registration across head splices', () => {
  it('re-indexes mounted rows without re-observing them', async () => {
    const { harness, scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');
    const handle = harness.handle()!;

    const observeSpy = vi.spyOn(ResizeObserver.prototype, 'observe');
    const unobserveSpy = vi.spyOn(ResizeObserver.prototype, 'unobserve');
    try {
      // Prepend 3 rows far above the mounted tail window: every mounted
      // row's index shifts (+3) under identical item keys, so no row
      // remounts — and none may be re-observed, because per spec each
      // observe() schedules a fresh delivery and re-registering the
      // window would buy a spurious O(window) RO burst per prepend.
      const prepended: HarnessRow[] = Array.from({ length: 3 }, (_, i) => ({
        id: `older-${i}`,
        heightPx: ROW_PX,
        label: `Older ${i}`,
      }));
      harness.setRows([...prepended, ...harness.getRows()], { shift: true });
      await waitForStableGeometry(scrollEl, 'splice settle');
      expect(observeSpy).not.toHaveBeenCalled();
      expect(unobserveSpy).not.toHaveBeenCalled();
    } finally {
      observeSpy.mockRestore();
      unobserveSpy.mockRestore();
    }

    // Measurement bookkeeping follows the live index: the row that
    // mounted as index 50 is index 53 now, and its resize must record
    // there (a stale WeakMap entry would leave sizeAt(53) unchanged).
    harness.resizeRow('row-50', 150);
    await waitFor(() => handle.sizeAt(53) === 150, 'measured under live index');
    expect(handle.getItemOffset(54) - handle.getItemOffset(53)).toBe(150);
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

  it('a viewport taken over mid-convergence is never yanked back', async () => {
    // A pending index scroll outlives its first write by settle windows of
    // real time — the shape every bottom-held restore leaves behind. If the
    // reader (or the spring) moves the viewport inside that window, a later
    // engine update must not re-fire the stale absolute target over them:
    // that write was the release-then-glide "snaps mid-animation" bug.
    const { harness, scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');
    await pinToBottomAndSettle(scrollEl, 'bottom settle');
    const handle = harness.handle()!;

    handle.scrollToIndex(ROW_COUNT - 1, { align: 'end' });
    await raf();

    // Takeover: scroll away from where the navigation's write left things.
    const takeover = scrollEl.scrollTop - 350;
    scrollEl.scrollTop = takeover;
    await raf();

    // A mounted row above the new viewport re-measures, moving the stale
    // target — the late-typesetting shape that used to re-fire it.
    const grownIndex = mountedRowIndexes(scrollEl)[0];
    harness.resizeRow(`row-${grownIndex}`, ROW_PX + 200);
    await waitForStableGeometry(scrollEl, 'remeasure settle');

    expect(Math.abs(scrollEl.scrollTop - takeover)).toBeLessThanOrEqual(2);
  });

  it('keeps converging when compensation writes move the position on its behalf', async () => {
    // The production topology: a consumer (the controller) performs every
    // compensation write. Converging into unmeasured content triggers
    // above-viewport re-measures whose compensations move the position for
    // the navigation's own good — the takeover check must expect those
    // shifts, not read its own side's writes as a takeover and die early.
    const ctx = mountHarness({
      onCompensation: (c) => {
        ctx.scrollEl.scrollTop = c.target;
      },
    });
    const { harness, scrollEl } = ctx;
    await waitForStableGeometry(scrollEl, 'mount');
    const handle = harness.handle()!;

    handle.scrollToIndex(30, { align: 'start' });
    await waitForStableGeometry(scrollEl, 'index scroll settle');

    expect(Math.abs(scrollEl.scrollTop - handle.getItemOffset(30))).toBeLessThanOrEqual(2);
    const row = rowEl(scrollEl, 'row-30')!;
    expect(
      Math.abs(row.getBoundingClientRect().top - scrollEl.getBoundingClientRect().top),
    ).toBeLessThanOrEqual(2);
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

  it('a cancelled touch still releases the synthetic scrollend', async () => {
    // A system gesture / context menu ends a touch with touchcancel and
    // no touchend. A stuck `touching` flag would re-arm the debounce
    // forever: scrollend (snapshot save, row-UI-state prune) never fires.
    const onscrollend = vi.fn();
    const { scrollEl } = mountHarness({ onscrollend });
    await waitForStableGeometry(scrollEl, 'mount');
    onscrollend.mockClear();

    scrollEl.dispatchEvent(new TouchEvent('touchstart'));
    scrollEl.scrollTop = 1000;
    // Well past the 150ms debounce: the open touch holds scrollend.
    await waitMs(250);
    expect(onscrollend).not.toHaveBeenCalled();

    scrollEl.dispatchEvent(new TouchEvent('touchcancel'));
    await waitFor(() => onscrollend.mock.calls.length > 0, 'scrollend after touchcancel');
    expect(onscrollend).toHaveBeenCalledTimes(1);
  });

  it('a wheel event inside the continuation window holds the synthetic scrollend', async () => {
    // Wheel events landing 50–150ms after the last scroll event mean the
    // gesture is still alive through dropped frames; the debounce must
    // renew once instead of firing mid-gesture.
    const onscrollend = vi.fn();
    let scrolledAt = 0;
    const { scrollEl } = mountHarness({
      onscrollend,
      onscroll: () => {
        scrolledAt = performance.now();
      },
    });
    await waitForStableGeometry(scrollEl, 'mount');
    onscrollend.mockClear();

    scrolledAt = 0;
    scrollEl.scrollTop = 1000;
    await waitFor(() => scrolledAt > 0, 'scroll event');
    await waitFor(() => performance.now() - scrolledAt > 55, 'continuation window open');
    // Precondition, not an assertion of the code under test: the wheel
    // must land inside the 50–150ms window for this test to mean anything.
    expect(performance.now() - scrolledAt).toBeLessThan(150);
    scrollEl.dispatchEvent(new WheelEvent('wheel', { deltaY: 10 }));

    // Not delivered on the original 150ms schedule (the renewed window
    // cannot deliver before ~300ms after the scroll event)...
    await waitMs(120);
    expect(onscrollend).not.toHaveBeenCalled();
    // ...but the renewed window drains and delivers exactly once.
    await waitFor(() => onscrollend.mock.calls.length > 0, 'held scrollend');
    expect(onscrollend).toHaveBeenCalledTimes(1);
  });

  it('ctrl-wheel (pinch zoom) does not hold the synthetic scrollend', async () => {
    const onscrollend = vi.fn();
    let scrolledAt = 0;
    const { scrollEl } = mountHarness({
      onscrollend,
      onscroll: () => {
        scrolledAt = performance.now();
      },
    });
    await waitForStableGeometry(scrollEl, 'mount');
    onscrollend.mockClear();

    scrolledAt = 0;
    scrollEl.scrollTop = 1000;
    await waitFor(() => scrolledAt > 0, 'scroll event');
    await waitFor(() => performance.now() - scrolledAt > 55, 'continuation window open');
    expect(performance.now() - scrolledAt).toBeLessThan(150);
    scrollEl.dispatchEvent(new WheelEvent('wheel', { deltaY: 10, ctrlKey: true }));

    await waitFor(() => onscrollend.mock.calls.length > 0, 'scrollend on schedule');
    // Fired on the ORIGINAL 150ms schedule: a held scrollend could not
    // have arrived before ~300ms after the scroll event.
    expect(performance.now() - scrolledAt).toBeLessThan(280);
    expect(onscrollend).toHaveBeenCalledTimes(1);
  });
});

describe('revalidate', () => {
  it('counts as scroll input: a top-anchored surface un-blanks without a scroll event', async () => {
    // The engine tail-seeds its window until its first scroll input
    // (chat's bottom-anchored mount seeding). A top-anchored surface
    // (review pane) that mounts at scrollTop 0 fires no scroll event —
    // a 0 → 0 restore write doesn't either — so the window sits at the
    // tail while the viewport shows the top: blank until the user
    // scrolls. ReviewDiffBody calls revalidate() after position restore;
    // this pins that revalidate's applyScroll counts as that first input.
    const { harness, scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');
    // The bug shape: viewport at the top, window at the tail.
    expect(scrollEl.scrollTop).toBe(0);
    expect(rowEl(scrollEl, 'row-0')).toBeNull();

    harness.handle()!.revalidate();
    await waitFor(() => rowEl(scrollEl, 'row-0') !== null, 'top rows after revalidate');
    // The window moved to the true offset — the tail seed evicted.
    expect(rowEl(scrollEl, `row-${ROW_COUNT - 1}`)).toBeNull();
  });

  it('feeds the engine the content-box viewport (unit parity with the RO path)', async () => {
    const { harness, scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');
    const handle = harness.handle()!;
    expect(handle.getViewportSize()).toBe(VIEWPORT_PX);

    // Composer-clearance shape: padding-bottom on the scroller. The
    // content box is unchanged so the scroller RO stays silent —
    // revalidate()'s cold-path read is the only sampler, and it must
    // report the RO's unit (content-box), not clientHeight's padding box.
    scrollEl.style.paddingBottom = '160px';
    harness.handle()!.revalidate();
    expect(handle.getViewportSize()).toBe(VIEWPORT_PX);

    // Outcome: an align-end scroll after revalidate lands on the true
    // bottom (an inflated viewport lands short by the padding).
    handle.scrollToIndex(ROW_COUNT - 1, { align: 'end' });
    await waitForStableGeometry(scrollEl, 'align-end settle');
    expect(
      Math.abs(scrollEl.scrollTop - (scrollEl.scrollHeight - scrollEl.clientHeight)),
    ).toBeLessThanOrEqual(1);
  });
});

describe('hidden pane guard (display:none RO deliveries)', () => {
  it('zero-size deliveries from a hidden subtree never record', async () => {
    const { harness, host, scrollEl } = mountHarness();
    await waitForStableGeometry(scrollEl, 'mount');
    const handle = harness.handle()!;
    const totalBefore = handle.getTotalSize();
    const snapshotBefore = handle.takeSnapshot();
    expect(snapshotBefore.some((size) => size === 0)).toBe(false);

    // Hiding the pane makes the RO deliver 0×0 for the scroller and every
    // mounted row; without the offsetParent guard those would collapse
    // totalSize and poison the priors snapshot persisted for the thread.
    host.style.display = 'none';
    await raf();
    await raf();
    await raf();
    expect(handle.getTotalSize()).toBe(totalBefore);
    expect(handle.getViewportSize()).toBe(VIEWPORT_PX);
    expect(handle.takeSnapshot()).toEqual(snapshotBefore);

    host.style.display = '';
    await raf();
    await raf();
    expect(handle.getTotalSize()).toBe(totalBefore);
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
