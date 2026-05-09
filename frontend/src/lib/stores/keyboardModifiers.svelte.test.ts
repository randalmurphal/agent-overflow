import {
  afterEach,
  beforeEach,
  describe,
  expect,
  it,
  vi,
} from 'vitest';
import {
  getJumpHintsVisible,
  jumpLabelForThread,
  resetKeyboardModifiersForTest,
  setKeyboardModifierPlatformForTest,
  subscribeJumpHints,
} from './keyboardModifiers.svelte';

function dispatchModKeyDown(target?: HTMLElement, key: 'Control' | 'Meta' = 'Control'): void {
  const event = new KeyboardEvent('keydown', { key, bubbles: true });
  if (target) {
    Object.defineProperty(event, 'target', { value: target });
  }
  window.dispatchEvent(event);
}

function dispatchModKeyUp(key: 'Control' | 'Meta' = 'Control'): void {
  window.dispatchEvent(new KeyboardEvent('keyup', { key, bubbles: true }));
}

function dispatchBlur(): void {
  window.dispatchEvent(new Event('blur'));
}

function makeRow(threadId: string): HTMLElement {
  const div = document.createElement('div');
  div.dataset.sidebarThreadId = threadId;
  div.setAttribute('data-sidebar-thread-id', threadId);
  document.body.appendChild(div);
  return div;
}

describe('keyboardModifiers store', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetKeyboardModifiersForTest();
    document.body.innerHTML = '';
  });

  afterEach(() => {
    resetKeyboardModifiersForTest();
    vi.useRealTimers();
    document.body.innerHTML = '';
  });

  describe('hint visibility timing', () => {
    it('shows hints after the 100ms delay', () => {
      const release = subscribeJumpHints();
      makeRow('t1');
      dispatchModKeyDown();
      expect(getJumpHintsVisible()).toBe(false);
      vi.advanceTimersByTime(99);
      expect(getJumpHintsVisible()).toBe(false);
      vi.advanceTimersByTime(2);
      expect(getJumpHintsVisible()).toBe(true);
      release();
    });

    it('cancels the timer if keyup arrives before the delay elapses', () => {
      const release = subscribeJumpHints();
      makeRow('t1');
      dispatchModKeyDown();
      vi.advanceTimersByTime(50);
      dispatchModKeyUp();
      vi.advanceTimersByTime(200);
      expect(getJumpHintsVisible()).toBe(false);
      release();
    });

    it('clears visibility on keyup after a successful show', () => {
      const release = subscribeJumpHints();
      makeRow('t1');
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      expect(getJumpHintsVisible()).toBe(true);
      dispatchModKeyUp();
      expect(getJumpHintsVisible()).toBe(false);
      release();
    });

    it('clears visibility on window blur (cmd-tab)', () => {
      const release = subscribeJumpHints();
      makeRow('t1');
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      expect(getJumpHintsVisible()).toBe(true);
      dispatchBlur();
      expect(getJumpHintsVisible()).toBe(false);
      release();
    });

    it('ignores Meta on non-macOS hosts', () => {
      const release = subscribeJumpHints();
      makeRow('t1');
      dispatchModKeyDown(undefined, 'Meta');
      vi.advanceTimersByTime(200);
      expect(getJumpHintsVisible()).toBe(false);
      release();
    });

    it('ignores Control on macOS hosts', () => {
      setKeyboardModifierPlatformForTest(true);
      const release = subscribeJumpHints();
      makeRow('t1');
      dispatchModKeyDown(undefined, 'Control');
      vi.advanceTimersByTime(200);
      expect(getJumpHintsVisible()).toBe(false);
      release();
    });
  });

  describe('editable-target behavior', () => {
    it('shows hints when the modifier hold starts inside an input', () => {
      const release = subscribeJumpHints();
      const input = document.createElement('input');
      document.body.appendChild(input);
      makeRow('t1');
      dispatchModKeyDown(input);
      vi.advanceTimersByTime(200);
      expect(getJumpHintsVisible()).toBe(true);
      release();
    });

    it('shows hints when the modifier hold starts inside a textarea', () => {
      const release = subscribeJumpHints();
      const textarea = document.createElement('textarea');
      document.body.appendChild(textarea);
      makeRow('t1');
      dispatchModKeyDown(textarea);
      vi.advanceTimersByTime(200);
      expect(getJumpHintsVisible()).toBe(true);
      release();
    });
  });

  describe('jump label map', () => {
    it('assigns labels 1..N for the first 9 sidebar rows in DOM order', () => {
      const release = subscribeJumpHints();
      for (let i = 1; i <= 12; i += 1) makeRow(`t${i}`);
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      expect(jumpLabelForThread('t1')).toBe('1');
      expect(jumpLabelForThread('t9')).toBe('9');
      // Beyond the cap, no label.
      expect(jumpLabelForThread('t10')).toBeUndefined();
      release();
    });

    it('returns an empty map when no rows are present', () => {
      const release = subscribeJumpHints();
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      expect(jumpLabelForThread('any')).toBeUndefined();
      release();
    });

    it('clears the label map on keyup', () => {
      const release = subscribeJumpHints();
      makeRow('t1');
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      expect(jumpLabelForThread('t1')).toBe('1');
      dispatchModKeyUp();
      expect(jumpLabelForThread('t1')).toBeUndefined();
      release();
    });
  });

  describe('refcount lifecycle', () => {
    it('keeps listeners installed while at least one subscriber is active', () => {
      const releaseA = subscribeJumpHints();
      const releaseB = subscribeJumpHints();
      releaseA();
      makeRow('t1');
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      // B is still subscribed → listener still installed → keydown handled.
      expect(getJumpHintsVisible()).toBe(true);
      releaseB();
    });

    it('tears down listeners after the last subscriber leaves', () => {
      const release = subscribeJumpHints();
      release();
      // No subscribers left — keydown should be ignored.
      makeRow('t1');
      dispatchModKeyDown();
      vi.advanceTimersByTime(200);
      expect(getJumpHintsVisible()).toBe(false);
    });
  });
});
