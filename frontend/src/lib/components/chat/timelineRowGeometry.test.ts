import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createTimelineRowGeometryReservation,
  observeScrollSurfaceContentWidth,
  ROW_GEOMETRY_CONTENT_ATTR,
  type TimelineRowGeometryCache,
  type TimelineRowGeometryReservationParams,
} from './timelineRowGeometry';

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
    expect(cache.rememberedHeights()).toEqual([96]);

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
    const cache = makeCache([[key, 235]]);
    const action = createTimelineRowGeometryReservation(cache);
    const handle = action(row, key);

    observer().trigger(content, 169, 800);
    vi.advanceTimersByTime(750);

    expect(row.style.minHeight).toBe('');
    expect(cache.rememberedHeights()).toEqual([169]);

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
    expect(row.style.minHeight).toBe('250px');
    expect(releaseArms()).toBe(2);

    handle?.destroy?.();
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
    heights.set(cacheKey(key), height);
  }
  return {
    cachedTimelineRowHeight(key) {
      return heights.get(cacheKey(key));
    },
    rememberTimelineRowHeight(key, height) {
      remembered.push(height);
      heights.set(cacheKey(key), height);
    },
    rememberedHeights() {
      return remembered;
    },
  };
}

function cacheKey(key: TimelineRowGeometryReservationParams): string {
  return JSON.stringify([key.key, key.signature, key.width]);
}

function observer(): FireableResizeObserver {
  const instance = FireableResizeObserver.instances.at(-1);
  if (!instance) throw new Error('ResizeObserver was not created');
  return instance;
}
