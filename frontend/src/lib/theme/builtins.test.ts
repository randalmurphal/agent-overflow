// Conformance gate for the curated built-in palettes.
//
// A curated theme is DATA, and data that ships inside the binary gets no
// second chance to be corrected by the person who wrote it: nobody edits it,
// nobody sees a warning surface for it, and a mistyped key is simply a color
// that never arrives. So everything a user's theme file would be told about at
// load time is asserted here instead — the built-ins go through the same
// parser, so these tests are the built-ins' warning surface.
//
// The value-level question (is this readable?) lives next door in
// `builtins.contrast.test.ts`.

import { describe, expect, it } from 'vitest';
import {
  BUILTIN_CODE_THEME_ID,
  BUILTIN_THEMES,
  BUILTIN_UI_THEME_ID,
  CURATED_BUILTIN_SPECS,
  defineBuiltinTheme,
} from './builtins';
import { THEME_VARIANTS, type ThemeVariantName } from './themeParse';
import { resolveTheme, serializeThemeCss } from './themeResolve';
import {
  THEME_SECTIONS,
  TOKEN_REGISTRY,
  isSharedDefaultToken,
  tokenKeysInSection,
} from './tokenRegistry';

const RESERVED = new Set<string>([BUILTIN_UI_THEME_ID, BUILTIN_CODE_THEME_ID]);
const CURATED = CURATED_BUILTIN_SPECS.map((spec) => defineBuiltinTheme(spec));

describe('curated built-in themes', () => {
  it('ships the requested set, each on the axes it declares', () => {
    expect(CURATED_BUILTIN_SPECS.map((spec) => spec.id)).toEqual([
      'blacklight',
      'catppuccin',
      'dracula',
      'gruvbox',
      'monokai',
      'nord',
      'one-dark',
      'solarized',
      'tokyo-night',
      'high-contrast',
    ]);

    // Every curated palette is a code theme. All but Monokai also publish
    // workbench colors and so are offered on the UI axis too — Monokai has no
    // canonical UI specification, so it stays code-only rather than getting
    // chrome invented for it.
    for (const spec of CURATED_BUILTIN_SPECS) {
      expect(spec.axes, `${spec.id} axes`).toEqual({ ui: spec.id !== 'monokai', code: true });
    }
  });

  it('declares the UI axis exactly when it defines colors', () => {
    // The declared axes replace derived ones for the identity built-ins, which
    // is what makes this worth checking: a curated palette that declared `ui`
    // without a colors section would be offered and then paint nothing.
    for (const theme of CURATED) {
      const definesColors = THEME_VARIANTS.some((v) => theme.variants[v]?.colors !== undefined);
      expect(definesColors, `${theme.id} axes.ui vs colors`).toBe(theme.axes.ui);
    }
  });

  it('reaches the registry through the same list the app resolves from', () => {
    const ids = BUILTIN_THEMES.map((theme) => theme.id);
    expect(ids.slice(0, 2)).toEqual([BUILTIN_UI_THEME_ID, BUILTIN_CODE_THEME_ID]);
    expect(ids.slice(2)).toEqual(CURATED_BUILTIN_SPECS.map((spec) => spec.id));
    expect(new Set(ids).size, 'ids must be unique').toBe(ids.length);
    for (const theme of BUILTIN_THEMES) expect(theme.builtin).toBe(true);
  });

  it('uses kebab-case ids and gives every theme a display name', () => {
    for (const spec of CURATED_BUILTIN_SPECS) {
      expect(spec.id, `${spec.id} is not kebab-case`).toMatch(/^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/);
      expect(RESERVED.has(spec.id), `${spec.id} shadows a reserved id`).toBe(false);
      expect(spec.name.trim().length).toBeGreaterThan(0);
      expect(spec.name).not.toBe(spec.id);
    }
  });

  it('parses with ZERO warnings — no unknown key, no unusable value', () => {
    for (const theme of CURATED) {
      expect(theme.warnings, `${theme.id} must parse clean`).toEqual([]);
    }
  });

  it('declares only tokens the registry claims, in the section that owns them', () => {
    const offenders: string[] = [];
    for (const theme of CURATED) {
      for (const variantName of THEME_VARIANTS) {
        const variant = theme.variants[variantName];
        if (!variant) continue;
        for (const section of THEME_SECTIONS) {
          const values = variant[section];
          if (!values) continue;
          const known = tokenKeysInSection(section);
          for (const key of Object.keys(values)) {
            if (!known.has(key)) offenders.push(`${theme.id}.${variantName}.${section}.${key}`);
          }
        }
      }
    }
    expect(offenders).toEqual([]);
  });

  it('covers each section it opens IN FULL, so no token falls back mid-palette', () => {
    // A half-covered section is the failure mode a sparse user file is allowed
    // and a shipped palette is not: twenty Monokai families plus one github
    // leftover is not Monokai, it is a bug nobody can see. Every section a
    // curated variant opens must therefore carry every key in that section.
    const gaps: string[] = [];
    for (const theme of CURATED) {
      for (const variantName of THEME_VARIANTS) {
        const variant = theme.variants[variantName];
        if (!variant) continue;
        for (const section of THEME_SECTIONS) {
          const values = variant[section];
          if (!values) continue;
          for (const key of tokenKeysInSection(section)) {
            // The two derived ANSI aliases and the derived UI roles are
            // overridable, not required — but a curated palette that opens the
            // section states them anyway, so they are checked like the rest,
            // minus the roles a theme has no business restating.
            if (OPTIONAL_KEYS.has(key)) continue;
            if (!(key in values)) gaps.push(`${theme.id}.${variantName}.${section}.${key}`);
          }
        }
      }
    }
    expect(gaps).toEqual([]);
  });

  it('never states a shared-default token in one variant of a pair', () => {
    // The mode-invariant leak, enforced at the data: tokens whose app default
    // is declared once for both modes (`modeInvariant`, and every derived
    // role in tokens.css) land in the emitted `:root` with nothing in
    // `html.light` to out-cascade them, so a two-variant theme stating one
    // in dark alone paints the DARK value on its own light ground (found
    // live: latte's bold rendered mocha's mustard at 1.1:1). User files get
    // the resolver's `mode-invariant` warning; a curated palette must simply
    // never do it — state both or neither.
    const shared = TOKEN_REGISTRY.filter(isSharedDefaultToken);
    const splits: string[] = [];
    for (const theme of CURATED) {
      const { dark, light } = theme.variants;
      if (!dark || !light) continue;
      for (const token of shared) {
        const inDark = dark[token.section]?.[token.key] !== undefined;
        const inLight = light[token.section]?.[token.key] !== undefined;
        // A section one variant does not open at all is a different, LOUD
        // shape (the axis listing changes), so only opened sections count.
        if (!dark[token.section] || !light[token.section]) continue;
        if (inDark !== inLight) {
          splits.push(`${theme.id}.${token.section}.${token.key} (stated in ${inDark ? 'dark' : 'light'} only)`);
        }
      }
    }
    expect(splits).toEqual([]);
  });

  it('keeps the markdown prose roles ADOPTED, not just permitted', () => {
    // The md-* roles are in OPTIONAL_KEYS and the contrast suite measures
    // stated values only, so without this pin every md-* statement could be
    // deleted and the whole suite would stay green — the theme character the
    // roles exist to deliver has no other floor. Every UI-axis curated
    // variant states the five prose roles plus the inline-code chip pair
    // (text beside its ground, both UI-axis).
    const uiVariants = CURATED_BUILTIN_SPECS.filter((spec) => spec.axes.ui).flatMap((spec) =>
      THEME_VARIANTS.map((v) => spec[v]).filter((variant) => variant !== undefined),
    );
    expect(uiVariants.length).toBe(13);
    for (const variant of uiVariants) {
      expect(variant.colors?.['md-heading']).toBeDefined();
      expect(variant.colors?.['md-inline-code']).toBeDefined();
      expect(variant.colors?.['code-inline-bg']).toBeDefined();
    }
  });

  it('resolves in BOTH modes without a warning, on whichever axis it serves', () => {
    for (const spec of CURATED_BUILTIN_SPECS) {
      for (const mode of ['dark', 'light'] as const) {
        const resolved = resolveTheme({
          mode,
          appearance: {
            uiTheme: spec.axes.ui ? spec.id : BUILTIN_UI_THEME_ID,
            codeTheme: spec.id,
          },
          themes: [],
          revision: 1,
        });
        expect(resolved.warnings, `${spec.id} in ${mode} mode`).toEqual([]);
        expect(resolved.code.id).toBe(spec.id);
        expect(resolved.code.fallback).toBe(false);
        expect(
          serializeThemeCss(resolved.declarations).length,
          `${spec.id} emitted nothing in ${mode}`,
        ).toBeGreaterThan(0);
      }
    }
  });

  it('renders a dark-only code theme as an island in light mode', () => {
    // The rule that makes the dark-only palettes usable at all: their sole
    // variant is emitted into both blocks, so a light UI gets Monokai code on
    // a Monokai ground rather than Monokai text on a white one.
    const resolved = resolveTheme({
      mode: 'light',
      appearance: { uiTheme: BUILTIN_UI_THEME_ID, codeTheme: 'monokai' },
      themes: [],
      revision: 1,
    });
    expect(resolved.declarations.light.find((d) => d.key === 'code-block')?.value).toBe('#272822');
    expect(resolved.declarations.light.find((d) => d.key === 'syntax-keyword')?.value).toBe(
      '#f92672',
    );
    // …and it must not reach the UI axis, which stays the app's own light
    // chrome. A code theme that repainted `surface-0` would be a UI theme.
    expect(resolved.declarations.light.some((d) => d.section === 'colors')).toBe(false);
  });

  it('lets High Contrast repaint chrome on the UI axis, per mode', () => {
    for (const [mode, ground] of [
      ['dark', '#000000'],
      ['light', '#ffffff'],
    ] as const) {
      const resolved = resolveTheme({
        mode,
        appearance: { uiTheme: 'high-contrast', codeTheme: 'high-contrast' },
        themes: [],
        revision: 1,
      });
      expect(resolved.ui.id).toBe('high-contrast');
      expect(resolved.ui.fallback).toBe(false);
      expect(resolved.windowBackground, `${mode} window ground`).toBe(ground);
    }
  });

  it('states hex values, so the terminal bridge and the probe stay cheap', () => {
    const offenders: string[] = [];
    for (const spec of CURATED_BUILTIN_SPECS) {
      for (const variantName of THEME_VARIANTS) {
        const variant = spec[variantName as ThemeVariantName];
        if (!variant) continue;
        for (const section of THEME_SECTIONS) {
          for (const [key, value] of Object.entries(variant[section] ?? {})) {
            if (!/^#(?:[0-9a-f]{6}|[0-9a-f]{8})$/.test(value)) {
              offenders.push(`${spec.id}.${variantName}.${section}.${key} = ${value}`);
            }
          }
        }
      }
    }
    expect(offenders).toEqual([]);
  });
});

/**
 * Tokens a curated palette may leave to the cascade even inside a section it
 * otherwise fills. Two groups, both deliberate:
 *
 * The brand marks, the media-overlay pair and the design paper are
 * mode-invariant by design — a theme does not get to restate someone else's
 * logo color, and a scrim sits on user media rather than on app chrome.
 *
 * The rest are DERIVED roles that follow a base the theme has already set: the
 * card and generic-icon aliases, the foreground fade hierarchy over
 * `text-primary`, and the subtle border over `border`. Leaving them alone is
 * the correct default — that is what makes a palette that states twenty colors
 * come out coherent across sixty — so they are optional rather than required.
 * A theme overrides them only when its own contrast demands it, which is
 * exactly what High Contrast does and why it states all four.
 */
const OPTIONAL_KEYS = new Set<string>([
  'provider-codex',
  'provider-claude',
  'provider-claude-tui',
  'scrim',
  'scrim-fg',
  'design-paper',
  'card',
  'ico-generic',
  'fg-muted',
  'fg-subtle',
  'fg-hint',
  'border-subtle',
  // The markdown prose roles are optional in the full-coverage sense only —
  // a palette without a hue for one may restate its own text tier or leave
  // the derivation. In practice every UI-axis curated variant states all of
  // them (the adoption pin below), and the split-statement test holds a
  // two-variant theme to stating a shared-default role in both variants or
  // neither, because the default is declared once for both modes and a
  // dark-only literal strands in light mode.
  'md-heading',
  'md-bold',
  'md-link',
  'md-inline-code',
  'md-blockquote',
  'md-marker',
]);
