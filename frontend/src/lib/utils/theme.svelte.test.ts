// Covers the collapsed theme pipeline: ONE prefers-color-scheme
// subscription (`stores/themeMode.svelte.ts`), and `applyThemeClass` as a
// pure DOM stamp driven from it — the shape App.svelte wires up.
//
// Before the collapse there were two independent matchMedia listeners (one
// here, one in the store), so an OS flip travelled two unrelated paths with
// no ordering relationship. The "one source" test below is the regression
// pin for that: a settings flip must move the html class and a
// getResolvedTheme consumer within the same flush.

import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import { flushSync } from 'svelte';
import { applyThemeClass } from './theme';
import { getResolvedTheme, teardownThemeModeForTest } from '../stores/themeMode.svelte';
import { loadSettings, resetSettingsForTest } from '../stores/settings.svelte';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { Settings } from '../types/settings';

const BASE_SETTINGS: Partial<Settings> = {
  theme: 'system',
  timestampFormat: 'locale',
  network: { bindAll: false },
};

interface FakeMediaQueryList {
  matches: boolean;
  /** Listeners currently attached — the leak check reads this. */
  readonly listenerCount: number;
  addEventListener: (type: 'change', cb: (e: MediaQueryListEvent) => void) => void;
  removeEventListener: (type: 'change', cb: (e: MediaQueryListEvent) => void) => void;
  fire(matches: boolean): void;
}

function makeFakeMatchMedia(initialDark: boolean): FakeMediaQueryList {
  const handlers: Array<(e: MediaQueryListEvent) => void> = [];
  const list: FakeMediaQueryList = {
    matches: initialDark,
    get listenerCount() {
      return handlers.length;
    },
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
      for (const h of [...handlers]) h(e);
    },
  };
  return list;
}

let mediaList: FakeMediaQueryList;

async function setTheme(theme: 'light' | 'dark' | 'system'): Promise<void> {
  setBindingMock('GetSettings', async () => ({ ...BASE_SETTINGS, theme }) as Settings);
  await loadSettings();
}

function htmlClasses(): string[] {
  return [...document.documentElement.classList];
}

describe('applyThemeClass', () => {
  beforeEach(() => {
    document.documentElement.classList.remove('light', 'dark');
  });

  it("stamps both class names so only the resolved one is present", () => {
    applyThemeClass('light');
    expect(htmlClasses()).toContain('light');
    expect(htmlClasses()).not.toContain('dark');

    applyThemeClass('dark');
    // `.dark` is what the vendored streamdown MutationObserver reads; only
    // `html.light` is read by our own CSS. Both are written.
    expect(htmlClasses()).toContain('dark');
    expect(htmlClasses()).not.toContain('light');
  });

  it('does not touch the class attribute when already correct', async () => {
    // Asserted through a REAL MutationObserver rather than classList
    // spies, because the claim is about what an observer sees: the
    // vendored streamdown watches the root element's attributes, and a
    // `classList.add` of a class already present is a no-op the spy would
    // have counted anyway. This is the actual contract.
    applyThemeClass('dark');
    const records: MutationRecord[] = [];
    const observer = new MutationObserver((mutations) => records.push(...mutations));
    observer.observe(document.documentElement, { attributes: true });
    try {
      applyThemeClass('dark');
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(records).toEqual([]);

      // …and the same observer DOES see a real change, so the assertion
      // above cannot pass by being blind.
      applyThemeClass('light');
      await new Promise((resolve) => setTimeout(resolve, 0));
      expect(records.length).toBeGreaterThan(0);
    } finally {
      observer.disconnect();
    }
  });

  it('heals a root that somehow carries both classes', () => {
    document.documentElement.classList.add('light', 'dark');
    applyThemeClass('dark');
    expect(htmlClasses()).toContain('dark');
    expect(htmlClasses()).not.toContain('light');
  });
});

describe('theme pipeline (App.svelte wiring)', () => {
  beforeEach(() => {
    mediaList = makeFakeMatchMedia(true);
    vi.stubGlobal(
      'matchMedia',
      vi.fn(() => mediaList),
    );
    teardownThemeModeForTest();
    resetSettingsForTest();
    document.documentElement.classList.remove('light', 'dark');
  });

  afterEach(() => {
    teardownThemeModeForTest();
    resetSettingsForTest();
    vi.unstubAllGlobals();
  });

  /** The exact effect App.svelte runs, plus a second consumer of the store. */
  function mountPipeline(): { consumerReads: string[]; stop: () => void } {
    const consumerReads: string[] = [];
    const stop = $effect.root(() => {
      // `$effect.pre`, matching App.svelte: a render effect on the root
      // component runs before every descendant USER effect in the flush,
      // which is what puts the class stamp ahead of anything that
      // resolves a palette off the cascade (the mermaid bridge reads it
      // from inside the vendored `{@attach}`, a user effect).
      $effect.pre(() => {
        applyThemeClass(getResolvedTheme());
      });
      // Stands in for the xterm / mermaid consumers, which read the same
      // resolver rather than a second subscription of their own.
      $effect(() => {
        consumerReads.push(getResolvedTheme());
      });
    });
    flushSync();
    return { consumerReads, stop };
  }

  it('paints from the default settings before GetSettings answers', () => {
    // Default `theme: 'system'` with a dark OS preference — the first frame
    // must already be stamped, not blank.
    const { consumerReads, stop } = mountPipeline();
    try {
      expect(htmlClasses()).toContain('dark');
      expect(consumerReads.at(-1)).toBe('dark');
    } finally {
      stop();
    }
  });

  it('moves the html class and store consumers from one source', async () => {
    const { consumerReads, stop } = mountPipeline();
    try {
      await setTheme('light');
      flushSync();
      expect(htmlClasses()).toContain('light');
      expect(consumerReads.at(-1)).toBe('light');

      await setTheme('dark');
      flushSync();
      expect(htmlClasses()).toContain('dark');
      expect(consumerReads.at(-1)).toBe('dark');
    } finally {
      stop();
    }
  });

  it('follows OS flips while the setting is system', async () => {
    const { consumerReads, stop } = mountPipeline();
    try {
      await setTheme('system');
      flushSync();
      expect(htmlClasses()).toContain('dark');

      mediaList.fire(false);
      flushSync();
      expect(htmlClasses()).toContain('light');
      expect(consumerReads.at(-1)).toBe('light');

      mediaList.fire(true);
      flushSync();
      expect(htmlClasses()).toContain('dark');
      expect(consumerReads.at(-1)).toBe('dark');
    } finally {
      stop();
    }
  });

  it('ignores OS flips while the setting is explicit', async () => {
    const { stop } = mountPipeline();
    try {
      await setTheme('light');
      flushSync();
      mediaList.fire(true);
      flushSync();
      expect(htmlClasses()).toContain('light');
    } finally {
      stop();
    }
  });

  it('keeps exactly one matchMedia listener across system→explicit→system', async () => {
    const { stop } = mountPipeline();
    try {
      await setTheme('system');
      flushSync();
      const attached = mediaList.listenerCount;
      expect(attached).toBe(1);

      await setTheme('dark');
      flushSync();
      await setTheme('system');
      flushSync();
      await setTheme('light');
      flushSync();
      await setTheme('system');
      flushSync();

      // One resolver, one subscription — re-reads must not stack listeners.
      expect(mediaList.listenerCount).toBe(1);
      mediaList.fire(false);
      flushSync();
      expect(htmlClasses()).toContain('light');
    } finally {
      stop();
    }
  });

  it('leaves no matchMedia listener behind on teardown', async () => {
    const { stop } = mountPipeline();
    try {
      await setTheme('system');
      flushSync();
      expect(mediaList.listenerCount).toBe(1);
    } finally {
      stop();
    }
    // The store's teardown is what every suite's `beforeEach` relies on;
    // a leak here means listeners accumulate across the whole run and an
    // OS flip in one test moves another test's document.
    teardownThemeModeForTest();
    expect(mediaList.listenerCount).toBe(0);
  });
});
