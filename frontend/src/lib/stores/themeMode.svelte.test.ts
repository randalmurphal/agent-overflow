import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync } from 'svelte';
import { getResolvedTheme, teardownThemeModeForTest } from './themeMode.svelte';
import {
  resetAppearanceForTest,
  setAppearance,
  type AppearanceMode,
} from './appearance.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';

interface FakeMediaQueryList {
  matches: boolean;
  addEventListener: (type: 'change', cb: (e: MediaQueryListEvent) => void) => void;
  removeEventListener: (type: 'change', cb: (e: MediaQueryListEvent) => void) => void;
  fire(matches: boolean): void;
}

function makeFakeMatchMedia(initialDark: boolean): FakeMediaQueryList {
  const handlers: Array<(e: MediaQueryListEvent) => void> = [];
  const list: FakeMediaQueryList = {
    matches: initialDark,
    addEventListener(_type, cb) {
      handlers.push(cb);
    },
    removeEventListener(_type, cb) {
      const idx = handlers.indexOf(cb);
      if (idx >= 0) handlers.splice(idx, 1);
    },
    fire(matches: boolean) {
      list.matches = matches;
      const e = { matches } as unknown as MediaQueryListEvent;
      for (const h of handlers) h(e);
    },
  };
  return list;
}

let mediaList: FakeMediaQueryList;

async function setTheme(mode: AppearanceMode): Promise<void> {
  await setAppearance({ mode });
}

describe('themeMode', () => {
  beforeEach(() => {
    mediaList = makeFakeMatchMedia(true);
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => mediaList),
    );
    teardownThemeModeForTest();
    resetAppearanceForTest();
    localStorage.clear();
    setBindingMock('SetAppearance', async () => undefined);
  });

  afterEach(() => {
    teardownThemeModeForTest();
    resetAppearanceForTest();
    localStorage.clear();
    vi.unstubAllGlobals();
  });

  it("returns 'dark' for theme='dark'", async () => {
    await setTheme('dark');
    expect(getResolvedTheme()).toBe('dark');
  });

  it("returns 'light' for theme='light'", async () => {
    await setTheme('light');
    expect(getResolvedTheme()).toBe('light');
  });

  it("returns the system preference for theme='system'", async () => {
    await setTheme('system');
    expect(getResolvedTheme()).toBe('dark');
  });

  it('reflects system color-scheme changes for theme=system', async () => {
    await setTheme('system');
    // First read installs the listener; lazily-attached on first read.
    expect(getResolvedTheme()).toBe('dark');
    mediaList.fire(false);
    expect(getResolvedTheme()).toBe('light');
    mediaList.fire(true);
    expect(getResolvedTheme()).toBe('dark');
  });

  it('teardownThemeModeForTest removes the matchMedia listener', async () => {
    await setTheme('system');
    getResolvedTheme();
    teardownThemeModeForTest();
    // After teardown, firing the now-detached query has no effect on
    // the cached state. The next read re-detects from matchMedia,
    // which is now the (refreshed) initial value.
    expect(getResolvedTheme()).toBe('dark');
  });

  it('mode=light overrides system preference', async () => {
    // matchMedia says dark, but appearance.mode=light wins.
    await setTheme('light');
    expect(getResolvedTheme()).toBe('light');
  });

  it('mode=dark overrides system preference', async () => {
    // matchMedia returns false (light) by default after fresh teardown.
    mediaList.matches = false;
    await setTheme('dark');
    expect(getResolvedTheme()).toBe('dark');
  });

  it('does not wake mode consumers when only the window-ground cache moves', async () => {
    // The resolver is read by every palette consumer in the app. Reading the
    // whole selection box here made the applier's `windowBackground` write —
    // a value no mode consumer can observe — trigger a full re-resolve plus a
    // `CSS.supports` pass in every one of them, all settling identical.
    await setTheme('dark');
    const reads: string[] = [];
    const stop = $effect.root(() => {
      const mode = $derived(getResolvedTheme());
      $effect(() => {
        reads.push(mode);
      });
    });

    try {
      flushSync();
      expect(reads).toHaveLength(1);

      await setAppearance({ windowBackground: '#101017' });
      flushSync();
      expect(reads).toHaveLength(1);

      // …and a real mode change still lands.
      await setAppearance({ mode: 'light' });
      flushSync();
      expect(reads).toEqual(['dark', 'light']);
    } finally {
      stop();
    }
  });
});
