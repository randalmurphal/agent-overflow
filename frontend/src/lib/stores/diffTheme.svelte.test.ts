import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { getDiffTheme, teardownDiffThemeForTest } from './diffTheme.svelte';
import { loadSettings } from './settings.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { tokenCacheKey } from '../utils/tokenCache';
import { getSharedTokenCache, resetSharedTokenCacheForTest } from '../utils/tokenCacheReactive.svelte';
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

describe('diffTheme', () => {
  beforeEach(() => {
    mediaList = makeFakeMatchMedia(true);
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => mediaList),
    );
    teardownDiffThemeForTest();
    resetSharedTokenCacheForTest();
  });

  afterEach(() => {
    teardownDiffThemeForTest();
    resetSharedTokenCacheForTest();
    vi.unstubAllGlobals();
  });

  it("returns 'github-dark' for theme='dark'", async () => {
    await setTheme('dark');
    expect(getDiffTheme()).toBe('github-dark');
  });

  it("returns 'github-light' for theme='light'", async () => {
    await setTheme('light');
    expect(getDiffTheme()).toBe('github-light');
  });

  it("returns the system preference for theme='system'", async () => {
    await setTheme('system');
    expect(getDiffTheme()).toBe('github-dark');
  });

  it('reflects system color-scheme changes for theme=system', async () => {
    await setTheme('system');
    // First read installs the listener; lazily-attached on first read.
    expect(getDiffTheme()).toBe('github-dark');
    mediaList.fire(false);
    expect(getDiffTheme()).toBe('github-light');
    mediaList.fire(true);
    expect(getDiffTheme()).toBe('github-dark');
  });

  it('teardownDiffThemeForTest removes the matchMedia listener', async () => {
    await setTheme('system');
    getDiffTheme();
    teardownDiffThemeForTest();
    // After teardown, firing the now-detached query has no effect on
    // the cached state. The next read re-detects from matchMedia,
    // which is now the (refreshed) initial value.
    expect(getDiffTheme()).toBe('github-dark');
  });

  it('getDiffTheme is a pure read — does not evict the cache (eviction is the caller\'s responsibility)', async () => {
    // The store deliberately does NOT touch the shared cache when
    // resolving the theme — Svelte 5 forbids state mutations during
    // `$derived` recomputation, and DiffSidebarBody.svelte calls
    // `$derived(getDiffTheme())`. Eviction lives in a sibling
    // `$effect` in DiffSidebarBody. This test pins the contract.
    const cache = getSharedTokenCache();
    cache.set(tokenCacheKey('github-dark', 'typescript', 'const x = 1;'), [
      { content: 'const', color: '#ff79c6' },
    ]);
    cache.set(tokenCacheKey('github-light', 'typescript', 'const x = 1;'), [
      { content: 'const', color: '#000000' },
    ]);
    expect(cache.size).toBe(2);

    await setTheme('dark');
    expect(getDiffTheme()).toBe('github-dark');
    await setTheme('light');
    expect(getDiffTheme()).toBe('github-light');

    // Both themes' entries survive — eviction isn't this function's job.
    expect(cache.get(tokenCacheKey('github-dark', 'typescript', 'const x = 1;'))).toBeDefined();
    expect(cache.get(tokenCacheKey('github-light', 'typescript', 'const x = 1;'))).toBeDefined();
  });
});
