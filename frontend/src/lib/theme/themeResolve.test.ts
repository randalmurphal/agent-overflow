import { describe, expect, it } from 'vitest';
import { BUILTIN_THEMES, defineBuiltinTheme } from './builtins';
import { parseTheme, type ParsedTheme } from './themeParse';
import {
  buildThemeCatalog,
  isSafeDeclarationValue,
  isSafeTokenKey,
  pickCodeVariant,
  pickUiVariant,
  resolveTheme,
  serializeThemeCss,
  themesForAxis,
  type ResolveInput,
  type ResolvedDeclarations,
  type ResolvedMode,
} from './themeResolve';

function theme(id: string, doc: Record<string, unknown>): ParsedTheme {
  const parsed = parseTheme(id, JSON.stringify(doc));
  expect(parsed.warnings, `fixture "${id}" should parse clean`).toEqual([]);
  return parsed;
}

function resolve(overrides: Partial<ResolveInput> & { mode: ResolvedMode }) {
  return resolveTheme({
    appearance: { uiTheme: 'default', codeTheme: 'github' },
    themes: [],
    revision: 1,
    ...overrides,
  });
}

/**
 * The CSS a resolution would emit.
 *
 * `resolveTheme` deliberately does NOT carry a `cssText` field: the only
 * production caller enriches the declaration set first and serializes that,
 * so a text field on the result would be built and thrown away on every
 * resolve. Tests go the same way the applier does.
 */
function cssOf(resolved: { declarations: ResolvedDeclarations }): string {
  return serializeThemeCss(resolved.declarations);
}

/** Value assigned to `--x` in one block of the emitted CSS, or undefined. */
function declOf(cssText: string, selector: string, key: string): string | undefined {
  const block = new RegExp(`${selector.replace('.', '\\.')} \\{\\n([\\s\\S]*?)\\n\\}`).exec(cssText);
  if (!block) return undefined;
  return new RegExp(`^ {2}--${key}: (.+);$`, 'm').exec(block[1]!)?.[1];
}

const DARK_UI = theme('nightfall', {
  name: 'Nightfall',
  dark: { colors: { 'surface-0': 'oklch(0.1 0 0)', accent: 'rebeccapurple' } },
});

const BOTH_UI = theme('duo', {
  name: 'Duo',
  dark: { colors: { 'surface-0': 'darkslategray', accent: 'aqua' } },
  light: { colors: { 'surface-0': 'ivory', accent: 'navy' } },
});

const DARK_CODE = theme('monokai-ish', {
  name: 'Monokai-ish',
  dark: {
    syntax: { 'syntax-keyword': 'hotpink' },
    ansi: { 'ansi-fg-31': 'tomato' },
    code: { 'code-block': 'oklch(0.2 0 0)' },
  },
});

describe('serialization gates', () => {
  it('accepts the shapes a color value really takes', () => {
    for (const value of [
      'red',
      'oklch(0.58 0.19 276)',
      'oklch(0 0 0 / 60%)',
      'rgb(12 34 56)',
      'color-mix(in oklab, oklch(0.5 0.1 250) 55%, transparent)',
      'color(display-p3 1 0 0)',
      'light-dark(white, black)',
      '#d97757',
    ]) {
      expect(isSafeDeclarationValue(value), value).toBe(true);
    }
  });

  it('refuses everything that could end the declaration or the element', () => {
    for (const value of [
      'red; --accent: blue',
      'red } html { --accent: blue',
      'red /* comment */',
      'red\n--accent: blue',
      'red !important',
      "red'",
      'red\\3b',
      '</style><script>x()</script>',
      '@import "x"',
      // An unclosed function swallows to EOF in CSS's tokenizer, which would
      // eat the closing brace of its own block and everything after it.
      'oklch(0.5 0.1 250',
      'red)',
      '((((((((((red))))))))))',
    ]) {
      expect(isSafeDeclarationValue(value), value).toBe(false);
    }
  });

  it('refuses every function that would fetch (R3: theme values are not a network surface)', () => {
    // The apply-time `CSS.supports('color', …)` gate is SKIPPED on an engine
    // that cannot discriminate colors, and there is no CSP behind it, so a
    // value carrying a fetch would be a beacon. This gate is pure and
    // unconditional, which is the whole point.
    for (const value of [
      'url(//evil.example/beacon)',
      'url(https://evil.example/x.png)',
      'URL(//evil.example/x)',
      'url (//evil.example/x)',
      'url\t(//evil.example/x)',
      'image-set(//evil.example/x 1x)',
      '-webkit-image-set(//evil.example/x 1x)',
      'IMAGE-SET(//evil.example/x 1x)',
      'src(//evil.example/x)',
      'color-mix(in oklab, url(//evil.example/x) 50%, red)',
    ]) {
      expect(isSafeDeclarationValue(value), value).toBe(false);
    }
  });

  it('refuses references, which blank every consumer when they fail (R5)', () => {
    // `var(--nope)` is well-shaped and is a valid color declaration as far as
    // any static check goes; it makes the custom property invalid at
    // COMPUTED-VALUE time, unsetting every property that reads it. One bad
    // token must cost one token, so this cannot be admitted at all.
    for (const value of [
      'var(--nope)',
      'var(--surface-1)',
      'VAR(--surface-1)',
      'var (--surface-1)',
      'oklch(from var(--accent) l c h / 0.18)',
      'color-mix(in oklab, var(--accent) 55%, transparent)',
      'attr(data-x color)',
      'env(safe-area-inset-top)',
      'ENV(x)',
    ]) {
      expect(isSafeDeclarationValue(value), value).toBe(false);
    }
  });

  it('refuses property names that are not plain kebab identifiers', () => {
    expect(isSafeTokenKey('surface-0')).toBe(true);
    expect(isSafeTokenKey('ansi-fg-31')).toBe(true);
    for (const key of ['Surface', '-surface', 'surface_0', 'surface;x', '', 'surface-', '0surface']) {
      expect(isSafeTokenKey(key), key).toBe(false);
    }
  });
});

describe('catalog', () => {
  it('lets a user file shadow a built-in id, as user-sourced', () => {
    const mine = theme('default', { name: 'Mine', dark: { colors: { accent: 'red' } } });
    const catalog = buildThemeCatalog([mine]);
    expect(catalog.get('default')?.source).toBe('user');
    expect(catalog.get('default')?.theme.name).toBe('Mine');
    expect(catalog.get('github')?.source).toBe('builtin');

    const resolved = resolve({ mode: 'dark', themes: [mine] });
    expect(resolved.ui).toEqual({ id: 'default', name: 'Mine', source: 'user', fallback: false });
    expect(declOf(cssOf(resolved), ':root', 'accent')).toBe('red');
  });

  it('lists each axis by what a file actually defines', () => {
    const catalog = buildThemeCatalog([DARK_UI, DARK_CODE, BOTH_UI]);
    // The built-ins the app ships are on the list too, so the expectation is
    // "the user files land on the right axis, in id order, among them" — read
    // from BUILTIN_THEMES rather than restated, so a new curated palette does
    // not fail an assertion that is not about it.
    const expected = (axis: 'ui' | 'code', users: string[]): string[] =>
      [
        ...BUILTIN_THEMES.filter((t) => (axis === 'ui' ? t.axes.ui : t.axes.code)).map((t) => t.id),
        ...users,
      ].sort((a, b) => a.localeCompare(b));

    expect(themesForAxis(catalog, 'ui').map((e) => e.theme.id)).toEqual(
      expected('ui', ['duo', 'nightfall']),
    );
    expect(themesForAxis(catalog, 'code').map((e) => e.theme.id)).toEqual(
      expected('code', ['monokai-ish']),
    );
  });
});

describe('variant picking', () => {
  it('UI axis: matching variant only', () => {
    expect(Object.keys(pickUiVariant(DARK_UI, 'dark'))).toEqual(['dark']);
    expect(Object.keys(pickUiVariant(DARK_UI, 'light'))).toEqual([]);
    expect(Object.keys(pickUiVariant(BOTH_UI, 'dark'))).toEqual(['dark', 'light']);
    expect(Object.keys(pickUiVariant(BOTH_UI, 'light'))).toEqual(['dark', 'light']);

    const lightOnly = theme('daylight', { light: { colors: { accent: 'navy' } } });
    expect(Object.keys(pickUiVariant(lightOnly, 'light'))).toEqual(['light']);
    expect(Object.keys(pickUiVariant(lightOnly, 'dark'))).toEqual([]);
  });

  it('code axis: sole variant serves both modes', () => {
    const picked = pickCodeVariant(DARK_CODE);
    expect(picked.dark).toBe(picked.light);

    const lightOnlyCode = theme('paper', { light: { syntax: { 'syntax-tag': 'green' } } });
    const paper = pickCodeVariant(lightOnlyCode);
    expect(paper.dark).toBe(paper.light);
    expect(paper.dark?.syntax).toEqual({ 'syntax-tag': 'green' });

    const both = pickCodeVariant(
      theme('duo-code', {
        dark: { syntax: { 'syntax-tag': 'lime' } },
        light: { syntax: { 'syntax-tag': 'green' } },
      }),
    );
    expect(both.dark).not.toBe(both.light);
  });
});

describe('resolveTheme', () => {
  it('emits nothing at all for the built-in identity pair', () => {
    const resolved = resolve({ mode: 'dark' });
    expect(cssOf(resolved)).toBe('');
    expect(resolved.warnings).toEqual([]);
    expect(resolved.windowBackground).toBeUndefined();
    expect(resolved.paletteIdentity).toBe('default|github|1');
    expect(resolved.ui.fallback).toBe(false);
    expect(resolved.code.fallback).toBe(false);
  });

  it('splits a two-variant UI theme across the two blocks, identically in either mode', () => {
    const dark = resolve({ mode: 'dark', themes: [BOTH_UI], appearance: { uiTheme: 'duo', codeTheme: 'github' } });
    const light = resolve({ mode: 'light', themes: [BOTH_UI], appearance: { uiTheme: 'duo', codeTheme: 'github' } });

    expect(cssOf(dark)).toBe(cssOf(light));
    expect(declOf(cssOf(dark), ':root', 'surface-0')).toBe('darkslategray');
    expect(declOf(cssOf(dark), 'html.light', 'surface-0')).toBe('ivory');
    // The window ground is the one mode-dependent output.
    expect(dark.windowBackground).toBe('darkslategray');
    expect(light.windowBackground).toBe('ivory');
  });

  it('withholds a single-variant UI theme from the mode it does not speak', () => {
    const inMode = resolve({
      mode: 'dark',
      themes: [DARK_UI],
      appearance: { uiTheme: 'nightfall', codeTheme: 'github' },
    });
    expect(declOf(cssOf(inMode), ':root', 'accent')).toBe('rebeccapurple');
    expect(inMode.windowBackground).toBe('oklch(0.1 0 0)');

    const offMode = resolve({
      mode: 'light',
      themes: [DARK_UI],
      appearance: { uiTheme: 'nightfall', codeTheme: 'github' },
    });
    // Nothing, in either block: leaving the dark values standing in `:root`
    // would half-apply the theme through the mode-invariant tokens.
    expect(cssOf(offMode)).toBe('');
    expect(offMode.windowBackground).toBeUndefined();
    expect(offMode.warnings).toEqual([]);
  });

  it('makes a single-variant code theme a dark island in both modes', () => {
    for (const mode of ['dark', 'light'] as const) {
      const resolved = resolve({
        mode,
        themes: [DARK_CODE],
        appearance: { uiTheme: 'default', codeTheme: 'monokai-ish' },
      });
      expect(declOf(cssOf(resolved), ':root', 'syntax-keyword')).toBe('hotpink');
      expect(declOf(cssOf(resolved), 'html.light', 'syntax-keyword')).toBe('hotpink');
      expect(declOf(cssOf(resolved), 'html.light', 'ansi-fg-31')).toBe('tomato');
      expect(declOf(cssOf(resolved), 'html.light', 'code-block')).toBe('oklch(0.2 0 0)');
      // A code theme never speaks for the window ground.
      expect(resolved.windowBackground).toBeUndefined();
    }
  });

  it('merges the two axes into one pair of blocks', () => {
    const resolved = resolve({
      mode: 'dark',
      themes: [BOTH_UI, DARK_CODE],
      appearance: { uiTheme: 'duo', codeTheme: 'monokai-ish' },
    });
    expect(declOf(cssOf(resolved), ':root', 'surface-0')).toBe('darkslategray');
    expect(declOf(cssOf(resolved), ':root', 'syntax-keyword')).toBe('hotpink');
    expect(declOf(cssOf(resolved), 'html.light', 'surface-0')).toBe('ivory');
    expect(declOf(cssOf(resolved), 'html.light', 'syntax-keyword')).toBe('hotpink');
    expect(resolved.paletteIdentity).toBe('duo|monokai-ish|1');
  });

  it('takes only its own axis from a file that serves both', () => {
    const dual = theme('dual', {
      dark: { colors: { accent: 'aqua' }, syntax: { 'syntax-keyword': 'hotpink' } },
    });
    const uiOnly = resolve({
      mode: 'dark',
      themes: [dual],
      appearance: { uiTheme: 'dual', codeTheme: 'github' },
    });
    expect(declOf(cssOf(uiOnly), ':root', 'accent')).toBe('aqua');
    expect(declOf(cssOf(uiOnly), ':root', 'syntax-keyword')).toBeUndefined();

    const both = resolve({
      mode: 'dark',
      themes: [dual],
      appearance: { uiTheme: 'dual', codeTheme: 'dual' },
    });
    expect(declOf(cssOf(both), ':root', 'syntax-keyword')).toBe('hotpink');
    // Selected on both axes, its warnings are still reported once.
    expect(both.warnings).toEqual([]);
  });

  it('falls back with a warning when a selection names nothing', () => {
    const resolved = resolve({
      mode: 'dark',
      appearance: { uiTheme: 'gone', codeTheme: 'also-gone' },
    });
    expect(resolved.ui).toEqual({ id: 'default', name: 'Default', source: 'builtin', fallback: true });
    expect(resolved.code).toEqual({ id: 'github', name: 'GitHub', source: 'builtin', fallback: true });
    expect(resolved.warnings.map((w) => w.code)).toEqual(['unknown-theme', 'unknown-theme']);
    expect(resolved.warnings.map((w) => w.path)).toEqual(['uiTheme', 'codeTheme']);
    expect(resolved.paletteIdentity).toBe('default|github|1');
  });

  it('falls back when a theme is selected on an axis it does not serve', () => {
    const resolved = resolve({
      mode: 'dark',
      themes: [DARK_CODE],
      appearance: { uiTheme: 'monokai-ish', codeTheme: 'monokai-ish' },
    });
    expect(resolved.ui.id).toBe('default');
    expect(resolved.ui.fallback).toBe(true);
    expect(resolved.code.id).toBe('monokai-ish');
    // NOT `unknown-theme`: the file exists. Two different facts with two
    // different fixes, so they do not share a code.
    expect(resolved.warnings.map((w) => w.code)).toEqual(['axis-unusable']);
    expect(resolved.warnings[0]!.themeId).toBe('monokai-ish');
    expect(resolved.warnings[0]!.message).toContain('colors');
  });

  it('propagates a selected-but-unusable theme own warnings, which are the reason', () => {
    // The repro: a file that is not JSON at all. It exists, so it is not
    // `unknown-theme`; it parses to nothing, so it serves no axis; and its
    // parse warning is the ONLY thing that tells the user what to fix. The
    // warning loop iterates the RESOLVED entries — the built-in fallbacks —
    // so without explicit propagation the file's own findings are lost.
    const notJson = parseTheme('mine', '{ not json');
    const resolved = resolve({
      mode: 'dark',
      themes: [notJson],
      appearance: { uiTheme: 'mine', codeTheme: 'github' },
    });
    expect(resolved.warnings.map((w) => w.code)).toEqual(['axis-unusable', 'invalid-json']);
    expect(resolved.warnings.every((w) => w.themeId === 'mine')).toBe(true);
  });

  it('reports an unusable file once even when both axes selected it', () => {
    const notJson = parseTheme('mine', '{ not json');
    const resolved = resolve({
      mode: 'dark',
      themes: [notJson],
      appearance: { uiTheme: 'mine', codeTheme: 'mine' },
    });
    expect(resolved.warnings.map((w) => w.code)).toEqual([
      'axis-unusable',
      'axis-unusable',
      'invalid-json',
    ]);
  });

  it('surfaces the applied themes own warnings and only theirs', () => {
    const broken = parseTheme('broken', JSON.stringify({ dark: { colors: { accent: 'red', nope: 'red' } } }));
    const unusedBroken = parseTheme('unused', JSON.stringify({ dark: { colors: { alsoNope: 'red' } } }));

    const resolved = resolve({
      mode: 'dark',
      themes: [broken, unusedBroken],
      appearance: { uiTheme: 'broken', codeTheme: 'github' },
    });
    expect(resolved.warnings.map((w) => w.themeId)).toEqual(['broken']);
    expect(resolved.warnings.map((w) => w.code)).toEqual(['unknown-key']);
  });

  it('warns when a two-variant theme states a mode-invariant token once', () => {
    // `accent-fg` has no `html.light` declaration to out-cascade `:root`, so a
    // value in the dark block alone paints in BOTH modes — which contradicts
    // what a two-variant file claims to be doing.
    const split = theme('split', {
      dark: { colors: { 'accent-fg': 'black', accent: 'aqua' } },
      light: { colors: { accent: 'navy' } },
    });
    const resolved = resolve({
      mode: 'dark',
      themes: [split],
      appearance: { uiTheme: 'split', codeTheme: 'github' },
    });
    const warning = resolved.warnings.find((w) => w.code === 'mode-invariant');
    expect(warning?.path).toBe('dark.colors.accent-fg');
    expect(warning?.themeId).toBe('split');
    expect(warning?.message).toContain('both modes');

    // Both blocks still emit it — the warning is about ambiguity, not a
    // refusal. The declaration only lands in `:root`, which is exactly the
    // reach being reported.
    expect(declOf(cssOf(resolved), ':root', 'accent-fg')).toBe('black');
    expect(declOf(cssOf(resolved), 'html.light', 'accent-fg')).toBeUndefined();
  });

  it('warns when a two-variant theme states a DERIVED shared-default role once', () => {
    // The other stranded class: `md-bold` defaults in tokens.css's single
    // `:root` block, whose `var()` carries the mode only while the theme
    // leaves it alone. A dark-only literal lands in the emitted `:root` with
    // nothing in `html.light` to out-cascade it — the leak found live when
    // latte's bold rendered mocha's mustard on the light ground.
    const split = theme('split-derived', {
      dark: { colors: { 'md-bold': 'gold', accent: 'aqua' } },
      light: { colors: { accent: 'navy' } },
    });
    const resolved = resolve({
      mode: 'dark',
      themes: [split],
      appearance: { uiTheme: 'split-derived', codeTheme: 'github' },
    });
    const warning = resolved.warnings.find((w) => w.code === 'mode-invariant');
    expect(warning?.path).toBe('dark.colors.md-bold');
    expect(warning?.message).toContain('derived role declared once for both modes');
  });

  it('stays silent when the token is stated in both variants, or in neither', () => {
    const both = theme('both', {
      dark: { colors: { 'accent-fg': 'black', scrim: 'black', 'md-bold': 'gold' } },
      light: { colors: { 'accent-fg': 'white', scrim: 'white', 'md-bold': 'maroon' } },
    });
    const neither = theme('neither', {
      dark: { colors: { accent: 'aqua' } },
      light: { colors: { accent: 'navy' } },
    });
    for (const file of [both, neither]) {
      const resolved = resolve({
        mode: 'dark',
        themes: [file],
        appearance: { uiTheme: file.id, codeTheme: 'github' },
      });
      expect(resolved.warnings.map((w) => w.code), file.id).toEqual([]);
    }
  });

  it('stays silent for a one-variant theme, which never claimed two palettes', () => {
    const darkOnly = theme('dusk', { dark: { colors: { 'accent-fg': 'black' } } });
    const resolved = resolve({
      mode: 'dark',
      themes: [darkOnly],
      appearance: { uiTheme: 'dusk', codeTheme: 'github' },
    });
    expect(resolved.warnings).toEqual([]);
  });

  it('drops a hostile value and keeps the rest of the theme', () => {
    const hostile = parseTheme(
      'hostile',
      JSON.stringify({
        dark: {
          colors: {
            accent: 'red; --surface-0: magenta',
            'surface-1': 'red } html { --surface-0: magenta',
            'surface-2': 'oklch(0.2 0 0',
            'surface-3': 'darkslategray',
          },
        },
      }),
    );
    const resolved = resolve({
      mode: 'dark',
      themes: [hostile],
      appearance: { uiTheme: 'hostile', codeTheme: 'github' },
    });

    expect(cssOf(resolved)).toBe(':root {\n  --surface-3: darkslategray;\n}');
    expect(resolved.warnings.map((w) => w.code)).toEqual([
      'unsafe-value',
      'unsafe-value',
      'unsafe-value',
    ]);
    expect(resolved.warnings.map((w) => w.path)).toEqual([
      'dark.colors.surface-1',
      'dark.colors.surface-2',
      'dark.colors.accent',
    ]);
    // Nothing escaped: exactly one block, exactly one declaration.
    expect(cssOf(resolved).match(/\{/g)).toHaveLength(1);
    expect(cssOf(resolved)).not.toContain('magenta');
  });

  it('refuses a hostile value for the window ground rather than passing it on', () => {
    const hostile = parseTheme(
      'hostile',
      JSON.stringify({ dark: { colors: { 'surface-0': 'red; x: y' } } }),
    );
    const resolved = resolve({
      mode: 'dark',
      themes: [hostile],
      appearance: { uiTheme: 'hostile', codeTheme: 'github' },
    });
    expect(resolved.windowBackground).toBeUndefined();
  });

  it('emits declarations in registry order, not JSON key order', () => {
    const scrambled = theme('scrambled', {
      dark: { colors: { accent: 'aqua', 'surface-1': 'gray', 'surface-0': 'black' } },
    });
    const resolved = resolve({
      mode: 'dark',
      themes: [scrambled],
      appearance: { uiTheme: 'scrambled', codeTheme: 'github' },
    });
    expect(cssOf(resolved)).toBe(
      ':root {\n  --surface-0: black;\n  --surface-1: gray;\n  --accent: aqua;\n}',
    );
  });

  it('carries per-token structure so apply time can reject one value', () => {
    const resolved = resolve({
      mode: 'dark',
      themes: [BOTH_UI, DARK_CODE],
      appearance: { uiTheme: 'duo', codeTheme: 'monokai-ish' },
    });

    expect(resolved.declarations.root.map((d) => d.key)).toEqual([
      'surface-0',
      'accent',
      'syntax-keyword',
      'ansi-fg-31',
      'code-block',
    ]);
    expect(resolved.declarations.root[0]).toEqual({
      key: 'surface-0',
      cssVar: '--surface-0',
      value: 'darkslategray',
      section: 'colors',
      variant: 'dark',
      themeId: 'duo',
    });
    // Provenance is per declaration, so a merged block still says which file
    // to blame for which token.
    expect(resolved.declarations.root.find((d) => d.key === 'syntax-keyword')?.themeId).toBe(
      'monokai-ish',
    );
    expect(resolved.declarations.light.every((d) => d.variant === 'light')).toBe(true);

    // What the applier does: drop one token, keep the theme.
    const kept = {
      root: resolved.declarations.root.filter((d) => d.key !== 'accent'),
      light: resolved.declarations.light,
    };
    const reserialized = serializeThemeCss(kept);
    expect(reserialized).toBe(serializeThemeCss(resolved.declarations).replace('  --accent: aqua;\n', ''));
    expect(declOf(reserialized, ':root', 'surface-0')).toBe('darkslategray');
  });

  it('serializes an empty declaration set to nothing at all', () => {
    expect(serializeThemeCss({ root: [], light: [] })).toBe('');
  });

  it('writes the declaration cssVar, and gates on the name it writes', () => {
    // `cssVar` is the field that reaches the stylesheet, so it is the field
    // the last-door check has to test — checking `key` while emitting
    // `cssVar` is exactly how a property name escapes.
    expect(
      serializeThemeCss({
        root: [
          { key: 'accent', cssVar: '--accent: red; --surface-0', value: 'magenta', section: 'colors', variant: 'dark', themeId: 't' },
          { key: 'accent', cssVar: '--Accent', value: 'magenta', section: 'colors', variant: 'dark', themeId: 't' },
          { key: 'surface-0', cssVar: '--surface-0', value: 'black', section: 'colors', variant: 'dark', themeId: 't' },
        ],
        light: [],
      }),
    ).toBe(':root {\n  --surface-0: black;\n}');
  });

  it('re-checks the gates when a caller hands declarations back', () => {
    // The applier could be handed anything; the serializer is the last door.
    expect(
      serializeThemeCss({
        root: [
          { key: 'accent', cssVar: '--accent', value: 'red; --surface-0: magenta', section: 'colors', variant: 'dark', themeId: 't' },
          { key: 'surface-0', cssVar: '--surface-0', value: 'black', section: 'colors', variant: 'dark', themeId: 't' },
        ],
        light: [],
      }),
    ).toBe(':root {\n  --surface-0: black;\n}');
  });

  it('bumps the palette identity on a revision change and nothing else', () => {
    const a = resolve({ mode: 'dark', revision: 7 });
    const b = resolve({ mode: 'dark', revision: 8 });
    expect(a.paletteIdentity).toBe('default|github|7');
    expect(b.paletteIdentity).toBe('default|github|8');
    expect(cssOf(a)).toBe(cssOf(b));
    // Mode is deliberately not part of the identity; consumers pair it with
    // the resolved mode they already read.
    expect(resolve({ mode: 'light', revision: 7 }).paletteIdentity).toBe(a.paletteIdentity);
  });

  it('never throws when the caller supplies its own builtins', () => {
    const resolved = resolveTheme({
      mode: 'dark',
      appearance: { uiTheme: 'nope', codeTheme: 'nope' },
      themes: [],
      builtins: [],
      revision: 0,
    });
    expect(cssOf(resolved)).toBe('');
    expect(resolved.ui.id).toBe('default');
    expect(resolved.code.id).toBe('github');
  });
});

describe('builtins', () => {
  it('lead with the two identity themes: named, axis-declared, emitting nothing', () => {
    // The reserved pair only. The curated palettes that follow them are data,
    // and `builtins.test.ts` is where their contents are held to account.
    const identity = BUILTIN_THEMES.slice(0, 2);
    expect(identity.map((t) => t.id)).toEqual(['default', 'github']);
    for (const builtin of identity) {
      expect(builtin.warnings, `${builtin.id} should be clean`).toEqual([]);
      expect(builtin.variants).toEqual({});
      expect(builtin.builtin).toBe(true);
    }
    expect(identity[0]!.axes).toEqual({ ui: true, code: false });
    expect(identity[1]!.axes).toEqual({ ui: false, code: true });
  });

  it('shapes a curated palette as a data addition, validated like a user file', () => {
    // Phase 3's code themes are exactly this call with real values in it.
    const curated = defineBuiltinTheme({
      id: 'curated',
      name: 'Curated',
      axes: { ui: false, code: true },
      dark: { syntax: { 'syntax-keyword': 'hotpink' }, code: { 'terminal-bg': 'black' } },
    });
    expect(curated.warnings).toEqual([]);
    expect(curated.variants.dark?.syntax).toEqual({ 'syntax-keyword': 'hotpink' });

    const resolved = resolveTheme({
      mode: 'light',
      appearance: { uiTheme: 'default', codeTheme: 'curated' },
      themes: [],
      builtins: [...BUILTIN_THEMES, curated],
      revision: 2,
    });
    expect(declOf(cssOf(resolved), 'html.light', 'syntax-keyword')).toBe('hotpink');
    expect(resolved.code.source).toBe('builtin');
  });

  it('reports a typo in a curated palette the same way a user file would', () => {
    const typo = defineBuiltinTheme({
      id: 'typo',
      name: 'Typo',
      axes: { ui: false, code: true },
      dark: { syntax: { 'syntax-keywrod': 'hotpink' } },
    });
    expect(typo.warnings.map((w) => w.code)).toEqual(['unknown-key']);
  });
});
