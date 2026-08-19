// Readability gate for the curated built-in palettes.
//
// The other half of shipping palettes as data: a curated theme cannot be
// "mostly right". Nobody tunes it after install, and the person most likely to
// pick High Contrast is the person least able to work around a value that
// missed. So every curated variant is measured here — pure WCAG 2.x math over
// the hex the theme states, no DOM, no probe — against floors that say what
// the surface is FOR:
//
//   • text on the app ground …… 4.5:1, and 7:1 for High Contrast, which is the
//     whole reason that theme exists (WCAG AA body text / AAA respectively).
//   • High Contrast's foreground hierarchy …… 4.5:1 for all four tiers. The
//     default theme's `fg-hint` sits near 3:1 by design; the high-contrast
//     theme states its tiers literally so that recession cannot happen, and
//     this is the assertion that keeps that promise honest.
//   • syntax on the code-block ground …… 3:1 (WCAG's non-text / large-text
//     floor, which is the right one for short monospace runs read in bulk),
//     with a per-theme exception list for the faint-by-design comment tone.
//   • the ANSI slots ordinary output is painted with (37, 97) …… 4.5:1 on the
//     terminal ground.
//
// The 14 remaining ANSI slots are deliberately NOT measured. A palette's ANSI
// black on its own dark ground is invisible by construction — that is what
// ANSI black IS — and "fixing" it would mean shipping something no Monokai or
// Dracula user would recognise.
//
// EXCEPTIONS carry their measured ratio, and the measurement is re-checked, so
// an entry cannot drift away from the value it was granted for. They also have
// a hard floor of their own: faint-by-design is a reason for 2.7:1, never for
// 1.9:1. A palette whose comment tone falls below that is adjusted in the data
// instead, with the canonical value recorded beside it (see solarized.ts and
// catppuccin.ts).

import { describe, expect, it } from 'vitest';
import { CURATED_BUILTIN_SPECS, defineBuiltinTheme } from './builtins';
import { THEME_VARIANTS, type ThemeVariant, type ThemeVariantName } from './themeParse';
import { tokenKeysInSection } from './tokenRegistry';

// ---------------------------------------------------------------------------
// WCAG 2.x relative luminance and contrast ratio
// ---------------------------------------------------------------------------

/** `#rgb`, `#rrggbb` and `#rrggbbaa` (alpha ignored — it composites, we compare). */
export function parseHex(hex: string): readonly [number, number, number] {
  const body = hex.replace('#', '');
  const expand = body.length === 3 || body.length === 4;
  const pick = (i: number): number =>
    expand
      ? parseInt(body[i]!.repeat(2), 16)
      : parseInt(body.slice(i * 2, i * 2 + 2), 16);
  const rgb = [pick(0), pick(1), pick(2)] as const;
  if (rgb.some((channel) => Number.isNaN(channel))) throw new Error(`not a hex color: ${hex}`);
  return rgb;
}

/** WCAG 2.x relative luminance, sRGB. */
export function relativeLuminance(hex: string): number {
  const [r, g, b] = parseHex(hex).map((channel) => {
    const c = channel / 255;
    return c <= 0.03928 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4;
  }) as [number, number, number];
  return 0.2126 * r + 0.7152 * g + 0.0722 * b;
}

/** WCAG 2.x contrast ratio, 1:1 … 21:1. Order-independent. */
export function contrastRatio(a: string, b: string): number {
  const la = relativeLuminance(a);
  const lb = relativeLuminance(b);
  return (Math.max(la, lb) + 0.05) / (Math.min(la, lb) + 0.05);
}

const round = (ratio: number): number => Math.round(ratio * 100) / 100;

// ---------------------------------------------------------------------------
// Documented exceptions
// ---------------------------------------------------------------------------

interface Exception {
  /** Measured at the time it was granted; re-checked below. */
  readonly ratio: number;
  readonly reason: string;
}

/** Keyed `<themeId>.<variant>.<token>`. */
const CONTRAST_EXCEPTIONS: Record<string, Exception> = {
  'tokyo-night.dark.syntax-comment': {
    ratio: 2.77,
    reason:
      "tokyonight's canonical comment tone (#565f89) on its own ground. Comments are faint by design in this palette and the value is one of its signatures; the markup-quote family, which carries prose, is moved off it to dark5 instead.",
  },
  'solarized.dark.syntax-comment': {
    ratio: 2.79,
    reason:
      'Schoonover uses base01 for comments on base03, and the base ramp IS Solarized — a comment tone one step in would be a different theme. Solarized LIGHT does not get the same latitude: base1 measures 1.9:1 there, under the hard floor, so it is adjusted in the data.',
  },
};

/** Even a faint-by-design role has a floor. */
const EXCEPTION_HARD_FLOOR = 2.5;
const SYNTAX_FLOOR = 3;
const TEXT_FLOOR = 4.5;
const HIGH_CONTRAST_TEXT_FLOOR = 7;

// ---------------------------------------------------------------------------

const SYNTAX_KEYS = [...tokenKeysInSection('syntax')];
const FG_TIERS = ['text-primary', 'text-secondary', 'fg-muted', 'fg-subtle', 'fg-hint'] as const;

interface Case {
  readonly themeId: string;
  readonly variantName: ThemeVariantName;
  readonly variant: ThemeVariant;
}

const CASES: Case[] = [];
for (const spec of CURATED_BUILTIN_SPECS) {
  const parsed = defineBuiltinTheme(spec);
  for (const variantName of THEME_VARIANTS) {
    const variant = parsed.variants[variantName];
    if (variant) CASES.push({ themeId: parsed.id, variantName, variant });
  }
}

/** Every violation in one array, so one run reports the whole palette. */
function violations(check: (report: (message: string) => void) => void): string[] {
  const found: string[] = [];
  check((message) => found.push(message));
  return found;
}

describe('curated palette contrast', () => {
  it('measures something — the cases exist and carry their grounds', () => {
    // Seven themes; Catppuccin, Solarized and High Contrast ship both variants.
    expect(CASES.length).toBe(10);
    for (const { themeId, variantName, variant } of CASES) {
      expect(variant.code?.['code-block'], `${themeId}.${variantName} code-block`).toBeDefined();
      expect(variant.code?.['terminal-bg'], `${themeId}.${variantName} terminal-bg`).toBeDefined();
    }
  });

  it('pins the WCAG math against known pairs', () => {
    expect(round(contrastRatio('#000000', '#ffffff'))).toBe(21);
    expect(round(contrastRatio('#ffffff', '#ffffff'))).toBe(1);
    // GitHub dark's comment grey on our default code-block ground, both ways.
    expect(round(contrastRatio('#8b949e', '#161b22'))).toBe(round(contrastRatio('#161b22', '#8b949e')));
    expect(parseHex('#abc')).toEqual([0xaa, 0xbb, 0xcc]);
    expect(parseHex('#00000080')).toEqual([0, 0, 0]);
  });

  it('keeps every syntax family readable on its own code-block ground', () => {
    expect(
      violations((report) => {
        for (const { themeId, variantName, variant } of CASES) {
          const ground = variant.code?.['code-block'];
          if (!ground) continue;
          for (const key of SYNTAX_KEYS) {
            const value = variant.syntax?.[key];
            if (!value) continue;
            const ratio = contrastRatio(value, ground);
            const exception = CONTRAST_EXCEPTIONS[`${themeId}.${variantName}.${key}`];
            const floor = exception ? EXCEPTION_HARD_FLOOR : SYNTAX_FLOOR;
            if (ratio < floor) {
              report(
                `${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1, floor ${floor}:1`,
              );
            }
          }
        }
      }),
    ).toEqual([]);
  });

  it('holds each documented exception to the ratio it was granted for', () => {
    const drift: string[] = [];
    for (const [key, exception] of Object.entries(CONTRAST_EXCEPTIONS)) {
      const [themeId, variantName, token] = key.split('.') as [string, ThemeVariantName, string];
      const found = CASES.find((c) => c.themeId === themeId && c.variantName === variantName);
      const value = found?.variant.syntax?.[token];
      if (!value || !found?.variant.code?.['code-block']) {
        drift.push(`${key}: no such token — a stale exception`);
        continue;
      }
      const ratio = round(contrastRatio(value, found.variant.code['code-block']));
      if (Math.abs(ratio - exception.ratio) > 0.05) {
        drift.push(`${key}: recorded ${exception.ratio}:1, measures ${ratio}:1`);
      }
      // An exception that now clears the floor is not an exception any more.
      if (ratio >= SYNTAX_FLOOR) drift.push(`${key}: measures ${ratio}:1 — delete the exception`);
      expect(exception.reason.length).toBeGreaterThan(40);
    }
    expect(drift).toEqual([]);
  });

  it('keeps plain terminal output legible on the terminal ground', () => {
    expect(
      violations((report) => {
        for (const { themeId, variantName, variant } of CASES) {
          const ground = variant.code?.['terminal-bg'];
          if (!ground) continue;
          for (const key of ['ansi-fg-37', 'ansi-fg-97']) {
            const value = variant.ansi?.[key];
            if (!value) continue;
            const ratio = contrastRatio(value, ground);
            if (ratio < TEXT_FLOOR) {
              report(
                `${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1, floor ${TEXT_FLOOR}:1`,
              );
            }
          }
        }
      }),
    ).toEqual([]);
  });

  it('keeps the whole foreground hierarchy readable on the app ground', () => {
    expect(
      violations((report) => {
        for (const { themeId, variantName, variant } of CASES) {
          const ground = variant.colors?.['surface-0'];
          if (!ground) continue;
          const highContrast = themeId === 'high-contrast';
          for (const key of FG_TIERS) {
            const value = variant.colors?.[key];
            if (!value) continue;
            const floor =
              key === 'text-primary' && highContrast ? HIGH_CONTRAST_TEXT_FLOOR : TEXT_FLOOR;
            const ratio = contrastRatio(value, ground);
            if (ratio < floor) {
              report(
                `${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1, floor ${floor}:1`,
              );
            }
          }
        }
      }),
    ).toEqual([]);
  });

  it('gives a UI theme a visible border scale and a usable accent', () => {
    expect(
      violations((report) => {
        for (const { themeId, variantName, variant } of CASES) {
          const colors = variant.colors;
          const ground = colors?.['surface-0'];
          if (!colors || !ground) continue;

          // The point of the high-contrast theme's border overrides: the
          // SOFTEST tier still has to be a line you can see. 3:1 is WCAG's
          // non-text floor, which is exactly what a hairline is.
          for (const key of ['border-subtle', 'border', 'border-strong']) {
            const value = colors[key];
            if (!value) continue;
            const ratio = contrastRatio(value, ground);
            if (ratio < SYNTAX_FLOOR) {
              report(`${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1`);
            }
          }

          // An accent that cannot carry its own label is the failure the
          // accent-fg token exists to prevent, so the pair is measured.
          const accent = colors.accent;
          const accentFg = colors['accent-fg'];
          if (accent && accentFg) {
            const ratio = contrastRatio(accent, accentFg);
            if (ratio < TEXT_FLOOR) {
              report(
                `${themeId}.${variantName}: accent-fg ${accentFg} on accent ${accent} is ${round(ratio)}:1`,
              );
            }
          }

          // Status colors are read as text on the ground.
          for (const key of ['info', 'success', 'error', 'warning', 'accent']) {
            const value = colors[key];
            if (!value) continue;
            const ratio = contrastRatio(value, ground);
            if (ratio < TEXT_FLOOR) {
              report(
                `${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1, floor ${TEXT_FLOOR}:1`,
              );
            }
          }
        }
      }),
    ).toEqual([]);
  });

  it('separates the elevation ladder so surfaces do not collapse together', () => {
    expect(
      violations((report) => {
        for (const { themeId, variantName, variant } of CASES) {
          const colors = variant.colors;
          if (!colors) continue;
          const ladder = ['surface-0', 'surface-1', 'surface-2', 'surface-3'];
          for (let i = 1; i < ladder.length; i += 1) {
            const lower = colors[ladder[i - 1]!];
            const upper = colors[ladder[i]!];
            if (!lower || !upper) continue;
            const delta = Math.abs(relativeLuminance(upper) - relativeLuminance(lower));
            if (delta < 0.004) {
              report(
                `${themeId}.${variantName}: ${ladder[i - 1]} and ${ladder[i]} are indistinguishable (Δ luminance ${delta.toFixed(4)})`,
              );
            }
          }
        }
      }),
    ).toEqual([]);
  });
});
