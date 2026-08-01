// Characterization suite for the scroll intent machine.
//
// Written BEFORE the wheel-attribution rework so the behavior it pins is
// today's behavior, not the behavior we are about to build. The case that
// matters most here is `wheel up from a descendant escapes` — that is the
// latent bug (a nested scroller consumes the delta, the outer pane never
// moves, yet the outer machine treats it as "the user left the bottom").
// When attribution lands, that expectation flips deliberately and the diff
// says so.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  createScrollIntent,
  resetScrollIntentModuleStateForTest,
  type ScrollIntent,
  type ScrollIntentDeps,
} from './intent';
import { AUTO_FOLLOW_BOTTOM_EPSILON_PX } from './resolver';
import { registerNestedScroller } from './wheelAttribution';

// The deferred scroll-intent pass runs behind a 1ms timer so a concurrent
// RO callback can stamp its resize classification first. Real timers plus a
// short wait is more honest here than faking the clock: the machine also
// reads `performance.now()` for its down-intent window, and the two would
// have to be faked in lockstep.
const DEFERRED_PASS_MS = 8;
const settleDeferredPass = () => new Promise((r) => setTimeout(r, DEFERRED_PASS_MS));

interface Harness {
  intent: ScrollIntent;
  scrollEl: HTMLDivElement;
  child: HTMLDivElement;
  deps: ScrollIntentDeps;
  state: {
    isAtBottom: boolean;
    escaped: boolean;
    distanceFromBottom: number;
    resizeCorrelated: boolean;
  };
  spring: {
    requestStop: ReturnType<typeof vi.fn>;
    cancel: ReturnType<typeof vi.fn>;
    clearStopRequest: ReturnType<typeof vi.fn>;
    clearStructuralAppend: ReturnType<typeof vi.fn>;
  };
  refreshIsNearBottom: ReturnType<typeof vi.fn>;
  noteScrollActivity: ReturnType<typeof vi.fn>;
  noteUserScroll: ReturnType<typeof vi.fn>;
}

function harness(
  geometry: { scrollHeight?: number; clientHeight?: number } = {},
): Harness {
  const scrollEl = document.createElement('div');
  const child = document.createElement('div');
  scrollEl.appendChild(child);
  document.body.appendChild(scrollEl);

  Object.defineProperty(scrollEl, 'scrollHeight', {
    configurable: true,
    get: () => geometry.scrollHeight ?? 2000,
  });
  Object.defineProperty(scrollEl, 'clientHeight', {
    configurable: true,
    get: () => geometry.clientHeight ?? 500,
  });

  const state = {
    isAtBottom: true,
    escaped: false,
    // Far enough from the bottom that a down-intent does NOT trip the
    // immediate re-stick branch; the tests that want that branch set it to 0.
    distanceFromBottom: 100,
    resizeCorrelated: false,
  };

  const spring = {
    requestStop: vi.fn(),
    cancel: vi.fn(),
    clearStopRequest: vi.fn(),
    clearStructuralAppend: vi.fn(),
  };
  const refreshIsNearBottom = vi.fn(() => state.distanceFromBottom);
  const noteScrollActivity = vi.fn();
  const noteUserScroll = vi.fn();

  const deps: ScrollIntentDeps = {
    getScrollEl: () => scrollEl,
    isAtBottom: () => state.isAtBottom,
    setIsAtBottom: (next) => {
      state.isAtBottom = next;
    },
    escaped: () => state.escaped,
    setEscaped: (next) => {
      state.escaped = next;
    },
    isNearBottom: () => state.distanceFromBottom <= AUTO_FOLLOW_BOTTOM_EPSILON_PX,
    pauseDepth: () => 0,
    distanceFromBottom: () => state.distanceFromBottom,
    refreshIsNearBottom,
    spring,
    sampleResizeCorrelation: () => state.resizeCorrelated,
    resizeDifferenceNow: () => 0,
    noteScrollActivity,
    noteUserScroll,
  };

  const intent = createScrollIntent(deps);
  intent.attach(scrollEl);

  return {
    intent,
    scrollEl,
    child,
    deps,
    state,
    spring,
    refreshIsNearBottom,
    noteScrollActivity,
    noteUserScroll,
  };
}

function wheel(target: Element, init: WheelEventInit): void {
  const event = new WheelEvent('wheel', { bubbles: true, ...init });
  // happy-dom's WheelEvent drops modifier keys from the init dict, so a
  // ctrlKey passed above would arrive `undefined` and silently pass the
  // pinch-zoom guard it is meant to exercise. Stamp it explicitly.
  if (init.ctrlKey !== undefined) {
    Object.defineProperty(event, 'ctrlKey', { value: init.ctrlKey });
  }
  target.dispatchEvent(event);
}

function key(target: Element, k: string): void {
  target.dispatchEvent(new KeyboardEvent('keydown', { bubbles: true, key: k }));
}

function touch(target: Element, type: string, clientY: number): void {
  const event = new Event(type, { bubbles: true });
  Object.defineProperty(event, 'touches', {
    value: [{ clientY }],
  });
  target.dispatchEvent(event);
}

function pointerDown(
  target: Element,
  init: { button?: number; clientX?: number; isPrimary?: boolean } = {},
): void {
  // happy-dom's PointerEvent coverage is thin; a MouseEvent carries every
  // field the handler reads (button, clientX) and dispatches under the
  // 'pointerdown' type just the same.
  const event = new MouseEvent('pointerdown', {
    bubbles: true,
    button: init.button ?? 0,
    clientX: init.clientX ?? 0,
  });
  Object.defineProperty(event, 'isPrimary', { value: init.isPrimary ?? true });
  target.dispatchEvent(event);
}

let active: Harness | null = null;

beforeEach(() => {
  resetScrollIntentModuleStateForTest();
});

afterEach(() => {
  // detach() clears the 30s scrollbar-drag failsafe timer and the document
  // listeners it installs; without it a gutter-drag test leaks both.
  active?.intent.detach();
  active?.scrollEl.remove();
  active = null;
});

function build(geometry?: { scrollHeight?: number; clientHeight?: number }): Harness {
  active = harness(geometry);
  return active;
}

describe('wheel', () => {
  it('up escapes bottom-follow', () => {
    const h = build();

    wheel(h.scrollEl, { deltaY: -10 });

    expect(h.state.escaped).toBe(true);
    expect(h.state.isAtBottom).toBe(false);
    expect(h.spring.requestStop).toHaveBeenCalled();
    expect(h.spring.cancel).toHaveBeenCalled();
  });

  it('up from an unregistered descendant escapes', () => {
    const h = build();

    wheel(h.child, { deltaY: -10 });

    expect(h.state.escaped).toBe(true);
  });

  it('down while escaped and away from the bottom records down intent without re-sticking', () => {
    const h = build();
    h.state.escaped = true;
    h.state.distanceFromBottom = 100;

    wheel(h.scrollEl, { deltaY: 10 });

    expect(h.intent.debugState().recentDownIntentActive).toBe(true);
    expect(h.state.escaped).toBe(true);
  });

  it('down while escaped AT the bottom re-sticks immediately', () => {
    const h = build();
    h.state.escaped = true;
    h.state.distanceFromBottom = 0;

    wheel(h.scrollEl, { deltaY: 10 });

    expect(h.state.escaped).toBe(false);
    expect(h.state.isAtBottom).toBe(true);
    expect(h.spring.clearStopRequest).toHaveBeenCalled();
  });

  it('down while already following records nothing', () => {
    const h = build();
    h.state.escaped = false;

    wheel(h.scrollEl, { deltaY: 10 });

    expect(h.intent.debugState().recentDownIntentActive).toBe(false);
  });

  it('ignores ctrl-wheel (pinch zoom)', () => {
    const h = build();

    wheel(h.scrollEl, { deltaY: -10, ctrlKey: true });

    expect(h.state.escaped).toBe(false);
  });

  it('ignores a zero delta', () => {
    const h = build();

    wheel(h.scrollEl, { deltaY: 0 });

    expect(h.state.escaped).toBe(false);
  });

  it('ignores wheel when the content does not overflow', () => {
    const h = build({ scrollHeight: 500, clientHeight: 500 });

    wheel(h.scrollEl, { deltaY: -10 });

    expect(h.state.escaped).toBe(false);
  });
});

// The outer pane does not move when a nested box absorbs the gesture, so
// nothing about the user's relationship to the outer pane changed. Before
// attribution this escaped bottom-follow and the pane silently stopped
// following mid-turn.
describe('nested-scroller attribution', () => {
  function nestedBox(
    h: Harness,
    geometry: { scrollTop: number; clientHeight?: number; scrollHeight?: number },
  ): { box: HTMLDivElement; leaf: HTMLSpanElement; release: () => void } {
    const box = document.createElement('div');
    h.child.appendChild(box);
    box.scrollTop = geometry.scrollTop;
    Object.defineProperty(box, 'clientHeight', {
      configurable: true,
      get: () => geometry.clientHeight ?? 100,
    });
    Object.defineProperty(box, 'scrollHeight', {
      configurable: true,
      get: () => geometry.scrollHeight ?? 500,
    });
    const leaf = document.createElement('span');
    box.appendChild(leaf);
    return { box, leaf, release: registerNestedScroller(box) };
  }

  it('wheel up consumed by a registered box does not escape', () => {
    const h = build();
    const { leaf, release } = nestedBox(h, { scrollTop: 200 });

    wheel(leaf, { deltaY: -10 });
    release();

    expect(h.state.escaped).toBe(false);
    expect(h.state.isAtBottom).toBe(true);
  });

  it('wheel up at the registered box top chains out and escapes', () => {
    const h = build();
    const { leaf, release } = nestedBox(h, { scrollTop: 0 });

    wheel(leaf, { deltaY: -10 });
    release();

    expect(h.state.escaped).toBe(true);
  });

  it('wheel down consumed by a registered box records no down intent', () => {
    const h = build();
    h.state.escaped = true;
    const { leaf, release } = nestedBox(h, { scrollTop: 0 });

    wheel(leaf, { deltaY: 10 });
    release();

    expect(h.intent.debugState().recentDownIntentActive).toBe(false);
  });

  it('wheel down at the registered box bottom chains out and records down intent', () => {
    const h = build();
    h.state.escaped = true;
    const { leaf, release } = nestedBox(h, {
      scrollTop: 400,
      clientHeight: 100,
      scrollHeight: 500,
    });

    wheel(leaf, { deltaY: 10 });
    release();

    expect(h.intent.debugState().recentDownIntentActive).toBe(true);
  });

  it('touch drag consumed by a registered box does not escape', () => {
    const h = build();
    const { leaf, release } = nestedBox(h, { scrollTop: 200 });

    touch(leaf, 'touchstart', 100);
    touch(leaf, 'touchmove', 140);
    release();

    expect(h.state.escaped).toBe(false);
  });

  it('touch drag at the registered box top chains out and escapes', () => {
    const h = build();
    const { leaf, release } = nestedBox(h, { scrollTop: 0 });

    touch(leaf, 'touchstart', 100);
    touch(leaf, 'touchmove', 140);
    release();

    expect(h.state.escaped).toBe(true);
  });
});

describe('keyboard', () => {
  it.each(['PageUp', 'ArrowUp', 'Home'])('%s escapes', (k) => {
    const h = build();

    key(h.scrollEl, k);

    expect(h.state.escaped).toBe(true);
  });

  it.each(['PageDown', 'ArrowDown', 'End'])('%s records down intent while escaped', (k) => {
    const h = build();
    h.state.escaped = true;

    key(h.scrollEl, k);

    expect(h.intent.debugState().recentDownIntentActive).toBe(true);
  });

  it('ignores unrelated keys', () => {
    const h = build();

    key(h.scrollEl, 'a');

    expect(h.state.escaped).toBe(false);
    expect(h.intent.debugState().recentDownIntentActive).toBe(false);
  });
});

describe('touch', () => {
  it('finger moving down (page scrolls up) escapes', () => {
    const h = build();

    touch(h.scrollEl, 'touchstart', 100);
    touch(h.scrollEl, 'touchmove', 140);

    expect(h.state.escaped).toBe(true);
  });

  it('finger moving up while escaped records down intent', () => {
    const h = build();
    h.state.escaped = true;

    touch(h.scrollEl, 'touchstart', 140);
    touch(h.scrollEl, 'touchmove', 100);

    expect(h.intent.debugState().recentDownIntentActive).toBe(true);
  });

  it('ignores sub-pixel jitter', () => {
    const h = build();

    touch(h.scrollEl, 'touchstart', 100);
    touch(h.scrollEl, 'touchmove', 100.5);

    expect(h.state.escaped).toBe(false);
  });

  it('touchend clears the baseline so the next gesture starts fresh', () => {
    const h = build();

    touch(h.scrollEl, 'touchstart', 100);
    touch(h.scrollEl, 'touchend', 100);
    // Without a baseline this move is ignored rather than read as a 40px drag.
    touch(h.scrollEl, 'touchmove', 140);

    expect(h.state.escaped).toBe(false);
  });
});

describe('pointer', () => {
  function withScrollbarGutter(h: Harness): void {
    Object.defineProperty(h.scrollEl, 'offsetWidth', { configurable: true, get: () => 110 });
    Object.defineProperty(h.scrollEl, 'clientWidth', { configurable: true, get: () => 100 });
    Object.defineProperty(h.scrollEl, 'getBoundingClientRect', {
      configurable: true,
      value: () => ({ left: 0, right: 110, top: 0, bottom: 500, width: 110, height: 500 }),
    });
  }

  it('middle-click escapes (autoscroll)', () => {
    const h = build();

    pointerDown(h.scrollEl, { button: 1 });

    expect(h.state.escaped).toBe(true);
  });

  it('press in the scrollbar gutter escapes and arms a drag session', () => {
    const h = build();
    withScrollbarGutter(h);

    pointerDown(h.scrollEl, { clientX: 105 });

    expect(h.state.escaped).toBe(true);
    expect(h.intent.debugState().scrollbarDragSessionActive).toBe(true);
  });

  it('press in the content area does nothing', () => {
    const h = build();
    withScrollbarGutter(h);

    pointerDown(h.scrollEl, { clientX: 40 });

    expect(h.state.escaped).toBe(false);
    expect(h.intent.debugState().scrollbarDragSessionActive).toBe(false);
  });

  it('press with no scrollbar gutter does nothing', () => {
    const h = build();

    pointerDown(h.scrollEl, { clientX: 105 });

    expect(h.state.escaped).toBe(false);
  });

  it('a document pointerup ends the drag session', () => {
    const h = build();
    withScrollbarGutter(h);
    pointerDown(h.scrollEl, { clientX: 105 });

    document.dispatchEvent(new MouseEvent('pointerup', { bubbles: true }));

    expect(h.intent.debugState().scrollbarDragSessionActive).toBe(false);
  });
});

describe('programmatic write tagging', () => {
  it('a scroll matching a recorded write is not interpreted as intent', () => {
    const h = build();
    h.intent.noteProgrammaticWrite(600);
    h.scrollEl.scrollTop = 600;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    // The tagged path bails before the near-bottom refresh — that skip is the
    // observable proof the event was attributed to our own write.
    expect(h.refreshIsNearBottom).not.toHaveBeenCalled();
  });

  it('an untagged scroll is interpreted', () => {
    const h = build();
    h.scrollEl.scrollTop = 600;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.refreshIsNearBottom).toHaveBeenCalled();
  });

  it('renews the content-layer lease on every scroll, tagged or not', () => {
    const h = build();
    h.intent.noteProgrammaticWrite(600);
    h.scrollEl.scrollTop = 600;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.noteScrollActivity).toHaveBeenCalledTimes(1);
  });

  // One write carries a duplicate budget (PROGRAMMATIC_SCROLL_EVENT_DUPLICATE_
  // BUDGET = 4) so browser-coalesced events for the SAME write are all
  // absorbed. The budget is finite on purpose: a genuine user scroll that
  // happens to land on the same offset must not be swallowed forever.
  it('absorbs coalesced duplicates for one write, then interprets the next scroll', () => {
    const h = build();
    h.intent.noteProgrammaticWrite(600);
    h.scrollEl.scrollTop = 600;

    for (let i = 0; i < 4; i += 1) {
      h.scrollEl.dispatchEvent(new Event('scroll'));
    }
    expect(h.refreshIsNearBottom).not.toHaveBeenCalled();

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.refreshIsNearBottom).toHaveBeenCalled();
  });

  it('does not tag a scroll to a different offset', () => {
    const h = build();
    h.intent.noteProgrammaticWrite(600);
    h.scrollEl.scrollTop = 601;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.refreshIsNearBottom).toHaveBeenCalled();
  });
});

describe('provenance ledger classification (noteUserScroll)', () => {
  // The ledger's invariant: every EXPLAINED mover records its position,
  // leaving the browser's max-scroll clamp as the only mover the ledger
  // cannot account for. So user-classified scrolls record, and both
  // "already explained" (tagged) and "this IS the clamp shape"
  // (resize-correlated) events must not.

  it('records a user-classified scroll position', () => {
    const h = build();
    h.scrollEl.scrollTop = 600;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.noteUserScroll).toHaveBeenCalledWith(600);
  });

  it('does not record a tagged programmatic scroll (the write already explained it)', () => {
    const h = build();
    h.intent.noteProgrammaticWrite(600);
    h.scrollEl.scrollTop = 600;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.noteUserScroll).not.toHaveBeenCalled();
  });

  it('does not record a resize-correlated scroll — recording it would launder clamp evidence', () => {
    const h = build();
    h.state.resizeCorrelated = true;
    h.scrollEl.scrollTop = 463;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.noteUserScroll).not.toHaveBeenCalled();
  });
});

describe('deferred scroll pass', () => {
  it('re-sticks when a downward scroll lands at the bottom with live down intent', async () => {
    const h = build();
    h.state.escaped = true;
    h.state.distanceFromBottom = 100;
    // Baseline the re-stick direction check, then arm down intent.
    h.scrollEl.scrollTop = 400;
    h.scrollEl.dispatchEvent(new Event('scroll'));
    wheel(h.scrollEl, { deltaY: 10 });

    h.state.distanceFromBottom = 0;
    h.scrollEl.scrollTop = 500;
    h.scrollEl.dispatchEvent(new Event('scroll'));
    await settleDeferredPass();

    expect(h.state.escaped).toBe(false);
    expect(h.state.isAtBottom).toBe(true);
  });

  it('does not re-stick without down intent', async () => {
    const h = build();
    h.state.escaped = true;
    h.scrollEl.scrollTop = 400;
    h.scrollEl.dispatchEvent(new Event('scroll'));

    h.state.distanceFromBottom = 0;
    h.scrollEl.scrollTop = 500;
    h.scrollEl.dispatchEvent(new Event('scroll'));
    await settleDeferredPass();

    expect(h.state.escaped).toBe(true);
  });

  it('a resize-correlated scroll does not re-stick without down intent', async () => {
    const h = build();
    h.state.escaped = true;
    h.state.resizeCorrelated = true;
    h.scrollEl.scrollTop = 400;
    h.scrollEl.dispatchEvent(new Event('scroll'));

    h.state.distanceFromBottom = 0;
    h.scrollEl.scrollTop = 500;
    h.scrollEl.dispatchEvent(new Event('scroll'));
    await settleDeferredPass();

    expect(h.state.escaped).toBe(true);
  });

  it('skips the deferred pass entirely while following', async () => {
    const h = build();
    h.state.escaped = false;

    h.scrollEl.scrollTop = 500;
    h.scrollEl.dispatchEvent(new Event('scroll'));
    await settleDeferredPass();

    expect(h.state.escaped).toBe(false);
    expect(h.state.isAtBottom).toBe(true);
  });
});

describe('restore consent', () => {
  it('armRestoreSnap escapes defensively and arms consent, in that order', () => {
    const h = build();

    h.intent.armRestoreSnap();

    expect(h.state.escaped).toBe(true);
    expect(h.intent.restoreConsentArmed()).toBe(true);
  });

  it('a user escape invalidates pending consent', () => {
    const h = build();
    h.intent.armRestoreSnap();

    wheel(h.scrollEl, { deltaY: -10 });

    expect(h.intent.restoreConsentArmed()).toBe(false);
  });

  it('a down intent invalidates pending consent', () => {
    const h = build();
    h.intent.armRestoreSnap();

    wheel(h.scrollEl, { deltaY: 10 });

    expect(h.intent.restoreConsentArmed()).toBe(false);
  });

  it('survives detach — attach() detaches up front, and that must not wipe consent', () => {
    const h = build();
    h.intent.armRestoreSnap();

    h.intent.detach();

    expect(h.intent.restoreConsentArmed()).toBe(true);
  });
});

describe('detach', () => {
  it('stops interpreting events', () => {
    const h = build();

    h.intent.detach();
    wheel(h.scrollEl, { deltaY: -10 });

    expect(h.state.escaped).toBe(false);
  });

  it('clears the down-intent window and any drag session', () => {
    const h = build();
    h.state.escaped = true;
    wheel(h.scrollEl, { deltaY: 10 });

    h.intent.detach();

    expect(h.intent.debugState().recentDownIntentActive).toBe(false);
    expect(h.intent.debugState().scrollbarDragSessionActive).toBe(false);
  });
});
