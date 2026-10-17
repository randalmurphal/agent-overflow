// How far a row Claude discarded is dimmed (app.css `.discarded-row`,
// `.discarded-reasoning`), derived from the applied theme.
//
// The dim must leave the row's text readable: the opacity is the least that
// keeps the text at the 4.5:1 body floor on the app ground, solved against
// the colors the cascade actually paints, so every built-in and user theme
// in either mode gets its own strength. Text the theme already paints at or
// under the floor is not dimmed at all: the dim never takes a theme below
// what it chose.
//
// Like the `--accent-fg` derivation (themeApply.svelte.ts), the colors are
// normalized through `utils/cssColorProbe`; unlike it, the inputs include
// app.css's own palette and the derived fg tiers, which only the live
// cascade can answer, so this runs after the theme has applied (App.svelte)
// and writes one style element. Until it has run, or where the probe cannot
// answer, the CSS falls back to no dim.

import { readTokenColors, rgbChannels, type RgbChannels } from '../utils/cssColorProbe';
import { WINDOW_GROUND_KEY, cssVarName } from './tokenRegistry';

/** WCAG AA body text. */
export const DISCARDED_TEXT_FLOOR = 4.5;

/** Reasoning text is `--fg-muted` at 70% (TailClampedText's `text-fg-muted/70`). */
export const REASONING_TEXT_ALPHA = 0.7;

export const DISCARDED_DIM_STYLE_ID = 'discarded-dim';

/** Text-row prose color (AssistantMessage's body) and the ground it sits on. */
export const DISCARDED_TEXT_TOKEN = cssVarName('fg-muted');
export const DISCARDED_GROUND_TOKEN = cssVarName(WINDOW_GROUND_KEY);

type Rgb = readonly [number, number, number];

function luminance([red, green, blue]: Rgb): number {
  const linear = (channel: number): number => {
    const value = channel / 255;
    return value <= 0.03928 ? value / 12.92 : ((value + 0.055) / 1.055) ** 2.4;
  };
  return 0.2126 * linear(red) + 0.7152 * linear(green) + 0.0722 * linear(blue);
}

function contrast(a: Rgb, b: Rgb): number {
  const la = luminance(a);
  const lb = luminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

/** The 8-bit pixel `fg` at `alpha` paints over `ground`. */
function composite(fg: Rgb, ground: Rgb, alpha: number): Rgb {
  const mix = (f: number, g: number): number => Math.round(alpha * f + (1 - alpha) * g);
  return [mix(fg[0], ground[0]), mix(fg[1], ground[1]), mix(fg[2], ground[2])];
}

/** Contrast of `text` (with its own alpha) on `ground`, inside a group at `opacity`. */
export function dimmedContrast(text: RgbChannels, ground: RgbChannels, opacity: number): number {
  const bg: Rgb = [ground.r, ground.g, ground.b];
  return contrast(composite([text.r, text.g, text.b], bg, text.a * opacity), bg);
}

/**
 * The least opacity, in hundredths, at which `text` on `ground` still meets
 * `floor`. 1 when no dim meets it, including text that is at or under the
 * floor undimmed. Contrast rises monotonically with opacity (the composite
 * moves channel by channel from the ground toward the text), so the first
 * step that meets the floor is the least.
 */
export function solveDimOpacity(
  text: RgbChannels,
  ground: RgbChannels,
  floor: number = DISCARDED_TEXT_FLOOR,
): number {
  for (let hundredths = 1; hundredths < 100; hundredths++) {
    const opacity = hundredths / 100;
    if (dimmedContrast(text, ground, opacity) >= floor) return opacity;
  }
  return 1;
}

export interface DiscardedDim {
  /** `.discarded-row` opacity: prose in `--fg-muted`. */
  readonly text: number;
  /** `.discarded-reasoning` opacity: `--fg-muted` at {@link REASONING_TEXT_ALPHA}. */
  readonly reasoning: number;
}

/** Both strengths for one resolved text color and ground. */
export function deriveDiscardedDim(fgMuted: RgbChannels, ground: RgbChannels): DiscardedDim {
  return {
    text: solveDimOpacity(fgMuted, ground),
    reasoning: solveDimOpacity({ ...fgMuted, a: fgMuted.a * REASONING_TEXT_ALPHA }, ground),
  };
}

export function serializeDiscardedDim(dim: DiscardedDim): string {
  return `:root{--fade-discarded:${dim.text};--fade-discarded-reasoning:${dim.reasoning}}`;
}

/** The colors the derivation reads, normalized; undefined when the probe cannot answer. */
export function readDiscardedDimInputs(): { fgMuted: RgbChannels; ground: RgbChannels } | undefined {
  const colors = readTokenColors([DISCARDED_TEXT_TOKEN, DISCARDED_GROUND_TOKEN]);
  const fgMuted = rgbChannels(colors[DISCARDED_TEXT_TOKEN]);
  const ground = rgbChannels(colors[DISCARDED_GROUND_TOKEN]);
  return fgMuted && ground ? { fgMuted, ground } : undefined;
}

/**
 * Re-derives the strengths from the live cascade and writes them, one style
 * element rewritten only when its text changes. Run after every theme or
 * mode change has applied. Returns what was derived, or undefined when the
 * probe could not answer and the CSS fallback (no dim) applies.
 */
export function applyDiscardedDim(): DiscardedDim | undefined {
  if (typeof document === 'undefined') return undefined;
  const inputs = readDiscardedDimInputs();
  const dim = inputs ? deriveDiscardedDim(inputs.fgMuted, inputs.ground) : undefined;
  const cssText = dim ? serializeDiscardedDim(dim) : '';
  let element = document.getElementById(DISCARDED_DIM_STYLE_ID);
  if (!element) {
    if (!cssText) return undefined;
    element = document.createElement('style');
    element.id = DISCARDED_DIM_STYLE_ID;
    (document.body ?? document.head)?.appendChild(element);
  }
  if (element.textContent !== cssText) element.textContent = cssText;
  return dim;
}
