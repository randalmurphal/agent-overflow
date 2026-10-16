// The discarded-row dim, derived from every built-in UI theme in each mode
// it ships, through the real cascade: app.css, the theme's style element,
// the canvas probe and the CSS that consumes the result. Real Chromium,
// because the probe needs a canvas and the default palette is oklch.
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import '../../app.css';
import { applyThemeClass } from '../utils/theme';
import { BUILTIN_THEMES } from './builtins';
import {
  DISCARDED_DIM_STYLE_ID,
  DISCARDED_TEXT_FLOOR,
  REASONING_TEXT_ALPHA,
  applyDiscardedDim,
  dimmedContrast,
  readDiscardedDimInputs,
} from './discardedDim';
import { USER_THEME_STYLE_ID, applyTheme, resetThemeApplyForTest } from './themeApply.svelte';
import type { ResolvedMode } from './themeResolve';

let priorClass = '';

beforeEach(() => {
  priorClass = document.documentElement.className;
  resetThemeApplyForTest();
});

afterEach(() => {
  document.documentElement.className = priorClass;
  document.getElementById(USER_THEME_STYLE_ID)?.remove();
  document.getElementById(DISCARDED_DIM_STYLE_ID)?.remove();
  resetThemeApplyForTest();
});

const CASES: { themeId: string; mode: ResolvedMode }[] = BUILTIN_THEMES.filter((theme) => theme.axes.ui).flatMap(
  (theme) => {
    const modes = (['dark', 'light'] as const).filter((mode) => theme.id === 'default' || theme.variants[mode]);
    return modes.map((mode) => ({ themeId: theme.id, mode }));
  },
);

function applyBuiltin(themeId: string, mode: ResolvedMode): void {
  applyThemeClass(mode);
  applyTheme({ mode, appearance: { uiTheme: themeId, codeTheme: 'github' }, themes: [], revision: 1 });
}

function computedOpacity(className: string): string {
  const el = document.createElement('div');
  el.className = className;
  document.body.appendChild(el);
  try {
    return getComputedStyle(el).opacity;
  } finally {
    el.remove();
  }
}

describe('discarded-row dim per built-in theme', () => {
  it('covers every built-in UI theme in each mode it ships', () => {
    expect(CASES.length).toBe(15);
    expect(CASES).toContainEqual({ themeId: 'gruvbox', mode: 'dark' });
    expect(CASES).toContainEqual({ themeId: 'default', mode: 'light' });
  });

  it.each(CASES)('$themeId $mode keeps both row kinds at the floor, or undimmed', ({ themeId, mode }) => {
    applyBuiltin(themeId, mode);
    const dim = applyDiscardedDim();
    const inputs = readDiscardedDimInputs();
    expect(dim, 'the probe answered').toBeDefined();
    expect(inputs).toBeDefined();
    const { fgMuted, ground } = inputs!;
    const kinds = [
      { name: 'text', text: fgMuted, opacity: dim!.text, className: 'discarded-row' },
      { name: 'reasoning', text: { ...fgMuted, a: fgMuted.a * REASONING_TEXT_ALPHA }, opacity: dim!.reasoning, className: 'discarded-reasoning' },
    ];
    for (const kind of kinds) {
      const undimmed = dimmedContrast(kind.text, ground, 1);
      const dimmed = dimmedContrast(kind.text, ground, kind.opacity);
      if (undimmed <= DISCARDED_TEXT_FLOOR) {
        expect(kind.opacity, `${kind.name}: ${undimmed.toFixed(2)}:1 undimmed`).toBe(1);
      } else {
        expect(dimmed, `${kind.name} at ${kind.opacity}`).toBeGreaterThanOrEqual(DISCARDED_TEXT_FLOOR);
        if (kind.opacity < 1) {
          expect(dimmedContrast(kind.text, ground, kind.opacity - 0.01), `${kind.name} is the least dim`).toBeLessThan(
            DISCARDED_TEXT_FLOOR,
          );
        }
      }
      // The CSS consumes what was solved.
      expect(Number(computedOpacity(kind.className))).toBeCloseTo(kind.opacity, 5);
    }
  });

  it('re-derives when the theme or the mode changes', () => {
    applyBuiltin('default', 'dark');
    const defaultDark = applyDiscardedDim();
    applyBuiltin('gruvbox', 'dark');
    const gruvboxDark = applyDiscardedDim();
    applyBuiltin('default', 'light');
    const defaultLight = applyDiscardedDim();
    expect(gruvboxDark).not.toEqual(defaultDark);
    expect(defaultLight).not.toEqual(defaultDark);
    expect(Number(computedOpacity('discarded-row'))).toBeCloseTo(defaultLight!.text, 5);
    // One element, rewritten in place.
    expect(document.querySelectorAll(`#${DISCARDED_DIM_STYLE_ID}`).length).toBe(1);
  });

  it('dims nothing until the derivation has run', () => {
    applyBuiltin('default', 'dark');
    expect(computedOpacity('discarded-row')).toBe('1');
    expect(computedOpacity('discarded-reasoning')).toBe('1');
  });
});
