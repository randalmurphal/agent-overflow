// The two apply-time jobs that only a real engine can do: per-token
// `CSS.supports('color', …)` rejection, and the `--accent-fg` derivation that
// needs a canvas to normalize `oklch()`.
//
// Runs in the `browser` vitest project (real Chromium) for the same reason
// `mermaidTokens.browser.test.ts` does — happy-dom answers `false` to every
// `CSS.supports('color', …)` probe and has no canvas, so the applier
// deliberately SKIPS both jobs there. A unit test would assert on a code path
// that never runs in production. The production stylesheet is imported because
// the ground probe reads the live cascade.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import '../../app.css';
import { applyThemeClass } from '../utils/theme';
import {
  THEME_BOOT_MAX_CSS,
  THEME_BOOT_STORAGE_KEY,
  USER_THEME_STYLE_ID,
  applyTheme,
  deriveAccentForeground,
  enrichDeclarations,
  readWindowGroundHex,
  resetThemeApplyForTest,
  stampBootTheme,
} from './themeApply.svelte';
import { parseTheme, type ThemeWarning } from './themeParse';
import { resolveTheme, type ResolveInput } from './themeResolve';

const HEX = /^#[0-9a-f]{6}$/;
const WHITE = 'rgb(255, 255, 255)';
const BLACK = 'rgb(0, 0, 0)';

function theme(id: string, body: Record<string, unknown>) {
  return parseTheme(id, JSON.stringify(body));
}

function input(overrides: Partial<ResolveInput> = {}): ResolveInput {
  return {
    mode: 'dark',
    appearance: { uiTheme: 'default', codeTheme: 'github' },
    themes: [],
    revision: 1,
    ...overrides,
  };
}

let priorClass = '';

beforeEach(() => {
  priorClass = document.documentElement.className;
  resetThemeApplyForTest();
  localStorage.removeItem(THEME_BOOT_STORAGE_KEY);
});

afterEach(() => {
  document.documentElement.className = priorClass;
  document.getElementById(USER_THEME_STYLE_ID)?.remove();
  resetThemeApplyForTest();
  localStorage.removeItem(THEME_BOOT_STORAGE_KEY);
});

describe('per-token rejection', () => {
  it('drops only the value the engine cannot parse, and says which', () => {
    // `themeResolve.isSafeDeclarationValue` already refuses anything with
    // CSS punctuation in it, so a value that reaches this check is
    // structurally plausible and merely NOT A COLOR — which is exactly the
    // class of mistake a hand-authored file makes.
    const broken = theme('broken', {
      name: 'Broken',
      dark: { colors: { 'surface-1': 'definitely-not-a-color', 'text-primary': 'rgb(1, 2, 3)' } },
    });
    const resolved = resolveTheme(
      input({ themes: [broken], appearance: { uiTheme: 'broken', codeTheme: 'github' } }),
    );
    const warnings: ThemeWarning[] = [];
    const enriched = enrichDeclarations(resolved.declarations, warnings);

    const keys = enriched.root.map((decl) => decl.key);
    expect(keys).toContain('text-primary');
    expect(keys).not.toContain('surface-1');

    expect(warnings).toHaveLength(1);
    // Distinct from the resolver's `unsafe-value`, which is about characters a
    // declaration cannot carry. This one is structurally fine and simply is
    // not a color, which is a different thing for the user to go look at.
    expect(warnings[0]!.code).toBe('not-a-color');
    expect(warnings[0]!.themeId).toBe('broken');
    expect(warnings[0]!.path).toBe('dark.colors.surface-1');
    // The sentence has to name the token, or the user cannot find the line.
    expect(warnings[0]!.message).toContain('surface-1');
  });

  it('rejects per token rather than per file', () => {
    const mixed = theme('mixed', {
      dark: {
        colors: {
          'surface-1': 'notacolor',
          'surface-2': 'alsonotacolor',
          accent: 'oklch(0.7 0.15 250)',
          'text-primary': '#abcdef',
        },
      },
    });
    const resolved = resolveTheme(
      input({ themes: [mixed], appearance: { uiTheme: 'mixed', codeTheme: 'github' } }),
    );
    const warnings: ThemeWarning[] = [];
    const enriched = enrichDeclarations(resolved.declarations, warnings);
    const keys = new Set(enriched.root.map((decl) => decl.key));

    expect(warnings).toHaveLength(2);
    expect(keys.has('accent')).toBe(true);
    expect(keys.has('text-primary')).toBe(true);
  });
});

describe('accent foreground derivation', () => {
  it('picks the label with the higher contrast against the accent', () => {
    expect(deriveAccentForeground('#ffffff')).toBe(BLACK);
    expect(deriveAccentForeground('#000000')).toBe(WHITE);
    // A pale accent is the case app.css's fixed white label strands.
    expect(deriveAccentForeground('oklch(0.92 0.05 95)')).toBe(BLACK);
    expect(deriveAccentForeground('oklch(0.45 0.15 265)')).toBe(WHITE);
  });

  it('answers undefined for something it cannot normalize', () => {
    // Silence, not a guess: app.css's own `--accent-fg` then keeps applying,
    // which is the right answer for "we could not tell".
    expect(deriveAccentForeground('not-a-color')).toBeUndefined();
    expect(deriveAccentForeground('transparent')).toBeUndefined();
  });

  it('emits a derived accent-fg beside an accent the theme states alone', () => {
    const pale = theme('pale', { dark: { colors: { accent: 'oklch(0.93 0.06 95)' } } });
    const resolved = resolveTheme(
      input({ themes: [pale], appearance: { uiTheme: 'pale', codeTheme: 'github' } }),
    );
    const enriched = enrichDeclarations(resolved.declarations, []);
    const derived = enriched.root.find((decl) => decl.key === 'accent-fg');
    expect(derived?.value).toBe(BLACK);
    expect(derived?.cssVar).toBe('--accent-fg');
    // Attribution follows the accent it was derived from, so a warning about
    // it points at a real file.
    expect(derived?.themeId).toBe('pale');
  });

  it('never overrides a foreground the theme states itself', () => {
    const explicit = theme('explicit', {
      dark: { colors: { accent: 'oklch(0.93 0.06 95)', 'accent-fg': '#123456' } },
    });
    const resolved = resolveTheme(
      input({ themes: [explicit], appearance: { uiTheme: 'explicit', codeTheme: 'github' } }),
    );
    const enriched = enrichDeclarations(resolved.declarations, []);
    const foregrounds = enriched.root.filter((decl) => decl.key === 'accent-fg');
    expect(foregrounds).toHaveLength(1);
    expect(foregrounds[0]!.value).toBe('#123456');
  });

  it('derives one when the theme states a foreground the engine rejects', () => {
    // The derivation used to read the INPUT block, so a typo'd `accent-fg`
    // counted as "the theme decided" while the value itself was dropped by the
    // color check — leaving neither, and app.css's fixed white label stranded
    // on whatever accent the theme moved to. Deriving from what SURVIVED is
    // what closes it.
    const typo = theme('typo', {
      dark: { colors: { accent: 'oklch(0.93 0.06 95)', 'accent-fg': 'wihte' } },
    });
    const resolved = resolveTheme(
      input({ themes: [typo], appearance: { uiTheme: 'typo', codeTheme: 'github' } }),
    );
    const warnings: ThemeWarning[] = [];
    const enriched = enrichDeclarations(resolved.declarations, warnings);
    const foregrounds = enriched.root.filter((decl) => decl.key === 'accent-fg');

    expect(foregrounds).toHaveLength(1);
    expect(foregrounds[0]!.value).toBe(BLACK);
    // …and the typo is still reported, so the user can fix the line.
    expect(warnings.some((warning) => warning.path === 'dark.colors.accent-fg')).toBe(true);
  });

  it('derives per variant, so a two-variant theme gets two answers', () => {
    const both = theme('both', {
      dark: { colors: { accent: '#101010' } },
      light: { colors: { accent: '#f0f0f0' } },
    });
    const resolved = resolveTheme(
      input({ themes: [both], appearance: { uiTheme: 'both', codeTheme: 'github' } }),
    );
    const enriched = enrichDeclarations(resolved.declarations, []);
    expect(enriched.root.find((decl) => decl.key === 'accent-fg')?.value).toBe(WHITE);
    expect(enriched.light.find((decl) => decl.key === 'accent-fg')?.value).toBe(BLACK);
  });
});

describe('applyTheme', () => {
  it('writes one style element and reuses it across applies', () => {
    const first = theme('first', { dark: { colors: { 'surface-1': '#111111' } } });
    applyTheme(input({ themes: [first], appearance: { uiTheme: 'first', codeTheme: 'github' } }));

    const elements = document.querySelectorAll(`#${USER_THEME_STYLE_ID}`);
    expect(elements).toHaveLength(1);
    const element = elements[0] as HTMLStyleElement;
    expect(element.textContent).toContain('--surface-1: #111111');

    const second = theme('second', { dark: { colors: { 'surface-1': '#222222' } } });
    applyTheme(input({ themes: [second], appearance: { uiTheme: 'second', codeTheme: 'github' } }));
    expect(document.querySelectorAll(`#${USER_THEME_STYLE_ID}`)).toHaveLength(1);
    expect(document.getElementById(USER_THEME_STYLE_ID)).toBe(element);
    expect(element.textContent).toContain('--surface-1: #222222');
  });

  it('actually repaints the cascade, rejected tokens excluded', () => {
    const probe = document.createElement('div');
    probe.style.color = 'var(--surface-1)';
    probe.style.backgroundColor = 'var(--surface-2)';
    document.body.appendChild(probe);
    try {
      applyThemeClass('dark');
      const partly = theme('partly', {
        dark: { colors: { 'surface-1': 'rgb(1, 2, 3)', 'surface-2': 'notacolor' } },
      });
      const before = getComputedStyle(probe).backgroundColor;
      applyTheme(
        input({ themes: [partly], appearance: { uiTheme: 'partly', codeTheme: 'github' } }),
      );
      expect(getComputedStyle(probe).color).toBe('rgb(1, 2, 3)');
      // The rejected token kept app.css's value rather than becoming invalid.
      expect(getComputedStyle(probe).backgroundColor).toBe(before);
    } finally {
      probe.remove();
    }
  });

  it('derives an OMITTED prose role from the base the theme moved, in both modes', () => {
    // The per-theme opt-in contract in a live cascade: a theme stating only
    // `md-heading` leaves `md-bold` on its tokens.css derivation, which must
    // re-derive from the THEME's text-primary — in both modes, because the
    // single :root `var(--text-primary)` picks up whichever declaration the
    // mode resolves. This is the one-line assertion that would have caught
    // the original latte leak's sibling (a derivation that stopped deriving).
    const probe = document.createElement('div');
    probe.style.color = 'var(--md-bold)';
    document.body.appendChild(probe);
    try {
      const sparse = theme('sparse-md', {
        dark: {
          colors: { 'md-heading': 'rgb(200, 50, 50)', 'text-primary': 'rgb(10, 20, 30)' },
        },
        light: {
          colors: { 'md-heading': 'rgb(120, 10, 10)', 'text-primary': 'rgb(40, 50, 60)' },
        },
      });
      applyThemeClass('dark');
      applyTheme(
        input({ themes: [sparse], appearance: { uiTheme: 'sparse-md', codeTheme: 'github' } }),
      );
      expect(getComputedStyle(probe).color).toBe('rgb(10, 20, 30)');

      applyThemeClass('light');
      applyTheme(
        input({
          mode: 'light',
          themes: [sparse],
          appearance: { uiTheme: 'sparse-md', codeTheme: 'github' },
        }),
      );
      expect(getComputedStyle(probe).color).toBe('rgb(40, 50, 60)');
    } finally {
      probe.remove();
      applyThemeClass('dark');
    }
  });

  it('reports its rejections on the applied theme, not just to the console', () => {
    const applied = applyTheme(
      input({
        themes: [theme('bad', { dark: { colors: { 'surface-1': 'notacolor' } } })],
        appearance: { uiTheme: 'bad', codeTheme: 'github' },
      }),
    );
    expect(applied.warnings.some((warning) => warning.path === 'dark.colors.surface-1')).toBe(true);
    expect(applied.ui.id).toBe('bad');
  });

  it('clears the boot script inline ground once the real cascade is live', () => {
    // The inline style is what stops a white first frame; left in place it
    // would out-specify every later theme change for the whole session.
    document.documentElement.style.backgroundColor = '#123456';
    applyTheme(input());
    expect(document.documentElement.style.backgroundColor).toBe('');
  });
});

describe('first-paint guard', () => {
  // The pre-effect runs at mount, before GetThemeFiles has answered, so a
  // selected USER theme resolves to the built-in fallback there. Writing that
  // resolution overwrites the boot script's cached CSS with the fallback's and
  // clears the inline ground — recreating the flash the stamp exists to
  // prevent, for exactly the users who wrote a theme file.
  function bootStampedStyle(css: string): HTMLStyleElement {
    const existing = document.getElementById(USER_THEME_STYLE_ID);
    existing?.remove();
    const element = document.createElement('style');
    element.id = USER_THEME_STYLE_ID;
    element.textContent = css;
    document.body.appendChild(element);
    return element;
  }

  it('leaves the boot-stamped style and ground alone until the load settles', () => {
    const stamped = ':root {\n  --surface-1: rgb(9, 9, 9);\n}';
    const element = bootStampedStyle(stamped);
    document.documentElement.style.backgroundColor = '#090909';

    // What the mount-time resolution looks like: the file is not loaded yet.
    const applied = applyTheme(
      input({ themes: [], appearance: { uiTheme: 'mine', codeTheme: 'github' } }),
      false,
    );

    expect(element.textContent).toBe(stamped);
    expect(document.documentElement.style.backgroundColor).toBe('rgb(9, 9, 9)');
    // Nothing was published either, so no consumer keys off the fallback.
    expect(applied.paletteIdentity).toBe('');
    expect(applied.warnings).toHaveLength(0);

    document.documentElement.style.removeProperty('background-color');
  });

  it('replaces it once the load has settled, even when the theme is missing', () => {
    // The degraded and remote paths settle too — refused and failed are both
    // answers — so the guard must not be able to hold the boot CSS forever.
    const element = bootStampedStyle(':root {\n  --surface-1: rgb(9, 9, 9);\n}');
    const applied = applyTheme(
      input({ themes: [], appearance: { uiTheme: 'mine', codeTheme: 'github' } }),
      true,
    );

    expect(element.textContent).not.toContain('rgb(9, 9, 9)');
    expect(applied.ui.fallback).toBe(true);
  });
});

describe('window ground probe', () => {
  it('normalizes the live --surface-0 to #rrggbb in both modes', () => {
    applyThemeClass('dark');
    applyTheme(input());
    const dark = readWindowGroundHex();
    expect(dark).toMatch(HEX);

    applyThemeClass('light');
    applyTheme(input({ mode: 'light' }));
    const light = readWindowGroundHex();
    expect(light).toMatch(HEX);
    // Proof it reads the cascade rather than a constant — and the reason the
    // native window has to be repainted on a mode change at all.
    expect(light).not.toBe(dark);
  });

  it('follows a theme that moves the ground', () => {
    applyThemeClass('dark');
    applyTheme(input());
    const builtin = readWindowGroundHex();
    const ground = theme('ground', { dark: { colors: { 'surface-0': 'rgb(9, 8, 7)' } } });
    applyTheme(input({ themes: [ground], appearance: { uiTheme: 'ground', codeTheme: 'github' } }));
    expect(readWindowGroundHex()).toBe('#090807');
    expect(readWindowGroundHex()).not.toBe(builtin);
  });
});

describe('boot stamp', () => {
  it('records the mode, the ground and small CSS under the shared key', () => {
    stampBootTheme('light', '#fafafb', ':root { --surface-1: #fff }');
    const raw = localStorage.getItem(THEME_BOOT_STORAGE_KEY);
    expect(raw).toBeTruthy();
    expect(JSON.parse(raw!)).toEqual({
      c: 'light',
      b: '#fafafb',
      s: ':root { --surface-1: #fff }',
    });
  });

  it('drops oversized CSS but keeps the two strings that stop the flash', () => {
    stampBootTheme('dark', '#101017', 'x'.repeat(THEME_BOOT_MAX_CSS + 1));
    const parsed = JSON.parse(localStorage.getItem(THEME_BOOT_STORAGE_KEY)!) as Record<
      string,
      string
    >;
    expect(parsed.s).toBe('');
    expect(parsed.c).toBe('dark');
    expect(parsed.b).toBe('#101017');
  });
});
