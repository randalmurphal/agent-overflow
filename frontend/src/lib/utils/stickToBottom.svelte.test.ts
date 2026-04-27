import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { nextFrame } from '../../test/helpers/scrollDom';
import { createStickToBottomController, type StickToBottomController } from './stickToBottom.svelte';

// Geometry holder lets each test mutate scrollHeight / clientHeight to
// simulate content growth without rebuilding the element. The defineProperty
// getters re-read from this object every time.
type Geometry = {
  scrollHeight: number;
  clientHeight: number;
};

function attachGeometry(el: HTMLElement, geo: Geometry): void {
  Object.defineProperty(el, 'scrollHeight', {
    configurable: true,
    get: () => geo.scrollHeight,
  });
  Object.defineProperty(el, 'clientHeight', {
    configurable: true,
    get: () => geo.clientHeight,
  });
}

function fireScroll(el: HTMLElement): void {
  el.dispatchEvent(new Event('scroll'));
}

function fireWheel(el: HTMLElement, deltaY: number): void {
  el.dispatchEvent(new WheelEvent('wheel', { deltaY, bubbles: true }));
}

function fireKey(el: HTMLElement, key: string): void {
  el.dispatchEvent(new KeyboardEvent('keydown', { key, bubbles: true }));
}

function firePointerDown(el: HTMLElement): void {
  el.dispatchEvent(new PointerEvent('pointerdown', { pointerId: 1, bubbles: true }));
}

function firePointerUp(el: HTMLElement): void {
  el.dispatchEvent(new PointerEvent('pointerup', { pointerId: 1, bubbles: true }));
}

function fireTouchStart(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchstart', { bubbles: true, touches: [{ clientY } as Touch] }));
}

function fireTouchMove(el: HTMLElement, clientY: number): void {
  el.dispatchEvent(new TouchEvent('touchmove', { bubbles: true, touches: [{ clientY } as Touch] }));
}

function fireClickCapture(target: HTMLElement): void {
  target.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true }));
}

function waitMs(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

describe('createStickToBottomController', () => {
  let container: HTMLDivElement;
  let geo: Geometry;
  let controller: StickToBottomController;

  beforeEach(() => {
    geo = { scrollHeight: 1000, clientHeight: 600 };
    container = document.createElement('div');
    attachGeometry(container, geo);
    container.scrollTop = 400; // exactly at bottom: 1000 - 600 - 400 = 0.
    document.body.appendChild(container);
    controller = createStickToBottomController({ getContainer: () => container });
    controller.attach();
  });

  afterEach(() => {
    controller.destroy();
    container.remove();
  });

  describe('initial state', () => {
    it('starts in stick state', () => {
      expect(controller.intent).toBe('stick');
      expect(controller.isSticky).toBe(true);
    });
  });

  describe('intent transitions — never derived from geometry', () => {
    it('pure scrollHeight growth does not transition intent', () => {
      geo.scrollHeight = 2000;
      fireScroll(container);
      expect(controller.intent).toBe('stick');
    });

    it('programmatic scrollTop change does not transition intent', () => {
      // Simulate a programmatic scroll that lands off-bottom; intent
      // must stay sticky because no user gesture happened.
      container.scrollTop = 100;
      fireScroll(container);
      expect(controller.intent).toBe('stick');
    });
  });

  describe('up-gestures flip intent free immediately', () => {
    it('wheel-up flips intent to free', () => {
      fireWheel(container, -50);
      expect(controller.intent).toBe('free');
    });

    it('keydown PageUp flips intent to free', () => {
      fireKey(container, 'PageUp');
      expect(controller.intent).toBe('free');
    });

    it('keydown ArrowUp flips intent to free', () => {
      fireKey(container, 'ArrowUp');
      expect(controller.intent).toBe('free');
    });

    it('keydown Home flips intent to free', () => {
      fireKey(container, 'Home');
      expect(controller.intent).toBe('free');
    });

    it('touchmove down (finger pulls content down) flips intent to free', () => {
      fireTouchStart(container, 100);
      fireTouchMove(container, 150);
      expect(controller.intent).toBe('free');
    });
  });

  describe('forceStick', () => {
    it('always sticks regardless of prior state', () => {
      fireWheel(container, -50);
      expect(controller.intent).toBe('free');
      controller.forceStick();
      expect(controller.intent).toBe('stick');
    });

    it('forceStick scrolls to bottom on next rAF', async () => {
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);
      geo.scrollHeight = 1100;
      controller.forceStick();
      await nextFrame();
      expect(container.scrollTop).toBe(500);
      expect(controller.intent).toBe('stick');
    });

    it('regression: forceStick + async growth (Shiki/KaTeX) lands at bottom via settle re-check', async () => {
      // The original Send-then-Implement bug: send fires forceStick;
      // user message arrives synchronously and the controller scrolls
      // to the new bottom; then ChatMarkdown finishes Shiki highlighting
      // a few frames later and grows row heights asynchronously. The
      // 200ms settle re-check must absorb that late growth.
      const c = createStickToBottomController({
        getContainer: () => container,
        settleTimeoutMs: 60,
      });
      c.attach();
      // Free state, off-bottom.
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);

      // Send fires: forceStick. Synchronous content arrived (user msg).
      geo.scrollHeight = 1100;
      c.forceStick();
      expect(container.scrollTop).toBe(500); // sync scroll lands.

      // A tick or two later, async Shiki resolves and grows the row.
      geo.scrollHeight = 1200;
      // We do NOT call notifyContentMaybeGrew — pretend the consumer
      // missed it. The settle re-check is the safety net.
      await waitMs(80);
      expect(container.scrollTop).toBe(600);
      expect(c.intent).toBe('stick');
      c.destroy();
    });

    it('forceStick while pointerDown defers the scroll to avoid yanking a drag', async () => {
      // User is mid-drag (e.g., dragging the scrollbar). A remote
      // event triggers forceStick. We must NOT scroll-jack them — they
      // chose their drag position. Intent flips sticky; scroll defers
      // until pointerup, when the resume path scrolls them to bottom
      // if intent is still sticky and content actually grew.
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);
      expect(controller.intent).toBe('free');

      firePointerDown(container);
      geo.scrollHeight = 1100;
      controller.forceStick();
      // No synchronous scroll — drag is held.
      expect(container.scrollTop).toBe(100);
      expect(controller.intent).toBe('stick');

      // Release the drag at the same scroll position (no net change).
      firePointerUp(container);
      // Now resume: notifyContentMaybeGrew should fire and rAF scrolls.
      await nextFrame();
      expect(container.scrollTop).toBe(500);
    });
  });

  describe('gesture-confirmed restick', () => {
    it('wheel-down landing within threshold of bottom resticks', () => {
      // Move to free.
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);
      expect(controller.intent).toBe('free');

      // Wheel-down + scroll back to bottom.
      fireWheel(container, 50);
      container.scrollTop = 400;
      fireScroll(container);
      expect(controller.intent).toBe('stick');
    });

    it('keydown PageDown landing at bottom resticks', () => {
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);

      fireKey(container, 'PageDown');
      container.scrollTop = 400;
      fireScroll(container);
      expect(controller.intent).toBe('stick');
    });

    it('touchmove up + scroll-to-bottom resticks', () => {
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);

      fireTouchStart(container, 200);
      fireTouchMove(container, 100); // finger moves up → content moves up → user wants down
      container.scrollTop = 400;
      fireScroll(container);
      expect(controller.intent).toBe('stick');
    });

    it('does not restick when scrollHeight grew this frame', () => {
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);
      expect(controller.intent).toBe('free');

      // wheel-down + content growth in the same frame.
      fireWheel(container, 50);
      geo.scrollHeight = 1100;
      container.scrollTop = 500; // would be at the new bottom
      fireScroll(container);
      // The race gate kicks in: scrollHeight changed since last scroll
      // event, so we treat this as content arriving simultaneously
      // rather than a user reaching the bottom by intent.
      expect(controller.intent).toBe('free');
    });

    it('does not restick when no recent down-gesture', () => {
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);

      // No gesture, just scroll back to bottom (e.g. from programmatic
      // scrollTop write or browser focus jump).
      container.scrollTop = 400;
      fireScroll(container);
      expect(controller.intent).toBe('free');
    });

    it('does not restick when down-gesture is older than gestureWindowMs', async () => {
      const c = createStickToBottomController({
        getContainer: () => container,
        gestureWindowMs: 50,
      });
      c.attach();
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);

      fireWheel(container, 50);
      // Wait beyond the gesture window.
      await waitMs(80);
      container.scrollTop = 400;
      fireScroll(container);
      expect(c.intent).toBe('free');
      c.destroy();
    });

    it('does not restick beyond threshold', () => {
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);

      fireWheel(container, 50);
      // Only halfway down — well outside the 8px threshold.
      container.scrollTop = 200;
      fireScroll(container);
      expect(controller.intent).toBe('free');
    });

    it('honors gestureRestick: false', () => {
      const c = createStickToBottomController({
        getContainer: () => container,
        gestureRestick: false,
      });
      c.attach();
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);
      expect(c.intent).toBe('free');

      fireWheel(container, 50);
      container.scrollTop = 400;
      fireScroll(container);
      expect(c.intent).toBe('free'); // gesture-confirmed restick disabled
      c.destroy();
    });
  });

  describe('pointerdown suspends auto-scroll', () => {
    it('content growth during pointerdown does not auto-scroll', async () => {
      firePointerDown(container);
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(400); // unchanged

      firePointerUp(container);
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500); // resumed
    });

    it('drag scroll up flips intent to free on pointerup', () => {
      firePointerDown(container);
      // Simulate dragging the scrollbar UP.
      container.scrollTop = 100;
      fireScroll(container); // scroll fires during drag
      firePointerUp(container);
      expect(controller.intent).toBe('free');
    });

    it('drag scroll down to bottom resticks on pointerup', () => {
      // Start in free state.
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);
      expect(controller.intent).toBe('free');

      firePointerDown(container);
      container.scrollTop = 400; // dragged to bottom
      fireScroll(container);
      firePointerUp(container);
      expect(controller.intent).toBe('stick');
    });
  });

  describe('notifyContentMaybeGrew', () => {
    it('scrolls to bottom on next rAF when sticky', async () => {
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500);
    });

    it('is a no-op when free', async () => {
      fireWheel(container, -50);
      container.scrollTop = 100;
      fireScroll(container);
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(100);
    });

    it('coalesces multiple rapid calls into one rAF', async () => {
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      controller.notifyContentMaybeGrew();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500);
    });

    it('respects intent flip during the rAF window', async () => {
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      // Flip intent before rAF fires.
      fireWheel(container, -50);
      await nextFrame();
      expect(container.scrollTop).toBe(400); // not auto-scrolled
    });
  });

  describe('pauseAutoScroll (depth-counted)', () => {
    it('suspends auto-scroll until release', async () => {
      const release = controller.pauseAutoScroll();
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(400);
      release();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500);
    });

    it('depth-counts re-entry', async () => {
      const r1 = controller.pauseAutoScroll();
      const r2 = controller.pauseAutoScroll();
      r1();
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(400); // still paused
      r2();
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500);
    });

    it('release is idempotent', async () => {
      const release = controller.pauseAutoScroll();
      release();
      release(); // no-op
      release(); // no-op
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500);
    });
  });

  describe('settle re-check (absorbs async layout)', () => {
    it('fires after settleTimeoutMs if still sticky and off-bottom', async () => {
      const c = createStickToBottomController({
        getContainer: () => container,
        settleTimeoutMs: 60,
      });
      c.attach();
      geo.scrollHeight = 1100;
      c.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500);
      // Simulate async layout that landed AFTER the rAF.
      geo.scrollHeight = 1200;
      // Intentionally do NOT call notifyContentMaybeGrew — the settle
      // timeout is what catches this.
      await waitMs(80);
      expect(container.scrollTop).toBe(600);
      c.destroy();
    });

    it('settle re-check bails if intent flipped during the window', async () => {
      const c = createStickToBottomController({
        getContainer: () => container,
        settleTimeoutMs: 60,
      });
      c.attach();
      geo.scrollHeight = 1100;
      c.notifyContentMaybeGrew();
      await nextFrame();
      expect(container.scrollTop).toBe(500);

      fireWheel(container, -50);
      geo.scrollHeight = 1200;
      await waitMs(80);
      // Should NOT have auto-scrolled despite content growth.
      expect(container.scrollTop).toBe(500);
      c.destroy();
    });
  });

  describe('click-anchor preservation', () => {
    function attachRectGetter(el: HTMLElement, top: { value: number }): void {
      Object.defineProperty(el, 'getBoundingClientRect', {
        configurable: true,
        value: () => ({
          top: top.value,
          bottom: top.value + 30,
          height: 30,
          left: 0,
          right: 100,
          width: 100,
          x: 0,
          y: top.value,
          toJSON: () => ({}),
        }),
      });
    }

    it('keeps clicked button visually fixed and flips intent free', async () => {
      const button = document.createElement('button');
      container.appendChild(button);
      const top = { value: 100 };
      attachRectGetter(button, top);

      fireClickCapture(button);
      // Layout shifted: button moved down 50px.
      top.value = 150;
      await nextFrame();
      expect(container.scrollTop).toBe(450); // 400 + 50
      expect(controller.intent).toBe('free');
    });

    it('summary element is a click-anchor target', async () => {
      const details = document.createElement('details');
      const summary = document.createElement('summary');
      details.appendChild(summary);
      container.appendChild(details);
      const top = { value: 100 };
      attachRectGetter(summary, top);

      fireClickCapture(summary);
      top.value = 130;
      await nextFrame();
      expect(container.scrollTop).toBe(430);
    });

    it('role=button is a click-anchor target', async () => {
      const div = document.createElement('div');
      div.setAttribute('role', 'button');
      container.appendChild(div);
      const top = { value: 100 };
      attachRectGetter(div, top);

      fireClickCapture(div);
      top.value = 110;
      await nextFrame();
      expect(container.scrollTop).toBe(410);
    });

    it('skips when target has data-scroll-anchor-ignore', async () => {
      const button = document.createElement('button');
      button.setAttribute('data-scroll-anchor-ignore', '');
      container.appendChild(button);
      const top = { value: 100 };
      attachRectGetter(button, top);

      fireClickCapture(button);
      top.value = 150;
      await nextFrame();
      expect(container.scrollTop).toBe(400);
      expect(controller.intent).toBe('stick');
    });

    it('skips when ancestor has data-scroll-anchor-ignore', async () => {
      const wrapper = document.createElement('div');
      wrapper.setAttribute('data-scroll-anchor-ignore', '');
      const button = document.createElement('button');
      wrapper.appendChild(button);
      container.appendChild(wrapper);
      const top = { value: 100 };
      attachRectGetter(button, top);

      fireClickCapture(button);
      top.value = 150;
      await nextFrame();
      expect(container.scrollTop).toBe(400);
      expect(controller.intent).toBe('stick');
    });

    it('skips the scroll adjustment when delta < 0.5px but still flips intent free', async () => {
      const button = document.createElement('button');
      container.appendChild(button);
      const top = { value: 100 };
      attachRectGetter(button, top);

      fireClickCapture(button);
      top.value = 100.2; // sub-pixel shift
      await nextFrame();
      expect(container.scrollTop).toBe(400);
      // Intent flip is the user-interaction signal — it fires even when
      // the layout adjustment is too small to be worth applying.
      expect(controller.intent).toBe('free');
    });

    it('skips when |delta| > clickAnchorMaxDelta', async () => {
      // Replace the default controller with one configured for a small
      // max-delta. Two controllers attached to the same container would
      // both run click-anchor, and the default's larger max would shift
      // scrollTop before the small-max controller's skip could be observed.
      controller.destroy();
      controller = createStickToBottomController({
        getContainer: () => container,
        clickAnchorMaxDelta: 100,
      });
      controller.attach();

      const button = document.createElement('button');
      container.appendChild(button);
      const top = { value: 100 };
      attachRectGetter(button, top);

      fireClickCapture(button);
      top.value = 500; // 400px shift, beyond max
      await nextFrame();
      expect(container.scrollTop).toBe(400); // unchanged
      expect(controller.intent).toBe('free'); // intent still flipped
    });

    it('skips when element disconnected before rAF', async () => {
      const button = document.createElement('button');
      container.appendChild(button);
      const top = { value: 100 };
      attachRectGetter(button, top);

      fireClickCapture(button);
      button.remove(); // disconnected
      await nextFrame();
      expect(container.scrollTop).toBe(400);
    });

    it('latest-wins: rapid clicks cancel prior rAF', async () => {
      const button1 = document.createElement('button');
      container.appendChild(button1);
      const button2 = document.createElement('button');
      container.appendChild(button2);
      const top1 = { value: 100 };
      const top2 = { value: 200 };
      attachRectGetter(button1, top1);
      attachRectGetter(button2, top2);

      fireClickCapture(button1);
      // Click 2 immediately, before button1's rAF fires.
      top1.value = 80; // would have shifted -20
      fireClickCapture(button2);
      top2.value = 300; // shifts +100

      await nextFrame();
      // Only button2's anchor adjustment applies.
      expect(container.scrollTop).toBe(500); // 400 + 100
    });
  });

  describe('attach / destroy lifecycle', () => {
    it('attach is idempotent', () => {
      controller.attach();
      controller.attach();
      // No assertion beyond "doesn't throw and doesn't double-fire" — the
      // wheel handler test below confirms listeners aren't doubled.
      fireWheel(container, -50);
      expect(controller.intent).toBe('free');
    });

    it('destroy removes listeners', () => {
      controller.destroy();
      fireWheel(container, -50);
      expect(controller.intent).toBe('stick'); // unchanged because listeners detached
    });

    it('re-attaches to a new container if getContainer changes', () => {
      // Tear down the default controller first so its listeners on
      // `container` can't pollute the swapper's intent.
      controller.destroy();

      const otherContainer = document.createElement('div');
      attachGeometry(otherContainer, { scrollHeight: 500, clientHeight: 200 });
      otherContainer.scrollTop = 300;
      document.body.appendChild(otherContainer);
      let useOther = false;
      const swapper = createStickToBottomController({
        getContainer: () => (useOther ? otherContainer : container),
      });
      swapper.attach();

      // Free state on first container.
      fireWheel(container, -50);
      expect(swapper.intent).toBe('free');

      // Swap.
      useOther = true;
      swapper.attach();

      // Listener should now be on otherContainer; firing wheel on the
      // original should NOT change intent.
      swapper.forceStick();
      fireWheel(container, -50);
      expect(swapper.intent).toBe('stick');

      // Firing on the new container should.
      fireWheel(otherContainer, -50);
      expect(swapper.intent).toBe('free');

      swapper.destroy();
      otherContainer.remove();
    });

    it('forceStick on a destroyed controller does not throw or schedule scrolls', async () => {
      controller.destroy();
      // Should not throw.
      expect(() => controller.forceStick()).not.toThrow();
      // Pending settle re-check, if scheduled, must not crash on the
      // (still-mounted) container after destroy.
      await waitMs(220);
      expect(container.scrollTop).toBe(400); // unchanged
    });

    it('survives an undefined container at construction time', async () => {
      const c = createStickToBottomController({ getContainer: () => undefined });
      // Must not throw on any of the lifecycle entry points.
      expect(() => c.attach()).not.toThrow();
      expect(() => c.forceStick()).not.toThrow();
      expect(() => c.notifyContentMaybeGrew()).not.toThrow();
      await nextFrame();
      await waitMs(220);
      c.destroy();
    });

    it('destroy cancels pending rAFs and timeouts', async () => {
      geo.scrollHeight = 1100;
      controller.notifyContentMaybeGrew();
      controller.destroy();
      await nextFrame();
      // No scroll happened because rAF was cancelled.
      expect(container.scrollTop).toBe(400);
    });
  });
});
