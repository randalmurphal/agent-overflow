// Registry ↔ stylesheet drift, in BOTH directions.
//
// The registry is the theme system's vocabulary and the stylesheets are its
// implementation, and neither is allowed to move without the other. A token
// added to app.css with no registry entry is a color a theme file cannot
// reach — invisible, because everything still compiles and renders. A
// registry entry with no declaration is a token a theme file CAN name and
// that paints nothing — equally invisible, and worse, because the schema
// advertises it.
//
// The CSS is parsed by the same `parseCssBlocks` the reference generator
// uses, so the doc and this test cannot disagree about what the stylesheets
// say. The block-matching follows `themeTokens.test.ts`'s approach for
// `@theme`: strip comments, brace-match, read declarations — generalized to
// every top-level rule rather than one named at-rule.

import { describe, expect, it } from 'vitest';
import {
  EXCLUDED_VAR_NAMES,
  EXCLUDED_VAR_PREFIXES,
  SECTION_AXIS,
  THEME_SECTIONS,
  TOKEN_REGISTRY,
  cssVarName,
  isExcludedVar,
  isSharedDefaultToken,
  tokensInSection,
} from './tokenRegistry';
import { isSafeTokenKey } from './themeResolve';
import { readCssVars } from '../../../scripts/generate-theme-reference.mjs';

/** `{ 'app.css': { root: Map, light: Map }, … }`. */
const CSS = readCssVars() as Record<string, { root: Map<string, string>; light: Map<string, string> }>;

/** Every `--*` declared in a scanned block, with where it was found. */
function declaredVars(): Map<string, string[]> {
  const found = new Map<string, string[]>();
  for (const [file, blocks] of Object.entries(CSS)) {
    for (const [block, vars] of Object.entries(blocks)) {
      for (const name of vars.keys()) {
        const where = `${file} ${block === 'root' ? ':root' : 'html.light'}`;
        found.set(name, [...(found.get(name) ?? []), where]);
      }
    }
  }
  return found;
}

const DECLARED = declaredVars();
const REGISTRY_VARS = new Set(TOKEN_REGISTRY.map((token) => cssVarName(token.key)));

describe('token registry', () => {
  it('read stylesheets that actually contain a palette', () => {
    // A parser that silently matched nothing would make every rule below pass
    // vacuously, which is the one failure mode a drift test cannot survive.
    expect(DECLARED.size).toBeGreaterThan(80);
    expect(CSS['app.css']!.light.size).toBeGreaterThan(20);
    expect(CSS['tokens.css']!.light.size).toBe(0);
  });

  it('declares every registry token in the file the registry names', () => {
    const missing: string[] = [];
    for (const token of TOKEN_REGISTRY) {
      const file = CSS[token.cssFile];
      if (!file || !file.root.has(cssVarName(token.key))) {
        missing.push(`${cssVarName(token.key)} (registry says ${token.cssFile} :root)`);
      }
    }
    expect(
      missing,
      'Registry tokens with no declaration. A theme file can name these and they paint nothing — declare them or delete the entry.',
    ).toEqual([]);
  });

  it('knows every color variable the stylesheets declare', () => {
    const unknown: string[] = [];
    for (const [name, where] of DECLARED) {
      if (REGISTRY_VARS.has(name)) continue;
      if (isExcludedVar(name)) continue;
      unknown.push(`${name} (${where.join(', ')})`);
    }
    expect(
      unknown,
      'Variables the theme registry does not know. Add a registry entry in src/lib/theme/tokenRegistry.ts so theme files can reach the token, or — if it is not a color — add it to EXCLUDED_VAR_NAMES / EXCLUDED_VAR_PREFIXES with the reason.',
    ).toEqual([]);
  });

  it('keeps the by-name exclusion list shrink-only', () => {
    // Prefix exclusions cover families that live in `@theme` and other blocks
    // this test does not scan, so they are documentation. The by-NAME list is
    // narrow enough to hold to the stricter rule: an entry that no longer
    // offends is a stale exception that would grandfather the next one.
    const stale = Object.keys(EXCLUDED_VAR_NAMES).filter((name) => !DECLARED.has(name));
    expect(stale, 'Exclusions that no longer match anything — delete them.').toEqual([]);
    for (const name of Object.keys(EXCLUDED_VAR_NAMES)) {
      expect(REGISTRY_VARS.has(name), `${name} is both excluded and registered`).toBe(false);
    }
    for (const prefix of Object.keys(EXCLUDED_VAR_PREFIXES)) {
      expect(prefix.startsWith('--') && prefix.endsWith('-')).toBe(true);
    }
  });

  it('holds the section counts the format documents', () => {
    // Pinned so a deletion is loud: the schema, TOKENS.md and every theme file
    // on disk are written against these key spaces.
    expect(tokensInSection('colors')).toHaveLength(46);
    expect(tokensInSection('syntax')).toHaveLength(21);
    expect(tokensInSection('ansi')).toHaveLength(16);
    expect(tokensInSection('code')).toHaveLength(2);
    expect(TOKEN_REGISTRY).toHaveLength(85);
  });

  it('keeps entries well-formed', () => {
    const keys = new Set<string>();
    for (const token of TOKEN_REGISTRY) {
      expect(keys.has(token.key), `duplicate registry key ${token.key}`).toBe(false);
      keys.add(token.key);
      // The key becomes a CSS property name, so its shape is a safety
      // property, not a style preference.
      expect(isSafeTokenKey(token.key), `${token.key} is not a safe custom-property name`).toBe(true);
      expect(THEME_SECTIONS).toContain(token.section);
      expect(token.axis).toBe(SECTION_AXIS[token.section]);
      expect(token.description.length, `${token.key} has no description`).toBeGreaterThan(10);
      expect(token.description.endsWith('.'), `${token.key}'s description is not a sentence`).toBe(
        true,
      );
    }
  });

  it('marks exactly the tokens with no light-mode escape as mode-invariant', () => {
    // Read off the SAME stylesheet parse the rules above use, never trusted as
    // a hand-set flag: `modeInvariant` is a claim that `:root` has nothing in
    // `html.light` to out-cascade it, which is a fact about the CSS. A derived
    // token is excluded even with no light declaration — its DEFAULT
    // re-derives from a base that does carry the mode. That protects only the
    // default: a theme's stated literal replaces the derivation and strands
    // exactly like a mode-invariant token, which is why
    // `warnModeInvariantSplits` / `isSharedDefaultToken` treat every
    // tokens.css-declared role as shared (see the test below).
    const expected: string[] = [];
    for (const token of TOKEN_REGISTRY) {
      const file = CSS[token.cssFile]!;
      const stranded = !token.derived && !file.light.has(cssVarName(token.key));
      if (stranded) expected.push(token.key);
      expect(
        token.modeInvariant,
        `${token.key} is marked modeInvariant=${token.modeInvariant} but ${stranded ? 'has no' : 'has a'} html.light declaration in ${token.cssFile}`,
      ).toBe(stranded);
    }
    // Pinned so that adding a light declaration for one of these — or adding a
    // new stranded token — is a decision someone makes on purpose.
    expect(expected).toEqual([
      'accent-fg',
      'provider-claude',
      'scrim',
      'scrim-fg',
      'design-paper',
    ]);
  });

  it('keeps isSharedDefaultToken aligned with what the stylesheets strand', () => {
    // `isSharedDefaultToken` proxies "a stated :root value has nothing in
    // html.light to out-cascade it" through `modeInvariant || cssFile ===
    // 'tokens.css'`. The proxy is sound only while the CSS cooperates —
    // tokens.css's light block stays empty (pinned above) AND every non-shared
    // token really does carry an html.light declaration. This derives the fact
    // from the same parse, so a derived app.css token added :root-only (the
    // one shape neither flag would catch) fails here instead of stranding
    // silently in light mode.
    for (const token of TOKEN_REGISTRY) {
      const stranded = !CSS[token.cssFile]!.light.has(cssVarName(token.key));
      expect(
        isSharedDefaultToken(token),
        `${token.key}: ${stranded ? 'no' : 'has an'} html.light declaration in ${token.cssFile}, but isSharedDefaultToken says ${isSharedDefaultToken(token)}`,
      ).toBe(stranded);
    }
  });

  it('marks exactly the tokens whose default is an expression as derived', () => {
    // Derivedness is a claim about the DEFAULT VALUE, so read it off the
    // stylesheet rather than trusting the flag: a token defaulting to
    // `var()` / `color-mix()` follows its base, a literal does not.
    for (const token of TOKEN_REGISTRY) {
      const value = CSS[token.cssFile]!.root.get(cssVarName(token.key))!;
      const expression = value.includes('var(') || value.includes('color-mix(');
      expect(token.derived, `${token.key} is marked derived=${token.derived} but defaults to ${value}`).toBe(
        expression,
      );
    }
  });
});
