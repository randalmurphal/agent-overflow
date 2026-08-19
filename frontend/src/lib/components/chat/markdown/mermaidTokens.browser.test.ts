import { afterEach, beforeEach, describe, expect, it } from 'vitest';
// Import the REAL production stylesheet: this file's whole subject is
// whether app TOKENS survive the trip into mermaid's config, so stubbing
// the palette would test nothing. Runs in the `browser` vitest project
// (real Chromium via Playwright) because happy-dom has no cascade for
// `oklch()` and no canvas to normalize it with — see frontend/vitest.config.ts.
import '../../../../app.css';
import {
  readTokenColors,
  readTokenFontFamily,
  readTokenStyles,
  toConcreteColor,
} from '../../../utils/cssColorProbe';
import { applyThemeClass } from '../../../utils/theme';
import {
  resetMermaidThemeCaches,
  resolveMermaidThemeConfig,
} from './mermaidTokens';

const RGB = /^rgba?\((\d+), (\d+), (\d+)(, [\d.]+)?\)$/;

// The variables that must survive; `darkMode` and `fontFamily` are not
// colors and are asserted separately.
const COLOR_VARS = [
  'background',
  'mainBkg',
  'primaryColor',
  'secondaryColor',
  'tertiaryColor',
  'primaryTextColor',
  'textColor',
  'primaryBorderColor',
  'lineColor',
  'noteBkgColor',
  'noteTextColor',
];

let priorClass = '';

beforeEach(() => {
  priorClass = document.documentElement.className;
  resetMermaidThemeCaches();
});

afterEach(() => {
  document.documentElement.className = priorClass;
  resetMermaidThemeCaches();
});

describe('cssColorProbe (real cascade + canvas)', () => {
  it('normalizes oklch to sRGB, which the browser will not do on its own', () => {
    // getComputedStyle().color serializes IN THE DECLARED SPACE per CSS
    // Color 4 — it hands back `oklch(...)` verbatim, and so does
    // `ctx.fillStyle`'s getter. Only painting and reading the pixel
    // back leaves the color space. If this ever starts failing because
    // the input passed straight through, the canvas hop was removed on
    // a false premise.
    const rgb = toConcreteColor('oklch(0.178 0.014 285.82)');
    expect(rgb).toMatch(RGB);
    expect(rgb).not.toContain('oklch');
  });

  it('normalizes color-mix tokens, which compute to oklab', () => {
    const rgb = toConcreteColor(
      'color-mix(in oklab, oklch(0.95 0.006 285.82) 80%, transparent)',
    );
    expect(rgb).toMatch(RGB);
    expect(rgb).not.toContain('oklab');
  });

  it('round-trips rgb() rather than passing it through', () => {
    expect(toConcreteColor('rgb(16, 16, 23)')).toBe('rgb(16, 16, 23)');
  });

  it('drops a fully transparent color', () => {
    // Uniform with the hex path, and the reason the round trip seeds
    // `fillStyle` with `transparent`: a REJECTED value and a genuinely
    // transparent one both leave alpha 0, and both mean "omit".
    expect(toConcreteColor('rgba(0, 0, 0, 0)')).toBeUndefined();
    expect(toConcreteColor('transparent')).toBeUndefined();
    expect(toConcreteColor('not-a-color')).toBeUndefined();
  });

  it('reads several tokens through one probe', () => {
    applyThemeClass('dark');
    const colors = readTokenColors(['--surface-0', '--surface-1', '--surface-2']);
    for (const [token, value] of Object.entries(colors)) {
      expect(value, token).toMatch(RGB);
    }
    // Distinct tokens must land in distinct slots — a slot-reuse bug
    // would smear one token's value across the batch.
    expect(new Set(Object.values(colors)).size).toBe(3);
  });

  it('reports an unresolved token as undefined instead of a plausible color', () => {
    // An invalid `var()` is "invalid at computed value time", which makes
    // the property INHERIT or reset rather than disappear — without the
    // sentinel this read back as a real, entirely wrong color.
    const colors = readTokenColors(['--ao-token-that-does-not-exist', '--surface-1']);
    expect(colors['--ao-token-that-does-not-exist']).toBeUndefined();
    expect(colors['--surface-1']).toMatch(RGB);
  });

  it('reads a font stack from the same probe pass', () => {
    const { colors, fontFamily } = readTokenStyles(['--surface-1'], '--font-sans');
    expect(colors['--surface-1']).toMatch(RGB);
    expect(fontFamily).toContain('Geist Sans');
    expect(readTokenFontFamily('--font-sans')).toContain('Geist Sans');
    expect(readTokenFontFamily('--ao-font-that-does-not-exist')).toBeUndefined();
  });
});

describe('mermaid token bridge (real cascade)', () => {
  for (const mode of ['light', 'dark'] as const) {
    it(`resolves every ${mode} diagram variable to a parseable color`, () => {
      // The stamp is App.svelte's job now, not the resolver's — this is
      // standing in for it.
      applyThemeClass(mode);
      const config = resolveMermaidThemeConfig(mode);
      const vars = config.themeVariables as Record<string, unknown>;
      expect(config.theme).toBe('base');
      expect(config.darkMode).toBe(mode === 'dark');
      for (const name of COLOR_VARS) {
        expect(vars[name], name).toMatch(RGB);
      }
      // The label font comes from the app's sans stack, not mermaid's
      // hardcoded `monospace`.
      expect(String(vars.fontFamily)).toContain('Geist Sans');
    });
  }

  it('produces genuinely different palettes per mode', () => {
    // The proof that the probe reads the live cascade rather than a
    // constant — and the reason the vendored SVG cache had to start
    // keying on themeVariables (both modes pin `theme: 'base'`).
    applyThemeClass('light');
    const light = resolveMermaidThemeConfig('light').themeVariables as Record<
      string,
      unknown
    >;
    applyThemeClass('dark');
    const dark = resolveMermaidThemeConfig('dark').themeVariables as Record<
      string,
      unknown
    >;
    for (const name of COLOR_VARS) {
      expect(dark[name], name).not.toBe(light[name]);
    }
  });

  it('hands back a STABLE object for an unchanged palette identity', () => {
    // Load-bearing, not an optimization detail: `ChatMarkdown`'s
    // `$derived` re-evaluates on every settings write (the settings
    // object is replaced wholesale), and only identity-stability stops
    // the vendored `{@attach}` from re-running `mermaid.render` for every
    // visible diagram on an unrelated save.
    applyThemeClass('dark');
    const first = resolveMermaidThemeConfig('dark');
    expect(resolveMermaidThemeConfig('dark')).toBe(first);
    resetMermaidThemeCaches();
    expect(resolveMermaidThemeConfig('dark')).not.toBe(first);
  });

  it('resolves without stamping the document itself', () => {
    // The resolver READS the cascade; App.svelte's `$effect.pre` writes
    // the class. Two writers would race for it.
    applyThemeClass('dark');
    const before = document.documentElement.className;
    resolveMermaidThemeConfig('light');
    expect(document.documentElement.className).toBe(before);
  });
});
