// Run-map follow controller (RUN-MAP.md §9).
//
// happy-dom lays nothing out, so the harness below states the geometry
// the controller reads back: a scroller box at a fixed viewport top with
// a stated clientHeight/scrollHeight, and rows whose rects derive from a
// content-space `docY` minus the live scrollTop. That is enough to
// exercise every decision this controller makes, because all of them are
// rect arithmetic — but it does mean these tests prove the DECISIONS,
// not the rendering. What a real engine has to confirm (deferred to the
// e2e map suite): that the overlay body actually lands on the value we
// write, that `overflow-anchor: none` keeps native anchoring out of the
// compensation path, and that a glide reads as smooth motion.
//
// The transition matrix is the point: escape twice, engage twice, a
// retarget mid-glide, a target that vanishes mid-glide, and compensation
// with and without follow engaged. State coverage is not transition
// coverage.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getSettings } from '../../stores/settings.svelte';
import {
  BAND_REST_FRACTION,
  createRunMapFollow,
  RESIZE_MIN_INTERVAL_MS,
  type RunMapFollow,
  type RunMapScrollWrite,
} from './runMapFollow.svelte';

const SCROLLER_TOP = 100;
const CLIENT_HEIGHT = 400;
const SCROLL_HEIGHT = 2000;
const OFFSET_WIDTH = 612;
const CLIENT_WIDTH = 600;
/**
 * The rest line, DERIVED from the controller's own band fraction rather than
 * restated: a restated 120 keeps passing after the fraction changes, asserting
 * a placement the surface no longer makes.
 */
const REST_LINE_PX = CLIENT_HEIGHT * BAND_REST_FRACTION;
const TARGET_DOC_Y = 1000;
const RESTING_SCROLL_TOP = TARGET_DOC_Y - REST_LINE_PX;
/** The §9.12 rate contract, as the controller states it. */
const RESIZE_COOLDOWN_MS = RESIZE_MIN_INTERVAL_MS;

function domRect(top: number, height: number, left = 0, width = CLIENT_WIDTH): DOMRect {
  return {
    x: left,
    y: top,
    top,
    bottom: top + height,
    left,
    right: left + width,
    width,
    height,
    toJSON: () => ({}),
  } as DOMRect;
}

interface RowModel {
  docY: number;
  height: number;
  readonly el: HTMLElement;
}

interface Harness {
  readonly follow: RunMapFollow;
  readonly scroller: HTMLElement;
  readonly rows: RowModel[];
  readonly writes: RunMapScrollWrite[];
  scrollTop(): number;
  setScrollTop(value: number): void;
  setTarget(row: RowModel | null): void;
  setFollowDefault(value: boolean): void;
  detachScroller(): void;
  reattachScroller(): void;
}

// ===== Frame control =====
//
// The glide is the only cross-frame writer, so the tests drive rAF
// directly rather than through fake timers: a frame's timestamp IS the
// ease input, and stating it explicitly makes the ease assertions exact
// instead of approximate.
let pendingFrames = new Map<number, (ts: number) => void>();
let nextFrameId = 1;

function frameCount(): number {
  return pendingFrames.size;
}

function runFrame(ts: number): void {
  const due = [...pendingFrames.values()];
  pendingFrames.clear();
  for (const cb of due) cb(ts);
}

function drainFrames(startTs = 0, stepMs = 50, maxFrames = 40): void {
  let ts = startTs;
  for (let i = 0; i < maxFrames && frameCount() > 0; i++) {
    ts += stepMs;
    runFrame(ts);
  }
}

/**
 * Dispatch a synthetic input event: a plain `Event` with the fields the
 * handler reads assigned on top. happy-dom's coverage of the
 * WheelEvent/PointerEvent/TouchEvent constructors varies by version, and
 * the controller only ever reads properties.
 */
function fire(el: HTMLElement, type: string, props: Record<string, unknown> = {}): void {
  const event = new Event(type, { bubbles: true });
  Object.assign(event, props);
  el.dispatchEvent(event);
}

function touch(y: number): { clientY: number }[] {
  return [{ clientY: y }];
}

// ===== ResizeObserver stub =====
//
// `disconnect()` is modelled rather than stubbed away: the cleanup test's
// whole claim is that the controller disconnects, and an observer that
// keeps firing after disconnect would let that regression pass.
interface ObserverEntry {
  readonly cb: ResizeObserverCallback;
  live: boolean;
}
const resizeObservers: ObserverEntry[] = [];

class RecordingResizeObserver {
  private readonly entry: ObserverEntry;
  constructor(cb: ResizeObserverCallback) {
    this.entry = { cb, live: true };
    resizeObservers.push(this.entry);
  }
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {
    this.entry.live = false;
  }
}

/** Stubs ResizeObserver and returns a "fire every live observer" trigger. */
function installResizeObserver(): () => void {
  resizeObservers.length = 0;
  vi.stubGlobal('ResizeObserver', RecordingResizeObserver);
  return () => {
    for (const entry of resizeObservers) {
      if (entry.live) entry.cb([], {} as ResizeObserver);
    }
  };
}

const DEFAULT_ROWS = [
  { docY: 0, height: 200 },
  { docY: 200, height: 200 },
  { docY: TARGET_DOC_Y, height: 40 },
];

function makeHarness(
  options: {
    rows?: { docY: number; height: number }[];
    targetIndex?: number | null;
    followDefault?: boolean;
  } = {},
): Harness {
  const layout = { scrollTop: 0 };
  let followDefault = options.followDefault ?? true;
  let detached = false;

  const scroller = document.createElement('div');
  Object.defineProperty(scroller, 'scrollTop', {
    get: () => layout.scrollTop,
    set: (value: number) => {
      layout.scrollTop = value;
    },
    configurable: true,
  });
  for (const [key, value] of Object.entries({
    scrollHeight: SCROLL_HEIGHT,
    clientHeight: CLIENT_HEIGHT,
    clientWidth: CLIENT_WIDTH,
    offsetWidth: OFFSET_WIDTH,
  })) {
    Object.defineProperty(scroller, key, { get: () => value, configurable: true });
  }
  scroller.getBoundingClientRect = () => domRect(SCROLLER_TOP, CLIENT_HEIGHT, 0, OFFSET_WIDTH);
  document.body.appendChild(scroller);

  const rows: RowModel[] = (options.rows ?? DEFAULT_ROWS).map((spec, index) => {
    const el = document.createElement('div');
    el.textContent = `row ${index}`;
    const row: RowModel = { docY: spec.docY, height: spec.height, el };
    el.getBoundingClientRect = () => domRect(SCROLLER_TOP + row.docY - layout.scrollTop, row.height);
    scroller.appendChild(el);
    return row;
  });

  const targetIndex = options.targetIndex === undefined ? rows.length - 1 : options.targetIndex;
  let target: RowModel | null = targetIndex === null ? null : (rows[targetIndex] ?? null);

  // Per-instance, so two harnesses in one file cannot interleave into one log
  // and nothing has to be reset between suites.
  const writes: RunMapScrollWrite[] = [];

  const follow = createRunMapFollow({
    scroller: () => (detached ? null : scroller),
    followTargetEl: () => target?.el ?? null,
    followDefault: () => followDefault,
    onWrite: (write) => writes.push(write),
  });

  return {
    follow,
    scroller,
    rows,
    writes,
    scrollTop: () => layout.scrollTop,
    setScrollTop: (value) => {
      layout.scrollTop = value;
    },
    setTarget: (row) => {
      target = row;
    },
    setFollowDefault: (value) => {
      followDefault = value;
    },
    detachScroller: () => {
      detached = true;
    },
    reattachScroller: () => {
      detached = false;
    },
  };
}

/** Placed + attached + engaged, the state most rules start from. */
function engagedHarness(): Harness {
  const h = makeHarness();
  h.follow.placeOnOpen();
  h.follow.attach();
  h.writes.length = 0;
  return h;
}

beforeEach(() => {
  pendingFrames = new Map();
  nextFrameId = 1;
  vi.stubGlobal('requestAnimationFrame', (cb: (ts: number) => void): number => {
    const id = nextFrameId++;
    pendingFrames.set(id, cb);
    return id;
  });
  vi.stubGlobal('cancelAnimationFrame', (id: number): void => {
    pendingFrames.delete(id);
  });
});

afterEach(() => {
  getSettings().lowPowerMode = false;
  window.getSelection()?.removeAllRanges();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  document.body.innerHTML = '';
});

describe('placeOnOpen', () => {
  it('parks the target on the rest line when the run opens running', () => {
    const h = makeHarness({ followDefault: true });
    h.follow.placeOnOpen();
    expect(h.writes).toEqual([{ top: RESTING_SCROLL_TOP, cause: 'place' }]);
    expect(h.scrollTop()).toBe(RESTING_SCROLL_TOP);
    expect(h.follow.engaged).toBe(true);
  });

  it('goes to the top when the run opens parked or terminal', () => {
    const h = makeHarness({ followDefault: false });
    h.follow.placeOnOpen();
    expect(h.writes).toEqual([{ top: 0, cause: 'place' }]);
    expect(h.follow.engaged).toBe(false);
    expect(h.follow.chipVisible).toBe(true);
  });

  it('goes to the top when the follow target has not rendered', () => {
    const h = makeHarness({ followDefault: true, targetIndex: null });
    h.follow.placeOnOpen();
    expect(h.writes).toEqual([{ top: 0, cause: 'place' }]);
    expect(h.follow.chipVisible).toBe(false);
  });

  it('never animates, even under reduced motion, and runs before attach()', () => {
    getSettings().lowPowerMode = true;
    const h = makeHarness({ followDefault: true });
    h.follow.placeOnOpen();
    expect(h.writes).toEqual([{ top: RESTING_SCROLL_TOP, cause: 'place' }]);
    expect(frameCount()).toBe(0);
    // Placement ran with no listeners installed; attaching afterwards is
    // the component's real order (bind:this, then the $effect).
    expect(() => h.follow.attach()()).not.toThrow();
  });

  it('re-reads followDefault per open', () => {
    const h = makeHarness({ followDefault: true });
    h.follow.placeOnOpen();
    expect(h.follow.engaged).toBe(true);
    h.setFollowDefault(false);
    h.follow.placeOnOpen();
    expect(h.follow.engaged).toBe(false);
  });

  it('does not throw when the scroller is gone', () => {
    const h = makeHarness();
    h.detachScroller();
    expect(() => h.follow.placeOnOpen()).not.toThrow();
    expect(h.writes).toEqual([]);
  });
});

const ESCAPING_INPUTS: [string, string, Record<string, unknown>][] = [
  ['wheel up', 'wheel', { deltaY: -10 }],
  ['PageUp', 'keydown', { key: 'PageUp' }],
  ['ArrowUp', 'keydown', { key: 'ArrowUp' }],
  ['Home', 'keydown', { key: 'Home' }],
  ['middle click', 'pointerdown', { button: 1, clientX: 300 }],
  ['scrollbar-gutter pointerdown', 'pointerdown', { button: 0, clientX: 605 }],
];

const NON_ESCAPING_INPUTS: [string, string, Record<string, unknown>][] = [
  ['wheel down', 'wheel', { deltaY: 10 }],
  ['ctrl+wheel zoom', 'wheel', { deltaY: -10, ctrlKey: true }],
  ['zero-delta wheel', 'wheel', { deltaY: 0 }],
  ['ArrowDown', 'keydown', { key: 'ArrowDown' }],
  ['PageDown', 'keydown', { key: 'PageDown' }],
  ['pointerdown inside the content', 'pointerdown', { button: 0, clientX: 300 }],
  ['non-primary gutter pointerdown', 'pointerdown', {
    button: 0,
    clientX: 605,
    isPrimary: false,
  }],
];

describe('escape is event-sourced', () => {
  it.each(ESCAPING_INPUTS)('%s disengages follow', (_label, type, props) => {
    const h = engagedHarness();
    fire(h.scroller, type, props);
    expect(h.follow.engaged).toBe(false);
    expect(h.follow.chipVisible).toBe(true);
    expect(h.writes).toEqual([]);
  });

  it.each(NON_ESCAPING_INPUTS)('%s does not disengage follow', (_label, type, props) => {
    const h = engagedHarness();
    fire(h.scroller, type, props);
    expect(h.follow.engaged).toBe(true);
  });

  it('a touch drag downward (content moving up) disengages follow', () => {
    const h = engagedHarness();
    fire(h.scroller, 'touchstart', { touches: touch(300) });
    fire(h.scroller, 'touchmove', { touches: touch(312) });
    expect(h.follow.engaged).toBe(false);
  });

  it('a touch drag upward does not disengage follow', () => {
    const h = engagedHarness();
    fire(h.scroller, 'touchstart', { touches: touch(300) });
    fire(h.scroller, 'touchmove', { touches: touch(288) });
    expect(h.follow.engaged).toBe(true);
  });

  it('sub-pixel touch jitter does not disengage follow', () => {
    const h = engagedHarness();
    fire(h.scroller, 'touchstart', { touches: touch(300) });
    fire(h.scroller, 'touchmove', { touches: touch(300.5) });
    expect(h.follow.engaged).toBe(true);
  });

  it('a scroll event never escapes, not even one raised by a glide frame', () => {
    const h = engagedHarness();
    h.setScrollTop(0);
    h.follow.engage();
    runFrame(0);
    runFrame(50);
    expect(h.writes.length).toBeGreaterThan(0);
    fire(h.scroller, 'scroll');
    expect(h.follow.engaged).toBe(true);
    // The glide is untouched by the event and finishes its own motion; the
    // scroll only ever scheduled the chip's coalesced re-measure alongside it.
    drainFrames(50);
    expect(h.scrollTop()).toBe(RESTING_SCROLL_TOP);
    expect(h.writes.every((w) => w.cause === 'jump')).toBe(true);
  });

  it('escape cancels an in-flight glide instead of fighting the reader', () => {
    const h = engagedHarness();
    h.setScrollTop(0);
    h.follow.engage();
    runFrame(0);
    runFrame(50);
    const writesAtEscape = h.writes.length;
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(frameCount()).toBe(0);
    runFrame(100);
    expect(h.writes).toHaveLength(writesAtEscape);
  });

  it('escaping twice is idempotent', () => {
    const h = engagedHarness();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    fire(h.scroller, 'keydown', { key: 'Home' });
    expect(h.follow.engaged).toBe(false);
    expect(h.writes).toEqual([]);
  });

  it('does not escape on a map short enough to need no scrolling', () => {
    const h = makeHarness({ targetIndex: 0 });
    Object.defineProperty(h.scroller, 'scrollHeight', {
      get: () => CLIENT_HEIGHT,
      configurable: true,
    });
    h.follow.placeOnOpen();
    h.follow.attach();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(h.follow.engaged).toBe(true);
    expect(h.follow.chipVisible).toBe(false);
  });
});

describe('engage', () => {
  it('re-engages after an escape and glides the target to the rest line', () => {
    const h = engagedHarness();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    h.setScrollTop(0);
    h.writes.length = 0;

    h.follow.engage();
    expect(h.follow.engaged).toBe(true);
    // The first frame only establishes the time base.
    runFrame(0);
    expect(h.writes).toEqual([]);

    runFrame(100);
    expect(h.writes).toHaveLength(1);
    // ease-out cubic at t=0.4 → 1 - 0.6³ = 0.784.
    expect(h.writes[0]?.top).toBeCloseTo(RESTING_SCROLL_TOP * 0.784, 5);
    expect(h.writes[0]?.cause).toBe('jump');

    drainFrames(100);
    expect(h.writes.at(-1)).toEqual({ top: RESTING_SCROLL_TOP, cause: 'jump' });
    expect(h.writes.every((w) => w.cause === 'jump')).toBe(true);
    expect(frameCount()).toBe(0);
    expect(h.follow.chipVisible).toBe(false);
  });

  it('engaging twice keeps one glide rather than queueing a second', () => {
    const h = engagedHarness();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    h.setScrollTop(0);

    h.follow.engage();
    expect(frameCount()).toBe(1);
    h.follow.engage();
    expect(frameCount()).toBe(1);
    runFrame(0);
    expect(frameCount()).toBe(1);
    drainFrames();
    expect(h.scrollTop()).toBe(RESTING_SCROLL_TOP);
  });

  it('writes nothing when the target already sits on the rest line', () => {
    const h = engagedHarness();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    h.writes.length = 0;
    h.follow.engage();
    expect(h.writes).toEqual([]);
    expect(frameCount()).toBe(0);
    expect(h.follow.engaged).toBe(true);
  });

  it('does not throw with no follow target', () => {
    const h = makeHarness({ targetIndex: null, followDefault: false });
    h.follow.placeOnOpen();
    h.follow.attach();
    h.writes.length = 0;
    expect(() => h.follow.engage()).not.toThrow();
    expect(h.writes).toEqual([]);
    expect(h.follow.chipVisible).toBe(false);
  });
});

describe('reduced motion', () => {
  it('collapses a jump to one instant write', () => {
    const h = engagedHarness();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    h.setScrollTop(0);
    h.writes.length = 0;
    getSettings().lowPowerMode = true;

    h.follow.engage();
    expect(h.writes).toEqual([{ top: RESTING_SCROLL_TOP, cause: 'jump' }]);
    expect(frameCount()).toBe(0);
  });

  it('collapses a follow move to one instant write', () => {
    const h = engagedHarness();
    getSettings().lowPowerMode = true;
    h.rows[2]!.docY = 1600;
    h.follow.onFollowTargetChanged();
    expect(h.writes).toEqual([{ top: 1600 - REST_LINE_PX, cause: 'follow' }]);
    expect(frameCount()).toBe(0);
  });
});

describe('onFollowTargetChanged', () => {
  it('glides when the target has left the band', () => {
    const h = engagedHarness();
    h.rows[2]!.docY = 1600;
    h.follow.onFollowTargetChanged();
    expect(frameCount()).toBe(1);
    drainFrames();
    expect(h.writes.every((w) => w.cause === 'follow')).toBe(true);
    expect(h.scrollTop()).toBe(1600 - REST_LINE_PX);
  });

  it('writes nothing while the target is still inside the band', () => {
    const h = engagedHarness();
    // The band runs 60–280px below the viewport top; 200px is inside it.
    h.rows[2]!.docY = RESTING_SCROLL_TOP + 200;
    h.follow.onFollowTargetChanged();
    expect(h.writes).toEqual([]);
    expect(frameCount()).toBe(0);
  });

  it('never writes while disengaged', () => {
    const h = engagedHarness();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    h.writes.length = 0;
    h.rows[2]!.docY = 1600;
    h.follow.onFollowTargetChanged();
    expect(h.writes).toEqual([]);
    expect(frameCount()).toBe(0);
    expect(h.follow.chipVisible).toBe(true);
  });

  it('retargets the same glide rather than queueing a second', () => {
    const h = engagedHarness();
    h.rows[2]!.docY = 1600;
    h.follow.onFollowTargetChanged();
    runFrame(0);
    runFrame(100);
    const midGlide = h.scrollTop();
    expect(midGlide).toBeGreaterThan(RESTING_SCROLL_TOP);
    expect(midGlide).toBeLessThan(1600 - REST_LINE_PX);

    h.rows[2]!.docY = TARGET_DOC_Y;
    h.follow.onFollowTargetChanged();
    expect(frameCount()).toBe(1);
    drainFrames(100);
    expect(frameCount()).toBe(0);
    expect(h.scrollTop()).toBe(RESTING_SCROLL_TOP);
  });

  // The branch the band check exits early from is still a RETARGET: the glide
  // in flight is easing toward where the OLD target sat, and landing those
  // frames would carry the new one straight back out of the band that just
  // approved it.
  it('cancels a glide aimed at the old target even when the new one needs no move', () => {
    const h = engagedHarness();
    h.rows[2]!.docY = 1600;
    h.follow.onFollowTargetChanged();
    runFrame(0);
    runFrame(100);
    expect(h.writes.length).toBeGreaterThan(0);

    // The frontier moved to a row already sitting inside the band.
    const settled = h.scrollTop();
    h.rows[2]!.docY = settled + REST_LINE_PX;
    h.follow.onFollowTargetChanged();

    expect(frameCount()).toBe(0);
    const writesAtRetarget = h.writes.length;
    drainFrames(100);
    expect(h.writes).toHaveLength(writesAtRetarget);
    expect(h.scrollTop()).toBe(settled);
  });

  it('survives the target disappearing mid-glide', () => {
    const h = engagedHarness();
    h.rows[2]!.docY = 1600;
    h.follow.onFollowTargetChanged();
    runFrame(0);
    runFrame(100);
    h.setTarget(null);
    expect(() => drainFrames(100)).not.toThrow();
    expect(h.scrollTop()).toBe(1600 - REST_LINE_PX);
    expect(h.follow.chipVisible).toBe(false);
  });

  it('holds the follow write while the reader has text selected in the map', () => {
    const h = engagedHarness();
    const range = document.createRange();
    range.selectNodeContents(h.rows[0]!.el);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);

    h.rows[2]!.docY = 1600;
    h.follow.onFollowTargetChanged();
    expect(h.writes).toEqual([]);
    expect(h.follow.engaged).toBe(true);

    // Clearing the selection releases the hold on the next frontier move.
    selection?.removeAllRanges();
    h.follow.onFollowTargetChanged();
    expect(frameCount()).toBe(1);
  });
});

describe('holdAnchor', () => {
  /** Grow the first row by 100px, pushing everything below it down. */
  function growFirstRow(h: Harness): string {
    h.rows[0]!.height += 100;
    for (const row of h.rows.slice(1)) row.docY += 100;
    return 'mutated';
  }

  /**
   * Disengaged, scrolled to 250 — where row 1 (docY 200–400) straddles
   * the viewport top and is therefore the anchor.
   */
  function anchorHarness(followDefault = false): Harness {
    const h = makeHarness({ followDefault });
    h.follow.placeOnOpen();
    h.follow.attach();
    h.setScrollTop(250);
    h.writes.length = 0;
    return h;
  }

  it('compensates the anchor delta while not following', () => {
    const h = anchorHarness();
    const result = h.follow.holdAnchor(() => growFirstRow(h));

    expect(result).toBe('mutated');
    expect(h.writes).toEqual([{ top: 350, cause: 'compensate' }]);
    // Net-zero: the anchor row sits exactly where it did before.
    expect(h.rows[1]!.el.getBoundingClientRect().top).toBe(50);
  });

  /**
   * The production shape, which the flat harness above does not have: every
   * row sits under a stack of containers that each SPAN the growth — the run
   * detail's wrapper spans the document, the map's root spans the map, the
   * wave's body spans the wave. None of their top edges move when something
   * inside them grows, so an anchor search that stopped at any of them would
   * measure zero and compensate nothing at all.
   *
   * Nine levels, because the real chain is about that deep (body → detail →
   * column → section → map → ol → li → fold → body → ul → li) and a search
   * capped short of it fails exactly the way a browser would not.
   */
  function nestRows(h: Harness, depth: number): void {
    let inner = document.createElement('div');
    const spans: HTMLElement[] = [inner];
    for (let i = 1; i < depth; i++) {
      const outer = document.createElement('div');
      outer.appendChild(inner);
      spans.push(outer);
      inner = outer;
    }
    // Every wrapper spans the whole content, which is what makes them useless
    // as anchors: their top is the top of the document, whatever moves inside.
    for (const el of spans) {
      el.getBoundingClientRect = () => domRect(SCROLLER_TOP - h.scrollTop(), SCROLL_HEIGHT);
    }
    for (const row of h.rows) spans[0]!.appendChild(row.el);
    h.scroller.appendChild(inner);
  }

  it('descends through the containers that span the growth to the row itself', () => {
    const h = anchorHarness();
    nestRows(h, 9);

    const result = h.follow.holdAnchor(() => growFirstRow(h));

    expect(result).toBe('mutated');
    expect(h.writes).toEqual([{ top: 350, cause: 'compensate' }]);
    expect(h.rows[1]!.el.getBoundingClientRect().top).toBe(50);
  });

  it('still holds when the row itself has inner elements to descend into', () => {
    const h = anchorHarness();
    nestRows(h, 3);
    // A row is not a leaf in production — it is a bordered box around a flex
    // line around a glyph and a label. The anchor lands on whichever of those
    // straddles the top line, and the hold is the same either way.
    for (const row of h.rows) {
      const inner = document.createElement('span');
      inner.getBoundingClientRect = () => row.el.getBoundingClientRect();
      row.el.appendChild(inner);
    }

    h.follow.holdAnchor(() => growFirstRow(h));
    expect(h.writes).toEqual([{ top: 350, cause: 'compensate' }]);
  });

  it('writes nothing when the mutation moves nothing above the anchor', () => {
    const h = anchorHarness();
    h.follow.holdAnchor(() => {
      h.rows[2]!.docY += 300;
    });
    expect(h.writes).toEqual([]);
  });

  it('compensates nothing while follow is engaged — follow owns the viewport', () => {
    const h = anchorHarness(true);
    expect(h.follow.holdAnchor(() => growFirstRow(h))).toBe('mutated');
    expect(h.writes).toEqual([]);
    expect(h.scrollTop()).toBe(250);
  });

  it('still runs the mutation when there is no scroller', () => {
    const h = makeHarness({ followDefault: false });
    h.detachScroller();
    expect(h.follow.holdAnchor(() => 7)).toBe(7);
    expect(h.writes).toEqual([]);
  });

  /**
   * §7, "transport gap": a refetch replaces the whole view, so the map's
   * layout changes on a flush the controller does not own. The component
   * measures in a pre-effect and releases in the matching post effect, which
   * is what `captureAnchor` is — and the mutation between the two is a WHOLE
   * MODEL, not one fold.
   */
  describe('captureAnchor across a full model replacement', () => {
    /** Rebuild every row: new heights, new content offsets, same elements. */
    function replaceModel(h: Harness, heights: number[]): void {
      let docY = 0;
      h.rows.forEach((row, index) => {
        row.docY = docY;
        row.height = heights[index] ?? row.height;
        docY += row.height;
      });
    }

    it('holds the reader where they were when every row changes at once', () => {
      const h = anchorHarness();
      // Row 1 (docY 200) straddles the viewport top at scrollTop 250.
      const release = h.follow.captureAnchor();
      replaceModel(h, [520, 200, 40]);
      release();

      // Row 1 now starts 320px lower, so the viewport follows it exactly.
      expect(h.writes).toEqual([{ top: 570, cause: 'compensate' }]);
      expect(h.rows[1]!.el.getBoundingClientRect().top).toBe(50);
    });

    it('writes nothing when the replacement leaves the anchor where it was', () => {
      const h = anchorHarness();
      const release = h.follow.captureAnchor();
      replaceModel(h, [200, 260, 40]);
      release();
      expect(h.writes).toEqual([]);
    });

    it('leaves the viewport alone while following — follow owns it', () => {
      const h = anchorHarness(true);
      const release = h.follow.captureAnchor();
      replaceModel(h, [520, 200, 40]);
      release();
      expect(h.writes).toEqual([]);
      expect(h.scrollTop()).toBe(250);
    });

    it('drops the hold when the replacement removed the anchor element', () => {
      const h = anchorHarness();
      const release = h.follow.captureAnchor();
      h.rows[1]!.el.remove();
      replaceModel(h, [520, 200, 40]);
      expect(() => release()).not.toThrow();
      expect(h.writes).toEqual([]);
    });

    // A release is one statement about one measured delta. Called twice it
    // would compensate whatever moved in between — a scroll write with no
    // cause the reader could name (§9.1).
    it('is single-shot: a second release writes nothing', () => {
      const h = anchorHarness();
      const release = h.follow.captureAnchor();
      replaceModel(h, [520, 200, 40]);
      release();
      expect(h.writes).toEqual([{ top: 570, cause: 'compensate' }]);

      replaceModel(h, [900, 200, 40]);
      release();
      expect(h.writes).toHaveLength(1);
    });

    // Engagement is the viewport's ownership. A release that outlives the
    // moment it was measured in must not write into a glide's viewport.
    it('bails when follow engaged between the measurement and the release', () => {
      const h = anchorHarness();
      const release = h.follow.captureAnchor();
      replaceModel(h, [520, 200, 40]);
      h.follow.engage();
      h.writes.length = 0;

      release();
      expect(h.writes).toEqual([]);
    });

    it('bails when the controller was re-attached since the measurement', () => {
      const h = anchorHarness();
      const release = h.follow.captureAnchor();
      replaceModel(h, [520, 200, 40]);
      h.follow.attach();

      release();
      expect(h.writes).toEqual([]);
    });

    it('bails when the scroller was swapped out since the measurement', () => {
      const h = anchorHarness();
      const release = h.follow.captureAnchor();
      replaceModel(h, [520, 200, 40]);
      h.detachScroller();

      release();
      expect(h.writes).toEqual([]);
    });
  });

  it('propagates a throwing mutation instead of swallowing it', () => {
    const h = anchorHarness();
    expect(() =>
      h.follow.holdAnchor(() => {
        throw new Error('mutation failed');
      }),
    ).toThrow('mutation failed');
  });
});

describe('chipVisible', () => {
  it('is hidden while following a target that is in view', () => {
    const h = engagedHarness();
    expect(h.follow.chipVisible).toBe(false);
  });

  it('appears on escape and clears when the glide lands', () => {
    const h = engagedHarness();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(h.follow.chipVisible).toBe(true);

    // Reader scrolled clear of the target, then clicked the chip.
    h.setScrollTop(0);
    h.follow.engage();
    expect(h.follow.chipVisible).toBe(true); // engaged, target still off-screen
    drainFrames();
    expect(h.follow.chipVisible).toBe(false);
  });

  it('reappears while engaged when a scroll event leaves the target off-screen', () => {
    const h = engagedHarness();
    h.setScrollTop(0);
    fire(h.scroller, 'scroll');
    expect(h.follow.engaged).toBe(true);
    // Measured on the frame, not on the event.
    runFrame(0);
    expect(h.follow.chipVisible).toBe(true);
  });

  it('measures once per frame however many scroll events arrive', () => {
    const h = engagedHarness();
    h.setScrollTop(0);
    for (let i = 0; i < 20; i++) fire(h.scroller, 'scroll');
    expect(frameCount()).toBe(1);

    runFrame(0);
    expect(h.follow.chipVisible).toBe(true);
    // The frame is spent: nothing is left scheduled until the next event.
    expect(frameCount()).toBe(0);
  });

  it('drops a pending scroll measurement on cleanup', () => {
    const h = engagedHarness();
    const cleanup = h.follow.attach();
    fire(h.scroller, 'scroll');
    expect(frameCount()).toBe(1);
    cleanup();
    expect(frameCount()).toBe(0);
  });

  it('stays hidden with no follow target, engaged or not', () => {
    const h = engagedHarness();
    h.setTarget(null);
    h.follow.onFollowTargetChanged();
    expect(h.follow.chipVisible).toBe(false);
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(h.follow.chipVisible).toBe(false);
  });
});

describe('attach / detach', () => {
  it('cleanup removes every input listener', () => {
    const h = engagedHarness();
    const cleanup = h.follow.attach();
    cleanup();
    for (const [, type, props] of ESCAPING_INPUTS) {
      fire(h.scroller, type, props);
    }
    fire(h.scroller, 'touchstart', { touches: touch(300) });
    fire(h.scroller, 'touchmove', { touches: touch(320) });
    expect(h.follow.engaged).toBe(true);
  });

  it('cleanup cancels an in-flight glide', () => {
    const h = makeHarness();
    h.follow.placeOnOpen();
    const cleanup = h.follow.attach();
    h.setScrollTop(0);
    h.follow.engage();
    expect(frameCount()).toBe(1);
    cleanup();
    expect(frameCount()).toBe(0);
  });

  it('re-attaching does not double-install, and cleanup is idempotent', () => {
    const h = engagedHarness();
    const cleanup = h.follow.attach();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(h.follow.engaged).toBe(false);
    cleanup();
    expect(() => cleanup()).not.toThrow();
  });

  // §9.2 turns "attach did nothing" into a product defect, not a nuisance: the
  // write chokepoint reaches the scroller through the live getter whether or
  // not a listener was ever installed, so a silent no-op leaves follow running
  // with no escape. It therefore waits for a late bind and then gets loud.
  it('waits for a scroller that has not rendered yet, then installs on it', () => {
    const h = makeHarness();
    h.detachScroller();
    const cleanup = h.follow.attach();
    expect(frameCount()).toBe(1);

    h.reattachScroller();
    runFrame(0);
    expect(frameCount()).toBe(0);

    // Installed for real: the listeners are live on the element that arrived.
    h.follow.placeOnOpen();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(h.follow.engaged).toBe(false);
    cleanup();
  });

  it('throws rather than leaving a dead installation when the scroller never arrives', () => {
    const h = makeHarness();
    h.detachScroller();
    h.follow.attach();
    // Three frames of grace, then the wiring bug is stated.
    runFrame(0);
    runFrame(16);
    expect(() => runFrame(32)).toThrow(/overlay scroller never rendered/);
  });

  // The throw is the REPORT. On its own it changed no controller state, and
  // the write chokepoint reaches the element through the live getter rather
  // than through anything the failed install owned — so a scroller that turned
  // up moments later (the getter answering again) left follow gliding with not
  // one input listener attached, which is the inescapable follow §9.2 forbids
  // and the message itself names.
  it('exhausting the attach frames latches the controller shut, not just loud', () => {
    const h = makeHarness();
    h.follow.placeOnOpen();
    expect(h.follow.engaged).toBe(true);
    h.detachScroller();
    h.follow.attach();
    runFrame(0);
    runFrame(16);
    expect(() => runFrame(32)).toThrow(/overlay scroller never rendered/);

    expect([h.follow.engaged, h.follow.chipVisible]).toEqual([false, false]);
    // The element is back, and every writer stays inert: nothing may move a
    // viewport the reader has no way to take back.
    h.reattachScroller();
    h.writes.length = 0;
    h.follow.placeOnOpen();
    h.follow.engage();
    h.follow.onFollowTargetChanged();
    h.follow.holdAnchor(() => {});
    drainFrames();
    expect(h.writes).toEqual([]);
    expect([h.follow.engaged, h.follow.chipVisible]).toEqual([false, false]);
  });

  it('a later successful attach clears the latch completely', () => {
    const h = makeHarness();
    h.detachScroller();
    h.follow.attach();
    runFrame(0);
    runFrame(16);
    expect(() => runFrame(32)).toThrow(/overlay scroller never rendered/);

    h.reattachScroller();
    h.follow.attach();
    h.writes.length = 0;
    h.follow.placeOnOpen();
    expect(h.follow.engaged).toBe(true);
    expect(h.writes.map((write) => write.cause)).toEqual(['place']);
    // And the listeners are live again, so the reader can escape.
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(h.follow.engaged).toBe(false);
  });

  it('cleanup cancels a pending install rather than throwing later', () => {
    const h = makeHarness();
    h.detachScroller();
    const cleanup = h.follow.attach();
    cleanup();
    expect(frameCount()).toBe(0);
  });

  // Two attaches, then the FIRST cleanup: the stale closure must not tear down
  // the live installation. Un-guarded, this left the controller writing scroll
  // positions with no listener able to hear the reader ask it to stop.
  it('a stale cleanup does not tear down a newer attachment', () => {
    const h = makeHarness();
    h.follow.placeOnOpen();
    const firstCleanup = h.follow.attach();
    h.follow.attach();

    firstCleanup();
    fire(h.scroller, 'wheel', { deltaY: -10 });
    expect(h.follow.engaged).toBe(false);
  });
});

describe('resize', () => {
  it('re-checks the band while engaged, rate-bound to one run per 100ms', () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const fireResize = installResizeObserver();
    // Reduced motion makes each re-check exactly one countable write.
    getSettings().lowPowerMode = true;
    const h = engagedHarness();

    h.rows[2]!.docY = 1600;
    fireResize();
    expect(h.writes).toEqual([{ top: 1600 - REST_LINE_PX, cause: 'follow' }]);

    // Two more resizes inside the cooldown collapse into one trailing run.
    h.rows[2]!.docY = TARGET_DOC_Y;
    fireResize();
    fireResize();
    expect(h.writes).toHaveLength(1);

    vi.advanceTimersByTime(RESIZE_COOLDOWN_MS);
    expect(h.writes).toEqual([
      { top: 1600 - REST_LINE_PX, cause: 'follow' },
      { top: RESTING_SCROLL_TOP, cause: 'follow' },
    ]);

    vi.advanceTimersByTime(RESIZE_COOLDOWN_MS * 3);
    expect(h.writes).toHaveLength(2);
  });

  it('writes nothing while disengaged — a reflow we did not cause is not ours', () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const fireResize = installResizeObserver();
    const h = makeHarness({ followDefault: false });
    h.follow.placeOnOpen();
    h.follow.attach();
    h.writes.length = 0;

    h.rows[2]!.docY = 1600;
    fireResize();
    vi.advanceTimersByTime(RESIZE_COOLDOWN_MS * 2);
    expect(h.writes).toEqual([]);
    expect(h.follow.chipVisible).toBe(true);
  });

  it('stops firing after cleanup', () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout'] });
    const fireResize = installResizeObserver();
    getSettings().lowPowerMode = true;
    const h = makeHarness();
    h.follow.placeOnOpen();
    const cleanup = h.follow.attach();
    h.writes.length = 0;

    cleanup();
    h.rows[2]!.docY = 1600;
    fireResize();
    vi.advanceTimersByTime(RESIZE_COOLDOWN_MS * 2);
    expect(h.writes).toEqual([]);
  });
});

describe('holdAnchor rejects an async mutation', () => {
  it('throws rather than compensating against a layout that has not moved', () => {
    const h = makeHarness({ followDefault: false });
    h.follow.placeOnOpen();
    h.follow.attach();
    h.setScrollTop(250);
    h.writes.length = 0;
    expect(() => h.follow.holdAnchor(async () => 'later')).toThrow(/must be synchronous/);
    expect(h.writes).toEqual([]);
  });
});
