// Readability gate for the curated built-in palettes.
//
// The other half of shipping palettes as data: a curated theme cannot be
// "mostly right". Nobody tunes it after install, and the person most likely to
// pick High Contrast is the person least able to work around a value that
// missed. So every curated variant is measured here — pure WCAG 2.x math over
// the hex the theme states, no DOM, no probe — against floors that say what
// the surface is FOR:
//
//   • focal text on the app ground …… 4.5:1, and 7:1 for High Contrast, which
//     is the whole reason that theme exists (WCAG AA body text / AAA).
//   • supporting text and the muted body tier …… 3:1, for the reason recorded
//     at SUPPORTING_TEXT_FLOOR. A tier a theme STATES is held to 4.5:1
//     instead: stating it is the claim that it was tuned. High Contrast states
//     all four, and that is the assertion keeping its promise honest.
//   • syntax on the code-block ground …… 3:1 (WCAG's non-text / large-text
//     floor, which is the right one for short monospace runs read in bulk),
//     with a per-theme exception list for the faint-by-design comment tone.
//   • the ANSI slots ordinary output is painted with (37, 97) …… 4.5:1 on the
//     terminal ground.
//   • borders …… 1.5:1, and 3:1 for High Contrast; the accent against its own
//     foreground …… 4.5:1, because that pair is a real label on a real fill;
//     status colors …… 3:1, or text contrast in High Contrast.
//   • the surface ladder …… monotonic, with every adjacent pair separable.
//
// The derived foreground tiers are measured AS COMPOSITED (see compositeOver),
// not as the undiluted token, so a palette cannot pass a tier by declining to
// mention it.
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

/**
 * What a translucent foreground actually LOOKS like once it lands on a ground.
 *
 * This is how the derived foreground hierarchy has to be measured. `--fg-muted`
 * and friends are `color-mix(in oklab, var(--text-primary) N%, transparent)` —
 * i.e. the text color at N% alpha — so the pixel a reader sees is the mix of
 * that color with whatever is behind it, and asserting on the undiluted token
 * would measure a color the app never paints. Compositing is sRGB because that
 * is where the browser composites; the mix itself happens in oklab, so this is
 * an approximation, and it is a conservative one at these alphas.
 */
export function compositeOver(fg: string, bg: string, alpha: number): string {
  const [fr, fg_, fb] = parseHex(fg);
  const [br, bg_, bb] = parseHex(bg);
  const mix = (f: number, b: number): string =>
    Math.round(alpha * f + (1 - alpha) * b)
      .toString(16)
      .padStart(2, '0');
  return `#${mix(fr, br)}${mix(fg_, bg_)}${mix(fb, bb)}`;
}

/** The alphas `styles/tokens.css` fades the foreground hierarchy by. */
const FG_TIER_ALPHA: Readonly<Record<string, number>> = {
  'fg-muted': 0.8,
  'fg-subtle': 0.55,
  'fg-hint': 0.3,
};

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

/**
 * Supporting text and the muted body tier sit at WCAG's large-text / non-text
 * floor rather than at 4.5:1, and that is a decision rather than a shrug:
 * every one of these upstreams paints its secondary tone with the same value
 * it paints comments with, and forcing 4.5:1 there would mean recoloring the
 * one tier readers most associate with the palette's character. The FOCAL text
 * tier carries the 4.5:1 obligation, and it does so in every theme.
 */
const SUPPORTING_TEXT_FLOOR = 3;

/**
 * A hairline only has to be SEEN, not read, and upstream themes draw them
 * quietly — several draw them darker than their own ground. 1.5:1 is the point
 * at which a line is still a line. High Contrast is held to 3:1 instead: hard,
 * unmissable borders are half of what that theme is for.
 */
const BORDER_FLOOR = 1.5;
const HIGH_CONTRAST_BORDER_FLOOR = 3;

/**
 * Status colors and the accent are glyphs, fills and short labels as often as
 * they are body text, so they sit at 3:1 — except in High Contrast, where they
 * are held to text contrast like everything else.
 */
const STATUS_FLOOR = 3;

/** Adjacent surface tiers must be separable. See the ladder test. */
const LADDER_STEP_FLOOR = 1.04;

// ---------------------------------------------------------------------------

const SYNTAX_KEYS = [...tokenKeysInSection('syntax')];
const SURFACE_LADDER = ['surface-0', 'surface-1', 'surface-2', 'surface-3'] as const;

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
    // Nine themes; Catppuccin, Gruvbox, Solarized and High Contrast ship both
    // variants.
    expect(CASES.length).toBe(13);
    for (const { themeId, variantName, variant } of CASES) {
      expect(variant.code?.['code-block'], `${themeId}.${variantName} code-block`).toBeDefined();
      expect(variant.code?.['terminal-bg'], `${themeId}.${variantName} terminal-bg`).toBeDefined();
    }
    // The UI floors below skip a variant that carries no colors, so the count
    // of variants that DO is pinned: every curated palette but Monokai dresses
    // chrome, and a section quietly dropped would otherwise pass by absence.
    const withColors = CASES.filter((c) => c.variant.colors !== undefined);
    expect(withColors.length).toBe(12);
    expect(new Set(withColors.map((c) => c.themeId)).size).toBe(8);
    expect(CASES.filter((c) => c.variant.colors === undefined).map((c) => c.themeId)).toEqual([
      'monokai',
    ]);
  });

  it('pins the WCAG math against known pairs', () => {
    expect(round(contrastRatio('#000000', '#ffffff'))).toBe(21);
    expect(round(contrastRatio('#ffffff', '#ffffff'))).toBe(1);
    // GitHub dark's comment grey on our default code-block ground, both ways.
    expect(round(contrastRatio('#8b949e', '#161b22'))).toBe(round(contrastRatio('#161b22', '#8b949e')));
    expect(parseHex('#abc')).toEqual([0xaa, 0xbb, 0xcc]);
    expect(parseHex('#00000080')).toEqual([0, 0, 0]);

    // Compositing, both ends and the middle — the derived-tier floors are only
    // as honest as this is.
    expect(compositeOver('#ffffff', '#000000', 1)).toBe('#ffffff');
    expect(compositeOver('#ffffff', '#000000', 0)).toBe('#000000');
    expect(compositeOver('#ffffff', '#000000', 0.5)).toBe('#808080');
    expect(compositeOver('#ffffff', '#000000', 0.3)).toBe('#4d4d4d');
    // A fade reduces contrast, which is the entire reason it is measured.
    expect(contrastRatio(compositeOver('#ffffff', '#111111', 0.3), '#111111')).toBeLessThan(
      contrastRatio('#ffffff', '#111111'),
    );
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
          const colors = variant.colors;
          const ground = colors?.['surface-0'];
          if (!colors || !ground) continue;
          const highContrast = themeId === 'high-contrast';

          const check = (key: string, value: string, floor: number, note = ''): void => {
            const ratio = contrastRatio(value, ground);
            if (ratio < floor) {
              report(
                `${themeId}.${variantName}.${key}${note}: ${value} on ${ground} is ${round(ratio)}:1, floor ${floor}:1`,
              );
            }
          };

          const primary = colors['text-primary'];
          if (primary) {
            check(
              'text-primary',
              primary,
              highContrast ? HIGH_CONTRAST_TEXT_FLOOR : TEXT_FLOOR,
            );
          }
          const secondary = colors['text-secondary'];
          if (secondary) {
            check('text-secondary', secondary, highContrast ? TEXT_FLOOR : SUPPORTING_TEXT_FLOOR);
          }

          // The three fade tiers, measured as they RENDER. A theme that states
          // them is held to text contrast (that is why it bothered); a theme
          // that leaves them deriving is measured on the composited result, so
          // a palette cannot pass by declining to mention the tier that its own
          // text color would have made illegible.
          for (const [key, alpha] of Object.entries(FG_TIER_ALPHA)) {
            const stated = colors[key];
            if (stated) {
              check(key, stated, TEXT_FLOOR, ' (stated)');
              continue;
            }
            if (!primary) continue;
            // Only the body tier carries a floor when derived: subtle and hint
            // are de-emphasis roles whose whole job is to recede, and the
            // default theme's own values sit below 4.5:1 by design.
            if (key !== 'fg-muted') continue;
            check(key, compositeOver(primary, ground, alpha), SUPPORTING_TEXT_FLOOR, ' (derived)');
          }
        }
      }),
    ).toEqual([]);
  });

  it('keeps the markdown prose roles readable on the ground each one paints on', () => {
    // The md-* roles color the transcript the reader actually reads, so a
    // stated value is measured on the ground the app.css rule really puts
    // under it: surface-0 for the five prose roles (the user bubble's faint
    // accent tint over the same ground moves these by well under half a
    // step — checked, worst case ~10%), and the SAME VARIANT's
    // `code-inline-bg` for md-inline-code, which never renders on the page
    // ground at all. That pairing is guaranteed by construction — the role
    // lives in the `code` section beside its ground, so one theme supplies
    // both. Floors follow what each role IS: headings get 3:1 — NOT a WCAG
    // large-text claim (chat headings are 15-18px, under the threshold) but
    // a deliberate palette-fidelity concession for the heaviest, most
    // isolated text on the surface, the one place a canonical hue like
    // solarized's orange (3.3:1) is kept over a darkened invention; bold,
    // links and inline code are body-size but carry a second cue (weight,
    // underline, the chip ground), so they sit at 4:1 rather than the
    // plain-body 4.5:1; quotes and markers are de-emphasis roles at the
    // muted floor. A variant whose palette cannot clear a floor restates
    // its own text tier instead — see catppuccin latte and solarized
    // light — which is why every check here is on STATED values only.
    // High Contrast is held to its text floor on all of them, same promise
    // as everywhere else.
    const HEADING_FLOOR = 3;
    const CUED_TEXT_FLOOR = 4;
    const MD_FLOORS: Readonly<Record<string, number>> = {
      'md-heading': HEADING_FLOOR,
      'md-bold': CUED_TEXT_FLOOR,
      'md-link': CUED_TEXT_FLOOR,
      'md-blockquote': SUPPORTING_TEXT_FLOOR,
      'md-marker': SUPPORTING_TEXT_FLOOR,
    };
    // Keyed against the registry so a rename cannot silently retire a floor.
    for (const key of Object.keys(MD_FLOORS)) {
      expect(tokenKeysInSection('colors').has(key), `${key} is not a colors token`).toBe(true);
    }
    expect(tokenKeysInSection('code').has('md-inline-code')).toBe(true);
    expect(
      violations((report) => {
        for (const { themeId, variantName, variant } of CASES) {
          const highContrast = themeId === 'high-contrast';
          const colors = variant.colors;
          const ground = colors?.['surface-0'];
          if (colors && ground) {
            for (const [key, floor] of Object.entries(MD_FLOORS)) {
              const value = colors[key];
              if (!value) continue;
              const held = highContrast ? HIGH_CONTRAST_TEXT_FLOOR : floor;
              const ratio = contrastRatio(value, ground);
              if (ratio < held) {
                report(
                  `${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1, floor ${held}:1`,
                );
              }
            }
          }
          const chipText = variant.code?.['md-inline-code'];
          const chipGround = variant.code?.['code-inline-bg'];
          if (chipText && chipGround) {
            const held = highContrast ? HIGH_CONTRAST_TEXT_FLOOR : CUED_TEXT_FLOOR;
            const ratio = contrastRatio(chipText, chipGround);
            if (ratio < held) {
              report(
                `${themeId}.${variantName}.md-inline-code: ${chipText} on chip ${chipGround} is ${round(ratio)}:1, floor ${held}:1`,
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

          const highContrast = themeId === 'high-contrast';
          const borderFloor = highContrast ? HIGH_CONTRAST_BORDER_FLOOR : BORDER_FLOOR;

          // Every UI theme states a `border`; a palette that left the hairline
          // to the cascade would draw the default theme's line on its own
          // ground, which is the one combination nobody chose.
          if (!colors.border) report(`${themeId}.${variantName}: no border color`);
          for (const key of ['border-subtle', 'border', 'border-strong']) {
            const value = colors[key];
            if (!value) continue;
            const ratio = contrastRatio(value, ground);
            if (ratio < borderFloor) {
              report(
                `${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1, floor ${borderFloor}:1`,
              );
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

          // Status colors and the accent, read on the ground.
          const statusFloor = highContrast ? TEXT_FLOOR : STATUS_FLOOR;
          for (const key of ['info', 'success', 'error', 'warning', 'accent']) {
            const value = colors[key];
            if (!value) continue;
            const ratio = contrastRatio(value, ground);
            if (ratio < statusFloor) {
              report(
                `${themeId}.${variantName}.${key}: ${value} on ${ground} is ${round(ratio)}:1, floor ${statusFloor}:1`,
              );
            }
          }
        }
      }),
    ).toEqual([]);
  });

  it('builds a separated, monotonic elevation ladder', () => {
    // Two properties, and they fail differently. MONOTONIC: every step goes the
    // same way, so "higher tier" means one thing throughout a theme — a ladder
    // that lightens then darkens turns depth into noise. SEPARATED: adjacent
    // tiers are told apart, measured as a contrast RATIO rather than a raw
    // luminance delta, because at the near-black end of a dark palette real
    // steps are tiny in absolute luminance and enormous perceptually.
    expect(
      violations((report) => {
        for (const { themeId, variantName, variant } of CASES) {
          const colors = variant.colors;
          if (!colors) continue;
          const tiers = SURFACE_LADDER.map((key) => colors[key]);
          if (tiers.some((value) => !value)) {
            report(`${themeId}.${variantName}: incomplete surface ladder`);
            continue;
          }
          const luminances = tiers.map((value) => relativeLuminance(value!));
          const rising = luminances[luminances.length - 1]! > luminances[0]!;
          for (let i = 1; i < tiers.length; i += 1) {
            const from = SURFACE_LADDER[i - 1];
            const to = SURFACE_LADDER[i];
            if (rising !== luminances[i]! > luminances[i - 1]!) {
              report(
                `${themeId}.${variantName}: ${from} → ${to} reverses the ladder (${tiers[i - 1]} → ${tiers[i]})`,
              );
              continue;
            }
            const step = contrastRatio(tiers[i - 1]!, tiers[i]!);
            if (step < LADDER_STEP_FLOOR) {
              report(
                `${themeId}.${variantName}: ${from} and ${to} are indistinguishable (${tiers[i - 1]} vs ${tiers[i]}, ${round(step)}:1)`,
              );
            }
          }
        }
      }),
    ).toEqual([]);
  });
});
