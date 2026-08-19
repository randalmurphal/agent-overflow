// The bridge's DEGRADED half. happy-dom has no canvas, so
// `utils/cssColorProbe` can normalize nothing and every token reads back
// undefined — which is precisely the "palette unresolved" branch this file
// pins. The resolved palette itself is asserted in the real-Chromium
// `terminalTheme.browser.test.ts`; there is nothing meaningful a DOM without
// a cascade could say about it.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getXtermTheme, resetXtermThemeCache, xtermPaletteIdentity } from './terminalTheme';

beforeEach(() => {
  resetXtermThemeCache();
});

afterEach(() => {
  resetXtermThemeCache();
  vi.restoreAllMocks();
});

describe('getXtermTheme without a resolvable palette', () => {
  it("hands back xterm's own defaults rather than a stale duplicate", () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    // An empty ITheme leaves every xterm default in place, which is a legible
    // terminal. The alternative — the 44 hand-maintained hex values this
    // module used to carry — is the drift the bridge exists to delete.
    expect(getXtermTheme('dark')).toEqual({});
    expect(getXtermTheme('light')).toEqual({});
    warn.mockRestore();
  });

  it('reports the failure exactly once per session', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    getXtermTheme('dark');
    getXtermTheme('dark');
    getXtermTheme('light');
    expect(warn).toHaveBeenCalledTimes(1);
    warn.mockRestore();
  });

  it('does not cache the failure, so a later resolvable tick can win', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    const first = getXtermTheme('dark');
    expect(getXtermTheme('dark')).not.toBe(first);
    warn.mockRestore();
  });
});

describe('xtermPaletteIdentity', () => {
  it('carries the mode as well as the palette, because the probe reads the live cascade', () => {
    // The identity is the cache key a consumer's `$effect` tracks. Mode has to
    // be in it: nothing else distinguishes the light resolution of one theme
    // from its dark one.
    expect(xtermPaletteIdentity('dark')).not.toBe(xtermPaletteIdentity('light'));
    expect(xtermPaletteIdentity('dark').startsWith('dark|')).toBe(true);
  });
});
