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
  trackSidebarJumpRows,
} from './keyboardModifiers.svelte';
import { getSidebarJumpThreadIds } from './sidebarThreadOrder';

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

function makeRow(threadId: string, eligible = true): HTMLElement {
  const div = document.createElement('div');
  div.dataset.sidebarThreadId = threadId;
  div.setAttribute('data-sidebar-thread-id', threadId);
  if (eligible) div.setAttribute('data-sidebar-jump-target', '');
  document.body.appendChild(div);
  return div;
}

describe('keyboardModifiers store', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    resetKeyboardModifiersForTest();
    document.body.innerHTML = '';
    trackSidebarJumpRows(document.body);
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
    it('assigns labels 1..N for the first 9 eligible rows in DOM order', () => {
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

    it('skips rows without a front-burner pin target', () => {
      const release = subscribeJumpHints();
      makeRow('unpinned', false);
      makeRow('front');
      makeRow('back', false);
      makeRow('next-front');
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      expect(jumpLabelForThread('unpinned')).toBeUndefined();
      expect(jumpLabelForThread('back')).toBeUndefined();
      expect(jumpLabelForThread('front')).toBe('1');
      expect(jumpLabelForThread('next-front')).toBe('2');
      release();
    });

    it('keeps hints aligned with jumps across reorder, pin changes, and removal during a hold', async () => {
      const release = subscribeJumpHints();
      const first = makeRow('first');
      const second = makeRow('second');
      const third = makeRow('third', false);
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);

      const expectAligned = (ids: string[]) => {
        expect(getSidebarJumpThreadIds()).toEqual(ids);
        for (const [index, id] of ids.entries()) {
          expect(jumpLabelForThread(id)).toBe(String(index + 1));
        }
      };
      document.body.prepend(second);
      await vi.advanceTimersByTimeAsync(0);
      expectAligned(['second', 'first']);

      first.removeAttribute('data-sidebar-jump-target');
      third.setAttribute('data-sidebar-jump-target', '');
      await vi.advanceTimersByTimeAsync(0);
      expectAligned(['second', 'third']);
      expect(jumpLabelForThread('first')).toBeUndefined();

      second.remove();
      await vi.advanceTimersByTimeAsync(0);
      expectAligned(['third']);
      expect(jumpLabelForThread('second')).toBeUndefined();

      third.remove();
      await vi.advanceTimersByTimeAsync(0);
      expectAligned([]);
      expect(jumpLabelForThread('third')).toBeUndefined();
      release();
    });

    it('clears hints on sidebar removal and observes its replacement during the same hold', async () => {
      resetKeyboardModifiersForTest();
      const release = subscribeJumpHints();
      const root = document.createElement('aside');
      document.body.append(root);
      const tracking = trackSidebarJumpRows(root);
      root.append(makeRow('first'));
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      expect(jumpLabelForThread('first')).toBe('1');

      tracking.destroy();
      root.remove();
      await vi.advanceTimersByTimeAsync(0);
      expect(jumpLabelForThread('first')).toBeUndefined();

      const replacement = document.createElement('aside');
      document.body.append(replacement);
      replacement.append(makeRow('second'));
      const nextTracking = trackSidebarJumpRows(replacement);
      expect(jumpLabelForThread('second')).toBe('1');
      replacement.append(makeRow('third'));
      await vi.advanceTimersByTimeAsync(0);
      expect(jumpLabelForThread('third')).toBe('2');
      release();
      nextTracking.destroy();
    });

    it('does not rescan for status, title, or hint content changes', async () => {
      const release = subscribeJumpHints();
      const row = makeRow('first');
      dispatchModKeyDown();
      vi.advanceTimersByTime(101);
      const query = vi.spyOn(document, 'querySelectorAll');
      try {
        row.dataset.liveStatus = 'running';
        row.className = 'active';
        const title = document.createElement('span');
        row.append(title);
        title.textContent = 'A new title';
        await vi.advanceTimersByTimeAsync(0);
        expect(query).not.toHaveBeenCalled();
        expect(jumpLabelForThread('first')).toBe('1');
      } finally {
        query.mockRestore();
        release();
      }
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

    it('clears labels and stops observing after release, blur, and teardown', async () => {
      for (const end of ['keyup', 'blur', 'release']) {
        const release = subscribeJumpHints();
        const row = makeRow('first');
        dispatchModKeyDown();
        vi.advanceTimersByTime(101);
        expect(jumpLabelForThread('first')).toBe('1');
        if (end === 'keyup') dispatchModKeyUp();
        else if (end === 'blur') dispatchBlur();
        else release();
        row.dataset.sidebarThreadId = 'changed';
        await vi.advanceTimersByTimeAsync(0);
        expect(getJumpHintsVisible()).toBe(false);
        expect(jumpLabelForThread('first')).toBeUndefined();
        expect(jumpLabelForThread('changed')).toBeUndefined();
        release();
        row.remove();
      }
    });
  });
});
