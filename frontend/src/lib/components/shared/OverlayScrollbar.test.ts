import { afterEach, describe, expect, it } from 'vitest';
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
  Object.defineProperty(el, 'clientHeight', { get: () => clientHeight });
  Object.defineProperty(el, 'scrollHeight', { get: () => scrollHeight });
  Object.defineProperty(el, 'scrollTop', {
    get: () => scrollTop,
    set: (next: number) => {
      scrollTop = next;
      el.dispatchEvent(new Event('scroll'));
    },
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

function pointerEvent(type: string, clientY: number): Event {
  const event = new MouseEvent(type, { bubbles: true, clientY, button: 0 });
  // happy-dom's MouseEvent carries no pointerId, and the component releases
  // capture by id.
  Object.defineProperty(event, 'pointerId', { value: POINTER_ID });
  return event;
}

function down(el: Element, clientY: number): Promise<unknown> {
  return fireEvent(el, pointerEvent('pointerdown', clientY));
}

function move(el: Element, clientY: number): Promise<unknown> {
  return fireEvent(el, pointerEvent('pointermove', clientY));
}

function up(el: Element, clientY: number): Promise<unknown> {
  return fireEvent(el, pointerEvent('pointerup', clientY));
}

function loseCapture(el: Element): Promise<unknown> {
  return fireEvent(el, pointerEvent('lostpointercapture', 0));
}

afterEach(() => {
  document.body.innerHTML = '';
});

async function mount(options: {
  clientHeight?: number;
  scrollHeight?: number;
  trackPx?: number;
  onUserScrollStart?: () => void;
  onUserScrollEnd?: (atBottom: boolean) => void;
} = {}) {
  const target = makeScroller(options.clientHeight ?? 100, options.scrollHeight ?? 400);
  const view = render(OverlayScrollbar, {
    props: {
      target,
      ariaLabel: 'Scroll activity run',
      onUserScrollStart: options.onUserScrollStart,
      onUserScrollEnd: options.onUserScrollEnd,
    },
  });
  const track = view.getByTestId('overlay-scrollbar');
  stubTrackHeight(track, options.trackPx ?? 200);
  // Re-sample now that the track has a height: the mount pass measured 0.
  target.scrollTop = 0;
  await tick();
  return { ...view, target, track };
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
