// The tap hold's contract (see the module doc): a press on a surface the
// caller's row would move holds until the click that completes the tap, and
// every exit that means no click is coming ends the hold.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { COMPAT_GRACE_MS, installLongPressContextMenu } from './longPressContextMenu';
import { installTapHold } from './tapHold';

let surface: HTMLDivElement;
let row: HTMLDivElement;
let outside: HTMLDivElement;
let changes: boolean[];
let dispose: () => void;

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

function click(target: Element): void {
  target.dispatchEvent(new MouseEvent('click', { bubbles: true, cancelable: true, composed: true }));
}

function held(): boolean {
  return changes.at(-1) === true;
}

beforeEach(() => {
  vi.useFakeTimers();
  changes = [];
  surface = document.createElement('div');
  surface.setAttribute('data-hold', '');
  row = document.createElement('div');
  surface.appendChild(row);
  outside = document.createElement('div');
  document.body.append(surface, outside);
  dispose = installTapHold(
    (target) => target.closest('[data-hold]') !== null,
    (value) => changes.push(value),
  );
});

afterEach(() => {
  dispose();
  surface.remove();
  outside.remove();
  vi.useRealTimers();
});

describe('installTapHold', () => {
  it('holds a tap on the surface from its press to its click, past the release', () => {
    row.dispatchEvent(pointer('pointerdown'));
    expect(changes).toEqual([true]);
    row.dispatchEvent(pointer('pointerup'));
    vi.advanceTimersByTime(COMPAT_GRACE_MS - 1);
    expect(held()).toBe(true);
    click(row);
    expect(changes).toEqual([true, false]);
    vi.advanceTimersByTime(COMPAT_GRACE_MS);
    expect(changes).toEqual([true, false]);
  });

  it('holds nothing for a press elsewhere', () => {
    outside.dispatchEvent(pointer('pointerdown'));
    outside.dispatchEvent(pointer('pointerup'));
    click(outside);
    expect(changes).toEqual([]);
  });

  it('ends a mouse press at the click that follows its release', () => {
    row.dispatchEvent(pointer('pointerdown', { pointerType: 'mouse' }));
    row.dispatchEvent(pointer('pointerup', { pointerType: 'mouse' }));
    click(row);
    expect(changes).toEqual([true, false]);
  });

  it('ends at a cancelled pointer', () => {
    row.dispatchEvent(pointer('pointerdown'));
    row.dispatchEvent(pointer('pointercancel', { pointerId: 2 }));
    expect(held()).toBe(true);
    row.dispatchEvent(pointer('pointercancel'));
    expect(changes).toEqual([true, false]);
  });

  it('ends a long press at its release, though no click follows it', () => {
    row.dispatchEvent(pointer('pointerdown'));
    row.dispatchEvent(new MouseEvent('contextmenu', { bubbles: true, cancelable: true }));
    expect(held()).toBe(true);
    row.dispatchEvent(pointer('pointerup'));
    expect(changes).toEqual([true, false]);
  });

  it('ends a long press the detector turned into a menu, whose click it swallows', () => {
    const detector = installLongPressContextMenu({ isActive: () => true });
    try {
      row.addEventListener('contextmenu', (e) => e.preventDefault());
      row.dispatchEvent(pointer('pointerdown'));
      vi.advanceTimersByTime(1000);
      row.dispatchEvent(pointer('pointerup'));
      expect(changes).toEqual([true, false]);
      click(row);
      expect(changes).toEqual([true, false]);
    } finally {
      detector();
    }
  });

  it('ends when what was pressed scrolls, and not for another scroll', () => {
    row.dispatchEvent(pointer('pointerdown'));
    outside.dispatchEvent(new Event('scroll'));
    expect(held()).toBe(true);
    surface.dispatchEvent(new Event('scroll'));
    expect(changes).toEqual([true, false]);
  });

  it('ends at the release when the pressed element is gone', () => {
    row.dispatchEvent(pointer('pointerdown'));
    row.remove();
    window.dispatchEvent(pointer('pointerup'));
    expect(changes).toEqual([true, false]);
  });

  it('ends a release whose click never comes once the click window has passed', () => {
    row.dispatchEvent(pointer('pointerdown'));
    row.dispatchEvent(pointer('pointerup'));
    vi.advanceTimersByTime(COMPAT_GRACE_MS - 1);
    expect(held()).toBe(true);
    vi.advanceTimersByTime(1);
    expect(changes).toEqual([true, false]);
  });

  it('ends at a new press, and at a second finger', () => {
    row.dispatchEvent(pointer('pointerdown'));
    outside.dispatchEvent(pointer('pointerdown', { pointerId: 2 }));
    expect(changes).toEqual([true, false]);
    row.dispatchEvent(pointer('pointerdown', { pointerId: 3 }));
    row.dispatchEvent(pointer('pointerdown', { pointerId: 4, isPrimary: false }));
    expect(changes).toEqual([true, false, true, false]);
  });

  it('ends a hold in progress when disposed, and listens no more', () => {
    row.dispatchEvent(pointer('pointerdown'));
    dispose();
    expect(changes).toEqual([true, false]);
    row.dispatchEvent(pointer('pointerdown'));
    expect(changes).toEqual([true, false]);
    dispose = installTapHold(
      (target) => target.closest('[data-hold]') !== null,
      (value) => changes.push(value),
    );
    row.dispatchEvent(pointer('pointerdown'));
    expect(changes).toEqual([true, false, true]);
  });
});
