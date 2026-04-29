import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { createScrollIntentCore, type ScrollIntentCore } from './scrollIntentCore.svelte';

function fireWheel(el: HTMLElement, deltaY: number): void {
  el.dispatchEvent(new WheelEvent('wheel', { deltaY, bubbles: true }));
}

function fireKey(el: HTMLElement, key: string): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
}

function fireTouchStart(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchstart', { bubbles: true, touches: [{ clientY } as Touch] }));
}

function fireTouchMove(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchmove', { bubbles: true, touches: [{ clientY } as Touch] }));
}

function fireTouchEnd(el: HTMLElement): void {
  el.dispatchEvent(new TouchEvent('touchend', { bubbles: true, touches: [] }));
}

describe('createScrollIntentCore', () => {
  let core: ScrollIntentCore;
  let el: HTMLDivElement;
  let detachListeners: (() => void) | null = null;

  beforeEach(() => {
    el = document.createElement('div');
    document.body.appendChild(el);
    core = createScrollIntentCore();
    detachListeners = core.bindGestureListeners(el);
  });

  afterEach(() => {
    detachListeners?.();
    detachListeners = null;
    el.remove();
  });

  describe('initial state', () => {
    it('starts sticky', () => {
      expect(core.intent).toBe('stick');
      expect(core.isSticky).toBe(true);
    });

    it('reports no pointer down, not paused, no recent down-gesture', () => {
      expect(core.isPointerDown()).toBe(false);
      expect(core.isPaused()).toBe(false);
      expect(core.inDownGestureWindow()).toBe(false);
      expect(core.canAutoScroll()).toBe(true);
    });
  });

  describe('setIntent', () => {
    it('mutates intent', () => {
      core.setIntent('free');
      expect(core.intent).toBe('free');
      core.setIntent('stick');
      expect(core.intent).toBe('stick');
    });
  });

  describe('canAutoScroll', () => {
    it('is false when intent is free', () => {
      core.setIntent('free');
      expect(core.canAutoScroll()).toBe(false);
    });

    it('is false when pointer is down', () => {
      core.setPointerDown(true);
      expect(core.canAutoScroll()).toBe(false);
    });

    it('is false when paused', () => {
      core.pauseAutoScroll();
      expect(core.canAutoScroll()).toBe(false);
    });
  });

  describe('wheel gesture', () => {
    it('wheel-up flips to free', () => {
      fireWheel(el, -50);
      expect(core.intent).toBe('free');
    });

    it('wheel-down notes a down-gesture but does not flip from sticky', () => {
      fireWheel(el, 50);
      expect(core.intent).toBe('stick');
      expect(core.inDownGestureWindow()).toBe(true);
    });
  });

  describe('keyboard gesture', () => {
    it.each(['PageUp', 'ArrowUp', 'Home'])('%s flips to free', (key) => {
      fireKey(el, key);
      expect(core.intent).toBe('free');
    });

    it.each(['PageDown', 'ArrowDown', 'End'])('%s notes a down-gesture', (key) => {
      fireKey(el, key);
      expect(core.inDownGestureWindow()).toBe(true);
    });

    it('non-navigation keys do not affect intent or down-gesture', () => {
      fireKey(el, 'a');
      expect(core.intent).toBe('stick');
      expect(core.inDownGestureWindow()).toBe(false);
    });
  });

  describe('touch gesture', () => {
    it('finger moves down (dy > 1) flips to free', () => {
      fireTouchStart(el, 100);
      fireTouchMove(el, 120);
      expect(core.intent).toBe('free');
    });

    it('finger moves up (dy < -1) notes a down-gesture', () => {
      fireTouchStart(el, 200);
      fireTouchMove(el, 180);
      expect(core.intent).toBe('stick');
      expect(core.inDownGestureWindow()).toBe(true);
    });

    it('subthreshold finger movement does not flip intent', () => {
      fireTouchStart(el, 100);
      fireTouchMove(el, 100.5);
      expect(core.intent).toBe('stick');
      expect(core.inDownGestureWindow()).toBe(false);
    });

    it('touchend resets the touch baseline', () => {
      fireTouchStart(el, 100);
      fireTouchMove(el, 120);
      core.setIntent('stick');
      fireTouchEnd(el);
      // After touchend, a fresh touchstart starts a new dy chain.
      fireTouchMove(el, 200); // ignored — no active touch baseline
      expect(core.intent).toBe('stick');
    });
  });

  describe('down-gesture window', () => {
    it('returns true immediately after a down-gesture', () => {
      fireWheel(el, 50);
      expect(core.inDownGestureWindow()).toBe(true);
    });

    it('clearDownGesture resets the window', () => {
      fireWheel(el, 50);
      expect(core.inDownGestureWindow()).toBe(true);
      core.clearDownGesture();
      expect(core.inDownGestureWindow()).toBe(false);
    });

    it('expires after gestureWindowMs', async () => {
      detachListeners?.();
      core = createScrollIntentCore({ gestureWindowMs: 30 });
      detachListeners = core.bindGestureListeners(el);
      fireWheel(el, 50);
      expect(core.inDownGestureWindow()).toBe(true);
      await new Promise((r) => setTimeout(r, 60));
      expect(core.inDownGestureWindow()).toBe(false);
    });
  });

  describe('pauseAutoScroll', () => {
    it('returns an idempotent dispose function', () => {
      const release = core.pauseAutoScroll();
      expect(core.isPaused()).toBe(true);
      release();
      expect(core.isPaused()).toBe(false);
      // idempotent — a second call must not double-decrement.
      release();
      expect(core.isPaused()).toBe(false);
    });

    it('depth-counts so two leases must both release', () => {
      const r1 = core.pauseAutoScroll();
      const r2 = core.pauseAutoScroll();
      expect(core.isPaused()).toBe(true);
      r1();
      expect(core.isPaused()).toBe(true);
      r2();
      expect(core.isPaused()).toBe(false);
    });

    it('pauseDepth is bounded at 0 — a stray release does not produce negative state', () => {
      const release = core.pauseAutoScroll();
      release();
      release(); // idempotent on the same dispose closure
      // A fresh pause should still pause cleanly.
      core.pauseAutoScroll();
      expect(core.isPaused()).toBe(true);
    });
  });

  describe('setPointerDown', () => {
    it('mutates the pointer-down flag', () => {
      core.setPointerDown(true);
      expect(core.isPointerDown()).toBe(true);
      core.setPointerDown(false);
      expect(core.isPointerDown()).toBe(false);
    });
  });

  describe('listener lifecycle', () => {
    it('detacher removes wheel/key/touch listeners', () => {
      detachListeners?.();
      detachListeners = null;
      fireWheel(el, -50);
      // Without listeners, gestures have no effect.
      expect(core.intent).toBe('stick');
    });

    it('binding listeners on a different element works independently', () => {
      const other = document.createElement('div');
      document.body.appendChild(other);
      try {
        const detachOther = core.bindGestureListeners(other);
        try {
          fireWheel(other, -50);
          expect(core.intent).toBe('free');
        } finally {
          detachOther();
        }
      } finally {
        other.remove();
      }
    });
  });

  describe('resetTransientState', () => {
    it('clears pointer-down, down-gesture, and touch state', () => {
      core.setPointerDown(true);
      fireWheel(el, 50);
      fireTouchStart(el, 100);
      core.resetTransientState();
      expect(core.isPointerDown()).toBe(false);
      expect(core.inDownGestureWindow()).toBe(false);
      // Touch state is reset — a subsequent touchmove without a fresh
      // touchstart should be ignored (touchStartY is null).
      fireTouchMove(el, 200);
      expect(core.intent).toBe('stick');
    });

    it('does NOT reset intent or pause-depth', () => {
      core.setIntent('free');
      core.pauseAutoScroll();
      core.resetTransientState();
      expect(core.intent).toBe('free');
      expect(core.isPaused()).toBe(true);
    });
  });
});
