import { afterEach, describe, expect, it, vi } from 'vitest';
import { roleButtonKeys } from './roleButtonKeys';

// A role="button" element activates from the keyboard as a native button
// does: Enter on keydown, once per press; Space on keyup, only while the
// control kept focus.
describe('roleButtonKeys', () => {
  afterEach(() => {
    document.body.replaceChildren();
  });

  function control(): { el: HTMLElement; activate: ReturnType<typeof vi.fn>; other: HTMLElement } {
    const el = document.createElement('div');
    el.tabIndex = 0;
    const inner = document.createElement('button');
    el.append(inner);
    const other = document.createElement('button');
    document.body.append(el, other);
    const activate = vi.fn();
    const keys = roleButtonKeys(activate);
    el.addEventListener('keydown', (event) => { keys.keydown(event); });
    el.addEventListener('keyup', (event) => keys.keyup(event));
    el.addEventListener('blur', () => keys.blur());
    el.focus();
    return { el, activate, other };
  }

  function key(el: Element, type: 'keydown' | 'keyup', key: string, init: KeyboardEventInit = {}): KeyboardEvent {
    const event = new KeyboardEvent(type, { key, bubbles: true, cancelable: true, ...init });
    el.dispatchEvent(event);
    return event;
  }

  it('activates on Enter keydown, once however long it repeats', () => {
    const { el, activate } = control();
    expect(key(el, 'keydown', 'Enter').defaultPrevented).toBe(true);
    key(el, 'keydown', 'Enter', { repeat: true });
    key(el, 'keydown', 'Enter', { repeat: true });
    key(el, 'keyup', 'Enter');
    expect(activate).toHaveBeenCalledTimes(1);
    expect(activate.mock.calls[0][0]).toBeInstanceOf(KeyboardEvent);
  });

  it('arms on Space keydown without scrolling and activates once on keyup', () => {
    const { el, activate } = control();
    expect(key(el, 'keydown', ' ').defaultPrevented).toBe(true);
    key(el, 'keydown', ' ', { repeat: true });
    key(el, 'keydown', ' ', { repeat: true });
    expect(activate).not.toHaveBeenCalled();
    key(el, 'keyup', ' ', { ctrlKey: true });
    expect(activate).toHaveBeenCalledTimes(1);
    expect((activate.mock.calls[0][0] as KeyboardEvent).ctrlKey).toBe(true);
    // A second keyup without a new press does nothing.
    key(el, 'keyup', ' ');
    expect(activate).toHaveBeenCalledTimes(1);
  });

  it('does nothing when focus leaves while Space is held', () => {
    const { el, activate, other } = control();
    key(el, 'keydown', ' ');
    key(el, 'keydown', ' ', { repeat: true });
    // Tab away and back while held: the release opens nothing.
    other.focus();
    el.focus();
    key(el, 'keyup', ' ');
    expect(activate).not.toHaveBeenCalled();
  });

  it('leaves Space on a control inside it to that control', () => {
    const { el, activate } = control();
    const inner = el.querySelector('button')!;
    inner.focus();
    expect(key(inner, 'keydown', ' ').defaultPrevented).toBe(false);
    key(inner, 'keyup', ' ');
    expect(activate).not.toHaveBeenCalled();
  });

  it('ignores other keys', () => {
    const { el, activate } = control();
    expect(key(el, 'keydown', 'a').defaultPrevented).toBe(false);
    key(el, 'keyup', 'a');
    expect(activate).not.toHaveBeenCalled();
  });
});
