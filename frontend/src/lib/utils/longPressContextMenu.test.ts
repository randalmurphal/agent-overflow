// The long-press detector's contract (see the module doc): a held touch
// under the compact layout becomes one `contextmenu` at the pressed element,
// a handled press swallows the engine's own contextmenu and the
// compatibility mouse sequence that follows release, and every other press
// (mouse, moved, released early, editable target, unhandled) leaves the
// engine's behaviour untouched.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  installLongPressContextMenu,
  LONG_PRESS_HOLD_MS,
  LONG_PRESS_SLOP_PX,
} from './longPressContextMenu';

let row: HTMLDivElement;
let dispose: () => void;
let active: boolean;
let seen: string[];
let listeners: AbortController;

function pointer(type: string, init: Partial<PointerEventInit> = {}): PointerEvent {
  return new PointerEvent(type, {
    bubbles: true,
    cancelable: true,
    composed: true,
    pointerId: 1,
    pointerType: 'touch',
    isPrimary: true,
    button: 0,
    clientX: 20,
    clientY: 20,
    ...init,
  });
}

function mouse(type: string): MouseEvent {
  return new MouseEvent(type, { bubbles: true, cancelable: true, composed: true });
}

function hold(ms = LONG_PRESS_HOLD_MS): void {
  row.dispatchEvent(pointer('pointerdown'));
  vi.advanceTimersByTime(ms);
}

function release(): void {
  row.dispatchEvent(pointer('pointerup'));
}

beforeEach(() => {
  vi.useFakeTimers();
  active = true;
  seen = [];
  row = document.createElement('div');
  document.body.appendChild(row);
  // The row handles its menu the way every real site does: preventDefault.
  row.addEventListener('contextmenu', (e) => {
    seen.push(`contextmenu@${(e as MouseEvent).clientX},${(e as MouseEvent).clientY}`);
    e.preventDefault();
  });
  listeners = new AbortController();
  for (const type of ['mousedown', 'mouseup', 'click']) {
    document.addEventListener(type, () => seen.push(type), { signal: listeners.signal });
  }
  dispose = installLongPressContextMenu({ isActive: () => active });
});

afterEach(() => {
  dispose();
  listeners.abort();
  row.remove();
  vi.useRealTimers();
});

describe('installLongPressContextMenu', () => {
  it('raises one contextmenu at the pressed element after the hold', () => {
    hold(LONG_PRESS_HOLD_MS - 1);
    expect(seen).toEqual([]);
    vi.advanceTimersByTime(1);
    expect(seen).toEqual(['contextmenu@20,20']);
  });

  it('does nothing outside the compact layout or for a mouse', () => {
    active = false;
    hold();
    expect(seen).toEqual([]);
    active = true;
    row.dispatchEvent(pointer('pointerdown', { pointerType: 'mouse' }));
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual([]);
  });

  it('is cancelled by a release, a scroll, or a pointercancel', () => {
    row.dispatchEvent(pointer('pointerdown'));
    release();
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual([]);

    row.dispatchEvent(pointer('pointerdown'));
    row.dispatchEvent(pointer('pointermove', { clientX: 20 + LONG_PRESS_SLOP_PX + 1 }));
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual([]);

    row.dispatchEvent(pointer('pointerdown'));
    row.dispatchEvent(pointer('pointercancel'));
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual([]);
  });

  it('tolerates movement inside the slop', () => {
    row.dispatchEvent(pointer('pointerdown'));
    row.dispatchEvent(pointer('pointermove', { clientX: 20 + LONG_PRESS_SLOP_PX - 1 }));
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual(['contextmenu@20,20']);
  });

  it('leaves an editable target to the engine', () => {
    const input = document.createElement('textarea');
    row.appendChild(input);
    input.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual([]);
  });

  it('swallows the compatibility mouse sequence after a handled press', () => {
    hold();
    release();
    row.dispatchEvent(mouse('mousedown'));
    row.dispatchEvent(mouse('mouseup'));
    row.dispatchEvent(mouse('click'));
    expect(seen).toEqual(['contextmenu@20,20']);
    // The click ended the window: an ordinary tap afterwards passes.
    row.dispatchEvent(mouse('mousedown'));
    row.dispatchEvent(mouse('click'));
    expect(seen).toEqual(['contextmenu@20,20', 'mousedown', 'click']);
  });

  it('lets the sequence through once the grace after release has lapsed', () => {
    hold();
    release();
    vi.advanceTimersByTime(1000);
    row.dispatchEvent(mouse('click'));
    expect(seen).toEqual(['contextmenu@20,20', 'click']);
  });

  it('swallows the engine\'s own contextmenu once the synthetic one was handled', () => {
    hold();
    const native = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    row.dispatchEvent(native);
    expect(native.defaultPrevented).toBe(true);
    expect(seen).toEqual(['contextmenu@20,20']);
  });

  it('yields to an engine that raises contextmenu during the hold', () => {
    row.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(300);
    row.dispatchEvent(new MouseEvent('contextmenu', {
      bubbles: true, cancelable: true, clientX: 21, clientY: 22,
    }));
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual(['contextmenu@21,22']);
    // The engine's press was handled, so its release sequence is swallowed too.
    release();
    row.dispatchEvent(mouse('click'));
    expect(seen).toEqual(['contextmenu@21,22']);
  });

  it('forgets a press nobody handled and swallows nothing', () => {
    const prose = document.createElement('p');
    document.body.appendChild(prose);
    prose.dispatchEvent(pointer('pointerdown'));
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    const native = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    prose.dispatchEvent(native);
    expect(native.defaultPrevented).toBe(false);
    prose.dispatchEvent(pointer('pointerup'));
    prose.dispatchEvent(mouse('click'));
    expect(seen).toEqual(['click']);
    prose.remove();
  });

  it('does not fire for a target that left the document during the hold', () => {
    row.dispatchEvent(pointer('pointerdown'));
    row.remove();
    vi.advanceTimersByTime(LONG_PRESS_HOLD_MS);
    expect(seen).toEqual([]);
  });

  it('stops listening once disposed', () => {
    dispose();
    hold();
    expect(seen).toEqual([]);
    dispose = () => {};
  });
});
