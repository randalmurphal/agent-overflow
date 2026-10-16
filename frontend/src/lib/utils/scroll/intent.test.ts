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
  isSelectingInside,
  resetScrollIntentModuleStateForTest,
  type ScrollIntent,
  type ScrollIntentDeps,
} from './intent';
import { AUTO_FOLLOW_BOTTOM_EPSILON_PX } from './resolver';
import { registerNestedScroller } from './wheelAttribution';
import { observedScrollTopFromEvent } from './eventObservation';

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
    restoreConsentArmed: boolean;
    distanceFromBottom: number;
    resizeCorrelated: boolean;
    resizeDifference: number;
    lastExplained: number | null;
  };
  spring: {
    requestStop: ReturnType<typeof vi.fn>;
    cancel: ReturnType<typeof vi.fn>;
    clearStopRequest: ReturnType<typeof vi.fn>;
    clearStructuralAppend: ReturnType<typeof vi.fn>;
  };
  refreshIsNearBottom: ReturnType<typeof vi.fn>;
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
    restoreConsentArmed: false,
    // Far enough from the bottom that a down-intent does NOT trip the
    // immediate re-stick branch; the tests that want that branch set it to 0.
    distanceFromBottom: 100,
    resizeCorrelated: false,
    resizeDifference: 0,
    lastExplained: null as number | null,
  };

  const spring = {
    requestStop: vi.fn(),
    cancel: vi.fn(),
    clearStopRequest: vi.fn(),
    clearStructuralAppend: vi.fn(),
  };
  const refreshIsNearBottom = vi.fn(() => state.distanceFromBottom);
  const noteUserScroll = vi.fn();
  const followBottom = vi.fn();

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
    restoreConsentArmed: () => state.restoreConsentArmed,
    setRestoreConsentArmed: (next) => {
      state.restoreConsentArmed = next;
    },
    isNearBottom: () => state.distanceFromBottom <= AUTO_FOLLOW_BOTTOM_EPSILON_PX,
    pauseDepth: () => 0,
    distanceFromBottom: () => state.distanceFromBottom,
    refreshIsNearBottom,
    spring,
    sampleResizeCorrelation: () => state.resizeCorrelated,
    resizeDifferenceNow: () => state.resizeDifference,
    noteUserScroll,
    lastExplainedScrollTop: () => state.lastExplained,
    followBottom,
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

function touch(target: Element, type: string, clientY: number, clientX = 0): void {
  const event = new Event(type, { bubbles: true });
  Object.defineProperty(event, 'touches', {
    value: [{ clientY, clientX }],
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

  it('a sideways drag (panning a wide block) does not escape on its vertical jitter', () => {
    const h = build();

    touch(h.child, 'touchstart', 100, 100);
    touch(h.child, 'touchmove', 104, 160);
    touch(h.child, 'touchmove', 107, 220);

    expect(h.state.escaped).toBe(false);
  });

  it('a drag that turns vertical escapes from where it turned', () => {
    const h = build();

    touch(h.child, 'touchstart', 100, 100);
    touch(h.child, 'touchmove', 102, 160);
    expect(h.state.escaped).toBe(false);
    touch(h.child, 'touchmove', 140, 165);

    expect(h.state.escaped).toBe(true);
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
  it('reads the event position once and shares the tagged observation', () => {
    const h = build();
    let top = 600;
    let reads = 0;
    Object.defineProperty(h.scrollEl, 'scrollTop', {
      configurable: true,
      get: () => {
        reads++;
        return top;
      },
      set: (next: number) => {
        top = next;
      },
    });
    h.intent.noteProgrammaticWrite(top);
    const event = new Event('scroll');

    h.scrollEl.dispatchEvent(event);

    expect(reads).toBe(1);
    expect(observedScrollTopFromEvent(event)).toBe(600);
  });

  it('invalidates programmatic attribution before reader input moves the surface', () => {
    const h = build();
    let top = 600;
    let reads = 0;
    Object.defineProperty(h.scrollEl, 'scrollTop', {
      configurable: true,
      get: () => {
        reads++;
        return top;
      },
      set: (next: number) => {
        top = next;
      },
    });
    h.intent.noteProgrammaticWrite(top);
    wheel(h.scrollEl, { deltaY: 10 });
    top = 640;
    const event = new Event('scroll');

    h.scrollEl.dispatchEvent(event);

    expect(reads).toBe(1);
    expect(observedScrollTopFromEvent(event)).toBe(640);
  });

  it('does not substitute an authored offset for an external scroll that raced its event', () => {
    const h = build();
    let top = 600;
    Object.defineProperty(h.scrollEl, 'scrollTop', {
      configurable: true,
      get: () => top,
      set: (next: number) => {
        top = next;
      },
    });
    h.intent.noteProgrammaticWrite(top);

    // Native find-in-page, focus scrolling, and browser clamps can move a
    // scroller without first delivering one of our wheel/key/pointer events.
    // Scroll events may coalesce, so that move can land before the event for
    // the authored write above.
    top = 240;
    const event = new Event('scroll');
    h.scrollEl.dispatchEvent(event);

    expect(observedScrollTopFromEvent(event)).toBe(240);
    expect(h.refreshIsNearBottom).toHaveBeenCalledOnce();
  });

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

  it('evicts the oldest write when the fixed token ring reaches its cap', () => {
    const h = build();
    for (let top = 0; top <= 128; top += 1) {
      h.intent.noteProgrammaticWrite(top);
    }
    h.scrollEl.scrollTop = 0;

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

  it('ignores a descendant scroller\'s event that the capture-phase listener receives', () => {
    const h = build();
    h.scrollEl.scrollTop = 600;
    const nested = document.createElement('div');
    h.child.appendChild(nested);

    // Scroll does not bubble; the capture phase still visits the pane.
    nested.dispatchEvent(new Event('scroll'));

    expect(h.noteUserScroll).not.toHaveBeenCalled();
    expect(h.refreshIsNearBottom).not.toHaveBeenCalled();
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

  it('does not record the browser clamp onto a shrunken scroll range, even before its geometry sample', () => {
    const h = build();
    // The ledger holds the pinned bottom; a viewport that grew by a banner
    // row clamps scrollTop beneath it before any sample classifies the resize.
    h.state.lastExplained = 500;
    h.state.distanceFromBottom = 0;
    h.scrollEl.scrollTop = 468;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.noteUserScroll).not.toHaveBeenCalled();
  });

  it('records a gesture that lands at the bottom from above it', () => {
    const h = build();
    h.state.lastExplained = 400;
    h.state.distanceFromBottom = 0;
    h.scrollEl.scrollTop = 500;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.noteUserScroll).toHaveBeenCalledWith(500);
  });

  it('records a gesture that leaves the bottom for a position short of it', () => {
    const h = build();
    h.state.lastExplained = 500;
    h.state.distanceFromBottom = 40;
    h.scrollEl.scrollTop = 460;

    h.scrollEl.dispatchEvent(new Event('scroll'));

    expect(h.noteUserScroll).toHaveBeenCalledWith(460);
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

  describe('a down gesture whose scroll lands after the down-intent window', () => {
    // A loaded host can deliver the landing scroll event long after the
    // wheel; the window is 250 ms.
    const PAST_WINDOW_MS = 300;
    const pastWindow = () => new Promise((r) => setTimeout(r, PAST_WINDOW_MS));

    // The wheel asks for `deltaY` px; the view sits 100 px above the bottom.
    function wheeledDownWhileEscaped(deltaY = 100): Harness {
      const h = build();
      h.state.escaped = true;
      h.state.distanceFromBottom = 100;
      h.scrollEl.scrollTop = 400;
      h.scrollEl.dispatchEvent(new Event('scroll'));
      wheel(h.scrollEl, { deltaY });
      return h;
    }

    function landAt(h: Harness, distanceFromBottom: number, top = 500): void {
      h.state.distanceFromBottom = distanceFromBottom;
      h.scrollEl.scrollTop = top;
      h.scrollEl.dispatchEvent(new Event('scroll'));
    }

    it('re-sticks when it lands at the bottom', async () => {
      const h = wheeledDownWhileEscaped();
      await pastWindow();
      landAt(h, 0);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(false);
      expect(h.state.isAtBottom).toBe(true);
    });

    it('re-sticks when the landing is resize-correlated', async () => {
      const h = wheeledDownWhileEscaped();
      await pastWindow();
      h.state.resizeCorrelated = true;
      landAt(h, 0);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(false);
    });

    it('stays escaped when it lands outside the re-stick epsilon', async () => {
      const h = wheeledDownWhileEscaped();
      await pastWindow();
      landAt(h, AUTO_FOLLOW_BOTTOM_EPSILON_PX + 1);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(true);
    });

    it('stays escaped after an upward escape in between', async () => {
      const h = wheeledDownWhileEscaped();
      wheel(h.scrollEl, { deltaY: -10 });
      await pastWindow();
      landAt(h, 0);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(true);
    });

    it('stays escaped when an escape lands before the deferred pass', async () => {
      const h = wheeledDownWhileEscaped();
      await pastWindow();
      landAt(h, 0);
      wheel(h.scrollEl, { deltaY: -10 });
      await settleDeferredPass();

      expect(h.state.escaped).toBe(true);
    });

    it('is consumed by a landing short of the bottom that spends it, so a later scroll with no input to the end stays escaped', async () => {
      // Find-in-page, a focus scroll or an anchor moves the view with no
      // input of its own; the earlier gesture already landed.
      const h = wheeledDownWhileEscaped(40);
      landAt(h, 60, 440);
      await settleDeferredPass();
      expect(h.state.escaped).toBe(true);

      await pastWindow();
      landAt(h, 0, 500);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(true);
    });

    it('is not consumed by a scroll the controller wrote', async () => {
      const h = wheeledDownWhileEscaped();
      h.intent.noteProgrammaticWrite(450);
      h.scrollEl.scrollTop = 450;
      h.scrollEl.dispatchEvent(new Event('scroll'));
      await pastWindow();
      landAt(h, 0, 500);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(false);
    });

    it('is armed again by a second wheel after its first landing', async () => {
      const h = wheeledDownWhileEscaped(40);
      landAt(h, 60, 440);
      await settleDeferredPass();
      await pastWindow();
      wheel(h.scrollEl, { deltaY: 60 });
      await pastWindow();
      landAt(h, 0, 500);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(false);
    });

    it('stays escaped after the controller placed the view at the bottom and escaped again', async () => {
      const h = wheeledDownWhileEscaped();
      h.intent.clearRecentDownIntent();
      h.intent.setEscapedFromLock(true);
      await pastWindow();
      landAt(h, 0);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(true);
    });

    describe('a smooth-scrolled wheel that lands over several events', () => {
      it('re-sticks when the first late event is mid-animation and the next reaches the end', async () => {
        const h = wheeledDownWhileEscaped();
        await pastWindow();
        // The first event a stalled main thread delivers is mid-animation;
        // the next is the end of the same gesture.
        landAt(h, 40, 460);
        await new Promise((r) => setTimeout(r, 16));
        landAt(h, 0, 500);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(false);
      });

      it('is cleared once its events spend the travel short of the bottom', async () => {
        const h = wheeledDownWhileEscaped(60);
        await pastWindow();
        landAt(h, 80, 420);
        landAt(h, 40, 460);
        await settleDeferredPass();
        expect(h.state.escaped).toBe(true);

        // No input: a find-in-page or focus scroll reaches the end.
        landAt(h, 0, 500);
        await settleDeferredPass();
        expect(h.state.escaped).toBe(true);
      });

      it('does not take a jump further than its travel as its landing', async () => {
        const h = build();
        h.state.escaped = true;
        h.state.distanceFromBottom = 1000;
        h.scrollEl.scrollTop = 400;
        h.scrollEl.dispatchEvent(new Event('scroll'));
        wheel(h.scrollEl, { deltaY: 100 });
        await pastWindow();
        // A find-in-page jump lands at the end, far beyond one notch.
        landAt(h, 0, 1400);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(true);
      });

      it('is cleared when its travel is spent exactly', async () => {
        const h = build();
        h.state.escaped = true;
        h.state.distanceFromBottom = 67;
        h.scrollEl.scrollTop = 400;
        h.scrollEl.dispatchEvent(new Event('scroll'));
        wheel(h.scrollEl, { deltaY: 60 });
        await pastWindow();
        landAt(h, 7, 460);
        await settleDeferredPass();
        // A few pixels with no input reach the re-stick epsilon.
        landAt(h, 4, 463);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(true);
      });

      it('is cleared by an upward scroll mid-gesture', async () => {
        const h = wheeledDownWhileEscaped();
        landAt(h, 50, 450);
        landAt(h, 70, 430);
        await pastWindow();
        landAt(h, 0, 500);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(true);
      });

      it('is cleared by a native clamp landing short of where the wheel aimed', async () => {
        const h = wheeledDownWhileEscaped();
        await pastWindow();
        // The range shrank beneath the view: the browser clamps it to the
        // new maximum, an explained position beyond it.
        h.state.lastExplained = 600;
        landAt(h, 0, 450);
        await settleDeferredPass();
        expect(h.state.escaped).toBe(true);

        h.state.lastExplained = null;
        landAt(h, 0, 480);
        await settleDeferredPass();
        expect(h.state.escaped).toBe(true);
      });

      it('is extended by a second wheel', async () => {
        const h = wheeledDownWhileEscaped(40);
        landAt(h, 70, 430);
        wheel(h.scrollEl, { deltaY: 60 });
        await pastWindow();
        landAt(h, 0, 500);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(false);
      });

      it('measures a line-mode wheel in lines', async () => {
        const h = build();
        h.state.escaped = true;
        h.state.distanceFromBottom = 48;
        h.scrollEl.scrollTop = 400;
        h.scrollEl.dispatchEvent(new Event('scroll'));
        wheel(h.scrollEl, { deltaY: 3, deltaMode: WheelEvent.DOM_DELTA_LINE });
        await pastWindow();
        landAt(h, 0, 448);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(false);
      });
    });

    describe('an anchoring shift while a landing is pending', () => {
      function shiftBy(h: Harness, px: number, distanceFromBottom: number): void {
        h.state.resizeCorrelated = true;
        h.state.resizeDifference = px;
        landAt(h, distanceFromBottom, h.scrollEl.scrollTop + px);
        h.state.resizeCorrelated = false;
        h.state.resizeDifference = 0;
      }

      it('spends none of the travel, so the later landing at the bottom re-sticks', async () => {
        const h = wheeledDownWhileEscaped();
        // Rows inserted above move the view by the gesture's own size.
        shiftBy(h, 100, 100);
        await settleDeferredPass();
        await pastWindow();
        landAt(h, 0, 600);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(false);
      });

      it('is not the landing when it reaches the bottom itself', async () => {
        const h = wheeledDownWhileEscaped();
        await pastWindow();
        shiftBy(h, 100, 0);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(true);
      });
    });

    describe('End and PageDown carry their travel', () => {
      function keyedDownWhileEscaped(k: string, distanceFromBottom: number): Harness {
        const h = build();
        h.state.escaped = true;
        h.state.distanceFromBottom = distanceFromBottom;
        h.scrollEl.scrollTop = 400;
        h.scrollEl.dispatchEvent(new Event('scroll'));
        key(h.scrollEl, k);
        return h;
      }

      it('a late smooth End re-sticks at its final event', async () => {
        const h = keyedDownWhileEscaped('End', 1000);
        await pastWindow();
        landAt(h, 700, 700);
        landAt(h, 300, 1100);
        landAt(h, 0, 1400);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(false);
      });

      it('an upward event after End ends it', async () => {
        const h = keyedDownWhileEscaped('End', 1000);
        landAt(h, 700, 700);
        landAt(h, 750, 650);
        await pastWindow();
        landAt(h, 0, 1400);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(true);
      });

      it('a controller write ends an End', async () => {
        const h = keyedDownWhileEscaped('End', 1000);
        h.intent.noteProgrammaticWrite(500);
        h.scrollEl.scrollTop = 500;
        h.scrollEl.dispatchEvent(new Event('scroll'));
        await pastWindow();
        landAt(h, 0, 1400);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(true);
      });

      it('a PageDown that lands short of the bottom is cleared', async () => {
        // A viewport is 500 px; the browser's page step is shorter.
        const h = keyedDownWhileEscaped('PageDown', 2000);
        await pastWindow();
        landAt(h, 1760, 640);
        landAt(h, 1563, 837);
        await settleDeferredPass();
        // No input: a find-in-page or focus scroll reaches the end.
        landAt(h, 0, 2400);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(true);
      });

      it('a PageDown that reaches the bottom re-sticks', async () => {
        const h = keyedDownWhileEscaped('PageDown', 300);
        await pastWindow();
        landAt(h, 150, 550);
        landAt(h, 0, 700);
        await settleDeferredPass();

        expect(h.state.escaped).toBe(false);
      });
    });

    it('a key gesture is spent by its first scroll event, wherever it lands', async () => {
      const h = build();
      h.state.escaped = true;
      h.state.distanceFromBottom = 100;
      h.scrollEl.scrollTop = 400;
      h.scrollEl.dispatchEvent(new Event('scroll'));
      key(h.scrollEl, 'ArrowDown');
      landAt(h, 60, 440);
      await settleDeferredPass();
      await pastWindow();
      landAt(h, 0, 500);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(true);
    });

    it('a key gesture landing at the bottom late re-sticks', async () => {
      const h = build();
      h.state.escaped = true;
      h.state.distanceFromBottom = 100;
      h.scrollEl.scrollTop = 400;
      h.scrollEl.dispatchEvent(new Event('scroll'));
      key(h.scrollEl, 'End');
      await pastWindow();
      landAt(h, 0, 500);
      await settleDeferredPass();

      expect(h.state.escaped).toBe(false);
    });
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

describe('selection tracking: primary button state', () => {
  // `isSelectingInside` needs a held primary button AND a selection whose
  // range shares ancestry with the scroller. The selection half is stubbed;
  // these pin the button half, which is READ from `event.buttons` on pointer
  // events rather than latched from mousedown/mouseup. The latch stuck on a
  // native drag-and-drop (mousedown, then no mouseup or click) and paused
  // every spring until the next click (bug-report-20260903T221457Z).
  const pointer = (type: string, init: PointerEventInit) =>
    document.dispatchEvent(new PointerEvent(type, { bubbles: true, pointerType: 'mouse', ...init }));

  function stubSelectionInside(scrollEl: HTMLElement): void {
    const range = { commonAncestorContainer: scrollEl } as unknown as Range;
    vi.spyOn(window, 'getSelection').mockReturnValue({
      rangeCount: 1,
      getRangeAt: () => range,
    } as unknown as Selection);
  }

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('a held primary button with a selection crossing the scroller is selecting', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);

    pointer('pointerdown', { buttons: 1 });

    expect(isSelectingInside(h.scrollEl)).toBe(true);
  });

  it('no button held is never selecting, whatever the selection says', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);

    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });

  it('a release clears it', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);
    pointer('pointerdown', { buttons: 1 });

    pointer('pointerup', { buttons: 0 });

    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });

  it('a native drag starting clears it, because no release will follow', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);
    pointer('pointerdown', { buttons: 1 });

    document.dispatchEvent(new Event('dragstart', { bubbles: true }));

    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });

  it('a pointer move reporting the button up clears it (a release that never arrived)', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);
    pointer('pointerdown', { buttons: 1 });

    pointer('pointermove', { buttons: 0 });

    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });

  // A drag starting inside the scroller from anything but an explicit
  // draggable is vetoed: selected timeline text has no drop target in the
  // app, and picking it up handed the browser process a drag session the
  // page could not end. The veto keeps the button held for selection.
  describe('dragstart inside the scroller', () => {
    // happy-dom's DragEvent carries no button bitmask; a real one is a
    // MouseEvent, and that is the shape the handler reads.
    const dragstart = (target: Element, init: MouseEventInit = {}) => {
      const e = new MouseEvent('dragstart', { bubbles: true, cancelable: true, buttons: 1, ...init });
      target.dispatchEvent(e);
      return e;
    };

    it('from timeline content is canceled and leaves the button held', () => {
      const h = build();
      stubSelectionInside(h.scrollEl);
      pointer('pointerdown', { buttons: 1 });

      const e = dragstart(h.child);

      expect(e.defaultPrevented).toBe(true);
      expect(isSelectingInside(h.scrollEl)).toBe(true);
    });

    it('from an explicit draggable inside the scroller is left alone', () => {
      const h = build();
      stubSelectionInside(h.scrollEl);
      pointer('pointerdown', { buttons: 1 });
      const handle = document.createElement('div');
      handle.setAttribute('draggable', 'true');
      const grip = document.createElement('span');
      handle.append(grip);
      h.scrollEl.append(handle);

      const e = dragstart(grip);

      expect(e.defaultPrevented).toBe(false);
      expect(isSelectingInside(h.scrollEl)).toBe(false);
    });

    it('outside the scroller is left alone', () => {
      build();
      const outside = document.createElement('div');
      document.body.append(outside);
      try {
        const e = dragstart(outside);
        expect(e.defaultPrevented).toBe(false);
      } finally {
        outside.remove();
      }
    });

    it('is released with detach', () => {
      const h = build();
      h.intent.detach();

      const e = dragstart(h.child);

      expect(e.defaultPrevented).toBe(false);
    });
  });

  it('a pointer move reporting the button still down keeps it', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);
    pointer('pointerdown', { buttons: 1 });

    pointer('pointermove', { buttons: 1 });

    expect(isSelectingInside(h.scrollEl)).toBe(true);
  });

  it('a pointer cancel clears it', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);
    pointer('pointerdown', { buttons: 1 });

    pointer('pointercancel', { buttons: 0 });

    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });

  it('the window losing focus clears it', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);
    pointer('pointerdown', { buttons: 1 });

    window.dispatchEvent(new Event('blur'));

    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });

  it('a secondary or middle button is not a selection drag', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);

    pointer('pointerdown', { buttons: 2 });
    expect(isSelectingInside(h.scrollEl)).toBe(false);
    pointer('pointerdown', { buttons: 4 });
    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });

  it('a finger on the timeline is not a selection drag', () => {
    const h = build();
    stubSelectionInside(h.scrollEl);

    pointer('pointerdown', { buttons: 1, pointerType: 'touch' });

    expect(isSelectingInside(h.scrollEl)).toBe(false);
  });
});
