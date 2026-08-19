// The built-in themes: the two reserved IDENTITY themes, plus the curated
// palettes that ship with the app.
//
// `default` (UI) and `github` (code) emit no CSS at all. That is not a stub —
// it is the whole design. app.css and syntax.css already ARE those palettes,
// so the built-in default is the cascade with nothing layered on top, and a
// theme file's absence and the default's selection are the same state. It
// also means the default can never drift from the app: there is no second
// copy of the palette to keep in sync, and a phase-1 token added to app.css
// is in the default theme the moment it is declared.
//
// The curated palettes (One Dark, Monokai, Dracula, Solarized, Tokyo Night,
// Catppuccin, High Contrast) are DATA ADDITIONS on top of that: one module
// each under `builtins/`, exporting the same {@link BuiltinThemeSpec} shape a
// user file would carry, and registered in {@link CURATED_BUILTIN_SPECS}
// below. They go through `parseTheme` exactly as a file on disk does, so a
// typo in a curated palette surfaces as the same warning a user's typo would
// — `builtins.test.ts` asserts they parse and resolve clean, and
// `builtins.contrast.test.ts` holds their values to measured contrast floors,
// which together are what stops a mistyped key or an unreadable value from
// shipping.
//
// Adding a curated theme is: one module under `builtins/`, one entry in the
// list below. Nothing else — the tests iterate the list.

import { CATPPUCCIN } from './builtins/catppuccin';
import { DRACULA } from './builtins/dracula';
import { HIGH_CONTRAST } from './builtins/highContrast';
import { MONOKAI } from './builtins/monokai';
import { ONE_DARK } from './builtins/oneDark';
import { SOLARIZED } from './builtins/solarized';
import { TOKYO_NIGHT } from './builtins/tokyoNight';
import { parseThemeDoc, type ParsedTheme, type ThemeVariant } from './themeParse';
import type { ThemeSection } from './tokenRegistry';

/** Reserved id of the built-in UI theme — the app.css palette. */
export const BUILTIN_UI_THEME_ID = 'default';

/** Reserved id of the built-in code theme — the syntax.css palette. */
export const BUILTIN_CODE_THEME_ID = 'github';

/** A variant block as a theme file would write it. */
export type BuiltinVariant = {
  readonly [S in ThemeSection]?: Readonly<Record<string, string>>;
};

export interface BuiltinThemeSpec {
  readonly id: string;
  readonly name: string;
  /**
   * Which axes the theme is offered on. DECLARED rather than derived, because
   * an identity theme carries no sections to derive from — `default` is a UI
   * theme precisely by being the UI cascade.
   */
  readonly axes: { readonly ui: boolean; readonly code: boolean };
  readonly dark?: BuiltinVariant;
  readonly light?: BuiltinVariant;
}

/**
 * Builds a built-in through the SAME parser user files go through, so a
 * curated palette cannot take a shortcut a user file could not take. The
 * declared axes replace the derived ones (see above); everything else —
 * unknown keys, non-string values, over-long values — is reported exactly as
 * it would be for a file on disk.
 *
 * `parseThemeDoc`, not `parseTheme`: this module runs at init on the eager
 * startup path, and the specs are already objects. Round-tripping nine of them
 * through `JSON.stringify` + `JSON.parse` — ~31KB — bought nothing but the
 * file entry point's signature.
 */
export function defineBuiltinTheme(spec: BuiltinThemeSpec): ParsedTheme {
  const doc: Record<string, unknown> = { name: spec.name };
  if (spec.dark) doc.dark = spec.dark;
  if (spec.light) doc.light = spec.light;

  const parsed = parseThemeDoc(spec.id, doc);
  const variants: { dark?: ThemeVariant; light?: ThemeVariant } = {};
  if (parsed.variants.dark) variants.dark = parsed.variants.dark;
  if (parsed.variants.light) variants.light = parsed.variants.light;

  return {
    id: spec.id,
    name: spec.name,
    variants,
    axes: spec.axes,
    // An identity theme defines no tokens ON PURPOSE, so the parser's
    // "this theme defines nothing" warning is not a finding here.
    warnings: parsed.warnings.filter((w) => w.code !== 'empty-theme'),
    builtin: true,
  };
}

/**
 * The two reserved identity themes. They name the cascade rather than layering
 * over it, which is why they carry no variants at all.
 */
export const IDENTITY_BUILTIN_SPECS: readonly BuiltinThemeSpec[] = [
  { id: BUILTIN_UI_THEME_ID, name: 'Default', axes: { ui: true, code: false } },
  { id: BUILTIN_CODE_THEME_ID, name: 'GitHub', axes: { ui: false, code: true } },
];

/**
 * The curated palettes. Listed alphabetically with High Contrast last — it is
 * the accessibility choice rather than a taste one, and the only one that
 * serves both axes. Order here is editorial only: `themesForAxis` sorts a
 * picker's list by id, so nothing downstream depends on it.
 */
export const CURATED_BUILTIN_SPECS: readonly BuiltinThemeSpec[] = [
  CATPPUCCIN,
  DRACULA,
  MONOKAI,
  ONE_DARK,
  SOLARIZED,
  TOKYO_NIGHT,
  HIGH_CONTRAST,
];

export const BUILTIN_THEMES: readonly ParsedTheme[] = [
  ...IDENTITY_BUILTIN_SPECS,
  ...CURATED_BUILTIN_SPECS,
].map(defineBuiltinTheme);
