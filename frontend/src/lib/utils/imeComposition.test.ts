import { describe, expect, it } from 'vitest';
import { isImeComposingEvent } from './imeComposition';

function keyEvent(init: { isComposing?: boolean; keyCode?: number } = {}): KeyboardEvent {
  const ev = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true, cancelable: true });
  Object.defineProperty(ev, 'isComposing', { value: init.isComposing ?? false });
  Object.defineProperty(ev, 'keyCode', { value: init.keyCode ?? 13 });
  return ev;
}

describe('isImeComposingEvent', () => {
  it('is false for an ordinary keystroke', () => {
    expect(isImeComposingEvent(keyEvent())).toBe(false);
  });

  it('is true while the spec flag is set', () => {
    expect(isImeComposingEvent(keyEvent({ isComposing: true }))).toBe(true);
  });

  it('is true for the legacy 229 sentinel even after isComposing cleared', () => {
    // WebKit delivers the composition's final keydown after compositionend:
    // isComposing is already false but the key code is still the IME sentinel.
    expect(isImeComposingEvent(keyEvent({ isComposing: false, keyCode: 229 }))).toBe(true);
  });

  it('accepts a plain structural event shape', () => {
    expect(isImeComposingEvent({ isComposing: false, keyCode: 65 })).toBe(false);
    expect(isImeComposingEvent({ isComposing: true, keyCode: 229 })).toBe(true);
  });
});
