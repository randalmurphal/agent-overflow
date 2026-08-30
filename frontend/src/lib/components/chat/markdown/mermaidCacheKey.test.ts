import { readFileSync } from 'node:fs';
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';

// Regression for DIVERGENCE.md entry 18: the vendored mermaid SVG cache
// keys on the PALETTE, not on `mermaidConfig.theme`.
//
// The app pins `theme: 'base'` permanently (it is the only mermaid theme
// that derives its colors from `themeVariables`), so a theme-only key
// collapses light and dark onto one `base:<source>` entry — the first
// diagram rendered after boot would be served back on every theme flip,
// under a remount whose entire purpose is repainting it.
//
// This runs the vendored expression itself rather than grepping for it:
// the source is lifted out and evaluated, so the assertions below are
// about behavior. If the extraction stops matching, the vendored hunk was
// reformatted or removed — re-read it against the ledger entry before
// touching this file.
const MERMAID_SOURCE = readFileSync(
  // Resolved from this file, not from the runner's cwd: a cwd-relative
  // path silently reads nothing (or the wrong tree) the moment vitest is
  // invoked from anywhere but `frontend/`.
  resolve(
    dirname(fileURLToPath(import.meta.url)),
    '../../../markdown/render/elements/Mermaid.svelte',
  ),
  'utf8',
);

function extractPaletteKeyBuilder(): (vars: unknown) => string {
  const match = MERMAID_SOURCE.match(
    /const paletteKey =([\s\S]*?);\s*\n\s*const cacheKey/,
  );
  if (!match) {
    throw new Error(
      'vendored Mermaid.svelte no longer builds a `paletteKey` before `cacheKey` — see DIVERGENCE.md entry 18',
    );
  }
  return new Function('themeVariables', `return (${match[1]});`) as (
    vars: unknown,
  ) => string;
}

describe('vendored mermaid SVG cache key', () => {
  const buildPaletteKey = extractPaletteKeyBuilder();

  it('separates two palettes that share a theme name', () => {
    const light = buildPaletteKey({ background: 'rgb(250, 250, 251)', darkMode: false });
    const dark = buildPaletteKey({ background: 'rgb(16, 16, 23)', darkMode: true });
    expect(light).not.toBe(dark);
    expect(light).not.toBe('');
  });

  it('is stable under key order, so an unchanged palette still hits', () => {
    expect(buildPaletteKey({ a: '1', b: '2' })).toBe(buildPaletteKey({ b: '2', a: '1' }));
  });

  it('does not collide when a value contains the delimiters', () => {
    // The reason the key is stringified rather than joined. `fontFamily`
    // is a comma-separated font stack and colors are `rgb(a, b, c)`, so a
    // `k=v` + `,` join is ambiguous by construction: both of these
    // serialized to `a=x,b=y` and served each other's SVG.
    expect(buildPaletteKey({ a: 'x,b=y' })).not.toBe(buildPaletteKey({ a: 'x', b: 'y' }));
    // Not hypothetical: `fontFamily` really is a comma-separated font
    // stack, and colors really are `rgb(a, b, c)`.
    expect(
      buildPaletteKey({ fontFamily: 'Geist Sans, ui-sans-serif', textColor: 'rgb(1, 2, 3)' }),
    ).not.toBe(buildPaletteKey({ fontFamily: 'Geist Sans', textColor: 'rgb(1, 2, 3)' }));
  });

  it('degrades to upstream behavior when no variables are passed', () => {
    expect(buildPaletteKey(undefined)).toBe('');
  });

  it('folds the palette into the cache key alongside theme and source', () => {
    const cacheKeyLine = MERMAID_SOURCE.match(/const cacheKey = `([^`]*)`/);
    expect(cacheKeyLine).not.toBeNull();
    const template = cacheKeyLine![1];
    expect(template).toContain('${themeKey}');
    expect(template).toContain('${paletteKey}');
    expect(template).toContain('${sanitizedCode}');
  });
});
