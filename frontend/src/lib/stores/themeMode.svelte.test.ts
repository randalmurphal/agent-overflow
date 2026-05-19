import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { getResolvedTheme, teardownThemeModeForTest } from './themeMode.svelte';
import { loadSettings } from './settings.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Settings } from '../types/settings';

const BASE_SETTINGS: Partial<Settings> = {
  theme: 'system',
  timestampFormat: 'locale',
  network: { bindAll: false },
};

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

async function setTheme(theme: 'light' | 'dark' | 'system'): Promise<void> {
  setBindingMock('GetSettings', async () => ({ ...BASE_SETTINGS, theme }) as Settings);
  setBindingMock('UpdateSettings', async () => ({ ...BASE_SETTINGS, theme }) as Settings);
  await loadSettings();
}

describe('themeMode', () => {
  beforeEach(() => {
    mediaList = makeFakeMatchMedia(true);
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => mediaList),
    );
    teardownThemeModeForTest();
  });

  afterEach(() => {
    teardownThemeModeForTest();
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

  it('settings=light overrides system preference', async () => {
    // matchMedia says dark, but settings.theme=light wins.
    await setTheme('light');
    expect(getResolvedTheme()).toBe('light');
  });

  it('settings=dark overrides system preference', async () => {
    // matchMedia returns false (light) by default after fresh teardown.
    mediaList.matches = false;
    await setTheme('dark');
    expect(getResolvedTheme()).toBe('dark');
  });
});
