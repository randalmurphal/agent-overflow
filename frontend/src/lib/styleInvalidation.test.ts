// Style-invalidation tripwire for the global stylesheets.
//
// Blink builds invalidation sets from selector FEATURES (classes, ids, tags,
// attributes). A compound that has none, such as a bare structural pseudo like
// `:last-child` or `*` on the right side of a sibling combinator, lands in
// the UNIVERSAL sets, and from then on every matching DOM mutation anywhere
// in the document schedules that rule's invalidation, with
// `allDescendantsMightBeInvalid` when the rightmost compound is featureless.
// Relational `:has()` and tag-keyed sibling compounds can still make every old
// element of that tag a candidate. Global styles therefore require narrow
// class or attribute keys for those shapes too.
//
// Measured 2026-08-25 during two-pane streaming (soak rig, frames probe +
// invalidationTracking): `.markdown-body > :first-child > :first-child` and
// `.run-map-spine > * + *::before` produced 175 whole-subtree invalidations
// per 15s — 1,091-element document-wide style recalc passes every streaming
// beat, firing on sidebar nodes that have nothing to do with markdown, and
// the run-map rule taxed the app with its overlay CLOSED. Both were
// rewritten to carry narrow classes in every compound (`.md-blk`,
// `.run-map-node`, and parser-owned Markdown markers). This test keeps the
// shape from coming back.
//
// Scope: the global stylesheets only. Svelte component styles are scoped
// with a generated class per compound, so they cannot produce featureless
// compounds.
//
// The check is a heuristic over compound TEXT, not a CSS parser: it flags a
// structural pseudo or universal star only when its compound carries no
// class/id/tag/attribute alongside it. `:is(...)` / `:where(...)` /
// `:not(...)` count as featured only when their arguments contain a feature.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import { SRC_ROOT } from '../test/sourceScan';

const SHEETS = ['app.css', 'styles/tokens.css', 'styles/syntax.css'];

const STRUCTURAL_PSEUDO =
  /:(first-child|last-child|only-child|first-of-type|last-of-type|only-of-type|nth-[a-z-]+(\([^)]*\))?|empty)/;

/** Strip comments and extract every selector prelude (text before `{`),
 *  skipping at-rule preludes and declaration blocks. */
function selectorPreludes(css: string): string[] {
  const noComments = css.replace(/\/\*[\s\S]*?\*\//g, ' ');
  const preludes: string[] = [];
  let buf = '';
  let depth = 0;
  let inBlock = false;
  for (const ch of noComments) {
    if (ch === '{') {
      const text = buf.trim();
      if (text.startsWith('@')) {
        // At-rule opening a nested scope (@media, @layer, @supports):
        // selectors inside still get collected on their own braces.
        depth++;
      } else if (text.length > 0) {
        preludes.push(text);
        inBlock = true;
      }
      buf = '';
      continue;
    }
    if (ch === '}') {
      if (inBlock) inBlock = false;
      else if (depth > 0) depth--;
      buf = '';
      continue;
    }
    if (!inBlock) buf += ch;
  }
  return preludes;
}

/** Split one prelude into selectors, each into compounds (on combinators). */
function compoundsOf(selector: string): string[] {
  return selector
    .split(',')
    .flatMap((sel) => sel.split(/\s+|(?=[>+~])/g))
    .map((part) => part.replace(/^[>+~]/, '').trim())
    .filter((part) => part.length > 0);
}

/** Whether a compound carries at least one real feature. Functional
 *  pseudos contribute their arguments' features. Pseudo-elements
 *  (`::before`) are not features — they ride whatever the compound has. */
function hasFeature(compound: string): boolean {
  // Class, id, or attribute selector anywhere in the compound.
  if (/[.#[]/.test(compound)) return true;
  // Features inside :is()/:where()/:not()/:has() arguments already
  // matched above via class/attr; a tag inside them matches here too.
  const inner = compound.match(/:(?:is|where|not|has)\(([^)]*)\)/);
  if (inner && /[a-zA-Z]/.test(inner[1])) return true;
  // Leading type selector (a tag name), e.g. `li:first-child`, `tr:nth-child(even)`.
  return /^[a-zA-Z][a-zA-Z0-9-]*/.test(compound);
}

describe('global stylesheets avoid universal invalidation sets', () => {
  for (const sheet of SHEETS) {
    it(`${sheet} has no featureless structural-pseudo or universal-sibling compound`, () => {
      const css = readFileSync(join(SRC_ROOT, sheet), 'utf8');
      const offenders: string[] = [];
      for (const prelude of selectorPreludes(css)) {
        for (const sel of prelude.split(',')) {
          const trimmed = sel.trim();
          if (trimmed.length === 0) continue;
          // Blink's relational invalidation is descendant-wide even when the
          // :has() compound carries a tag/class feature. A task-list selector
          // matching `li:has(> input)` made every code-island retirement
          // restyle every prior list item in every visible Markdown pane.
          if (trimmed.includes(':has(')) {
            offenders.push(`${sheet}: "${trimmed}" — relational :has()`);
          }
          // (a) structural pseudo in a featureless compound
          for (const compound of compoundsOf(trimmed)) {
            if (STRUCTURAL_PSEUDO.test(compound) && !hasFeature(compound)) {
              offenders.push(`${sheet}: "${trimmed}" — featureless compound "${compound}"`);
            }
          }
          // (b) universal compound on the right of a sibling combinator
          if (/[+~]\s*\*/.test(trimmed)) {
            offenders.push(`${sheet}: "${trimmed}" — universal sibling`);
          }
          // A tag is a feature, but still a document-wide one. Blink matched
          // every Markdown `li` for a workflow-only `li + li` rule and every
          // Markdown `p` for `p + p` during completed code retirement.
          if (/[+~]\s*[a-zA-Z][a-zA-Z0-9-]*/.test(trimmed)) {
            offenders.push(`${sheet}: "${trimmed}" — tag-keyed sibling`);
          }
        }
      }
      expect(offenders).toEqual([]);
    });
  }

  it('uses explicit Markdown edge markers instead of structural pseudos', () => {
    const css = readFileSync(join(SRC_ROOT, 'app.css'), 'utf8');
    const selectors = selectorPreludes(css).join('\n');
    expect(selectors).not.toMatch(/\.md-blk:(?:first|last)-child/);
    expect(selectors).not.toContain('> p:first-child');
    expect(selectors).not.toContain('li:has(> input[type="checkbox"])');
    expect(selectors).not.toContain('.markdown-body p + p');
    expect(selectors).not.toContain('.run-map-spine > li + li');
    expect(css).toContain('.sd-trim-first-block');
    expect(css).toContain('.sd-trim-last-block');
    expect(css).toContain('> p.sd-first-block');
    expect(css).toContain('li.md-task-list-item');
    expect(css).toContain('.sd-paragraph-gap');
    expect(css).toContain('.run-map-node + .run-map-node');
  });
});
