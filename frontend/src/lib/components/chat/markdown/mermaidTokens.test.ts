import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  mermaidPaletteIdentity,
  resetMermaidThemeCaches,
  resolveMermaidThemeConfig,
} from './mermaidTokens';

// happy-dom has neither a canvas nor a cascade that computes `oklch()`,
// so EVERY token comes back unresolved here. That is not a limitation to
// work around — it is the degraded path, and this file is where its
// contract lives. What the palette actually resolves to against the real
// stylesheet is `mermaidTokens.browser.test.ts`'s job.

beforeEach(() => {
  resetMermaidThemeCaches();
});

afterEach(() => {
  resetMermaidThemeCaches();
});

describe('resolveMermaidThemeConfig', () => {
  it('pins the derive-from-variables theme in both modes', () => {
    // `base` is the ONLY mermaid theme that derives its whole palette
    // from themeVariables; 'default'/'dark' ignore most of them, which
    // is what left diagrams on a palette of their own.
    for (const mode of ['light', 'dark'] as const) {
      expect(resolveMermaidThemeConfig(mode).theme).toBe('base');
    }
  });

  it('carries the resolved mode as mermaid darkMode', () => {
    const dark = resolveMermaidThemeConfig('dark');
    const light = resolveMermaidThemeConfig('light');
    expect(dark.darkMode).toBe(true);
    expect(light.darkMode).toBe(false);
    // theme-base reads its own `darkMode` off themeVariables, not off
    // the top-level config field.
    expect((dark.themeVariables as Record<string, unknown>).darkMode).toBe(true);
    expect((light.themeVariables as Record<string, unknown>).darkMode).toBe(false);
  });

  it('emits no CSS the color math cannot parse', () => {
    // Whatever leaves this module is something mermaid's `khroma` color
    // math can parse. `oklch()` (what our tokens literally are) and
    // `var()` (an unresolved token) both throw inside it, taking the
    // whole diagram down.
    const CONCRETE = /^(?:#[0-9a-f]{3,8}|rgba?\([^)]*\))$/i;
    for (const mode of ['light', 'dark'] as const) {
      const vars = resolveMermaidThemeConfig(mode).themeVariables as Record<
        string,
        unknown
      >;
      for (const [key, value] of Object.entries(vars)) {
        if (key === 'darkMode' || key === 'fontFamily') continue;
        expect(typeof value, key).toBe('string');
        expect(value as string, key).toMatch(CONCRETE);
      }
    }
  });

  it('does NOT cache an empty palette, and says so once', () => {
    // A resolve that came back with nothing is a failure, not an answer.
    // Caching it would degrade every diagram for the rest of the session
    // on one bad moment; silence would make that invisible.
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const first = resolveMermaidThemeConfig('dark');
      const second = resolveMermaidThemeConfig('dark');
      expect(second).not.toBe(first);
      expect(warn).toHaveBeenCalledTimes(1);
      expect(String(warn.mock.calls[0][0])).toContain('mermaid palette unresolved');

      // Once per session, not once per diagram.
      resolveMermaidThemeConfig('light');
      expect(warn).toHaveBeenCalledTimes(1);
    } finally {
      warn.mockRestore();
    }
  });
});

describe('mermaidPaletteIdentity', () => {
  it('names the palette source, not just the mode', () => {
    // The font stack is resolved from `--font-sans` and BAKED into the
    // config, and `utils/fonts.ts#applyFonts` rewrites that variable
    // live — so the sans-font setting is part of the palette's identity,
    // and a mode-only key left every rendered diagram in the old font.
    // Phase 2 widens THIS string with the active theme file.
    const identity = mermaidPaletteIdentity();
    expect(identity).toMatch(/^(?:light|dark)\|/);
    expect(identity.split('|').length).toBeGreaterThan(1);
    expect(identity.split('|')[1]).toBeTruthy();
  });

  it('is a pure read — it must not stamp the document', () => {
    // The stamp is App.svelte's `$effect.pre`, a root render effect that
    // runs ahead of every descendant user effect. Nothing on the read
    // path may write the class, or two writers race for it.
    document.documentElement.classList.remove('light', 'dark');
    mermaidPaletteIdentity();
    resolveMermaidThemeConfig('light');
    resolveMermaidThemeConfig('dark');
    expect(document.documentElement.classList.contains('light')).toBe(false);
    expect(document.documentElement.classList.contains('dark')).toBe(false);
  });
});
