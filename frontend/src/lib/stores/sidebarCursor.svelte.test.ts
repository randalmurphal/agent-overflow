import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import {
  clearSidebarCursor,
  getSidebarCursorThreadId,
  resetSidebarCursorStore,
  setSidebarCursorForTest,
  stepSidebarCursor,
} from './sidebarCursor.svelte';

function seedRow(threadId: string): HTMLElement {
  const div = document.createElement('div');
  div.setAttribute('data-sidebar-thread-id', threadId);
  document.body.appendChild(div);
  return div;
}

function seedRows(ids: string[]): void {
  for (const id of ids) seedRow(id);
}

describe('sidebarCursor store', () => {
  beforeEach(() => {
    resetSidebarCursorStore();
    document.body.innerHTML = '';
  });
  afterEach(() => {
    resetSidebarCursorStore();
    document.body.innerHTML = '';
  });

  describe('stepSidebarCursor', () => {
    it.each([
      ['lands on the fallback id on cold start (no cursor) without stepping', 1, 'b', 'b'],
      ['lands on row 0 going down when fallback is null', 1, null, 'a'],
      ['lands on the last row going up when fallback is null', -1, null, 'c'],
    ] as const)('%s', (_name, delta, fallback, expected) => {
      seedRows(['a', 'b', 'c']);
      stepSidebarCursor(delta, fallback);
      expect(getSidebarCursorThreadId()).toBe(expected);
    });

    it('steps forward through the visible order on subsequent calls', () => {
      seedRows(['a', 'b', 'c']);
      setSidebarCursorForTest('a');
      stepSidebarCursor(1, 'a');
      expect(getSidebarCursorThreadId()).toBe('b');
      stepSidebarCursor(1, 'a');
      expect(getSidebarCursorThreadId()).toBe('c');
    });

    it.each([
      ['wraps to the first row when stepping past the end', 'c', 1, 'c', 'a'],
      ['wraps to the last row when stepping before the start', 'a', -1, 'a', 'c'],
      ['falls back to the supplied id when the current cursor falls out of the visible set', 'missing-thread', 1, 'b', 'b'],
    ] as const)('%s', (_name, cursor, delta, fallback, expected) => {
      seedRows(['a', 'b', 'c']);
      setSidebarCursorForTest(cursor);
      stepSidebarCursor(delta, fallback);
      expect(getSidebarCursorThreadId()).toBe(expected);
    });

    it('uses row 0 / last row when both cursor and fallback are absent from the visible set', () => {
      seedRows(['a', 'b']);
      setSidebarCursorForTest('missing');
      stepSidebarCursor(1, 'also-missing');
      expect(getSidebarCursorThreadId()).toBe('a');
      stepSidebarCursor(-1, 'also-missing');
      // Cursor is now 'a' (a valid row), so step -1 wraps to last row.
      expect(getSidebarCursorThreadId()).toBe('b');
    });

    it('clears the cursor and is a no-op when the sidebar renders no rows', () => {
      setSidebarCursorForTest('a');
      stepSidebarCursor(1, 'a');
      expect(getSidebarCursorThreadId()).toBeNull();
    });
  });

  describe('clearSidebarCursor', () => {
    it('drops the cursor back to null', () => {
      setSidebarCursorForTest('a');
      expect(getSidebarCursorThreadId()).toBe('a');
      clearSidebarCursor();
      expect(getSidebarCursorThreadId()).toBeNull();
    });
  });
});
