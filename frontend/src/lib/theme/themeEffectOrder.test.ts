// Pins the theme effect ORDER in `App.svelte`, which until now was enforced by
// a comment.
//
// The rule (docs/specs/theme-system.md §9.1, and this package's own headers):
// the mode-class stamp and the style rewrite are BOTH `$effect.pre`, in that
// order, and everything that READS the applied cascade back is a plain
// `$effect` after them. Svelte flushes every render effect in the tree before
// any user effect, so moving either half across that boundary opens a window —
// the whole render pass — in which `<html>`'s class says one mode and the
// user-theme element still holds the other's resolution. The resolved MODE is
// a resolver input, so that is not a theoretical mismatch: a UI theme with only
// a dark variant is emitted only in dark mode, and a consumer resolving colors
// off the cascade from a pre-effect samples the disagreement.
//
// Every symptom of getting this wrong is a one-frame flash under a mode change
// that nobody is looking at while the tests run. A source grep is the cheapest
// thing that fails instead — the same reason `themeBootStamp.test.ts` exists.

import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

const app = readFileSync(
  resolve(dirname(fileURLToPath(import.meta.url)), '../../App.svelte'),
  'utf8',
);

interface EffectOpener {
  readonly kind: 'pre' | 'user';
  readonly at: number;
}

/** Every `$effect(` / `$effect.pre(` in source order. */
function openers(source: string): EffectOpener[] {
  const found: EffectOpener[] = [];
  const pattern = /\$effect(\.pre)?\(/g;
  for (let match = pattern.exec(source); match; match = pattern.exec(source)) {
    found.push({ kind: match[1] ? 'pre' : 'user', at: match.index });
  }
  return found;
}

/** Which effect a call site sits inside: the nearest opener above it. */
function enclosing(source: string, needle: string): EffectOpener {
  const at = source.indexOf(needle);
  expect(at, `${needle} is not in App.svelte`).toBeGreaterThan(-1);
  const above = openers(source).filter((opener) => opener.at < at);
  const nearest = above.at(-1);
  expect(nearest, `${needle} is not inside any effect`).toBeDefined();
  return nearest!;
}

describe('App.svelte theme effect ordering', () => {
  it('stamps the mode class from an $effect.pre', () => {
    expect(enclosing(app, 'applyThemeClass(').kind).toBe('pre');
  });

  it('rewrites the style element from an $effect.pre', () => {
    expect(enclosing(app, 'applyTheme(').kind).toBe('pre');
  });

  it('stamps the class BEFORE the applier, in source order', () => {
    // Both are render effects, so they run in declaration order; reversed, the
    // rewrite would resolve against the previous mode's class.
    expect(app.indexOf('applyThemeClass(')).toBeLessThan(app.indexOf('applyTheme('));
  });

  it('reads the cascade back from a plain $effect, after both', () => {
    // The ground probe forces a style recalc and `syncWindowBackground` writes
    // store state — neither belongs in the render pass, and neither can run
    // before the rewrite above has landed.
    expect(enclosing(app, 'readWindowGroundHex()').kind).toBe('user');
    expect(enclosing(app, 'stampBootTheme(').kind).toBe('user');
    expect(enclosing(app, 'syncWindowBackground(').kind).toBe('user');
    expect(app.indexOf('applyTheme(')).toBeLessThan(app.indexOf('readWindowGroundHex()'));
  });

  it('gates both halves on the appearance load having settled', () => {
    // Otherwise the mount-time resolution — `themes: []`, so a selected USER
    // theme falls back — overwrites the boot script's cached CSS and re-stamps
    // the fallback for the next launch.
    expect(app).toContain('isAppearanceLoaded()');
    const applyAt = app.indexOf('applyTheme(');
    expect(app.indexOf('isAppearanceLoaded()', applyAt)).toBeGreaterThan(applyAt);
    const stampAt = app.indexOf('stampBootTheme(');
    expect(app.lastIndexOf('isAppearanceLoaded()', stampAt)).toBeGreaterThan(applyAt);
  });
});
