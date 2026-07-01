import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createTimelineRowGeometryReservation,
  observeScrollSurfaceContentWidth,
  ROW_GEOMETRY_CONTENT_ATTR,
  type TimelineRowGeometryCache,
  type TimelineRowGeometryReservationParams,
  type TimelineRowGeometryTraceEvent,
} from './timelineRowGeometry';
// The production cache-key builder keys the fake cache below so it cannot
// drift from real cache semantics.
import { timelineRowGeometryCacheKey } from '../../stores/threadRowUiState.svelte';

class FireableResizeObserver {
  static instances: FireableResizeObserver[] = [];

  readonly observed = new Set<Element>();

  constructor(private readonly callback: ResizeObserverCallback) {
    FireableResizeObserver.instances.push(this);
  }

  observe(target: Element): void {
    this.observed.add(target);
  }

  unobserve(target: Element): void {
    this.observed.delete(target);
  }

  disconnect(): void {
    this.observed.clear();
  }

  trigger(target: Element, height: number, width: number): void {
    if (!this.observed.has(target)) {
      throw new Error('target is not observed');
    }
    this.callback([
      {
        target,
        contentRect: { width, height },
      } as ResizeObserverEntry,
    ], this as unknown as ResizeObserver);
  }
}

describe('timeline row geometry reservation', () => {
  let originalResizeObserver: typeof ResizeObserver | undefined;

  beforeEach(() => {
    originalResizeObserver = globalThis.ResizeObserver;
    FireableResizeObserver.instances = [];
    globalThis.ResizeObserver = FireableResizeObserver as unknown as typeof ResizeObserver;
  });

  afterEach(() => {
    vi.useRealTimers();
    document.body.innerHTML = '';
    if (originalResizeObserver) {
      globalThis.ResizeObserver = originalResizeObserver;
    } else {
      Reflect.deleteProperty(globalThis, 'ResizeObserver');
    }
    FireableResizeObserver.instances = [];
  });

  it('remembers measured row height when there is no cached reservation', () => {
    const { row, content } = makeRow();
    const cache = makeCache();
    const action = createTimelineRowGeometryReservation(cache);
    const handle = action(row, rowKey());

    observer().trigger(content, 95.6, 800);

    expect(row.style.minHeight).toBe('');
    // Exact fractional height, NOT Math.round(95.6) === 96: the cache backs
    // the remount min-height floor, and a rounded floor releases with a
    // ±0.5px residue per row — the settle-flicker amplifier
    // (docs/architecture/settle-flicker-analysis.md).
    expect(cache.rememberedHeights()).toEqual([95.6]);

    handle?.destroy?.();
  });

  it('remembers a height under the measured width, not the laggy param width', () => {
    const { row, content } = makeRow();
    const cache = makeCache();
    const action = createTimelineRowGeometryReservation(cache);
    // params.width is the stale wide value the surface RO has not yet caught
    // up from (column reflowed 1137 -> 879); the row already reflowed and was
    // measured tall at the narrow width.
    const params: TimelineRowGeometryReservationParams = {
      key: 'l:thread-a:item-a',
      signature: 'signature-a',
      width: 1137,
      ownerItemIds: ['item-a'],
    };
    const handle = action(row, params);

    observer().trigger(content, 875, 879);

    // The tall narrow-layout height belongs to the width it was measured at
    // (879), never the laggy wide param width (1137) — otherwise a remount at
    // 1137 reserves 875 and strands the timeline above the composer.
    expect(cache.cachedTimelineRowHeight({ ...params, width: 879 })).toBe(875);
    expect(cache.cachedTimelineRowHeight({ ...params, width: 1137 })).toBeUndefined();

    handle?.destroy?.();
  });

  it('holds a cached height through transient smaller remount measurements', () => {
    vi.useFakeTimers();
    const { row, content } = makeRow();
    const key = rowKey();
    const cache = makeCache([[key, 235]]);
    const action = createTimelineRowGeometryReservation(cache);
    const handle = action(row, key);

    expect(row.style.minHeight).toBe('235px');

    observer().trigger(content, 169, 800);

    expect(row.style.minHeight).toBe('235px');
    expect(cache.rememberedHeights()).toEqual([]);

    observer().trigger(content, 235, 800);

    expect(row.style.minHeight).toBe('');
    expect(cache.rememberedHeights()).toEqual([235]);

    handle?.destroy?.();
  });

  it('releases a stale reservation if the row never returns to the cached height', () => {
    vi.useFakeTimers();
    const { row, content } = makeRow();
    const key = rowKey();
    const changedKey = { ...rowKey(), signature: 'signature-b' };
    const cache = makeCache([[key, 235], [changedKey, 250]]);
    const action = createTimelineRowGeometryReservation(cache);
    const handle = action(row, key);

    observer().trigger(content, 169, 800);
    vi.advanceTimersByTime(750);

    expect(row.style.minHeight).toBe('');
    expect(cache.rememberedHeights()).toEqual([169]);

    // The stale-timer release routes through rememberMeasuredHeight (committing
    // 169, below the 235 floor), which settles the row. A later signature churn
    // must therefore NOT re-floor it — the settled-height gate engages via the
    // release path, not only the normal at/above-floor settle.
    handle?.update?.(changedKey);
    expect(row.style.minHeight).toBe('');

    handle?.destroy?.();
  });

  it('skips re-reservation when update() receives value-equal params (raw fast path)', () => {
    vi.useFakeTimers();
    const { row } = makeRow();
    const key = rowKey();
    const cache = makeCache([[key, 235]]);
    const action = createTimelineRowGeometryReservation(cache);
    const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout');
    // Count only the reservation's 750ms stale-release timer, ignoring any
    // unrelated setTimeout traffic from the environment.
    const releaseArms = () => setTimeoutSpy.mock.calls.filter((call) => call[1] === 750).length;

    const handle = action(row, key);
    expect(releaseArms()).toBe(1); // first apply armed the stale-release timer
    expect(row.style.minHeight).toBe('235px');

    // The common per-render case: a fresh object with identical values.
    handle?.update?.({ ...rowKey() });

    expect(releaseArms()).toBe(1); // bailed before normalize — no re-arm
    expect(row.style.minHeight).toBe('235px');

    handle?.destroy?.();
  });

  it('treats a normalization-only update() difference as unchanged (post-normalize fallback)', () => {
    vi.useFakeTimers();
    const { row } = makeRow();
    const key = rowKey(); // key:'l:thread-a:item-a', signature:'signature-a', width:800, owners:['item-a']
    const cache = makeCache([[key, 235]]);
    const action = createTimelineRowGeometryReservation(cache);
    const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout');
    const releaseArms = () => setTimeoutSpy.mock.calls.filter((call) => call[1] === 750).length;

    const handle = action(row, key);
    expect(releaseArms()).toBe(1);

    // Raw differs (whitespace key, fractional width, duplicate owner) so the raw
    // fast path misses — but it normalizes to exactly `key`, so the
    // post-normalize fallback must still bail.
    handle?.update?.({
      key: '  l:thread-a:item-a  ',
      signature: 'signature-a',
      width: 800.4,
      ownerItemIds: ['item-a', 'item-a'],
    });

    expect(releaseArms()).toBe(1); // fallback bailed — no re-reserve
    expect(row.style.minHeight).toBe('235px');

    handle?.destroy?.();
  });

  it('re-reserves on an update() whose signature genuinely changed', () => {
    vi.useFakeTimers();
    const { row } = makeRow();
    const key = rowKey();
    const changedKey = { ...rowKey(), signature: 'signature-b' };
    const cache = makeCache([[key, 235], [changedKey, 250]]);
    const action = createTimelineRowGeometryReservation(cache);
    const setTimeoutSpy = vi.spyOn(globalThis, 'setTimeout');
    const releaseArms = () => setTimeoutSpy.mock.calls.filter((call) => call[1] === 750).length;

    const handle = action(row, key);
    expect(row.style.minHeight).toBe('235px');
    expect(releaseArms()).toBe(1);

    handle?.update?.(changedKey);

    // Re-reserved under the new signature's cached height; timer cleared + re-armed.
    // (Pre-measure only — see the settled-row guard below: once the row has
    // rendered its natural height, a signature-changed update must NOT re-floor.)
    expect(row.style.minHeight).toBe('250px');
    expect(releaseArms()).toBe(2);

    handle?.destroy?.();
  });

  it('never re-floors a row after it has settled to a measured height', () => {
    // The reservation exists ONLY to bridge a cold mount until the row renders.
    // Once a row has been measured at its natural height, a later update() whose
    // signature changed — which happens during streaming as the shell signature
    // recomputes on each timelineRevision — must NOT re-write min-height.
    // Re-flooring a still-visible, already-settled row with the stale integer
    // cached height is the settle "twitch": a 2-6px content-box flutter.
    const { row, content } = makeRow();
    const key = rowKey();
    const changedKey = { ...rowKey(), signature: 'signature-b' };
    const cache = makeCache([[key, 235], [changedKey, 250]]);
    const action = createTimelineRowGeometryReservation(cache);

    const handle = action(row, key);
    // Cold-mount floor is written — this legit anti-collapse path is preserved.
    expect(row.style.minHeight).toBe('235px');

    // Row renders to its natural height and settles: the floor releases.
    observer().trigger(content, 235, 800);
    expect(row.style.minHeight).toBe('');

    // Streaming churns the shell signature. Without the settled-height gate this
    // re-reserved to '250px' (the twitch); with it the row is left untouched.
    handle?.update?.(changedKey);
    expect(row.style.minHeight).toBe('');

    handle?.destroy?.();
  });

  it('keeps the floor eligible through a hold, then gates once the row settles', () => {
    // The hold path (measured height below the reserved floor — image/markdown
    // still reflowing) must NOT mark the row settled: a signature churn during
    // the hold has to re-floor under the new signature, and only the final
    // at-natural-height measure may shut the gate. Guards a regression that sets
    // hasSettledHeight too early in handleMeasuredHeight, which would both drop
    // the re-floor AND strand a permanent floor — applyParams clears the release
    // timer before the gate, so an early-set flag returns with a live
    // reservedHeight and no timer left to release it.
    vi.useFakeTimers();
    const { row, content } = makeRow();
    const key = rowKey();
    const changedKey = { ...rowKey(), signature: 'signature-b' };
    const cache = makeCache([[key, 235], [changedKey, 250]]);
    const action = createTimelineRowGeometryReservation(cache);

    const handle = action(row, key);
    expect(row.style.minHeight).toBe('235px');

    // Hold: measured shorter than the floor. Floor held, flag stays false.
    observer().trigger(content, 169, 800);
    expect(row.style.minHeight).toBe('235px');

    // Signature churn mid-hold still re-floors (flag was not set during hold).
    handle?.update?.(changedKey);
    expect(row.style.minHeight).toBe('250px');

    // Row reaches its natural height and settles — now the gate engages.
    observer().trigger(content, 250, 800);
    expect(row.style.minHeight).toBe('');

    handle?.update?.(rowKey());
    expect(row.style.minHeight).toBe('');

    handle?.destroy?.();
  });

  it('re-arms the cold-mount floor when the content element is swapped', () => {
    // Companion to the no-re-floor guarantee: when a NEW content element is
    // bound under the same action, bindContentElement resets the gate so the
    // cold-mount floor works again across a content-element remount. The
    // update() must carry a changed signature, or applyParams' value-equal fast
    // path bails before the gate and the reset would be unobservable.
    vi.useFakeTimers();
    const { row, content } = makeRow();
    const key = rowKey();
    const changedKey = { ...rowKey(), signature: 'signature-b' };
    const cache = makeCache([[key, 235], [changedKey, 250]]);
    const action = createTimelineRowGeometryReservation(cache);

    const handle = action(row, key);
    observer().trigger(content, 235, 800); // settle → gate engaged
    expect(row.style.minHeight).toBe('');

    // Swap in a fresh content element (a content-element remount).
    content.remove();
    const nextContent = document.createElement('div');
    nextContent.setAttribute(ROW_GEOMETRY_CONTENT_ATTR, '');
    row.append(nextContent);

    // The rebind resets hasSettledHeight, so the changed signature re-floors.
    handle?.update?.(changedKey);
    expect(row.style.minHeight).toBe('250px');

    handle?.destroy?.();
  });

  describe('trace hook', () => {
    // The hook exists to make floor/release churn diagnosable from a
    // Ctrl+Shift+B capture (timeline.row.geometry events). These tests lock the
    // transition→event mapping so a capture's event stream can be trusted to
    // mirror the state machine exactly.

    it('reports every state transition across a churn cycle on one mount', () => {
      vi.useFakeTimers();
      const { row, content } = makeRow();
      const key = rowKey();
      const uncachedKey = { ...rowKey(), signature: 'signature-uncached' };
      const cache = makeCache([[key, 235]]);
      const events: TimelineRowGeometryTraceEvent[] = [];
      const action = createTimelineRowGeometryReservation(cache, (event) => events.push(event));

      const handle = action(row, key); // cache hit → floor
      observer().trigger(content, 169, 800); // below floor → held
      handle?.update?.(uncachedKey); // churn to an uncached signature → miss releases the held floor
      handle?.update?.(key); // churn back → re-floors (SAME mount: living-row churn cycle)
      observer().trigger(content, 235, 800); // natural height → settles, floor releases
      handle?.update?.(uncachedKey); // churn after settle → gate blocks
      handle?.update?.({ ...rowKey(), key: '   ' }); // unusable key → reservation dropped
      handle?.destroy?.();

      expect(events.map((event) => event.action)).toEqual([
        'reserve',
        'hold',
        'skip-no-cache',
        'reserve',
        'settle',
        'release-measured',
        'skip-settled',
        'release-null-params',
        'destroy',
      ]);
      expect(events[0]).toMatchObject({
        key: key.key,
        itemId: 'item-a',
        signature: 'signature-a',
        width: 800,
        cachedHeight: 235,
        reservedHeight: 0,
        measuredHeight: 0,
      });
      expect(events[1]).toMatchObject({ measuredHeight: 169, reservedHeight: 235 });
      // The release leg of the churn cycle: the miss reports what it let go of.
      expect(events[2]).toMatchObject({
        signature: 'signature-uncached',
        releasedHeight: 235,
        measuredHeight: 169,
      });
      expect(events[4]).toMatchObject({ measuredHeight: 235, width: 800 });
      expect(events[5]).toMatchObject({ releasedHeight: 235, measuredHeight: 235 });
      expect(events[6]).toMatchObject({ signature: 'signature-uncached', measuredHeight: 235 });
      expect(events[8]).toMatchObject({ settled: true, releasedHeight: 0 });
      // One living action instance: every event shares its mountSeq.
      expect(new Set(events.map((event) => event.mountSeq)).size).toBe(1);
    });

    it('reports the stale-timer release and discriminates remounts by mountSeq', () => {
      vi.useFakeTimers();
      const { row, content } = makeRow();
      const key = rowKey();
      const cache = makeCache([[key, 235]]);
      const events: TimelineRowGeometryTraceEvent[] = [];
      const action = createTimelineRowGeometryReservation(cache, (event) => events.push(event));

      const first = action(row, key);
      observer().trigger(content, 169, 800);
      vi.advanceTimersByTime(750); // backstop: release-stale, then the release path settles at 169
      first?.destroy?.();

      const { row: nextRow } = makeRow();
      const second = action(nextRow, key); // same key remounted → fresh action instance
      second?.destroy?.();

      expect(events.map((event) => event.action)).toEqual([
        'reserve',
        'hold',
        'release-stale',
        'settle',
        'destroy',
        'reserve',
        'destroy',
      ]);
      expect(events[2]).toMatchObject({ releasedHeight: 235, measuredHeight: 169 });
      // A remount is a NEW mountSeq under a repeated key — the trace's
      // remount-vs-living-row-churn discriminator.
      const firstSeq = events[0].mountSeq;
      const secondSeq = events[5].mountSeq;
      expect(secondSeq).toBeGreaterThan(firstSeq);
      expect(events[4]).toMatchObject({ mountSeq: firstSeq, settled: true });
      expect(events[6]).toMatchObject({ mountSeq: secondSeq, settled: false });
    });

    it('reports a content-element rebind as a gate re-open on the same mount', () => {
      vi.useFakeTimers();
      const { row, content } = makeRow();
      const key = rowKey();
      const changedKey = { ...rowKey(), signature: 'signature-b' };
      const cache = makeCache([[key, 235], [changedKey, 250]]);
      const events: TimelineRowGeometryTraceEvent[] = [];
      const action = createTimelineRowGeometryReservation(cache, (event) => events.push(event));

      const handle = action(row, key);
      observer().trigger(content, 235, 800); // settle → gate engaged

      content.remove();
      const nextContent = document.createElement('div');
      nextContent.setAttribute(ROW_GEOMETRY_CONTENT_ATTR, '');
      row.append(nextContent);
      handle?.update?.(changedKey); // rebind re-opens the gate → re-floor on the SAME mount

      expect(events.map((event) => event.action)).toEqual([
        'reserve',
        'settle',
        'release-measured',
        'rebind',
        'reserve',
      ]);
      // `settled: true` at rebind time is the tell: a settled row became
      // floor-eligible again without a remount.
      expect(events[3]).toMatchObject({ settled: true, key: key.key });
      expect(new Set(events.map((event) => event.mountSeq)).size).toBe(1);

      handle?.destroy?.();
    });
  });

  describe('observeScrollSurfaceContentWidth', () => {
    it('reports the content-box width and never makes a synchronous layout read', () => {
      const surface = document.createElement('div');
      document.body.append(surface);
      // Border-box (getBoundingClientRect) deliberately disagrees with the
      // content-box (ResizeObserver) width by the scrollbar gutter. If the
      // observer ever consulted a layout query, the width would flip between
      // the two sources and oscillate — the feedback loop this guards against.
      const getRect = vi.fn(() => ({ width: 815 }) as DOMRect);
      Object.defineProperty(surface, 'getBoundingClientRect', {
        configurable: true,
        value: getRect,
      });

      const widths: number[] = [];
      const stop = observeScrollSurfaceContentWidth(surface, (width) => widths.push(width));

      // Nothing is reported synchronously, and no layout query is made.
      expect(widths).toEqual([]);
      expect(getRect).not.toHaveBeenCalled();

      observer().trigger(surface, 600, 800);

      expect(widths).toEqual([800]);
      expect(getRect).not.toHaveBeenCalled();

      stop();
    });

    it('observes the surface once and disconnects on cleanup', () => {
      const surface = document.createElement('div');
      const stop = observeScrollSurfaceContentWidth(surface, () => {});

      const ro = observer();
      expect(FireableResizeObserver.instances).toHaveLength(1); // exactly one observer created
      expect(ro.observed.has(surface)).toBe(true);

      stop();
      expect(ro.observed.size).toBe(0);
    });

    it('rounds and clamps the measured width', () => {
      const surface = document.createElement('div');
      const widths: number[] = [];
      const stop = observeScrollSurfaceContentWidth(surface, (width) => widths.push(width));

      observer().trigger(surface, 100, 784.6);
      observer().trigger(surface, 100, -5);
      observer().trigger(surface, 100, Number.NaN); // non-finite is ignored, not pushed

      expect(widths).toEqual([785, 0]);

      stop();
    });

    it('returns a no-op cleanup when ResizeObserver is unavailable', () => {
      const saved = globalThis.ResizeObserver;
      Reflect.deleteProperty(globalThis, 'ResizeObserver');
      try {
        const widths: number[] = [];
        const before = FireableResizeObserver.instances.length;
        const stop = observeScrollSurfaceContentWidth(
          document.createElement('div'),
          (width) => widths.push(width),
        );
        stop();

        expect(FireableResizeObserver.instances.length).toBe(before);
        expect(widths).toEqual([]);
      } finally {
        globalThis.ResizeObserver = saved;
      }
    });
  });
});

function makeRow(): {
  row: HTMLElement;
  content: HTMLElement;
} {
  const row = document.createElement('div');
  const content = document.createElement('div');
  content.setAttribute(ROW_GEOMETRY_CONTENT_ATTR, '');
  row.append(content);
  document.body.append(row);
  return { row, content };
}

function rowKey(): TimelineRowGeometryReservationParams {
  return {
    key: 'l:thread-a:item-a',
    signature: 'signature-a',
    width: 800,
    ownerItemIds: ['item-a'],
  };
}

function makeCache(
  entries: Array<[TimelineRowGeometryReservationParams, number]> = [],
): TimelineRowGeometryCache & { rememberedHeights(): number[] } {
  const heights = new Map<string, number>();
  const remembered: number[] = [];
  for (const [key, height] of entries) {
    heights.set(timelineRowGeometryCacheKey(key), height);
  }
  return {
    cachedTimelineRowHeight(key) {
      return heights.get(timelineRowGeometryCacheKey(key));
    },
    rememberTimelineRowHeight(key, height) {
      remembered.push(height);
      heights.set(timelineRowGeometryCacheKey(key), height);
    },
    rememberedHeights() {
      return remembered;
    },
  };
}

function observer(): FireableResizeObserver {
  const instance = FireableResizeObserver.instances.at(-1);
  if (!instance) throw new Error('ResizeObserver was not created');
  return instance;
}
