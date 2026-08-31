// Token-conformance tripwire for the theme system.
//
// The app already has a full semantic token vocabulary (`app.css` `:root` +
// `html.light`, `styles/tokens.css`, `styles/syntax.css`); theming is not an
// extraction job, it is keeping the vocabulary total. Phase 1 of
// `docs/architecture/theme-system.md` closed ~35 leak sites where app chrome bypassed
// it — raw Tailwind palette classes, black/white utilities, default Tailwind
// shadows, hex literals. Those leaks are individually invisible in review and
// collectively fatal to a user-editable theme: a user-picked accent or a
// custom palette simply doesn't reach them.
//
// This test reads the tree, so a re-leak fails `pnpm test` instead of
// surviving to the first person who tries a light theme.
//
// The allowlists are SHRINK-ONLY, on the same mechanic as
// `architecture.test.ts` (they share `test/sourceScan.ts`): a new offender
// fails, and so does an allowlist entry that no longer offends. Every entry
// carries the reason the literal is deliberate. An exception that has been
// fixed must be DELETED — a list that grandfathers is a list that stops
// describing the tree.
//
// Phase 2 landed the terminal bridge, and this file is where that showed up:
// `terminal/terminalTheme.ts`'s entry — 44 hand-maintained hex values
// duplicating the ANSI tokens — was DELETED rather than reworded, because the
// duplicate itself is gone (the palette is now resolved through
// `utils/cssColorProbe.ts`). That is what a shrink-only list is for.
//
// NOTE, and it is the same hazard this file polices: Tailwind's source scanner
// reads whole files, comments and string literals included. Spelling a real
// utility class ANYWHERE in this file compiles that rule (and its `--color-*`
// entry) into the production bundle — a dead rule shipped by the test that
// exists to stop dead rules. Every class reference below is therefore prose or
// a brace-set shape that cannot be a valid candidate.

import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import {
  SRC_ROOT,
  expectAllowlistExact,
  repoPath,
  scannedSources,
} from '../test/sourceScan';

const SOURCE_EXTENSIONS = /\.(ts|svelte|css)$/;

// ---------------------------------------------------------------------------
// Rules
//
// The class-shaped rules apply to EVERY scanned file, including the theme
// layer: a token file has no business emitting a raw palette utility either.
// ---------------------------------------------------------------------------

/**
 * The utility prefixes that take a color. ONE alternation, shared by the
 * palette rule and the black/white rule — they used to carry different lists,
 * and the shorter one let the fill / stroke / gradient-stop forms of
 * black and white through untouched.
 */
const COLOR_PREFIXES =
  'text|bg|border|ring|stroke|fill|outline|decoration|from|via|to|caret|divide|shadow';

/** Tailwind's default palette. Our own scales resolve through `@theme`. */
const RAW_PALETTE_CLASS = new RegExp(
  `\\b(?:${COLOR_PREFIXES})-(?:slate|gray|zinc|neutral|stone|red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose)-\\d{2,3}\\b`,
  'g',
);

/**
 * The scrim / on-media family. These are not theme-neutral: a scrim that is
 * correct in both modes is its own ROLE — the overlay, scrim and scrim-fg
 * tokens — not an absence of one. Opacity modifiers are part of the match, so
 * a scrim utility written at partial alpha is caught too.
 */
const RAW_BLACK_WHITE_CLASS = new RegExp(
  `\\b(?:${COLOR_PREFIXES})-(?:black|white)(?:\\/\\d+)?\\b`,
  'g',
);

/**
 * Tailwind's default shadow scale (plus v4's inset and text shadow families);
 * ours are the sheet / menu / modal roles. Anchored on both sides so the
 * blur-only drop-shadow utilities — which carry no color and are legitimate —
 * do not match through their trailing size segment.
 */
const DEFAULT_SHADOW_CLASS =
  /(?<![\w-])(?:shadow|inset-shadow|text-shadow)-(?:2xs|xs|sm|md|lg|xl|2xl)(?![\w-])/g;

/**
 * Hex colors. Deliberately not restricted to `.svelte`/`.css`: a hex literal
 * in a `.ts` file is how the xterm palette and the JPEG matte got there, and
 * the whole tree currently carries exactly the deliberate set below — so the
 * broader scan costs nothing and catches the next one wherever it lands.
 *
 * All four CSS lengths are in scope (3, 4, 6, 8 digits — the 4/8 forms carry
 * alpha). The trailing guard is a hex-digit lookahead rather than a word
 * boundary: `\b` after six digits matches happily in the MIDDLE of a longer
 * run, so an eight-digit literal used to be read as a six-digit one plus two
 * stray characters and an `#abcd` alpha shorthand was missed outright.
 * Non-color shapes still don't match, and BOTH guards are load-bearing for
 * that: the lookahead says the hex run ended, the word boundary says the TOKEN
 * ended. Svelte's each-block keyword is the proof — three hex-looking letters
 * followed by a fourth letter, which the lookahead alone happily accepts.
 * A sha is far longer than eight digits and an id is not all hex. The leading
 * guard excludes HTML numeric character references, which are decimal digits
 * behind a `#` and read as a four-digit hex to everything that isn't looking
 * for the ampersand.
 */
const HEX_LITERAL = /(?<!&)#[0-9a-fA-F]{3,8}(?![0-9a-fA-F])\b/g;
const HEX_LENGTHS = new Set([3, 4, 6, 8]);

/**
 * A color function inside a Tailwind arbitrary value — the shape a raw shadow
 * or ring color returns in once the named-utility rules above are closed. A
 * literal color is a literal color whether it is spelled as a class or smuggled
 * into brackets. Token-derived values (a CSS variable reference inside the same
 * brackets) stay allowed: that IS the vocabulary, reached from a utility.
 *
 * `color-mix()` is deliberately not listed — it composes tokens rather than
 * stating a color, and the trailing paren in the pattern keeps it from
 * matching through the shared prefix.
 *
 * The leading guard excludes letters, digits and hyphens but NOT underscore:
 * `_` is Tailwind's space inside an arbitrary value, so it is what precedes
 * the function in every multi-part value this rule is aimed at.
 */
const ARBITRARY_COLOR_FUNCTION =
  /-\[[^\]]*(?<![a-zA-Z0-9-])(?:rgba?|hsla?|oklch|oklab|lab|lch|color)\(/g;

// ---------------------------------------------------------------------------
// Allowlists
// ---------------------------------------------------------------------------

/**
 * Files that still emit a raw palette / black-white / default-shadow /
 * arbitrary-color class.
 *
 * Empty, and that is the claim: phase 1 closed every one of them, so there is
 * no exception here to pre-approve the next one by. Key format, for whoever
 * writes the first entry: the file's path relative to `src/`, forward slashes,
 * e.g. `lib/components/chat/Foo.svelte`.
 */
const RAW_CLASS_ALLOWLIST: Record<string, string> = {};

/**
 * The curated built-in palettes (phase 3): one module per theme, each holding
 * nothing but the token → color table its upstream palette publishes. This is
 * the theme layer in the same sense app.css and syntax.css are — the values
 * ARE the theme — so it is one exception with one reason, spelled per file so
 * the list stays exact in both directions. A NEW file here needs a line here
 * too, which is the point: a hex outside these tables is still a leak.
 */
const CURATED_PALETTE_MODULES = [
  'blacklight',
  'catppuccin',
  'dracula',
  'gruvbox',
  'highContrast',
  'monokai',
  'nord',
  'oneDark',
  'solarized',
  'tokyoNight',
];

const CURATED_PALETTE_REASON =
  'curated built-in theme palette — the values ARE the theme layer, stated as hex so the xterm bridge and the color probe read them without a conversion (see lib/theme/builtins.ts)';

/**
 * Hex literals that are deliberate. Two shapes: the theme layer, where the
 * values ARE the vocabulary, and the handful of places where a hex is not a
 * UI color at all.
 */
const HEX_ALLOWLIST: Record<string, string> = {
  ...Object.fromEntries(
    CURATED_PALETTE_MODULES.map((name) => [
      `lib/theme/builtins/${name}.ts`,
      CURATED_PALETTE_REASON,
    ]),
  ),
  'app.css':
    'the theme layer itself — the `:root` dark palette and the `html.light` override block are the hex source of truth every token resolves from (phase 2 moves them into themes/*.json)',
  'styles/syntax.css':
    'the github-dark / github-light `--syntax-*` palettes — 21 token values per mode, the code axis of the theme layer',
  'lib/components/composer/imageCompress.ts':
    'JPEG has no alpha and canvas composites transparency onto black, so the export matte is an opaque white fill — an encoder detail on a produced file, not app chrome',
  'lib/components/chat/UserMessageBody.svelte':
    'the literal is the opaque stop of a `mask-image` gradient — an alpha channel, never painted',
  'lib/components/virtual/TimelineVirtualizerHarness.svelte':
    'test-only fixture host (imported solely by the virtualizer unit + browser tests); the literal is the harness backdrop, not a rendered surface',
};

/**
 * Utility classes whose name collides with a token family stem but which are
 * not color utilities at all. See the dead-token rule below for why a stem
 * collision is possible; each entry says why the class is real.
 */
const NON_COLOR_UTILITY_ALLOWLIST: Record<string, string> = {};

// ---------------------------------------------------------------------------
// The @theme vocabulary, read at test time
//
// The dead-token rule below is the other half of the leak story, and it is the
// one that bites hardest in review: a utility naming a token that does not
// exist compiles to NOTHING. It is not a wrong color, it is no color — the
// element silently inherits — and nothing in the type system, the linter or
// the build says a word. Two of them shipped in this phase alone (a surface
// tier that was never defined, and a foreground role borrowed from another
// design system's naming).
// ---------------------------------------------------------------------------

function themeBlock(): string {
  const css = readFileSync(join(SRC_ROOT, 'app.css'), 'utf8').replace(/\/\*[\s\S]*?\*\//g, ' ');
  const start = css.indexOf('@theme');
  if (start < 0) throw new Error('app.css has no @theme block; the token rules would be vacuous');
  const open = css.indexOf('{', start);
  let depth = 0;
  for (let i = open; i < css.length; i += 1) {
    if (css[i] === '{') depth += 1;
    else if (css[i] === '}') {
      depth -= 1;
      if (depth === 0) return css.slice(open + 1, i);
    }
  }
  throw new Error('app.css @theme block is unterminated');
}

function themeNames(block: string, kind: 'color' | 'shadow'): Set<string> {
  const names = new Set<string>();
  for (const match of block.matchAll(new RegExp(`--${kind}-([a-z0-9-]+)\\s*:`, 'g'))) {
    names.add(match[1]!);
  }
  if (names.size === 0) throw new Error(`app.css @theme declares no --${kind}-* tokens`);
  return names;
}

/** First segment of each token name — the family a utility is checked against. */
function stems(names: Iterable<string>): string[] {
  return [...new Set([...names].map((name) => name.split('-')[0]!))].sort();
}

const THEME_BLOCK = themeBlock();
const COLOR_TOKENS = themeNames(THEME_BLOCK, 'color');
const SHADOW_TOKENS = themeNames(THEME_BLOCK, 'shadow');

/**
 * Utility prefixes that resolve a `--color-*` token. Narrower than the raw
 * palette rule's list on purpose: the shadow prefix takes a `--shadow-*` name,
 * checked separately below.
 */
const TOKEN_COLOR_PREFIXES =
  'bg|text|border|ring|stroke|fill|divide|outline|decoration|caret|accent|from|via|to';

/**
 * `<prefix>-<name>` where `<name>`'s FIRST SEGMENT names a token family. The
 * stems are DERIVED from the `@theme` block rather than listed here, so adding
 * a family automatically brings its utilities under the rule.
 *
 * Restricting to family stems is what keeps the rule quiet: it never has to
 * decide whether an arbitrary suffix is a color, only whether a name that
 * claims a family actually resolves inside it. Variants and opacity modifiers
 * fall outside the match by construction — the leading guard rejects a name
 * segment, not a variant separator, and the trailing guard stops before a
 * slash.
 */
const TOKEN_COLOR_UTILITY = new RegExp(
  `(?<![\\w-])(?:${TOKEN_COLOR_PREFIXES})-((?:${stems(COLOR_TOKENS).join('|')})(?:-[a-z0-9]+)*)(?![\\w-])`,
  'g',
);

/** Same shape for the shadow roles. Default-scale sizes are the other rule's. */
const TOKEN_SHADOW_UTILITY = new RegExp(
  `(?<![\\w-])(?:shadow|inset-shadow|text-shadow)-((?:${stems(SHADOW_TOKENS).join('|')})(?:-[a-z0-9]+)*)(?![\\w-])`,
  'g',
);

// ---------------------------------------------------------------------------
// Scanner
// ---------------------------------------------------------------------------

/**
 * Comments are documentation, not output. `chat/markdown/streamdownTheme.ts`
 * describes the palette classes its table replaced in its comments, and a
 * scanner that counted those would push the tree toward documenting less.
 * (Class names are deliberately not spelled verbatim there either, for the
 * Tailwind-scans-comments reason in this file's header.)
 *
 * `//` is only treated as a line comment when it is not preceded by a colon,
 * a quote or a backslash, so a protocol-relative URL, a `//` inside a string
 * and an escaped slash all keep the rest of their line in the scan.
 */
function stripComments(text: string, path: string): string {
  let out = text.replace(/\/\*[\s\S]*?\*\//g, ' ');
  if (!path.endsWith('.css')) out = out.replace(/(^|[^:'"`\\])\/\/[^\n]*/g, '$1');
  if (path.endsWith('.svelte')) out = out.replace(/<!--[\s\S]*?-->/g, ' ');
  return out;
}

function findAll(text: string, pattern: RegExp): string[] {
  return [...new Set([...text.matchAll(pattern)].map((m) => m[0]))];
}

/** Unresolvable names for one token-family rule, deduped. */
function findDeadTokens(text: string, pattern: RegExp, tokens: Set<string>): string[] {
  const dead = new Set<string>();
  for (const match of text.matchAll(pattern)) {
    if (tokens.has(match[1]!)) continue;
    if (match[0] in NON_COLOR_UTILITY_ALLOWLIST) continue;
    dead.add(match[0]);
  }
  return [...dead];
}

interface Scan {
  readonly rawClasses: Map<string, string[]>;
  readonly hexes: Map<string, string[]>;
}

/**
 * One pass over the tree, producing both rules' offender maps.
 *
 * Streamed on purpose: each file is read, stripped, matched and DROPPED. The
 * previous shape held every file's text plus a stripped copy for the whole
 * suite — roughly 800 files' worth of strings retained to run two `for` loops.
 */
function scan(): Scan {
  const rawClasses = new Map<string, string[]>();
  const hexes = new Map<string, string[]>();

  for (const file of scannedSources(SOURCE_EXTENSIONS)) {
    const path = repoPath(file);
    const text = stripComments(readFileSync(file, 'utf8'), path);

    const reasons: string[] = [];
    const palette = findAll(text, RAW_PALETTE_CLASS);
    if (palette.length > 0) reasons.push(`raw Tailwind palette classes: ${palette.join(', ')}`);
    const blackWhite = findAll(text, RAW_BLACK_WHITE_CLASS);
    if (blackWhite.length > 0) reasons.push(`raw black/white utilities: ${blackWhite.join(', ')}`);
    const shadows = findAll(text, DEFAULT_SHADOW_CLASS);
    if (shadows.length > 0) reasons.push(`default Tailwind shadows: ${shadows.join(', ')}`);
    const arbitrary = findAll(text, ARBITRARY_COLOR_FUNCTION);
    if (arbitrary.length > 0) {
      reasons.push(`literal colors inside arbitrary values: ${arbitrary.join(', ')}`);
    }
    const deadColors = findDeadTokens(text, TOKEN_COLOR_UTILITY, COLOR_TOKENS);
    if (deadColors.length > 0) {
      reasons.push(`utilities naming a color token that does not exist: ${deadColors.join(', ')}`);
    }
    const deadShadows = findDeadTokens(text, TOKEN_SHADOW_UTILITY, SHADOW_TOKENS);
    if (deadShadows.length > 0) {
      reasons.push(`utilities naming a shadow token that does not exist: ${deadShadows.join(', ')}`);
    }
    if (reasons.length > 0) rawClasses.set(path, reasons);

    const found = findAll(text, HEX_LITERAL).filter((hex) => HEX_LENGTHS.has(hex.length - 1));
    if (found.length > 0) hexes.set(path, [`hex color literals: ${found.join(', ')}`]);
  }

  return { rawClasses, hexes };
}

const scanned = scan();

describe('theme tokens', () => {
  it('resolves every color and shadow class through the token vocabulary', () => {
    expectAllowlistExact(
      scanned.rawClasses,
      RAW_CLASS_ALLOWLIST,
      'New theme-token leaks.',
      'Use a semantic token utility (the surface / fg / border / info-success-warning-error roles, and the sheet-menu-modal shadow scale). A utility naming a token that does not exist compiles to nothing at all. If the role does not exist yet, add the token — see docs/architecture/theme-system.md §4.',
    );
  });

  it('keeps hex literals inside the theme layer and the documented exceptions', () => {
    expectAllowlistExact(
      scanned.hexes,
      HEX_ALLOWLIST,
      'New hex color literals.',
      'Define the value as a token in app.css / styles/ and reference it with var(--…) or its utility. If the literal is genuinely not a UI color, add it here WITH the reason.',
    );
  });

  it('checks the allowlists against a vocabulary it actually read', () => {
    // The rules are generated from `@theme`, so an unreadable or renamed block
    // would quietly narrow every one of them to nothing.
    expect(COLOR_TOKENS.size).toBeGreaterThan(20);
    expect(SHADOW_TOKENS.size).toBeGreaterThan(0);
    expect(Object.keys(NON_COLOR_UTILITY_ALLOWLIST)).toEqual([]);
  });

  // Both allowlists above are empty and every rule currently finds nothing,
  // which is exactly the state in which a broken pattern is indistinguishable
  // from a clean tree. These pin the patterns against the shapes they were
  // written for — including the near-misses each one got wrong once.
  //
  // The fixtures are ASSEMBLED at runtime rather than written whole: a
  // complete utility class spelled in this file would be scanned by Tailwind
  // and compiled into the bundle, which is the leak the suite exists to stop.
  describe('the rules match the shapes they claim', () => {
    const hit = (pattern: RegExp, sample: string): boolean =>
      new RegExp(pattern.source, pattern.flags.replace('g', '')).test(sample);

    it('catches raw palette classes on every color-taking prefix', () => {
      for (const prefix of ['text', 'bg', 'border', 'ring', 'fill', 'stroke', 'shadow', 'divide']) {
        expect(hit(RAW_PALETTE_CLASS, `${prefix}-blue-` + '500')).toBe(true);
      }
      expect(hit(RAW_PALETTE_CLASS, 'text-fg-' + 'muted')).toBe(false);
    });

    it('catches black/white on the same prefixes, opacity modifier included', () => {
      for (const prefix of ['fill', 'stroke', 'from', 'divide']) {
        expect(hit(RAW_BLACK_WHITE_CLASS, `${prefix}-` + 'black')).toBe(true);
      }
      expect(hit(RAW_BLACK_WHITE_CLASS, 'bg-' + 'black/45')).toBe(true);
      expect(hit(RAW_BLACK_WHITE_CLASS, 'text-' + 'white')).toBe(true);
    });

    it('catches default shadows without claiming the blur-only ones', () => {
      expect(hit(DEFAULT_SHADOW_CLASS, 'shadow-' + 'md')).toBe(true);
      expect(hit(DEFAULT_SHADOW_CLASS, 'inset-shadow-' + 'sm')).toBe(true);
      expect(hit(DEFAULT_SHADOW_CLASS, 'text-shadow-' + 'lg')).toBe(true);
      // Blur, not color — legitimate, and the rule used to eat it.
      expect(hit(DEFAULT_SHADOW_CLASS, 'drop-shadow-' + 'md')).toBe(false);
      expect(hit(DEFAULT_SHADOW_CLASS, 'shadow-' + 'menu')).toBe(false);
    });

    it('catches literal colors smuggled into arbitrary values', () => {
      expect(hit(ARBITRARY_COLOR_FUNCTION, 'shadow-' + '[0_1px_4px_rgba(0,0,0,0.4)]')).toBe(true);
      expect(hit(ARBITRARY_COLOR_FUNCTION, 'text-' + '[oklch(0.7_0.1_250)]')).toBe(true);
      // Token-derived values, and a composition of them, are the point.
      expect(hit(ARBITRARY_COLOR_FUNCTION, 'shadow-' + '[0_1px_4px_var(--overlay)]')).toBe(false);
      expect(hit(ARBITRARY_COLOR_FUNCTION, 'bg-' + '[color-mix(in_oklab,var(--accent),white)]')).toBe(
        false,
      );
    });

    it('catches hex in all four lengths without eating its neighbours', () => {
      for (const digits of ['abc', 'abcd', 'aabbcc', 'aabbccdd']) {
        expect(hit(HEX_LITERAL, `#${digits};`)).toBe(true);
      }
      // Svelte's each-block keyword, and an HTML numeric character reference.
      expect(hit(HEX_LITERAL, '{#each rows as row}')).toBe(false);
      expect(hit(HEX_LITERAL, '&#9656;')).toBe(false);
      // A commit sha is longer than any color literal.
      expect(hit(HEX_LITERAL, '#0123456789abcdef')).toBe(false);
    });

    it('catches a utility naming a token family member that does not exist', () => {
      const dead = 'text-fg-' + 'nonexistent';
      expect(findDeadTokens(dead, TOKEN_COLOR_UTILITY, COLOR_TOKENS)).toEqual([dead]);
      expect(findDeadTokens('text-fg-' + 'muted', TOKEN_COLOR_UTILITY, COLOR_TOKENS)).toEqual([]);
      // A variant prefix and an opacity modifier must not hide the name.
      const hidden = 'hover:bg-surface-' + '9/40';
      expect(findDeadTokens(hidden, TOKEN_COLOR_UTILITY, COLOR_TOKENS)).toEqual([
        'bg-surface-' + '9',
      ]);
      // Non-color utilities that merely start with a family stem are not names.
      for (const sample of ['text-sm', 'text-left', 'border-t', 'border-collapse', 'to-transparent']) {
        expect(findDeadTokens(sample, TOKEN_COLOR_UTILITY, COLOR_TOKENS)).toEqual([]);
      }
      const deadShadow = 'shadow-menu-' + 'strong';
      expect(findDeadTokens(deadShadow, TOKEN_SHADOW_UTILITY, SHADOW_TOKENS)).toEqual([deadShadow]);
      expect(findDeadTokens('shadow-' + 'menu', TOKEN_SHADOW_UTILITY, SHADOW_TOKENS)).toEqual([]);
    });
  });
});
