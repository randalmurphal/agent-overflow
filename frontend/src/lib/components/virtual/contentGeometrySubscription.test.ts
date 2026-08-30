// The virtualizer's content-geometry SUBSCRIPTION seam
// (TimelineVirtualizerHandle.subscribeContentGeometry).
//
// The fire-and-forget geometry prop this seam REPLACED lost any sample
// published before its consumer could take it: dropped by the consumer
// AND then suppressed forever by the adapter's field-by-field dedupe,
// because the tuple never changed again. In chat that consumer is the
// scroll controller, and the lost sample was the first-fire bottom pin —
// a populated first mount rendered at scrollTop=0 while claiming the
// bottom. The prop is gone; the harness's `onContentGeometry` is itself
// a mount-time subscription now.
//
// The subscription is the repair: instance-bound, and it replays on
// subscribe, so a consumer can wire its own machinery up FIRST
// (`stick.attach`) and still get the sample this source already
// published. These tests drive the real component through a controllable
// ResizeObserver — happy-dom measures nothing, so the scroller's RO
// entry is synthesized, which is exactly the input the adapter's width /
// viewport bookkeeping reads.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import TimelineVirtualizerHarness, { type HarnessRow } from './TimelineVirtualizerHarness.svelte';
import type { ContentGeometrySample, RowEstimate } from '../../utils/virtual/types';
import {
  createUseStickToBottomController,
  type UseStickToBottomController,
} from '../../utils/scroll/index.svelte';
import { resetScrollIntentModuleStateForTest } from '../../utils/scroll/intent';
import { MockResizeObserver, stubGeometry, type Geometry } from '../../utils/scroll/testGeometry';

const ROW_PX = 100;
const ROW_COUNT = 10;
const VIEWPORT_PX = 600;
const CONTENT_WIDTH_PX = 800;
// 10 rows × 100px in a 600px viewport → the bottom target is 400.
const CONTENT_PX = ROW_PX * ROW_COUNT;
const BOTTOM_TARGET_PX = CONTENT_PX - VIEWPORT_PX;

function makeRows(count: number): HarnessRow[] {
  return Array.from({ length: count }, (_, i) => ({
    id: `row-${i}`,
    heightPx: ROW_PX,
    label: `Row ${i}`,
  }));
}

function mountHarness(
  target: HTMLElement,
  props: {
    initialRows: HarnessRow[];
    renderAll: boolean;
    estimate: RowEstimate;
    onContentGeometry: (sample: ContentGeometrySample) => void;
  },
) {
  return mount(TimelineVirtualizerHarness, { target, props });
}

/**
 * The harness instance's exported surface (`handle()`, `scroller()`,
 * `setRows()`), inferred from a real call. `mount` is generic over
 * <Props, Exports> rather than over the component type, so naming its
 * return type by hand does not type-check.
 */
type HarnessApp = ReturnType<typeof mountHarness>;

interface MountedSource {
  app: HarnessApp;
  host: HTMLDivElement;
  /** Samples taken by the consumer subscribed at mount (before the
   * first publish, so it never sees a replay). */
  propSamples: ContentGeometrySample[];
  /** Deliver the scroller's RO entry — the adapter's first-sample gate. */
  reportScrollerBox(height?: number, width?: number): Promise<void>;
}

describe('TimelineVirtualizer content-geometry subscription', () => {
  let originalRO: typeof ResizeObserver | undefined;
  const mounted: MountedSource[] = [];
  let controller: UseStickToBottomController | undefined;
  let controllerEls: { scrollEl: HTMLDivElement; contentEl: HTMLDivElement } | undefined;

  function mountSource(rows = makeRows(ROW_COUNT)): MountedSource {
    const host = document.createElement('div');
    document.body.appendChild(host);
    const propSamples: ContentGeometrySample[] = [];
    const app = mountHarness(host, {
      initialRows: rows,
      renderAll: true,
      estimate: { at: () => ROW_PX },
      onContentGeometry: (sample) => propSamples.push(sample),
    });
    const source: MountedSource = {
      app,
      host,
      propSamples,
      async reportScrollerBox(height = VIEWPORT_PX, width = CONTENT_WIDTH_PX): Promise<void> {
        await tick();
        const scroller = app.scroller();
        // The adapter's observer is lazy, and each instance owns exactly
        // one — find it by the scroller it registered.
        const observer = MockResizeObserver.instances.find((candidate) =>
          scroller ? candidate.observed.includes(scroller) : false,
        );
        if (!scroller || !observer) throw new Error('harness scroller / observer missing');
        observer.fire(scroller, height, width);
        await tick();
      },
    };
    mounted.push(source);
    return source;
  }

  /** A sticky controller over the same 1000/600 geometry the source reports. */
  function attachController(): { controller: UseStickToBottomController; geom: Geometry } {
    const scrollEl = document.createElement('div');
    const contentEl = document.createElement('div');
    scrollEl.appendChild(contentEl);
    document.body.appendChild(scrollEl);
    const geom: Geometry = {
      scrollHeight: CONTENT_PX,
      clientHeight: VIEWPORT_PX,
      scrollTop: 0,
      contentHeight: CONTENT_PX,
    };
    stubGeometry(scrollEl, contentEl, geom);
    const created = createUseStickToBottomController({ externalContentGeometry: true });
    created.attach(scrollEl, contentEl);
    controller = created;
    controllerEls = { scrollEl, contentEl };
    return { controller: created, geom };
  }

  beforeEach(() => {
    resetScrollIntentModuleStateForTest();
    MockResizeObserver.instances = [];
    originalRO = globalThis.ResizeObserver;
    (globalThis as unknown as { ResizeObserver: typeof MockResizeObserver }).ResizeObserver =
      MockResizeObserver;
  });

  afterEach(() => {
    controller?.detach();
    controller = undefined;
    controllerEls?.scrollEl.remove();
    controllerEls = undefined;
    for (const source of mounted.splice(0)) {
      unmount(source.app);
      source.host.remove();
    }
    if (originalRO) {
      (globalThis as unknown as { ResizeObserver: typeof ResizeObserver }).ResizeObserver = originalRO;
    }
  });

  it('replays the already-published sample to a subscriber that arrives after it', async () => {
    const source = mountSource();
    await source.reportScrollerBox();
    // Published once, to the consumer that was wired at mount. The
    // dedupe now holds this tuple: nothing will offer it again.
    expect(source.propSamples).toHaveLength(1);

    const received: ContentGeometrySample[] = [];
    source.app.handle()?.subscribeContentGeometry((sample) => received.push(sample));

    expect(received).toHaveLength(1);
    expect(received[0]).toMatchObject({
      height: CONTENT_PX,
      width: CONTENT_WIDTH_PX,
      viewportHeight: VIEWPORT_PX,
    });
    // Replay is for the new subscriber only — the earlier consumer must
    // not see the same tuple twice.
    expect(source.propSamples).toHaveLength(1);
  });

  it('lands a sticky controller on the true bottom from the replayed sample alone', async () => {
    // The deterministic repro: content is published at 1000px before the
    // controller can take it, the viewport is 600px, and the sticky
    // controller must end up at 400 rather than sitting at 0.
    const source = mountSource();
    await source.reportScrollerBox();
    const { controller: stick, geom } = attachController();
    expect(geom.scrollTop).toBe(0);

    source.app.handle()?.subscribeContentGeometry((sample) => stick.deliverContentGeometry(sample));

    expect(geom.scrollTop).toBe(BOTTOM_TARGET_PX);
    expect(stick.isAtBottom).toBe(true);
  });

  it('replays an identical tuple from a NEW source instance', async () => {
    // What the `{#key pane.threadId}` remount produces: a fresh
    // virtualizer whose geometry happens to match the old one's. Dedupe
    // is per instance, so identity — not the tuple — decides.
    const first = mountSource();
    await first.reportScrollerBox();
    const firstSeen: ContentGeometrySample[] = [];
    first.app.handle()?.subscribeContentGeometry((sample) => firstSeen.push(sample));
    expect(firstSeen).toHaveLength(1);

    const second = mountSource();
    await second.reportScrollerBox();
    const secondSeen: ContentGeometrySample[] = [];
    second.app.handle()?.subscribeContentGeometry((sample) => secondSeen.push(sample));

    expect(secondSeen).toHaveLength(1);
    expect(secondSeen[0]).toEqual(firstSeen[0]);
  });

  it('stops delivering to an unsubscribed consumer', async () => {
    const source = mountSource();
    await source.reportScrollerBox();
    const received: ContentGeometrySample[] = [];
    const unsubscribe = source.app.handle()?.subscribeContentGeometry((sample) =>
      received.push(sample),
    );
    expect(received).toHaveLength(1);

    unsubscribe?.();
    // A real geometry change: one more row of content.
    source.app.setRows(makeRows(ROW_COUNT + 1));
    await tick();
    await source.reportScrollerBox();

    expect(received).toHaveLength(1);
    // The mount-time subscriber is still wired, so the change did
    // publish — the unsubscribed one simply did not hear it.
    expect(source.propSamples.length).toBeGreaterThan(1);
    expect(source.propSamples.at(-1)?.height).toBe(CONTENT_PX + ROW_PX);
  });
});
