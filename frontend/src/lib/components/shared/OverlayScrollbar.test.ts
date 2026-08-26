import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import OverlayScrollbar from './OverlayScrollbar.svelte';

// happy-dom reports zero geometry for everything, so the scroller under
// test is hand-built: real element, stubbed metrics. The math itself is
// covered exhaustively in utils/scroll/overlayScrollbar.test.ts — what
// this file pins is the wiring (capture, intent callbacks, live redraw).
function makeScroller(clientHeight: number, scrollHeight: number): HTMLElement {
  const el = document.createElement('div');
  let scrollTop = 0;
  // Configurable so a test can grow the surface mid-run, which is the only way
  // to observe a redraw driven by content growth.
  Object.defineProperty(el, 'clientHeight', { get: () => clientHeight, configurable: true });
  Object.defineProperty(el, 'scrollHeight', { get: () => scrollHeight, configurable: true });
  Object.defineProperty(el, 'scrollTop', {
    get: () => scrollTop,
    set: (next: number) => {
      scrollTop = next;
      el.dispatchEvent(new Event('scroll'));
    },
    configurable: true,
  });
  document.body.appendChild(el);
  return el;
}

function stubTrackHeight(track: Element, px: number): void {
  Object.defineProperty(track, 'clientHeight', { get: () => px });
  Object.defineProperty(track, 'getBoundingClientRect', {
    value: () => ({ top: 0, bottom: px, left: 0, right: 6, width: 6, height: px }),
  });
}

const POINTER_ID = 7;
/** A second finger. Touch surfaces have them; the thumb is one widget. */
const SECOND_POINTER_ID = 8;

/**
 * happy-dom has no pointer capture. Real browsers do, and the drag depends on
 * it, so stub rather than soften the component for the test env — faithfully
 * enough that `hasPointerCapture` answers honestly, which is also what lets a
 * test assert the component took the capture at all.
 */
function stubPointerCapture(el: Element): Set<number> {
  const captured = new Set<number>();
  const target = el as unknown as Record<string, unknown>;
  target.setPointerCapture = (id: number) => void captured.add(id);
  target.releasePointerCapture = (id: number) => void captured.delete(id);
  target.hasPointerCapture = (id: number) => captured.has(id);
  return captured;
}

function pointerEvent(type: string, clientY: number, pointerId = POINTER_ID): Event {
  const event = new MouseEvent(type, { bubbles: true, clientY, button: 0 });
  // happy-dom's MouseEvent carries no pointerId, and the component releases
  // capture by id and refuses events from a pointer that owns no drag.
  Object.defineProperty(event, 'pointerId', { value: pointerId });
  return event;
}

function down(el: Element, clientY: number, pointerId?: number): Promise<unknown> {
  return fireEvent(el, pointerEvent('pointerdown', clientY, pointerId));
}

function move(el: Element, clientY: number, pointerId?: number): Promise<unknown> {
  return fireEvent(el, pointerEvent('pointermove', clientY, pointerId));
}

function up(el: Element, clientY: number, pointerId?: number): Promise<unknown> {
  return fireEvent(el, pointerEvent('pointerup', clientY, pointerId));
}

function loseCapture(el: Element): Promise<unknown> {
  return fireEvent(el, pointerEvent('lostpointercapture', 0));
}

/**
 * A wheel over the strip, with the propagation the component is supposed to
 * stop observable: the outer listener stands in for the conversation's intent
 * machine, which is a real bubble-phase listener on an ancestor scroller.
 */
function wheelOverTrack(
  el: Element,
  deltaY: number,
  init: { deltaMode?: number; ctrlKey?: boolean } = {},
): { defaultPrevented: boolean; reachedOuter: boolean } {
  let reachedOuter = false;
  const outer = () => {
    reachedOuter = true;
  };
  document.body.addEventListener('wheel', outer);
  const event = new WheelEvent('wheel', {
    bubbles: true,
    cancelable: true,
    deltaY,
    deltaMode: init.deltaMode ?? 0,
  });
  // happy-dom's WheelEvent extends UIEvent, not MouseEvent, so it carries no
  // modifier state at all — the same gap that makes `pointerId` a manual
  // property above. A real one is a MouseEvent and reports ctrl.
  Object.defineProperty(event, 'ctrlKey', { value: init.ctrlKey ?? false });
  el.dispatchEvent(event);
  document.body.removeEventListener('wheel', outer);
  return { defaultPrevented: event.defaultPrevented, reachedOuter };
}

/**
 * happy-dom's stub ResizeObserver never delivers, and the streaming redraw
 * depends on the CONTENT element's growth: a scroller keeps its own size while
 * content grows inside it, so observing only the scroller misses every
 * streamed row. Capture what was observed and deliver by hand.
 */
function captureResizeObservers(): {
  observed: Element[];
  deliver: () => void;
  restore: () => void;
} {
  const real = globalThis.ResizeObserver;
  const observed: Element[] = [];
  const callbacks: ResizeObserverCallback[] = [];
  class Capturing {
    constructor(callback: ResizeObserverCallback) {
      callbacks.push(callback);
    }
    observe(el: Element): void {
      observed.push(el);
    }
    unobserve(): void {}
    disconnect(): void {}
  }
  globalThis.ResizeObserver = Capturing as unknown as typeof ResizeObserver;
  return {
    observed,
    deliver: () => {
      for (const callback of callbacks) {
        callback([], {} as ResizeObserver);
      }
    },
    restore: () => {
      globalThis.ResizeObserver = real;
    },
  };
}

afterEach(() => {
  document.body.innerHTML = '';
});

async function mount(options: {
  clientHeight?: number;
  scrollHeight?: number;
  trackPx?: number;
  ownerDrivenPosition?: () => boolean;
  onUserScrollStart?: () => void;
  onUserScrollEnd?: (atBottom: boolean) => void;
} = {}) {
  const target = makeScroller(options.clientHeight ?? 100, options.scrollHeight ?? 400);
  // The real shape: a content wrapper inside the scroller, which is what grows
  // while the scroller's own box stays capped.
  const content = document.createElement('div');
  target.appendChild(content);
  const view = render(OverlayScrollbar, {
    props: {
      target,
      content,
      ariaLabel: 'Scroll activity run',
      ownerDrivenPosition: options.ownerDrivenPosition,
      onUserScrollStart: options.onUserScrollStart,
      onUserScrollEnd: options.onUserScrollEnd,
    },
  });
  const track = view.getByTestId('overlay-scrollbar');
  stubTrackHeight(track, options.trackPx ?? 200);
  // Re-sample now that the track has a height: the mount pass measured 0.
  target.scrollTop = 0;
  await tick();
  return { ...view, target, content, track };
}

describe('<OverlayScrollbar>', () => {
  it('shows a thumb sized to the visible fraction', async () => {
    const { getByTestId } = await mount();

    const thumb = getByTestId('overlay-scrollbar-thumb');
    expect(thumb.style.height).toBe('50px');
    expect(thumb.style.top).toBe('0px');
  });

  it('stays out of the way when there is nothing to scroll', async () => {
    const { queryByTestId, getByTestId } = await mount({ clientHeight: 400, scrollHeight: 400 });

    expect(queryByTestId('overlay-scrollbar-thumb')).toBeNull();
    // Interactivity is dropped with the thumb — an invisible strip that
    // still swallowed clicks would be worse than the shift this replaces.
    expect(getByTestId('overlay-scrollbar').className).toContain('pointer-events-none');
  });

  it('follows the surface as it scrolls', async () => {
    const { getByTestId, target } = await mount();

    target.scrollTop = 300;
    await tick();

    expect(getByTestId('overlay-scrollbar-thumb').style.top).toBe('150px');
  });

  it('drags the surface by the distance the pointer moved', async () => {
    const { track, target } = await mount();
    stubPointerCapture(track);

    await down(track, 25); // on the thumb (0..50)
    await move(track, 45);
    await tick();

    expect(target.scrollTop).toBe(40);

    await move(track, 175);
    await tick();

    expect(target.scrollTop).toBe(300);
  });

  it('takes pointer capture on grab, so a drag survives leaving the strip', async () => {
    const { track } = await mount();
    const captured = stubPointerCapture(track);

    await down(track, 25);

    // A declaration check, deliberately. Capture ROUTING — later moves
    // arriving at the track while the pointer is nowhere near the 6px strip —
    // is a browser guarantee, and happy-dom implements none of it: a move
    // dispatched anywhere else simply would not reach the handler, so the
    // test would pin the shim rather than the behavior. What is falsifiable
    // here is that the component asked for the capture; deleting the
    // `setPointerCapture` call fails this.
    expect(captured.has(POINTER_ID)).toBe(true);

    await up(track, 25);
    expect(captured.has(POINTER_ID)).toBe(false);
  });

  it('ends the drag when the capture is taken away, not only on pointerup', async () => {
    const seen: string[] = [];
    const { track, target } = await mount({
      onUserScrollEnd: (atBottom) => seen.push(atBottom ? 'end:bottom' : 'end:free'),
    });
    stubPointerCapture(track);

    await down(track, 25);
    await move(track, 45);
    await tick();
    expect(target.scrollTop).toBe(40);

    // The browser fires this when it revokes the capture — the element left
    // the DOM, or the pointer was cancelled — and no pointerup follows.
    await loseCapture(track);
    await tick();

    expect(track.dataset.dragging).toBe('false');
    expect(seen).toEqual(['end:free']);

    // And the gesture is really over: a bare move must not scroll against
    // the origin the dead drag left behind.
    await move(track, 175);
    await tick();
    expect(target.scrollTop).toBe(40);
  });

  it('pages on a track click, and says so, so the owner stops following', async () => {
    const seen: string[] = [];
    const { track, target } = await mount({
      onUserScrollStart: () => seen.push('start'),
      onUserScrollEnd: (atBottom) => seen.push(atBottom ? 'end:bottom' : 'end:free'),
    });
    stubPointerCapture(track);

    // Below the thumb (0..50): a page, not a drag. It has no release, so it
    // states the whole gesture at once — a live surface that heard nothing
    // here would re-pin to the bottom on its next growth.
    await down(track, 120);
    await tick();

    expect(target.scrollTop).toBeGreaterThan(0);
    expect(seen).toEqual(['start', 'end:free']);
    expect(track.dataset.dragging).toBe('false');
  });

  it('states scroll intent on grab and release', async () => {
    const seen: string[] = [];
    const { track } = await mount({
      onUserScrollStart: () => seen.push('start'),
      onUserScrollEnd: (atBottom) => seen.push(atBottom ? 'end:bottom' : 'end:free'),
    });
    stubPointerCapture(track);

    await down(track, 25);
    await move(track, 45);
    await up(track, 45);

    expect(seen).toEqual(['start', 'end:free']);
  });

  it('reports a release at the bottom so the owner can re-stick', async () => {
    const seen: string[] = [];
    const { track } = await mount({
      onUserScrollEnd: (atBottom) => seen.push(atBottom ? 'end:bottom' : 'end:free'),
    });
    stubPointerCapture(track);

    await down(track, 25);
    await move(track, 175);
    await up(track, 175);

    expect(seen).toEqual(['end:bottom']);
  });

  it('lets only the pointer that grabbed the thumb drive or end the drag', async () => {
    const seen: string[] = [];
    const { track, target } = await mount({
      onUserScrollStart: () => seen.push('start'),
      onUserScrollEnd: (atBottom) => seen.push(atBottom ? 'end:bottom' : 'end:free'),
    });
    const captured = stubPointerCapture(track);

    await down(track, 25);
    // A second finger lands on the thumb while the first still holds it.
    await down(track, 25, SECOND_POINTER_ID);
    await tick();

    // One gesture, one start. Taking this press would rebase the drag on the
    // second finger's Y while the first one's moves keep arriving.
    expect(seen).toEqual(['start']);
    expect(captured.has(SECOND_POINTER_ID)).toBe(false);

    // Nor can it page the surface out from under the drag.
    await down(track, 120, SECOND_POINTER_ID);
    await move(track, 175, SECOND_POINTER_ID);
    await tick();
    expect(target.scrollTop).toBe(0);

    // And it cannot end a drag it does not own: the owner would hear the
    // release, be free to re-stick, and then be dragged again by pointer one.
    await up(track, 175, SECOND_POINTER_ID);
    await tick();
    expect(seen).toEqual(['start']);
    expect(track.dataset.dragging).toBe('true');

    await move(track, 45);
    await up(track, 45);
    await tick();

    expect(target.scrollTop).toBe(40);
    expect(seen).toEqual(['start', 'end:free']);
    expect(captured.has(POINTER_ID)).toBe(false);
  });

  it('scrolls the surface it sits beside, not the scroller behind it', async () => {
    const seen: string[] = [];
    const { track, target } = await mount({
      onUserScrollStart: () => seen.push('start'),
      onUserScrollEnd: (atBottom) => seen.push(atBottom ? 'end:bottom' : 'end:free'),
    });

    // The strip is a SIBLING of the surface, so this notch would otherwise
    // bubble straight to the conversation — scrolling it AND telling its
    // intent machine the reader left the bottom.
    const first = wheelOverTrack(track, 60);
    await tick();

    expect(target.scrollTop).toBe(60);
    expect(first.defaultPrevented).toBe(true);
    expect(first.reachedOuter).toBe(false);
    // Stated, not inferred: the same pair a drag reports, so a live surface
    // knows the position is the reader's now.
    expect(seen).toEqual(['start', 'end:free']);
  });

  it('reports a wheel that lands at the bottom, so the owner can re-stick', async () => {
    const seen: string[] = [];
    const { track, target } = await mount({
      onUserScrollEnd: (atBottom) => seen.push(atBottom ? 'end:bottom' : 'end:free'),
    });

    wheelOverTrack(track, 400);
    await tick();

    expect(target.scrollTop).toBe(300);
    expect(seen).toEqual(['end:bottom']);
  });

  it('gives the gesture up at the surface edge, so it still chains outward', async () => {
    const { track, target } = await mount();
    target.scrollTop = 300; // the bottom
    await tick();

    const past = wheelOverTrack(track, 120);

    // Nothing left to consume here: the conversation should scroll, exactly as
    // it does when a gesture reaches a nested box's own edge.
    expect(target.scrollTop).toBe(300);
    expect(past.defaultPrevented).toBe(false);
    expect(past.reachedOuter).toBe(true);
  });

  it('reads a line-mode notch in pixels', async () => {
    const { track, target } = await mount();

    // deltaMode 1 reports lines, and a raw 3 would move the surface by 3px.
    wheelOverTrack(track, 3, { deltaMode: 1 });
    await tick();

    expect(target.scrollTop).toBe(48);
  });

  it('leaves ctrl+wheel to the browser, which is zoom and not a scroll', async () => {
    const { track, target } = await mount();

    const zoom = wheelOverTrack(track, 60, { ctrlKey: true });

    expect(target.scrollTop).toBe(0);
    expect(zoom.defaultPrevented).toBe(false);
  });

  it('stays faded while the owner drives the position, and shows the moment the reader does', async () => {
    let ownerDriven = true;
    const { track, target } = await mount({ ownerDrivenPosition: () => ownerDriven });

    // A streaming surface pins itself to new content on every chunk. Fading on
    // those would mean a permanent bar for the whole turn.
    target.scrollTop = 100;
    await tick();
    expect(track.className).toContain('opacity-0');

    ownerDriven = false;
    target.scrollTop = 200;
    await tick();
    expect(track.className).toContain('opacity-100');
  });

  it('leaves the hidden thumb untouched while the owner drives, and repositions it on reveal', async () => {
    vi.useFakeTimers();
    try {
      let ownerDriven = false;
      const { getByTestId, target, track } = await mount({ ownerDrivenPosition: () => ownerDriven });
      expect(getByTestId('overlay-scrollbar-thumb').style.top).toBe('0px');

      // Streaming pins: owner-driven scroll events land while the bar is
      // hidden (mount's sample has faded by then). Restyling the invisible
      // thumb on each one kept the paint pipeline hot for entire streaming
      // turns (2026-08-25), so a hidden bar must not redraw at all.
      ownerDriven = true;
      vi.advanceTimersByTime(2000);
      target.scrollTop = 300;
      await tick();
      expect(getByTestId('overlay-scrollbar-thumb').style.top).toBe('0px');

      // The reveal transition re-samples, so the first frame the bar can
      // be seen already shows the true position.
      await fireEvent(track, new MouseEvent('pointerenter', { bubbles: true }));
      await tick();
      expect(getByTestId('overlay-scrollbar-thumb').style.top).toBe('150px');
    } finally {
      vi.useRealTimers();
    }
  });

  it('redraws when the content grows inside a scroller that did not', async () => {
    const ro = captureResizeObservers();
    try {
      const { getByTestId, target, content } = await mount();

      // The capped case: the clip's own box stops growing at its max-height,
      // so only the content element reports a streamed row.
      expect(ro.observed).toContain(content);
      expect(getByTestId('overlay-scrollbar-thumb').style.height).toBe('50px');

      Object.defineProperty(target, 'scrollHeight', { get: () => 800, configurable: true });
      ro.deliver();
      await tick();

      expect(getByTestId('overlay-scrollbar-thumb').style.height).toBe('25px');
    } finally {
      ro.restore();
    }
  });

  it('ignores a move that never started with a grab', async () => {
    const { track, target } = await mount();

    await move(track, 900);

    expect(target.scrollTop).toBe(0);
  });

  it('pages toward a click on the track', async () => {
    const { track, target } = await mount();

    await down(track, 180);

    expect(target.scrollTop).toBe(100);
  });

  it('does not page when the press lands on the thumb — that gesture is a drag', async () => {
    const { track, target } = await mount();
    stubPointerCapture(track);

    await down(track, 25);

    expect(target.scrollTop).toBe(0);
    expect(track.getAttribute('data-dragging')).toBe('true');
  });
});
