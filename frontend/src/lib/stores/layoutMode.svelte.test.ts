import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  getCompactScreen,
  installLayoutMode,
  isCompactLayout,
  setCompactLayoutForTest,
  showCompactList,
  showCompactThread,
} from './layoutMode.svelte';

type Listener = (ev: { matches: boolean }) => void;

function stubMatchMedia(initial: boolean) {
  const listeners = new Set<Listener>();
  const mql = {
    matches: initial,
    media: '',
    addEventListener: (_: string, fn: Listener) => listeners.add(fn),
    removeEventListener: (_: string, fn: Listener) => listeners.delete(fn),
  };
  const spy = vi.fn(() => mql);
  vi.stubGlobal('matchMedia', spy);
  return {
    spy,
    flip(next: boolean) {
      mql.matches = next;
      for (const fn of listeners) fn({ matches: next });
    },
    listeners,
  };
}

afterEach(() => {
  setCompactLayoutForTest(false);
  vi.unstubAllGlobals();
});

describe('layoutMode', () => {
  it('follows the viewport query and stamps <html>', () => {
    const media = stubMatchMedia(false);
    const dispose = installLayoutMode();
    expect(media.spy).toHaveBeenCalledWith('(max-width: 640px) and (pointer: coarse)');
    expect(isCompactLayout()).toBe(false);
    expect(document.documentElement.classList.contains('layout-compact')).toBe(false);

    media.flip(true);
    expect(isCompactLayout()).toBe(true);
    expect(document.documentElement.classList.contains('layout-compact')).toBe(true);
    expect(document.documentElement.dataset.compactScreen).toBe('list');

    dispose();
    expect(media.listeners.size).toBe(0);
    expect(isCompactLayout()).toBe(false);
    expect(document.documentElement.classList.contains('layout-compact')).toBe(false);
    expect(document.documentElement.dataset.compactScreen).toBeUndefined();
  });

  it('the screen is stamped only while compact', () => {
    setCompactLayoutForTest(true);
    showCompactThread();
    expect(getCompactScreen()).toBe('thread');
    expect(document.documentElement.dataset.compactScreen).toBe('thread');
    showCompactList();
    expect(document.documentElement.dataset.compactScreen).toBe('list');

    setCompactLayoutForTest(false);
    showCompactThread();
    expect(getCompactScreen()).toBe('thread');
    expect(document.documentElement.dataset.compactScreen).toBeUndefined();
  });

  it('is inert without matchMedia', () => {
    vi.stubGlobal('matchMedia', undefined);
    expect(() => installLayoutMode()()).not.toThrow();
    expect(isCompactLayout()).toBe(false);
  });
});
