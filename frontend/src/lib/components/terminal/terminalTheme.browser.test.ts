// The xterm palette bridge against the REAL cascade.
//
// Runs in the `browser` vitest project for the same reason
// `markdown/mermaidTokens.browser.test.ts` does: the app's palette is stated
// in `oklch()`, xterm's parser accepts only hex / `rgb()` / `rgba()`, and the
// conversion between the two needs a canvas. happy-dom has neither, so the
// resolved half of this module is unassertable there.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import '../../../app.css';
import { applyThemeClass } from '../../utils/theme';
import {
  applyTheme,
  resetThemeApplyForTest,
  USER_THEME_STYLE_ID,
} from '../../theme/themeApply.svelte';
import { parseTheme } from '../../theme/themeParse';
import { getXtermTheme, resetXtermThemeCache, xtermPaletteIdentity } from './terminalTheme';

// xterm parses these three forms and nothing else.
const XTERM_COLOR = /^(#[0-9a-fA-F]{3,8}|rgba?\([\d.,\s]+\))$/;

// `utils/cssColorProbe` uses `rgb(1, 2, 3)` as its unresolved-token sentinel,
// so a fixture that paints a token that exact color reads back as "did not
// resolve". Never use it as a theme value in this file.

const ANSI_KEYS = [
  'black', 'red', 'green', 'yellow', 'blue', 'magenta', 'cyan', 'white',
  'brightBlack', 'brightRed', 'brightGreen', 'brightYellow',
  'brightBlue', 'brightMagenta', 'brightCyan', 'brightWhite',
] as const;

const REQUIRED_KEYS = [
  'background', 'foreground', 'cursor', 'cursorAccent',
  'selectionBackground', 'selectionInactiveBackground',
  ...ANSI_KEYS,
] as const;

function baseInput(mode: 'dark' | 'light') {
  return {
    mode,
    appearance: { uiTheme: 'default', codeTheme: 'github' },
    themes: [],
    revision: 1,
  };
}

let priorClass = '';

beforeEach(() => {
  priorClass = document.documentElement.className;
  resetXtermThemeCache();
  resetThemeApplyForTest();
});

afterEach(() => {
  document.documentElement.className = priorClass;
  document.getElementById(USER_THEME_STYLE_ID)?.remove();
  resetXtermThemeCache();
  resetThemeApplyForTest();
});

describe('getXtermTheme (real cascade)', () => {
  for (const mode of ['dark', 'light'] as const) {
    it(`resolves every ${mode} slot to something xterm can parse`, () => {
      applyThemeClass(mode);
      applyTheme(baseInput(mode));
      const theme = getXtermTheme(mode) as Record<string, string>;
      for (const key of REQUIRED_KEYS) {
        expect(theme[key], key).toMatch(XTERM_COLOR);
      }
      // The whole point of the rewrite: nothing leaves here in a color space
      // xterm's parser would silently drop.
      for (const value of Object.values(theme)) {
        expect(value).not.toContain('oklch');
        expect(value).not.toContain('oklab');
      }
    });
  }

  it('produces genuinely different palettes per mode', () => {
    applyThemeClass('dark');
    applyTheme(baseInput('dark'));
    const dark = getXtermTheme('dark') as Record<string, string>;
    applyThemeClass('light');
    applyTheme(baseInput('light'));
    const light = getXtermTheme('light') as Record<string, string>;

    expect(dark.background).not.toBe(light.background);
    expect(dark.foreground).not.toBe(light.foreground);
    // A light-mode "white" that is actually white would be invisible on the
    // light ground — the old hand-maintained palette's one interesting claim,
    // now a property of the tokens instead of a duplicate of them.
    expect(light.white).not.toBe('rgb(255, 255, 255)');
  });

  it('paints the caret and its accent from the two tokens, swapped', () => {
    applyThemeClass('dark');
    applyTheme(baseInput('dark'));
    const theme = getXtermTheme('dark') as Record<string, string>;
    expect(theme.cursor).toBe(theme.foreground);
    expect(theme.cursorAccent).toBe(theme.background);
  });

  it('tints selection from the accent instead of restating a slab', () => {
    applyThemeClass('dark');
    applyTheme(baseInput('dark'));
    const theme = getXtermTheme('dark') as Record<string, string>;
    expect(theme.selectionBackground).toMatch(/^rgba\(\d+, \d+, \d+, 0\.4\)$/);
    expect(theme.selectionInactiveBackground).toMatch(/^rgba\(\d+, \d+, \d+, 0\.22\)$/);
    // Translucent, so the glyphs keep their own colors underneath — which is
    // why there is no `selectionForeground` to maintain per theme any more.
    expect(theme.selectionForeground).toBeUndefined();
  });

  it('hands back a STABLE object for an unchanged identity', () => {
    // Load-bearing, not an optimization: `term.options.theme = x` rebuilds
    // xterm's glyph atlas, so a fresh-but-equal object on an unrelated tick
    // re-rasterizes every open terminal.
    applyThemeClass('dark');
    applyTheme(baseInput('dark'));
    const first = getXtermTheme('dark');
    expect(getXtermTheme('dark')).toBe(first);
    resetXtermThemeCache();
    expect(getXtermTheme('dark')).not.toBe(first);
  });

  it('follows a theme-file edit under an unchanged selection', () => {
    // The revision is the third component of the palette identity, and this
    // is the whole reason it exists: the agent-edit loop must repaint an
    // already-mounted terminal.
    applyThemeClass('dark');
    const first = parseTheme(
      'term',
      JSON.stringify({ dark: { code: { 'terminal-bg': 'rgb(11, 22, 33)' } } }),
    );
    applyTheme({ ...baseInput('dark'), themes: [first], appearance: { uiTheme: 'default', codeTheme: 'term' } });
    const before = getXtermTheme('dark') as Record<string, string>;
    expect(before.background).toBe('rgb(11, 22, 33)');
    const identityBefore = xtermPaletteIdentity('dark');

    const edited = parseTheme(
      'term',
      JSON.stringify({ dark: { code: { 'terminal-bg': 'rgb(4, 5, 6)' } } }),
    );
    applyTheme({
      ...baseInput('dark'),
      themes: [edited],
      appearance: { uiTheme: 'default', codeTheme: 'term' },
      revision: 2,
    });
    expect(xtermPaletteIdentity('dark')).not.toBe(identityBefore);
    expect((getXtermTheme('dark') as Record<string, string>).background).toBe('rgb(4, 5, 6)');
  });

  it('themes the ANSI slots from the same tokens the chat renderer uses', () => {
    applyThemeClass('dark');
    const red = parseTheme(
      'red',
      JSON.stringify({ dark: { ansi: { 'ansi-fg-31': 'rgb(200, 10, 10)' } } }),
    );
    applyTheme({
      ...baseInput('dark'),
      themes: [red],
      appearance: { uiTheme: 'default', codeTheme: 'red' },
    });
    expect((getXtermTheme('dark') as Record<string, string>).red).toBe('rgb(200, 10, 10)');
  });
});
