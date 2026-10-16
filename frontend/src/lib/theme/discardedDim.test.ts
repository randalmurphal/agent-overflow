import { describe, expect, it } from 'vitest';
import tailClampedTextSource from '../components/chat/TailClampedText.svelte?raw';
import {
  DISCARDED_TEXT_FLOOR,
  REASONING_TEXT_ALPHA,
  deriveDiscardedDim,
  dimmedContrast,
  serializeDiscardedDim,
  solveDimOpacity,
} from './discardedDim';

const rgb = (r: number, g: number, b: number, a = 1) => ({ r, g, b, a });

describe('solveDimOpacity', () => {
  it('returns the least hundredth that keeps the floor', () => {
    const cases = [
      // Near-white at 80% on near-black, and near-black at 87% on near-white:
      // the default palette's fg-muted shape in each mode.
      { text: rgb(238, 238, 242, 0.8), ground: rgb(9, 9, 16) },
      { text: rgb(28, 32, 38, 0.87), ground: rgb(248, 250, 253) },
      { text: rgb(255, 255, 255), ground: rgb(0, 0, 0) },
    ];
    for (const { text, ground } of cases) {
      const opacity = solveDimOpacity(text, ground);
      expect(opacity).toBeLessThan(1);
      expect(dimmedContrast(text, ground, opacity)).toBeGreaterThanOrEqual(DISCARDED_TEXT_FLOOR);
      expect(dimmedContrast(text, ground, opacity - 0.01)).toBeLessThan(DISCARDED_TEXT_FLOOR);
    }
  });

  it('does not dim text the theme already paints at or under the floor', () => {
    // #767676 on white is 4.54:1; #777777 is 4.48:1.
    const ground = rgb(255, 255, 255);
    expect(dimmedContrast(rgb(0x77, 0x77, 0x77), ground, 1)).toBeLessThan(DISCARDED_TEXT_FLOOR);
    expect(solveDimOpacity(rgb(0x77, 0x77, 0x77), ground)).toBe(1);
    expect(solveDimOpacity(rgb(0, 0, 0, 0.2), ground)).toBe(1);
    // Just above the floor, the only dim that keeps it is none.
    expect(dimmedContrast(rgb(0x76, 0x76, 0x76), ground, 1)).toBeGreaterThanOrEqual(DISCARDED_TEXT_FLOOR);
    expect(solveDimOpacity(rgb(0x76, 0x76, 0x76), ground)).toBe(1);
  });
});

describe('deriveDiscardedDim', () => {
  it('solves reasoning against fg-muted at the reasoning alpha', () => {
    const fgMuted = rgb(238, 238, 242, 0.8);
    const ground = rgb(9, 9, 16);
    const dim = deriveDiscardedDim(fgMuted, ground);
    expect(dim.text).toBe(solveDimOpacity(fgMuted, ground));
    expect(dim.reasoning).toBe(solveDimOpacity({ ...fgMuted, a: 0.8 * REASONING_TEXT_ALPHA }, ground));
    // Fainter text takes a gentler dim.
    expect(dim.reasoning).toBeGreaterThan(dim.text);
    expect(serializeDiscardedDim(dim)).toBe(
      `:root{--fade-discarded:${dim.text};--fade-discarded-reasoning:${dim.reasoning}}`,
    );
  });

  it('matches the alpha the reasoning row paints its text at', () => {
    // TailClampedText owns the class; the solver must fade by the same amount.
    expect(tailClampedTextSource).toContain(`text-fg-muted/${Math.round(REASONING_TEXT_ALPHA * 100)} `);
  });
});
