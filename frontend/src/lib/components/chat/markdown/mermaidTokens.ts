// Mermaid palette bridge — app tokens → `mermaid` `themeVariables`.
//
// Without this, diagrams are the one markdown surface with a palette of
// its own: the vendored `Streamdown.svelte` falls back to mermaid's
// built-in `'dark'` / `'default'` themes, whose fills, strokes and label
// colors have no relationship to anything else on screen. We instead
// pin `theme: 'base'` — mermaid's derive-everything-from-variables theme
// — and hand it concrete colors resolved from the same tokens the rest
// of the app paints with.
//
// **Mermaid cannot be handed CSS.** Its internal color math runs through
// `khroma`, which parses hex / `rgb()` / `rgba()` / named colors and
// nothing else. A raw `var(--surface-1)` never resolves (mermaid bakes
// values into the SVG, it does not inherit them) and an `oklch(…)`
// string — which is what our tokens literally are — throws inside
// khroma's parser. Resolving a token to 8-bit sRGB is `utils/cssColorProbe.ts`'s
// job, including the browser facts that make it two hops; this module
// only decides WHICH tokens map to which mermaid variables, and when the
// answer may be reused.

import type { MermaidConfig } from 'mermaid';
import { getResolvedTheme, type ResolvedTheme } from '../../../stores/themeMode.svelte';
import { getSettings } from '../../../stores/settings.svelte';
import { getThemePaletteIdentity } from '../../../theme/themeApply.svelte';
import { readTokenStyles, resetCssColorProbe } from '../../../utils/cssColorProbe';

/** Tokens the diagram palette is derived from. Nothing here is invented:
 *  every entry is an existing app token, so a theme edit moves diagrams
 *  with everything else. */
const TOKENS = {
  surface0: '--surface-0',
  surface1: '--surface-1',
  surface2: '--surface-2',
  textPrimary: '--text-primary',
  textSecondary: '--text-secondary',
  borderStrong: '--border-strong',
} as const;

const FONT_TOKEN = '--font-sans';

type TokenName = keyof typeof TOKENS;
type Palette = Partial<Record<TokenName, string>>;

// A font stack is a list of family names, so the shape is narrow by
// nature: letters, digits, spaces, quotes, commas, hyphens and dots.
// The value is baked verbatim into rendered SVG, so it gets the same
// OMIT-never-pass-through policy the colors get. Theme FILES cannot reach
// `--font-sans` (the token registry is colors-only, deliberately), but
// Settings → Appearance can, and a user font name is user input either way.
const SAFE_FONT_FAMILY = /^[A-Za-z0-9 ,"'\-.]{1,256}$/;

const TOKEN_NAMES = Object.values(TOKENS);

function readPalette(): { colors: Palette; fontFamily: string | undefined } {
  const { colors: byToken, fontFamily } = readTokenStyles(TOKEN_NAMES, FONT_TOKEN);
  const colors: Palette = {};
  for (const [name, token] of Object.entries(TOKENS) as [TokenName, string][]) {
    const value = byToken[token];
    if (value) colors[name] = value;
  }
  return {
    colors,
    fontFamily: fontFamily && SAFE_FONT_FAMILY.test(fontFamily) ? fontFamily : undefined,
  };
}

/**
 * The diagram palette for a resolved light/dark mode.
 *
 * Deliberately conservative: diagrams should read as native chrome, not
 * as a second color system. Fills come from the surface ladder, text and
 * lines from the text ladder, and every mermaid variable we do not name
 * is left to `'base'`'s own derivation from those.
 */
function buildConfig(resolved: ResolvedTheme): {
  config: MermaidConfig;
  resolvedAnyColor: boolean;
} {
  const { colors, fontFamily } = readPalette();
  const darkMode = resolved === 'dark';

  const themeVariables: Record<string, string | boolean> = { darkMode };
  const set = (key: string, value: string | undefined): void => {
    if (value) themeVariables[key] = value;
  };

  // Ground the diagram on the same block background its wrapper paints.
  set('background', colors.surface1);
  set('mainBkg', colors.surface1);
  // Node fills sit one elevation step up, the way every other card does.
  set('primaryColor', colors.surface2);
  set('secondaryColor', colors.surface2);
  set('tertiaryColor', colors.surface0);
  set('primaryTextColor', colors.textPrimary);
  set('textColor', colors.textPrimary);
  set('primaryBorderColor', colors.borderStrong);
  set('lineColor', colors.textSecondary);
  set('noteBkgColor', colors.surface2);
  set('noteTextColor', colors.textPrimary);
  if (fontFamily) themeVariables.fontFamily = fontFamily;

  return {
    config: {
      theme: 'base',
      darkMode,
      themeVariables,
      // Vendor's default config hardcodes `fontFamily: 'monospace'` at the
      // top level and spreads ours after it, so this is what actually
      // reaches the labels; the themeVariables copy covers the paths that
      // read the theme instead of the config.
      ...(fontFamily ? { fontFamily } : {}),
    },
    resolvedAnyColor: Object.keys(colors).length > 0,
  };
}

/**
 * The identity of the palette a diagram would resolve to right now.
 *
 * Both reads are reactive, and BOTH belong here. The resolved mode is the
 * obvious half; `sansFont` is the half that was a live bug — `fontFamily`
 * is resolved from `--font-sans` and baked into the cached config, and
 * `utils/fonts.ts#applyFonts` rewrites that variable on a settings change,
 * so a mode-keyed cache handed every already-rendered diagram the old font
 * forever.
 *
 * The third half is phase 2's, and it arrived as promised through this ONE
 * string: `getThemePaletteIdentity()` is `uiTheme|codeTheme|revision`, so a
 * theme swap AND an agent's edit to the selected file both invalidate every
 * cached diagram. The revision component is why the edit case works at all —
 * the selection is unchanged when a file is rewritten under it.
 */
export function mermaidPaletteIdentity(): string {
  return paletteIdentityFor(getResolvedTheme());
}

function paletteIdentityFor(resolved: ResolvedTheme): string {
  return `${resolved}|${getSettings().sansFont}|${getThemePaletteIdentity()}`;
}

// The resolved palette is memoized for the CURRENT identity. Two reasons,
// and the second is the load-bearing one:
//
// 1. The probe is cheap but it is a forced style recalc, and a thread
//    full of diagrams would pay it once per mount otherwise.
// 2. **The returned object's IDENTITY is the contract.** `getSettings()`
//    is reassigned wholesale on every settings write, so the `$derived`
//    in `ChatMarkdown` that calls this invalidates on EVERY save. Only
//    handing back the same object reference stops the vendored
//    `Mermaid.svelte`'s `{@attach}` from re-running `mermaid.render` for
//    every visible diagram on an unrelated settings change. Never return
//    a fresh object for an unchanged identity.
//
// SINGLE ENTRY, on purpose. The identity carries the theme system's
// monotonic revision and the sans-font setting, so a keyed Map would
// accumulate one dead config per theme-file edit and per font change for
// the life of the session, with nothing in production ever evicting them.
// Every caller asks for the identity that is live right now, so a miss
// means the held entry is unreachable — and the miss path drops it before
// storing the new one.
let cachedIdentity: string | undefined;
let cachedConfig: MermaidConfig | undefined;

let warnedUnresolved = false;

/**
 * Builds (or returns the cached) mermaid config for `resolved`.
 *
 * Pure with respect to the DOM: it READS the cascade and never stamps it.
 * The `html.light` / `html.dark` stamp is App.svelte's `$effect.pre`,
 * which is a render effect on the root component and therefore runs
 * before every descendant user effect in the flush by construction — the
 * vendored `Mermaid.svelte` reads this config inside an `{@attach}`,
 * which Svelte builds as a user effect.
 *
 * The one caller passes `getResolvedTheme()`, the same resolver
 * `mermaidPaletteIdentity()` reads, so the `{#key}` on the host and the
 * memo key here cannot disagree.
 */
export function resolveMermaidThemeConfig(resolved: ResolvedTheme): MermaidConfig {
  const identity = paletteIdentityFor(resolved);
  if (cachedConfig && cachedIdentity === identity) return cachedConfig;
  // A miss retires the held config before anything replaces it, so the cache
  // holds at most one entry whether or not the rebuild below succeeds.
  cachedIdentity = undefined;
  cachedConfig = undefined;

  const { config, resolvedAnyColor } = buildConfig(resolved);
  if (!resolvedAnyColor) {
    // No canvas, no body, or every token unresolved. Serve this call so
    // diagrams still render (on mermaid's own `base` defaults), but do
    // NOT cache it: a transient failure must not degrade the surface for
    // the rest of the session, and errors are user-facing state rather
    // than silence.
    if (!warnedUnresolved) {
      warnedUnresolved = true;
      console.warn('mermaid palette unresolved; diagrams fall back to built-in theme');
    }
    return config;
  }

  cachedIdentity = identity;
  cachedConfig = config;
  return config;
}

/**
 * Drops the memoized palette AND the probe's canvas-context memo, and re-arms
 * the unresolved-palette warning.
 *
 * TESTS ONLY — production has no caller and needs none: the cache holds one
 * entry keyed on the palette identity, so a theme edit or a font change
 * evicts it by missing.
 */
export function resetMermaidThemeCaches(): void {
  cachedIdentity = undefined;
  cachedConfig = undefined;
  warnedUnresolved = false;
  resetCssColorProbe();
}
