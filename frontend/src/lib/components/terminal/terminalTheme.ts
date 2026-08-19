// xterm.js palette bridge — app tokens → `ITheme`.
//
// xterm cannot be handed CSS. Its renderers bake concrete colors into the
// glyph atlas and its parser accepts hex / `rgb()` / `rgba()` and nothing
// else, so an `oklch(…)` token — which is what most of our palette literally
// is — is rejected and silently falls back. This file therefore used to carry
// a hand-maintained hex DUPLICATE of the ANSI and terminal tokens, 44 values
// that drifted from `app.css` by construction and that a user-authored theme
// could not reach at all.
//
// It now resolves the same tokens the rest of the app paints with, through
// `utils/cssColorProbe.ts` — the module the mermaid bridge already goes
// through, and the one place that knows why resolution is two hops
// (`getComputedStyle` serializes in the DECLARED color space, so the canvas
// readback is what produces real channel values). The result is that chat
// ANSI output and terminal output are now the SAME palette rather than
// "aesthetically twinned", and both follow a theme file.
//
// The probe reads the LIVE cascade, so the `mode` argument is a cache key,
// not a selector: callers must pass the mode that is currently stamped on the
// document. That is the same contract `resolveMermaidThemeConfig` has, and
// for the same reason — the single caller passes `getResolvedTheme()`.

import type { ITheme } from '@xterm/xterm';
import type { ResolvedTheme } from '../../stores/themeMode.svelte';
import { getThemePaletteIdentity } from '../../theme/themeApply.svelte';
import { readTokenColors, rgbChannels } from '../../utils/cssColorProbe';

/**
 * ANSI slot → token, in xterm's own key order. The 16 foregrounds are the
 * `--ansi-fg-*` family the chat renderer uses; `--ansi-fg-37` and
 * `--ansi-fg-90` are aliases of the app text palette, which the probe
 * resolves through for free.
 */
const ANSI_SLOTS: readonly (readonly [keyof ITheme, string])[] = [
  ['black', '--ansi-fg-30'],
  ['red', '--ansi-fg-31'],
  ['green', '--ansi-fg-32'],
  ['yellow', '--ansi-fg-33'],
  ['blue', '--ansi-fg-34'],
  ['magenta', '--ansi-fg-35'],
  ['cyan', '--ansi-fg-36'],
  ['white', '--ansi-fg-37'],
  ['brightBlack', '--ansi-fg-90'],
  ['brightRed', '--ansi-fg-91'],
  ['brightGreen', '--ansi-fg-92'],
  ['brightYellow', '--ansi-fg-93'],
  ['brightBlue', '--ansi-fg-94'],
  ['brightMagenta', '--ansi-fg-95'],
  ['brightCyan', '--ansi-fg-96'],
  ['brightWhite', '--ansi-fg-97'],
];

const TERMINAL_BG = '--terminal-bg';
const TEXT_PRIMARY = '--text-primary';
const ACCENT = '--accent';

const TOKENS: readonly string[] = [
  TERMINAL_BG,
  TEXT_PRIMARY,
  ACCENT,
  ...ANSI_SLOTS.map(([, token]) => token),
];

/**
 * Selection alpha. The old palette stated selection as an opaque slab plus a
 * matching foreground, which meant restating the text color for every theme;
 * a translucent accent tint states the same intent once and lets the glyphs
 * keep their own colors underneath, so a code-colored line stays readable
 * while selected.
 */
const SELECTION_ALPHA = 0.4;
const SELECTION_INACTIVE_ALPHA = 0.22;

// The channel read is `utils/cssColorProbe`'s, not a regex of our own:
// `toConcreteColor` documents TWO output forms and a local copy here only
// understood `rgb()`, so a hex-valued accent silently lost its selection tint.
function withAlpha(concrete: string | undefined, alpha: number): string | undefined {
  const channels = rgbChannels(concrete);
  if (!channels) return undefined;
  return `rgba(${channels.r}, ${channels.g}, ${channels.b}, ${alpha})`;
}

function buildTheme(): { theme: ITheme; resolvedAny: boolean } {
  const colors = readTokenColors(TOKENS);
  const theme: Record<string, string> = {};
  const set = (key: string, value: string | undefined): void => {
    if (value) theme[key] = value;
  };

  const background = colors[TERMINAL_BG];
  const foreground = colors[TEXT_PRIMARY];
  set('background', background);
  set('foreground', foreground);
  // The caret is the text color on the terminal ground, and its accent is
  // what shows THROUGH the caret — so the pair is the same two tokens
  // swapped, in both modes, with no third value to maintain.
  set('cursor', foreground);
  set('cursorAccent', background);
  set('selectionBackground', withAlpha(colors[ACCENT], SELECTION_ALPHA));
  set('selectionInactiveBackground', withAlpha(colors[ACCENT], SELECTION_INACTIVE_ALPHA));
  for (const [key, token] of ANSI_SLOTS) set(key, colors[token]);

  return { theme: theme as ITheme, resolvedAny: Object.keys(theme).length > 0 };
}

// Memoized for the CURRENT `mode|paletteIdentity` and nothing else. Two
// reasons to memoize at all, and the second is the load-bearing one, exactly
// as in `markdown/mermaidTokens.ts`:
//
// 1. The probe is one forced style recalc, and every terminal pane would pay
//    it on mount and on every unrelated reactive tick otherwise.
// 2. The returned object's IDENTITY matters. `term.options.theme = x` makes
//    xterm rebuild its glyph atlas; handing back a fresh-but-equal object on
//    a tick that changed nothing would re-rasterize every open terminal.
//
// SINGLE ENTRY, on purpose. The key carries a monotonic revision, so a keyed
// Map would accumulate one dead palette per theme-file edit for the life of
// the session with nothing in production ever evicting them. Only the current
// identity is ever asked for — the mode is whatever is stamped on the document
// and the identity is whatever is applied — so a miss means the previous entry
// is unreachable, and the miss path drops it before it stores the new one.
let cachedIdentity: string | undefined;
let cachedTheme: ITheme | undefined;

let warnedUnresolved = false;

/** The cache key a consumer's effect must track: mode plus palette identity. */
export function xtermPaletteIdentity(mode: ResolvedTheme): string {
  return `${mode}|${getThemePaletteIdentity()}`;
}

/**
 * The xterm theme for the mode currently stamped on the document.
 *
 * A palette that cannot be resolved at all (no canvas, no body — happy-dom)
 * answers with an EMPTY theme rather than a frozen copy of yesterday's hex
 * values: xterm's own defaults are a legible terminal, and a stale duplicate
 * is the thing this bridge exists to delete. The failure is reported once,
 * and the result is not cached, so a transient failure cannot degrade the
 * surface for the rest of the session.
 */
export function getXtermTheme(mode: ResolvedTheme): ITheme {
  const identity = xtermPaletteIdentity(mode);
  if (cachedTheme && cachedIdentity === identity) return cachedTheme;
  // A miss retires the previous palette before anything replaces it, so the
  // cache holds at most one entry whether or not the rebuild below succeeds.
  cachedIdentity = undefined;
  cachedTheme = undefined;

  const { theme, resolvedAny } = buildTheme();
  if (!resolvedAny) {
    if (!warnedUnresolved) {
      warnedUnresolved = true;
      console.warn('terminal palette unresolved; xterm falls back to its built-in colors');
    }
    return theme;
  }
  cachedIdentity = identity;
  cachedTheme = theme;
  return theme;
}

/**
 * Drops the memoized palette and re-arms the unresolved warning.
 *
 * TESTS ONLY — production has no caller and needs none: the cache holds one
 * entry keyed on the palette identity, so a theme edit evicts it by missing.
 */
export function resetXtermThemeCache(): void {
  cachedIdentity = undefined;
  cachedTheme = undefined;
  warnedUnresolved = false;
}
