import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  IMAGE_TIER_MEMO_MAX_ENTRIES,
  IMAGE_TIER_WIDTHS,
  fullSizeMaxWidth,
  imageBoxContainer,
  __resetImageTiersForTest,
  imageTierFor,
  isHigherImageTier,
  lastImageTier,
  observeImageBox,
  rememberImageTier,
} from './imageTiers';
import { SRC_ROOT } from '../../test/sourceScan';
import { setCompactLayoutForTest } from '../stores/layoutMode.svelte';

class FakeResizeObserver {
  static instances: FakeResizeObserver[] = [];
  readonly observed = new Set<Element>();
  readonly observeCalls: Element[] = [];
  readonly unobserveCalls: Element[] = [];

  constructor(private readonly callback: ResizeObserverCallback) {
    FakeResizeObserver.instances.push(this);
  }

  observe(element: Element): void {
    this.observeCalls.push(element);
    this.observed.add(element);
  }

  unobserve(element: Element): void {
    this.unobserveCalls.push(element);
    this.observed.delete(element);
  }

  disconnect(): void {
    this.observed.clear();
  }

  /** Report `width` for each element still observed, in one delivery. */
  report(...entries: Array<[Element, number]>): void {
    const delivered = entries
      .filter(([element]) => this.observed.has(element))
      .map(([target, width]) => ({ target, contentRect: { width } }) as unknown as ResizeObserverEntry);
    this.callback(delivered, this as unknown as ResizeObserver);
  }
}

interface FakeQuery {
  media: string;
  listeners: Set<EventListener>;
}

let queries: FakeQuery[] = [];

function liveQuery(): FakeQuery {
  const live = queries.filter((query) => query.listeners.size > 0);
  expect(live).toHaveLength(1);
  return live[0]!;
}

function changeDevicePixelRatio(ratio: number): void {
  const query = liveQuery();
  vi.stubGlobal('devicePixelRatio', ratio);
  for (const listener of [...query.listeners]) listener(new Event('change'));
}

function observer(): FakeResizeObserver {
  expect(FakeResizeObserver.instances).toHaveLength(1);
  return FakeResizeObserver.instances[0]!;
}

beforeEach(() => {
  FakeResizeObserver.instances = [];
  queries = [];
  vi.stubGlobal('ResizeObserver', FakeResizeObserver);
  vi.stubGlobal('devicePixelRatio', 1);
  vi.stubGlobal('matchMedia', (media: string) => {
    const query: FakeQuery = { media, listeners: new Set() };
    queries.push(query);
    return {
      media,
      addEventListener: (_type: string, listener: EventListener) => query.listeners.add(listener),
      removeEventListener: (_type: string, listener: EventListener) => query.listeners.delete(listener),
    };
  });
});

afterEach(() => {
  __resetImageTiersForTest();
  vi.unstubAllGlobals();
});

describe('the tier ladder', () => {
  it('is the backend ladder, literally', () => {
    expect(IMAGE_TIER_WIDTHS).toEqual([320, 480, 720, 1080, 1440, 2160, 2880, 3840, 5120]);
    // The other copy: a change to either side fails here until both agree.
    const derive = readFileSync(resolve(SRC_ROOT, '..', '..', 'internal', 'attachment', 'derive.go'), 'utf8');
    const literal = /var DeriveWidths = \[\]int\{([^}]*)\}/.exec(derive)?.[1];
    expect(literal?.split(',').map((width) => Number(width.trim()))).toEqual([...IMAGE_TIER_WIDTHS]);
  });

  it('rounds the device width up to the next tier', () => {
    expect(imageTierFor(300, 1)).toBe(320);
    expect(imageTierFor(320, 1)).toBe(320);
    expect(imageTierFor(321, 1)).toBe(480);
    expect(imageTierFor(360, 2)).toBe(720);
    expect(imageTierFor(360.5, 2)).toBe(1080);
    expect(imageTierFor(576, 1.25)).toBe(720);
    expect(imageTierFor(5120, 1)).toBe(5120);
  });

  it('answers the original above the ladder and for a width that is not positive', () => {
    expect(imageTierFor(5121, 1)).toBe(0);
    expect(imageTierFor(3000, 2)).toBe(0);
    expect(imageTierFor(0, 1)).toBe(0);
    expect(imageTierFor(-4, 1)).toBe(0);
    expect(imageTierFor(Number.NaN, 1)).toBe(0);
  });

  it('treats a missing pixel ratio as 1', () => {
    expect(imageTierFor(300, 0)).toBe(320);
    expect(imageTierFor(300, Number.NaN)).toBe(320);
  });

  it('orders the original above every width', () => {
    expect(isHigherImageTier(1080, 720)).toBe(true);
    expect(isHigherImageTier(720, 1080)).toBe(false);
    expect(isHigherImageTier(720, 720)).toBe(false);
    expect(isHigherImageTier(0, 5120)).toBe(true);
    expect(isHigherImageTier(5120, 0)).toBe(false);
    expect(isHigherImageTier(0, 0)).toBe(false);
  });
});

describe('the shared box observer', () => {
  it('uses one observer for every image and dispatches each element to its own callbacks', () => {
    const paragraph = document.createElement('p');
    const cell = document.createElement('td');
    const first = vi.fn();
    const second = vi.fn();
    const third = vi.fn();
    observeImageBox(paragraph, first);
    observeImageBox(paragraph, second);
    observeImageBox(cell, third);

    const shared = observer();
    // One observation per element, however many images sit in it.
    expect(shared.observeCalls).toEqual([paragraph, cell]);
    shared.report([paragraph, 640], [cell, 200]);
    expect(first).toHaveBeenCalledWith(640);
    expect(second).toHaveBeenCalledWith(640);
    expect(third).toHaveBeenCalledWith(200);
    expect(first).toHaveBeenCalledTimes(1);
    expect(third).toHaveBeenCalledTimes(1);
  });

  it('gives a callback added to a measured element that width at once', () => {
    const paragraph = document.createElement('p');
    observeImageBox(paragraph, () => {});
    observer().report([paragraph, 512]);
    const late = vi.fn();
    observeImageBox(paragraph, late);
    expect(late).toHaveBeenCalledWith(512);
    // An element not yet measured has nothing to give.
    const fresh = vi.fn();
    observeImageBox(document.createElement('p'), fresh);
    expect(fresh).not.toHaveBeenCalled();
  });

  it('keeps observing an element until its last callback stops', () => {
    const paragraph = document.createElement('p');
    const first = vi.fn();
    const second = vi.fn();
    const stopFirst = observeImageBox(paragraph, first);
    const stopSecond = observeImageBox(paragraph, second);
    const shared = observer();

    stopFirst();
    stopFirst();
    expect(shared.unobserveCalls).toEqual([]);
    shared.report([paragraph, 300]);
    expect(first).not.toHaveBeenCalled();
    expect(second).toHaveBeenCalledWith(300);

    stopSecond();
    expect(shared.unobserveCalls).toEqual([paragraph]);
    shared.report([paragraph, 900]);
    expect(second).toHaveBeenCalledTimes(1);
  });

  it('re-dispatches every measured width when the device pixel ratio changes, and re-arms', () => {
    const paragraph = document.createElement('p');
    const cell = document.createElement('td');
    const unmeasured = document.createElement('li');
    const inParagraph = vi.fn();
    const inCell = vi.fn();
    const inList = vi.fn();
    observeImageBox(paragraph, inParagraph);
    observeImageBox(cell, inCell);
    observeImageBox(unmeasured, inList);
    observer().report([paragraph, 640], [cell, 200]);
    expect(liveQuery().media).toBe('(resolution: 1dppx)');

    changeDevicePixelRatio(2);
    expect(inParagraph).toHaveBeenLastCalledWith(640);
    expect(inParagraph).toHaveBeenCalledTimes(2);
    expect(inCell).toHaveBeenCalledTimes(2);
    expect(inList).not.toHaveBeenCalled();
    // The old query is released and one for the new ratio is listening.
    expect(liveQuery().media).toBe('(resolution: 2dppx)');

    changeDevicePixelRatio(1.5);
    expect(inParagraph).toHaveBeenCalledTimes(3);
    expect(liveQuery().media).toBe('(resolution: 1.5dppx)');
  });

  it('stops listening for the pixel ratio when nothing is observed', () => {
    const stop = observeImageBox(document.createElement('p'), () => {});
    expect(liveQuery()).toBeDefined();
    stop();
    expect(queries.every((query) => query.listeners.size === 0)).toBe(true);
    observeImageBox(document.createElement('p'), () => {});
    expect(liveQuery()).toBeDefined();
  });
});

describe('the container an image is measured by', () => {
  it('is the nearest block around the host, past links, emphasis and wrapper spans', () => {
    const paragraph = document.createElement('p');
    paragraph.innerHTML = '<a href="x"><em><span class="contents"><span id="host"></span></span></em></a>';
    expect(imageBoxContainer(paragraph.querySelector('#host')!)).toBe(paragraph);

    const cell = document.createElement('td');
    cell.innerHTML = '<span id="host"></span>';
    expect(imageBoxContainer(cell.querySelector('#host')!)).toBe(cell);

    expect(imageBoxContainer(document.createElement('span'))).toBeNull();
  });
});

describe('the last-tier memo', () => {
  it('answers the most recent tier per image', () => {
    expect(lastImageTier('a')).toBeUndefined();
    rememberImageTier('a', 720);
    rememberImageTier('b', 0);
    expect(lastImageTier('a')).toBe(720);
    expect(lastImageTier('b')).toBe(0);
    rememberImageTier('a', 1440);
    expect(lastImageTier('a')).toBe(1440);
  });

  it('is bounded, dropping the least recently remembered', () => {
    rememberImageTier('first', 320);
    rememberImageTier('second', 320);
    for (let i = 0; i < IMAGE_TIER_MEMO_MAX_ENTRIES - 2; i += 1) rememberImageTier(`fill-${i}`, 480);
    // Touching `first` makes `second` the oldest.
    rememberImageTier('first', 720);
    rememberImageTier('overflow', 1080);
    expect(lastImageTier('second')).toBeUndefined();
    expect(lastImageTier('first')).toBe(720);
    expect(lastImageTier('overflow')).toBe(1080);
  });
});

describe('fullSizeMaxWidth', () => {
  afterEach(() => setCompactLayoutForTest(false));

  it('is the original on a desktop layout and the top ladder tier on compact', () => {
    expect(fullSizeMaxWidth()).toBe(0);
    setCompactLayoutForTest(true);
    expect(fullSizeMaxWidth()).toBe(IMAGE_TIER_WIDTHS[IMAGE_TIER_WIDTHS.length - 1]);
    expect(fullSizeMaxWidth()).toBe(5120);
  });
});
