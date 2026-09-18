// Pins what the CSS pipeline EMITS for app.css, not what the source says.
// Production builds run app.css through Tailwind and then Lightning CSS, and
// Lightning CSS folds constant border longhands back into the `border`
// shorthand. The shorthand's border-image reset costs Blink a
// NinePieceImageData copy per element style (app.css header, the tailwindcss
// preflight patch), so the broad rules must reach the browser as longhands.
// The same pass must leave spacing utilities as literal lengths rather than
// `var(--spacing)` (app.css `@theme inline`) and keep the icon rule free of
// a per-element custom property (app.css mask-icon rule).
import { readFileSync } from 'node:fs';
import path from 'node:path';
import { compile, optimize } from '@tailwindcss/node';
import { describe, expect, it } from 'vitest';

const srcDir = path.resolve(import.meta.dirname, '..');
const appCssPath = path.join(srcDir, 'app.css');

interface LeafRule {
  selector: string;
  body: string;
}

/**
 * Leaf `selector{declarations}` blocks of minified CSS, descending into
 * at-rule blocks. Quoted strings are skipped so braces and semicolons inside
 * them cannot split a block; a block that mixes declarations with nested
 * blocks is not something the optimizer emits and is not handled.
 */
function leafRules(css: string): LeafRule[] {
  const out: LeafRule[] = [];
  const skipString = (from: number): number => {
    const quote = css[from];
    for (let i = from + 1; i < css.length; i++) {
      if (css[i] === '\\') i++;
      else if (css[i] === quote) return i;
    }
    throw new Error(`unterminated string at ${from}`);
  };
  const closingBrace = (open: number): number => {
    let depth = 0;
    for (let i = open; i < css.length; i++) {
      const c = css[i];
      if (c === '"' || c === "'") i = skipString(i);
      else if (c === '{') depth++;
      else if (c === '}' && --depth === 0) return i;
    }
    throw new Error(`unbalanced CSS at ${open}`);
  };
  const walk = (from: number, to: number): void => {
    let pos = from;
    let preludeStart = from;
    while (pos < to) {
      const c = css[pos];
      if (c === '"' || c === "'") {
        pos = skipString(pos) + 1;
      } else if (c === '{') {
        const close = closingBrace(pos);
        const selector = css.slice(preludeStart, pos).trim();
        const body = css.slice(pos + 1, close);
        if (body.includes('{')) walk(pos + 1, close);
        else out.push({ selector, body });
        pos = close + 1;
        preludeStart = pos;
      } else if (c === ';') {
        pos++;
        preludeStart = pos;
      } else {
        pos++;
      }
    }
  };
  walk(0, css.length);
  return out;
}

async function emittedCss(candidates: string[]): Promise<string> {
  const compiler = await compile(readFileSync(appCssPath, 'utf8'), {
    base: srcDir,
    from: appCssPath,
    onDependency: () => {},
  });
  return optimize(compiler.build(candidates), { file: appCssPath, minify: true }).code;
}

const BORDER_SHORTHAND = /(^|;)border:/;
const BORDER_LONGHAND = /(^|;)border-(width|style|color):/;
/** Lightning CSS splits the preflight selector list into rules headed by these. */
const PREFLIGHT_HEADS = ["*", "::file-selector-button"];

/**
 * The only rules allowed to keep the `border` shorthand: each matches a
 * handful of elements, and a constant color cannot be kept as a longhand
 * because Lightning CSS folds it back. Adding a selector here needs the same
 * justification.
 */
const SHORTHAND_ALLOWLIST = new Set([
  '::-webkit-scrollbar-thumb',
  '.status-glow-warning:before,.status-glow-info:before',
]);

describe('app.css as emitted by the build pipeline', () => {
  it('resets borders on every element without the border shorthand', async () => {
    const all = leafRules(await emittedCss([]));
    for (const head of PREFLIGHT_HEADS) {
      const rules = all.filter((r) => r.selector.split(',').includes(head));
      expect(rules.length).toBeGreaterThan(0);
      for (const rule of rules) expect(rule.body).not.toMatch(BORDER_SHORTHAND);
      const bodies = rules.map((r) => r.body);
      expect(bodies.some((b) => b.includes('border-width:0'))).toBe(true);
      expect(bodies.some((b) => b.includes('border-style:solid'))).toBe(true);
      expect(bodies.some((b) => /border-color:currentcolor/i.test(b))).toBe(true);
    }
  });

  it('keeps markdown borders as longhands', async () => {
    const rules = leafRules(await emittedCss([])).filter(
      (r) => r.selector.includes('.markdown-body') && BORDER_LONGHAND.test(r.body),
    );
    expect(rules.some((r) => r.body.includes('border-style:solid'))).toBe(true);
    for (const rule of rules) expect(rule.body).not.toMatch(BORDER_SHORTHAND);
  });

  it('keeps the border shorthand only on the allowlisted rare selectors', async () => {
    const shorthand = leafRules(await emittedCss([]))
      .filter((r) => BORDER_SHORTHAND.test(r.body))
      .map((r) => r.selector);
    expect(new Set(shorthand)).toEqual(SHORTHAND_ALLOWLIST);
  });

  it('compiles spacing utilities to literal lengths', async () => {
    const css = await emittedCss(['p-4', 'gap-2', 'mt-8', 'w-3.5', '-mx-1']);
    expect(css).toMatch(/\.p-4\{[^}]*padding:1rem/);
    expect(css).not.toContain('var(--spacing');
  });

  it('gives icon spans shared mask constants and no custom property to resolve', async () => {
    const rules = leafRules(await emittedCss([])).filter((r) => r.selector === '.lucide-icon,.mask-icon');
    expect(rules.length).toBeGreaterThan(0);
    const body = rules.map((r) => r.body).join(';');
    expect(body).toContain('mask-size:contain');
    expect(body).not.toContain('var(--mask-icon)');
    expect(body).not.toMatch(/(^|;)(-webkit-)?mask:/);
  });
});
