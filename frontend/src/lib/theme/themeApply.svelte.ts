// The apply step: resolved theme → one `<style id="user-theme">` rewrite.
//
// WHY ONE ELEMENT. Writing N custom properties onto `<html>` with
// `setProperty` costs N whole-document style invalidations, measured at ~13ms
// each at 5k nodes and ~90ms at 30k (docs/specs/theme-system.md §1.4 — the
// same measurement that made `utils/ambientTicker.ts` exist). A theme carries
// up to 79 tokens and a color-picker drag would issue them per frame. One
// element rewritten wholesale is one invalidation per change, and "a token the
// theme does not mention keeps the app's own declaration" falls out of the
// cascade instead of needing a reset value per token.
//
// WHAT THIS ADDS TO `themeResolve.ts`. The resolver is pure and cannot ask a
// browser anything, so it hands over `declarations` — every value it was
// willing to emit, with provenance — and this module does the two jobs that
// need an engine:
//
//   1. `CSS.supports('color', v)` per token. A value that is not a color is
//      DROPPED with a warning naming the token and the file, never fatally:
//      one bad value costs one token. The check is itself checked first (a
//      probe that answers `true` to everything, or `false` to everything, is
//      not usable) and skipped entirely when the engine cannot answer, because
//      dropping the whole palette is a far worse failure than trusting the
//      resolver's structural gate.
//
//   2. `--accent-fg` derivation. A theme that sets `accent` and not
//      `accent-fg` would otherwise keep app.css's white label, which a pale
//      accent strands. The accent is normalized through `utils/cssColorProbe`
//      — the same two-hop resolution the mermaid bridge uses, because
//      `getComputedStyle` does NOT return `rgb()` — and the label is picked by
//      WCAG contrast against white and black.
//
// ORDERING. `App.svelte` calls this from an `$effect.pre`, immediately after
// the `applyThemeClass` pre-effect, and that pairing is load-bearing rather
// than stylistic. Svelte flushes ALL render effects (`$effect.pre`, template
// effects) for the whole tree before ANY user effect, so a stamp in the render
// pass and a rewrite in the user pass would leave a window — the whole render
// pass — in which the document class says `light` while this element still
// holds the `dark` resolution. That window is not theoretical: the resolver
// takes the MODE as an input (a UI theme with only a dark variant is emitted
// only in dark mode, so the CSS is genuinely mode-dependent), and anything
// reading resolved colors out of the cascade from a pre-effect would sample
// the mismatch. Both halves in the render pass, in declaration order, closes
// it for every reader — pre-effects and user effects alike.
//
// FIRST PAINT. `applyTheme` takes a `settled` flag and does NOTHING while it
// is false. The pre-effect runs at mount, before the theme files have been
// fetched, so a selected user theme resolves to the built-in fallback there —
// and writing that would blow away the boot script's cached CSS and inline
// ground, which is the flash the stamp exists to prevent. See its doc comment.
//
// The window-ground probe and the boot stamp deliberately do NOT run here.
// They read the cascade back and write store state, which belongs in the user
// pass after this rewrite has landed; `App.svelte` runs them from a plain
// `$effect`.

import { readTokenColors, rgbChannels, toConcreteColor } from '../utils/cssColorProbe';
import type { ThemeWarning } from './themeParse';
import {
  resolveTheme,
  serializeThemeCss,
  type ResolveInput,
  type ResolvedDeclaration,
  type ResolvedDeclarations,
  type ResolvedMode,
  type ResolvedThemeRef,
} from './themeResolve';
import { WINDOW_GROUND_KEY, cssVarName } from './tokenRegistry';

/** The one element the whole user theme lives in. */
export const USER_THEME_STYLE_ID = 'user-theme';

/**
 * Where the first-paint stamp lives. `index.html`'s inline boot script reads
 * this key literally; `themeBootStamp.test.ts` pins the two spellings
 * together, because a rename that only lands here is silent — the app looks
 * right and the first frame is wrong.
 */
export const THEME_BOOT_STORAGE_KEY = 'agent-overflow:theme:boot';

/** Longest cached CSS the boot stamp will carry. */
export const THEME_BOOT_MAX_CSS = 32 * 1024;

export interface AppliedTheme {
  /** `uiTheme|codeTheme|revision`. Pair it with the resolved mode. */
  readonly paletteIdentity: string;
  readonly mode: ResolvedMode;
  /** What was actually written, after per-token rejection. */
  readonly cssText: string;
  /** Resolver warnings plus this step's per-token rejections. */
  readonly warnings: readonly ThemeWarning[];
  readonly ui: ResolvedThemeRef;
  readonly code: ResolvedThemeRef;
}

const EMPTY_REF: ResolvedThemeRef = { id: '', name: '', source: 'builtin', fallback: false };

const NOTHING_APPLIED: AppliedTheme = {
  paletteIdentity: '',
  mode: 'dark',
  cssText: '',
  warnings: [],
  ui: EMPTY_REF,
  code: EMPTY_REF,
};

let applied = $state.raw<AppliedTheme>(NOTHING_APPLIED);

/** The theme currently on screen. Reactive. */
export function getAppliedTheme(): AppliedTheme {
  return applied;
}

/**
 * The palette identity every cached-render consumer keys on — the mermaid
 * config memo and its `{#key}`, the xterm bridge. Reactive, and deliberately
 * WITHOUT the mode: consumers already read `getResolvedTheme()` for their
 * light/dark posture, so folding it in here would give the pair two spellings
 * that can disagree. The rule is "identity plus mode", and the pairing is the
 * consumer's.
 */
export function getThemePaletteIdentity(): string {
  return applied.paletteIdentity;
}

// ---------------------------------------------------------------------------
// Engine capability
// ---------------------------------------------------------------------------

let colorCheckUsable: boolean | undefined;

/**
 * Whether `CSS.supports('color', …)` can actually discriminate here.
 *
 * BOTH directions are probed. An engine that says yes to everything would
 * wave through the values this check exists to catch, and one that says no to
 * everything would delete the user's whole theme — and happy-dom is the
 * second kind, which is why the unit tests would otherwise assert on an empty
 * stylesheet and prove nothing.
 */
function canCheckColors(): boolean {
  if (colorCheckUsable !== undefined) return colorCheckUsable;
  try {
    colorCheckUsable =
      typeof CSS !== 'undefined' &&
      typeof CSS.supports === 'function' &&
      CSS.supports('color', 'rgb(1, 2, 3)') &&
      !CSS.supports('color', 'definitely-not-a-color');
  } catch {
    colorCheckUsable = false;
  }
  return colorCheckUsable;
}

/** Drops the memoized capability probe. Tests only. */
export function resetThemeApplyForTest(): void {
  colorCheckUsable = undefined;
  applied = NOTHING_APPLIED;
}

// ---------------------------------------------------------------------------
// Accent foreground derivation
// ---------------------------------------------------------------------------

const WHITE = 'rgb(255, 255, 255)';
const BLACK = 'rgb(0, 0, 0)';

/** WCAG 2.x relative luminance of an 8-bit sRGB triple. */
function relativeLuminance([red, green, blue]: [number, number, number]): number {
  const linear = (channel: number): number => {
    const value = channel / 255;
    return value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * linear(red) + 0.7152 * linear(green) + 0.0722 * linear(blue);
}

/**
 * The foreground a label on this accent should be painted in — whichever of
 * white and black has the higher WCAG contrast ratio against it.
 *
 * Returns `undefined` when the accent cannot be normalized (no canvas, an
 * unparseable value). The caller then emits nothing and app.css's own
 * `--accent-fg` keeps applying, which is the right answer for "we could not
 * tell": a guess here would be a wrong label on a filled button.
 */
export function deriveAccentForeground(accent: string): string | undefined {
  // rgbChannels handles BOTH toConcreteColor output forms (canvas rgb()/
  // rgba() and fast-path #hex) — the shared parser exists because a local
  // copy of the rgb() regex over in terminalTheme once silently dropped hex.
  const channels = rgbChannels(toConcreteColor(accent));
  if (!channels) return undefined;
  const luminance = relativeLuminance([channels.r, channels.g, channels.b]);
  const againstWhite = 1.05 / (luminance + 0.05);
  const againstBlack = (luminance + 0.05) / 0.05;
  return againstWhite >= againstBlack ? WHITE : BLACK;
}

// ---------------------------------------------------------------------------
// Enrichment
// ---------------------------------------------------------------------------

const ACCENT_KEY = 'accent';
const ACCENT_FG_KEY = 'accent-fg';

function enrichBlock(
  block: readonly ResolvedDeclaration[],
  warnings: ThemeWarning[],
): ResolvedDeclaration[] {
  const checking = canCheckColors();
  const kept: ResolvedDeclaration[] = [];

  for (const decl of block) {
    if (checking && !CSS.supports('color', decl.value)) {
      warnings.push({
        code: 'not-a-color',
        themeId: decl.themeId,
        path: `${decl.variant}.${decl.section}.${decl.key}`,
        message: `"${decl.value}" is not a color this browser understands, so "${decl.key}" was left at its default.`,
      });
      continue;
    }
    kept.push(decl);
  }

  // Derivation runs over what SURVIVED the check, not over the input block.
  // A theme that states `accent-fg` and typos it gets that value DROPPED
  // above; deciding "the theme stated one" from the input would then suppress
  // the derivation too and leave the accent with neither — app.css's fixed
  // white label on an accent the theme moved. The rule is unchanged for every
  // other case: a usable `accent-fg` is a decision, and this never overrides
  // it.
  const accentAt = kept.findIndex((decl) => decl.key === ACCENT_KEY);
  if (accentAt < 0) return kept;
  if (kept.some((decl) => decl.key === ACCENT_FG_KEY)) return kept;
  const accent = kept[accentAt]!;
  const derived = deriveAccentForeground(accent.value);
  if (!derived) return kept;
  kept.splice(accentAt + 1, 0, {
    key: ACCENT_FG_KEY,
    cssVar: cssVarName(ACCENT_FG_KEY),
    value: derived,
    section: accent.section,
    variant: accent.variant,
    themeId: accent.themeId,
  });
  return kept;
}

/**
 * Filters a resolved declaration set through the engine and adds the derived
 * accent foreground. Exported for the browser test, which is the only place
 * both halves can actually be exercised.
 */
export function enrichDeclarations(
  declarations: ResolvedDeclarations,
  warnings: ThemeWarning[],
): ResolvedDeclarations {
  return {
    root: enrichBlock(declarations.root, warnings),
    light: enrichBlock(declarations.light, warnings),
  };
}

// ---------------------------------------------------------------------------
// The style element
// ---------------------------------------------------------------------------

/**
 * The element, created if `index.html` did not ship one.
 *
 * `index.html` declares it in the BODY on purpose: Vite appends the app's own
 * stylesheet links to the end of `<head>`, so a style element inserted into
 * the head by the boot script would lose the source-order tie to `app.css`
 * and the cached first-paint palette would be ignored. A body element wins
 * that tie by document order without needing `!important` anywhere.
 */
function styleElement(): HTMLStyleElement | null {
  if (typeof document === 'undefined') return null;
  const existing = document.getElementById(USER_THEME_STYLE_ID);
  if (existing instanceof HTMLStyleElement) return existing;
  const created = document.createElement('style');
  created.id = USER_THEME_STYLE_ID;
  (document.body ?? document.head)?.appendChild(created);
  return created;
}

/**
 * Applies a resolved selection and records what landed.
 *
 * Total: every failure mode below this call is a warning plus a skipped
 * token, so there is no input for which this throws or leaves the app
 * unstyled. Returns what was applied so a caller can act on it without a
 * second read.
 *
 * `settled` is the FIRST-PAINT GUARD, and it is not optional decoration. The
 * caller's pre-effects run at mount, before `ThemeFilesGet` has answered, so
 * `themes` is still empty — a selected USER theme resolves to the built-in
 * fallback there. Writing that resolution would overwrite the boot script's
 * cached CSS and clear its inline ground, recreating the exact flash the stamp
 * exists to prevent, and it would do it for the one user whose theme is not a
 * built-in. Until the appearance store reports its first answer (success,
 * refusal or failure — `isAppearanceLoaded()`), this leaves both untouched and
 * publishes nothing: the boot stamp IS the right picture, because it was
 * written from this same client's last applied cascade.
 */
export function applyTheme(input: ResolveInput, settled = true): AppliedTheme {
  if (!settled) return applied;
  const resolved = resolveTheme(input);
  const warnings: ThemeWarning[] = [...resolved.warnings];
  const declarations = enrichDeclarations(resolved.declarations, warnings);
  const cssText = serializeThemeCss(declarations);

  const element = styleElement();
  if (element && element.textContent !== cssText) {
    element.textContent = cssText;
  }
  // The boot script paints the cached ground straight onto <html> so the very
  // first frame is not white. Once the real cascade is live that inline value
  // would out-compete every later theme change, so this is where it goes.
  if (typeof document !== 'undefined') {
    const root = document.documentElement;
    if (root.style.backgroundColor) root.style.removeProperty('background-color');
  }

  const next: AppliedTheme = {
    paletteIdentity: resolved.paletteIdentity,
    mode: resolved.mode,
    cssText,
    warnings,
    ui: resolved.ui,
    code: resolved.code,
  };
  if (!sameApplied(applied, next)) applied = next;
  return next;
}

/**
 * Warning identity, for the equality check below. A COUNT is not identity: a
 * file edit that fixes one typo and introduces another leaves the list the
 * same length, and the settings surface would keep rendering the warning the
 * user just fixed with no way to tell it had gone stale.
 */
function warningsDigest(warnings: readonly ThemeWarning[]): string {
  let out = '';
  for (const warning of warnings) {
    out += `${warning.code} ${warning.themeId ?? ''} ${warning.path}`;
  }
  return out;
}

function sameApplied(a: AppliedTheme, b: AppliedTheme): boolean {
  return (
    a.paletteIdentity === b.paletteIdentity &&
    a.mode === b.mode &&
    a.cssText === b.cssText &&
    a.ui.id === b.ui.id &&
    a.code.id === b.code.id &&
    a.ui.fallback === b.ui.fallback &&
    a.code.fallback === b.code.fallback &&
    warningsDigest(a.warnings) === warningsDigest(b.warnings)
  );
}

// ---------------------------------------------------------------------------
// Window ground + first-paint stamp
// ---------------------------------------------------------------------------

function hexFromChannels({ r, g, b }: { r: number; g: number; b: number }): string {
  const part = (channel: number): string =>
    Math.max(0, Math.min(255, Math.round(channel))).toString(16).padStart(2, '0');
  return `#${part(r)}${part(g)}${part(b)}`;
}

/**
 * The app ground as `#rrggbb`, read back off the LIVE cascade.
 *
 * Deliberately a probe rather than a read of the resolver's raw
 * `windowBackground`: the built-in palette sets the ground too and states it
 * in `oklch`, which is neither a hex the native window can take nor something
 * the resolver reports (it only reports what a THEME FILE contributed). One
 * probe answers for both cases, and it answers with what is actually on
 * screen rather than with what should be.
 *
 * Must run after `applyTheme` and after the mode class is stamped.
 */
export function readWindowGroundHex(): string | undefined {
  const token = cssVarName(WINDOW_GROUND_KEY);
  const raw = readTokenColors([token])[token];
  if (!raw) return undefined;
  const channels = rgbChannels(raw);
  return channels ? hexFromChannels(channels) : undefined;
}

/**
 * Records what the next launch should paint before the bundle loads.
 *
 * The CSS is included only when it is small enough to be free at parse time;
 * the mode class and the ground are what actually stop the flash, and they
 * are two short strings.
 */
export function stampBootTheme(mode: ResolvedMode, background: string | undefined, cssText: string): void {
  if (typeof localStorage === 'undefined') return;
  try {
    localStorage.setItem(
      THEME_BOOT_STORAGE_KEY,
      JSON.stringify({
        c: mode,
        b: background ?? '',
        s: cssText.length <= THEME_BOOT_MAX_CSS ? cssText : '',
      }),
    );
  } catch {
    // A full or disabled origin store costs a first-frame flash, nothing else.
  }
}
