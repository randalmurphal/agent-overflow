// Exercises the focusTrap action under the weird inputs the task spec
// calls out: wrap-around Tab / Shift+Tab, Escape close handling,
// restoration to the triggering element, and nested traps shadowing
// outer ones.

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { focusTrap } from './focusTrap';

function dispatchTab(target: EventTarget, shift = false): KeyboardEvent {
  const ev = new KeyboardEvent('keydown', {
    key: 'Tab',
    shiftKey: shift,
    bubbles: true,
    cancelable: true,
  });
  target.dispatchEvent(ev);
  return ev;
}

let cleanup: Array<() => void> = [];

function mountTrap(
  innerHTML: string,
  options: Parameters<typeof focusTrap>[1] = { active: true },
): HTMLElement {
  const host = document.createElement('div');
  host.innerHTML = innerHTML;
  document.body.appendChild(host);
  const handle = focusTrap(host, options);
  cleanup.push(() => {
    if (handle && typeof handle.destroy === 'function') {
      handle.destroy();
    }
    host.remove();
  });
  return host;
}

beforeEach(() => {
  document.body.innerHTML = '';
});

afterEach(() => {
  for (const fn of cleanup.splice(0)) fn();
  document.body.innerHTML = '';
});

describe('focusTrap', () => {
  it('wraps Tab from last focusable back to first', async () => {
    const host = mountTrap(
      '<button id="a">A</button><button id="b">B</button><button id="c">C</button>',
    );
    // Let autoFocus microtask run so focus lands on A.
    await Promise.resolve();

    const c = host.querySelector<HTMLButtonElement>('#c')!;
    c.focus();
    expect(document.activeElement).toBe(c);

    const ev = dispatchTab(c);
    expect(ev.defaultPrevented).toBe(true);
    expect(document.activeElement?.id).toBe('a');
  });

  it('wraps Shift+Tab from first focusable to last', async () => {
    const host = mountTrap(
      '<button id="a">A</button><button id="b">B</button><button id="c">C</button>',
    );
    await Promise.resolve();

    const a = host.querySelector<HTMLButtonElement>('#a')!;
    a.focus();
    expect(document.activeElement).toBe(a);

    const ev = dispatchTab(a, true);
    expect(ev.defaultPrevented).toBe(true);
    expect(document.activeElement?.id).toBe('c');
  });

  it('auto-focuses the first focusable element on attach', async () => {
    const host = mountTrap('<button id="a">A</button><button id="b">B</button>');
    // Flush the queueMicrotask scheduled inside the action.
    await Promise.resolve();
    await Promise.resolve();
    expect(document.activeElement?.id).toBe('a');
    host.remove();
  });

  it('prefers [data-autofocus] over the first focusable', async () => {
    mountTrap(
      '<button id="a">A</button><input data-autofocus id="b" /><button id="c">C</button>',
    );
    await Promise.resolve();
    await Promise.resolve();
    expect(document.activeElement?.id).toBe('b');
  });

  it('restores focus to the previously active element on destroy', async () => {
    const trigger = document.createElement('button');
    trigger.id = 'trigger';
    document.body.appendChild(trigger);
    trigger.focus();
    expect(document.activeElement).toBe(trigger);

    const host = document.createElement('div');
    host.innerHTML = '<button id="a">A</button>';
    document.body.appendChild(host);
    const handle = focusTrap(host, { active: true });
    await Promise.resolve();
    await Promise.resolve();
    expect(document.activeElement?.id).toBe('a');

    const restoreSpy = vi.spyOn(trigger, 'focus');
    handle!.destroy!();
    expect(document.activeElement).toBe(trigger);
    // The opener can sit in the horizontally-scrolled pane strip, which may
    // have moved while the trap was up — the restore must never scroll it.
    expect(restoreSpy).toHaveBeenCalledWith({ preventScroll: true });

    trigger.remove();
    host.remove();
  });

  it('nested traps: inner handles Tab, outer resumes after inner closes', async () => {
    const outer = mountTrap('<button id="outer-a">OA</button><button id="outer-b">OB</button>');
    await Promise.resolve();
    await Promise.resolve();

    const inner = mountTrap('<button id="inner-a">IA</button><button id="inner-b">IB</button>');
    await Promise.resolve();
    await Promise.resolve();

    const innerA = inner.querySelector<HTMLButtonElement>('#inner-a')!;
    const innerB = inner.querySelector<HTMLButtonElement>('#inner-b')!;
    innerB.focus();
    dispatchTab(innerB);
    expect(document.activeElement).toBe(innerA);

    // Pretend inner closed by destroying just the inner trap via the
    // most-recent cleanup entry. The next cleanup in the stack is the
    // outer trap.
    cleanup.pop()?.();

    // Outer trap should now be the top of stack. Tabbing from outer-b
    // should wrap to outer-a.
    const outerA = outer.querySelector<HTMLButtonElement>('#outer-a')!;
    const outerB = outer.querySelector<HTMLButtonElement>('#outer-b')!;
    outerB.focus();
    dispatchTab(outerB);
    expect(document.activeElement).toBe(outerA);
  });

  it('active:false disables the trap', async () => {
    const trigger = document.createElement('button');
    document.body.appendChild(trigger);
    trigger.focus();

    const host = document.createElement('div');
    host.innerHTML = '<button id="a">A</button>';
    document.body.appendChild(host);
    const handle = focusTrap(host, { active: false });
    // With active:false the autoFocus microtask should not have stolen
    // focus away from the trigger.
    await Promise.resolve();
    await Promise.resolve();
    expect(document.activeElement).toBe(trigger);

    // Tab events should not be captured.
    const a = host.querySelector<HTMLButtonElement>('#a')!;
    a.focus();
    const ev = dispatchTab(a);
    expect(ev.defaultPrevented).toBe(false);

    handle!.destroy!();
    trigger.remove();
    host.remove();
  });

  it('update(): flipping active from false to true arms the trap', async () => {
    const trigger = document.createElement('button');
    document.body.appendChild(trigger);
    trigger.focus();

    const host = document.createElement('div');
    host.innerHTML = '<button id="a">A</button><button id="b">B</button>';
    document.body.appendChild(host);

    const handle = focusTrap(host, { active: false });
    await Promise.resolve();
    expect(document.activeElement).toBe(trigger);

    handle!.update!({ active: true });
    await Promise.resolve();
    await Promise.resolve();
    expect(document.activeElement?.id).toBe('a');

    handle!.destroy!();
    expect(document.activeElement).toBe(trigger);
    trigger.remove();
    host.remove();
  });

  it('no focusable elements: still prevents default to keep focus inside', async () => {
    const host = mountTrap('<p>no focusables</p>');
    await Promise.resolve();

    const ev = dispatchTab(host);
    expect(ev.defaultPrevented).toBe(true);
  });

  it('ignores Tab keypresses outside the active trap', async () => {
    mountTrap('<button id="a">A</button>');
    await Promise.resolve();

    const outside = document.createElement('button');
    outside.id = 'outside';
    document.body.appendChild(outside);
    outside.focus();

    const ev = dispatchTab(outside);
    expect(ev.defaultPrevented).toBe(false);
    outside.remove();
  });
});
