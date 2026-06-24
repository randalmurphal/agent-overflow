import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createTimelineRowGeometryReservation,
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
