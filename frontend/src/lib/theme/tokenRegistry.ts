// THE canonical list of themeable tokens.
//
// Everything downstream of a theme file reads this: the parser decides which
// keys are known, the resolver decides which declarations it is willing to
// emit, `scripts/generate-theme-reference.mjs` turns it into the JSON Schema
// and the `TOKENS.md` reference that agents and humans edit theme files
// against, and `tokenRegistry.test.ts` checks it against the three CSS
// files in BOTH directions so neither side can move alone.
//
// One entry per CSS custom property, keyed by the var name minus the leading
// dashes — `surface-1`, `syntax-keyword`, `ansi-fg-31`. That one-to-one shape
// is deliberate: a theme file names the token the stylesheet names, so there
// is no mapping table to drift and no second vocabulary to learn.
//
// SCOPE, v1: colors only. The non-color scales — the shadow roles, the radius
// scale, the type scale, the font stacks — are deliberately NOT themeable.
// They are structure rather than palette: a shadow role is already derived
// from the palette (it mixes over black), and letting a theme file move radii
// or type sizes turns "pick a palette" into "rebuild the layout", which is
// exactly the failure mode the format is trying to avoid. The drift test's
// exclusion list below is where that decision is enforced.
//
// SECTIONS AND AXES. Sections exist so a theme file reads as four small
// vocabularies rather than one flat eighty-key blob, and they map onto the
// two independently-selectable axes:
//
//   colors → UI axis    app chrome: surfaces, text, borders, accent, status,
//                       provider identity, tool-kind icons, and the inline-code
//                       chip pair (monochrome prose furniture, not a
//                       highlighted surface — see md-inline-code's entry)
//   syntax → code axis  the 21 highlight families
//   ansi   → code axis  the 16 ANSI foregrounds chat output and the terminal
//                       share
//   code   → code axis  the grounds block-code surfaces own (block, terminal)
//
// A file that defines `colors` in any variant is listable on the UI axis; a
// file that defines any code section is listable on the code axis; a file may
// serve both.
//
// DERIVED ROLES. Some tokens default to a `var()` / `color-mix()` over another
// token rather than to a literal — the foreground hierarchy over the primary
// text color, the subtle border over the border, the card and code-block
// grounds over the first elevation tier, the two ANSI aliases, the generic
// tool icon. They are marked `derived` and are fully overridable; left alone
// they FOLLOW whatever base the theme sets, which is why a sparse theme that
// only moves the base palette still comes out coherent. `TOKENS.md` says so
// per row so an agent does not override six tokens to achieve what one does.
//
// MODE-INVARIANT ROLES. A handful of tokens are declared in `:root` with NO
// `html.light` counterpart and no derivation to carry the mode for them — the
// accent foreground, the media-overlay pair, the Claude brand mark, the design
// canvas paper. They are marked `modeInvariant`, and the flag is load-bearing
// rather than documentation: the emission model is "sparse over the base of
// that polarity", and for these tokens there IS no light-mode base to
// out-cascade a `:root` value, so a two-variant theme that states one in only
// one variant silently applies it to both modes. The resolver warns on exactly
// that (`themeResolve.ts`), and `tokenRegistry.test.ts` derives the flag's
// correctness from the same CSS parse the drift rules use, so it cannot be set
// by hand for a token that does not qualify.

// ---------------------------------------------------------------------------
// Format limits
//
// The value and name caps live HERE, in the one theme module that imports
// nothing, because two consumers must agree on them and only one of them has a
// bundler: `themeParse.ts` enforces them (and re-exports them, so the parser
// stays their documented home for every TypeScript caller), and
// `scripts/generate-theme-reference.mjs` writes them into the JSON Schema
// after importing this file through Node's type stripping — which resolves no
// extensionless specifiers, so an import-free module is the only one it can
// reach. They used to be hand-copied into the script, where parser-vs-schema
// drift was invisible to every gate.
// ---------------------------------------------------------------------------

/** Longest value a theme file may give a token. */
export const MAX_VALUE_LENGTH = 256;

/** Longest display name a theme file may carry. */
export const MAX_NAME_LENGTH = 80;

/** The four key spaces a theme file's variant block may carry. */
export type ThemeSection = 'colors' | 'syntax' | 'ansi' | 'code';

/** The two independently selectable axes. */
export type ThemeAxis = 'ui' | 'code';

/** Which stylesheet declares a token's default. */
export type TokenSourceFile = 'app.css' | 'tokens.css' | 'syntax.css';

export interface TokenEntry {
  /** JSON key in a theme file, and the CSS var name minus the leading dashes. */
  readonly key: string;
  readonly section: ThemeSection;
  readonly axis: ThemeAxis;
  /** One line, rendered verbatim into the schema and `TOKENS.md`. */
  readonly description: string;
  /**
   * True when the default is computed from another token, so the role follows
   * a base-palette change unless the theme overrides it explicitly.
   */
  readonly derived: boolean;
  /**
   * True when the token is declared in `:root` only, with no `html.light`
   * counterpart and no derivation to carry the mode for it — so a value stated
   * for one mode applies to BOTH. See the module header.
   */
  readonly modeInvariant: boolean;
  readonly cssFile: TokenSourceFile;
}

/** Section order, used by the parser, the schema and the reference doc. */
export const THEME_SECTIONS: readonly ThemeSection[] = ['colors', 'syntax', 'ansi', 'code'];

export const SECTION_AXIS: Readonly<Record<ThemeSection, ThemeAxis>> = {
  colors: 'ui',
  syntax: 'code',
  ansi: 'code',
  code: 'code',
};

/** Human labels for the reference doc's section headings. */
export const SECTION_TITLES: Readonly<Record<ThemeSection, string>> = {
  colors: 'colors — app chrome (UI axis)',
  syntax: 'syntax — highlight families (code axis)',
  ansi: 'ansi — ANSI 16 foregrounds (code axis)',
  code: 'code — block-code grounds (code axis)',
};

/**
 * The token the app's window/page ground is painted with (`html, body, #app`
 * and `.app-shell` in app.css). The resolver reports its resolved value as
 * `windowBackground` so the native window can be created in the right color
 * before the webview paints; nothing else about the app depends on the
 * choice, so if the ground ever moves to another token, move this with it.
 */
export const WINDOW_GROUND_KEY = 'surface-0';

/**
 * Tokens with a `:root` declaration and nothing in `html.light` to out-cascade
 * it. Stated as a list rather than per entry because it reads as one fact
 * about the stylesheets; `tokenRegistry.test.ts` derives membership from the
 * CSS in both directions, so this cannot be wrong for long.
 */
const MODE_INVARIANT_KEYS: ReadonlySet<string> = new Set([
  'accent-fg',
  'provider-claude',
  'scrim',
  'scrim-fg',
]);

function entry(
  section: ThemeSection,
  cssFile: TokenSourceFile,
  key: string,
  description: string,
  derived = false,
): TokenEntry {
  return {
    key,
    section,
    axis: SECTION_AXIS[section],
    description,
    derived,
    modeInvariant: MODE_INVARIANT_KEYS.has(key),
    cssFile,
  };
}

const COLOR_ENTRIES: readonly TokenEntry[] = [
  entry('colors', 'app.css', 'surface-0', 'App ground: the window and page background every pane sits on.'),
  entry('colors', 'app.css', 'surface-1', 'First elevation step: cards, inputs and panels lifted off the ground.'),
  entry('colors', 'app.css', 'surface-2', 'Second elevation step: chrome sitting on a card (menus, chips, hover fills).'),
  entry('colors', 'app.css', 'surface-3', 'Third elevation step: progress tracks and hover fills on chrome already at tier 2.'),
  entry('colors', 'app.css', 'border', 'Default hairline between regions.'),
  entry('colors', 'app.css', 'border-strong', 'Prominent hairline: turn dividers and hover-emphasized control borders.'),
  entry(
    'colors',
    'tokens.css',
    'border-subtle',
    'Softest hairline, for ambient chrome. Follows the border color.',
    true,
  ),
  entry('colors', 'app.css', 'text-primary', 'Focal text.'),
  entry('colors', 'app.css', 'text-secondary', 'Supporting text: secondary labels, quoted prose, muted glyphs.'),
  entry('colors', 'tokens.css', 'fg-muted', 'Body copy. Follows the focal text color at reduced strength.', true),
  entry('colors', 'tokens.css', 'fg-subtle', 'Labels and de-emphasized text. Follows the focal text color.', true),
  entry('colors', 'tokens.css', 'fg-hint', 'Timestamps and barely-there hints. Follows the focal text color.', true),
  entry('colors', 'app.css', 'accent', 'Primary accent: selection, links, focus rings and filled buttons.'),
  entry(
    'colors',
    'app.css',
    'accent-fg',
    'Foreground painted on an accent fill, so a pale accent cannot strand its own label.',
  ),
  entry(
    'colors',
    'tokens.css',
    'md-heading',
    'Markdown headings in chat prose (the code-axis counterpart is syntax-markup-heading). Follows the focal text color.',
    true,
  ),
  entry(
    'colors',
    'tokens.css',
    'md-bold',
    'Markdown bold text in chat prose. No code-axis counterpart exists, so curated themes pick an emphasis hue of their own. Follows the focal text color.',
    true,
  ),
  entry(
    'colors',
    'tokens.css',
    'md-link',
    'Markdown links in chat prose (the code-axis counterpart is syntax-markup-link). Follows the accent.',
    true,
  ),
  entry(
    'colors',
    'tokens.css',
    'md-blockquote',
    'Markdown block-quote text in chat prose (the code-axis counterpart is syntax-markup-quote). Follows the supporting text color.',
    true,
  ),
  entry(
    'colors',
    'tokens.css',
    'md-marker',
    'Markdown list bullets and numbers in chat prose (the code-axis counterpart is syntax-markup-list). Follows the muted body-text tier (fg-muted).',
    true,
  ),
  entry('colors', 'app.css', 'code-inline-bg', 'Ground behind inline code spans in prose.'),
  entry(
    'colors',
    'tokens.css',
    'md-inline-code',
    'Markdown inline-code text, painted on the code-inline-bg chip (the highlight counterpart is syntax-markup-raw). Both chip tokens are UI-axis: the chip is monochrome prose furniture, not a highlighted surface, and text and ground stay on ONE axis so no UI/code combination can split the pair. Follows the focal text color.',
    true,
  ),
  entry('colors', 'app.css', 'info', 'Informational status: input prompts and neutral notices.'),
  entry('colors', 'app.css', 'success', 'Success status: completed work, added lines, healthy state.'),
  entry('colors', 'app.css', 'error', 'Failure status: errors, removed lines, refusals.'),
  entry('colors', 'app.css', 'warning', 'Attention status: a human is blocked (approvals, pending input).'),
  entry('colors', 'app.css', 'provider-codex', 'Codex provider identity. Brand-locked by default.'),
  entry(
    'colors',
    'app.css',
    'provider-claude',
    'Claude provider identity. Brand-locked by default, with no separate light value.',
  ),
  entry(
    'colors',
    'app.css',
    'provider-claude-tui',
    'claude-tui provider identity: the phosphor green of a CRT terminal.',
  ),
  entry('colors', 'app.css', 'overlay', 'Backdrop dimming app chrome behind a modal or sheet. Varies with the mode.'),
  entry(
    'colors',
    'app.css',
    'scrim',
    'Chrome painted over USER MEDIA (lightbox grounds, thumbnail badges). Stored opaque and consumed at partial alpha; mode-invariant by default.',
  ),
  entry(
    'colors',
    'app.css',
    'scrim-fg',
    'Foreground of the media-overlay pair. Stored opaque and consumed at partial alpha; mode-invariant by default.',
  ),
  entry(
    'colors',
    'tokens.css',
    'card',
    'Ambient card ground, referenced at low alpha by tool rows and cards. Follows the first elevation tier.',
    true,
  ),
  entry('colors', 'app.css', 'ico-terminal', 'Tool-kind icon: shell and terminal commands.'),
  entry('colors', 'app.css', 'ico-file', 'Tool-kind icon: file edits and writes.'),
  entry('colors', 'app.css', 'ico-eye', 'Tool-kind icon: reads and views.'),
  entry('colors', 'app.css', 'ico-search', 'Tool-kind icon: search and pattern matching.'),
  entry('colors', 'app.css', 'ico-globe', 'Tool-kind icon: network and web fetches.'),
  entry('colors', 'app.css', 'ico-robot', 'Tool-kind icon: subagents and delegated work.'),
  entry('colors', 'app.css', 'ico-speech-bubble', 'Tool-kind icon: conversation and messaging tools.'),
  entry('colors', 'app.css', 'ico-checklist', 'Tool-kind icon: task lists and plans.'),
  entry('colors', 'app.css', 'ico-puzzle', 'Tool-kind icon: MCP servers and plugin tools.'),
  entry('colors', 'app.css', 'ico-clock', 'Tool-kind icon: waits, sleeps and scheduled work.'),
  entry('colors', 'app.css', 'ico-brain', 'Tool-kind icon: thinking and reasoning.'),
  entry(
    'colors',
    'app.css',
    'ico-compaction',
    'Tool-kind icon: context compaction. A muted slate that reads as a system operation.',
  ),
  entry(
    'colors',
    'app.css',
    'ico-generic',
    'Tool-kind icon fallback for unclassified tools. Follows the secondary text color.',
    true,
  ),
];

const SYNTAX_ENTRIES: readonly TokenEntry[] = [
  entry('syntax', 'syntax.css', 'syntax-keyword', 'Language keywords and control flow.'),
  entry('syntax', 'syntax.css', 'syntax-string', 'String literals.'),
  entry('syntax', 'syntax.css', 'syntax-string-special', 'Escapes, interpolation markers and regex literals.'),
  entry('syntax', 'syntax.css', 'syntax-comment', 'Comments and doc comments.'),
  entry('syntax', 'syntax.css', 'syntax-number', 'Numeric literals.'),
  entry('syntax', 'syntax.css', 'syntax-function', 'Function and method names at definition and call sites.'),
  entry('syntax', 'syntax.css', 'syntax-type', 'Types, classes, structs and interfaces.'),
  entry('syntax', 'syntax.css', 'syntax-variable-builtin', 'Built-in variables such as this, self and super.'),
  entry('syntax', 'syntax.css', 'syntax-property', 'Object properties and struct fields.'),
  entry('syntax', 'syntax.css', 'syntax-constant', 'Constants and enum members, including the language booleans.'),
  entry('syntax', 'syntax.css', 'syntax-tag', 'Markup tag names (HTML, JSX, XML).'),
  entry('syntax', 'syntax.css', 'syntax-attribute', 'Markup attribute names and annotations.'),
  entry('syntax', 'syntax.css', 'syntax-namespace', 'Modules, packages and namespace qualifiers.'),
  entry('syntax', 'syntax.css', 'syntax-label', 'Labels, goto targets and named arguments.'),
  entry('syntax', 'syntax.css', 'syntax-markup-heading', 'Markdown headings (also rendered bold).'),
  entry('syntax', 'syntax.css', 'syntax-markup-link', 'Markdown links and URLs.'),
  entry('syntax', 'syntax.css', 'syntax-markup-raw', 'Markdown inline code and fenced blocks.'),
  entry('syntax', 'syntax.css', 'syntax-markup-list', 'Markdown list markers.'),
  entry('syntax', 'syntax.css', 'syntax-markup-quote', 'Markdown block quotes.'),
  entry('syntax', 'syntax.css', 'syntax-added', 'Added lines in an embedded diff.'),
  entry('syntax', 'syntax.css', 'syntax-removed', 'Removed lines in an embedded diff.'),
];

/**
 * `[code, label, followsTextPalette]` for the ANSI foregrounds, in wire order.
 * Two of them default to an alias of the app text palette rather than to a
 * literal, which is what keeps plain output legible on a recolored ground.
 */
const ANSI_TABLE: readonly (readonly [string, string, boolean])[] = [
  ['30', 'black', false],
  ['31', 'red', false],
  ['32', 'green', false],
  ['33', 'yellow', false],
  ['34', 'blue', false],
  ['35', 'magenta', false],
  ['36', 'cyan', false],
  ['37', 'white', true],
  ['90', 'bright black, i.e. grey', true],
  ['91', 'bright red', false],
  ['92', 'bright green', false],
  ['93', 'bright yellow', false],
  ['94', 'bright blue', false],
  ['95', 'bright magenta', false],
  ['96', 'bright cyan', false],
  ['97', 'bright white', false],
];

const ANSI_ENTRIES: readonly TokenEntry[] = ANSI_TABLE.map(([code, label, derived]) =>
  entry(
    'ansi',
    'app.css',
    `ansi-fg-${code}`,
    derived
      ? `ANSI ${code} (${label}) foreground. Follows the app text palette unless overridden.`
      : `ANSI ${code} (${label}) foreground, in chat output and the terminal alike.`,
    derived,
  ),
);

const CODE_ENTRIES: readonly TokenEntry[] = [
  entry(
    'code',
    'tokens.css',
    'code-block',
    'Ground behind a fenced code block. Follows the first elevation tier, so a code theme can move blocks without moving every card with them.',
    true,
  ),
  entry('code', 'app.css', 'terminal-bg', 'Ground of the embedded terminal.'),
];

export const TOKEN_REGISTRY: readonly TokenEntry[] = [
  ...COLOR_ENTRIES,
  ...SYNTAX_ENTRIES,
  ...ANSI_ENTRIES,
  ...CODE_ENTRIES,
];

// ---------------------------------------------------------------------------
// Lookups
// ---------------------------------------------------------------------------

const BY_SECTION: ReadonlyMap<ThemeSection, readonly TokenEntry[]> = new Map(
  THEME_SECTIONS.map((section) => [
    section,
    TOKEN_REGISTRY.filter((token) => token.section === section),
  ]),
);

const KEYS_BY_SECTION: ReadonlyMap<ThemeSection, ReadonlySet<string>> = new Map(
  THEME_SECTIONS.map((section) => [
    section,
    new Set((BY_SECTION.get(section) ?? []).map((token) => token.key)),
  ]),
);

export function isThemeSection(value: string): value is ThemeSection {
  return (THEME_SECTIONS as readonly string[]).includes(value);
}

export function tokensInSection(section: ThemeSection): readonly TokenEntry[] {
  return BY_SECTION.get(section) ?? [];
}

export function tokenKeysInSection(section: ThemeSection): ReadonlySet<string> {
  return KEYS_BY_SECTION.get(section) ?? new Set<string>();
}

/** The CSS custom-property name a registry key maps onto. */
export function cssVarName(key: string): string {
  return `--${key}`;
}

/**
 * True when the token's app default is declared ONCE for both modes, so an
 * emitted `:root` value has nothing in `html.light` to out-cascade it and a
 * two-variant theme must state the token in both variants or neither. Two
 * shapes qualify: the `modeInvariant` literals, and every token whose default
 * lives in tokens.css — that file's `html.light` block is pinned EMPTY by
 * `tokenRegistry.test.ts`, and its declarations are derivations, which carry
 * the mode only while the theme leaves them alone (a stated literal replaces
 * the derivation; found live when latte's bold rendered mocha's mustard at
 * 1.1:1). The resolver's mode-invariant warning and the curated
 * split-statement test both read THIS predicate, so the pin has one dependent.
 */
export function isSharedDefaultToken(token: TokenEntry): boolean {
  return token.modeInvariant || token.cssFile === 'tokens.css';
}

// ---------------------------------------------------------------------------
// Drift-test exclusions
//
// Every `--*` declared in `:root` / `html.light` across the three stylesheets
// is either a registry key or listed here. That is what makes the drift test
// bidirectional: adding a color token to app.css without a registry entry
// fails, and so does adding one here to silence it, because the reason has to
// be written down and it has to be one of these two shapes.
// ---------------------------------------------------------------------------

/** Non-color families excluded wholesale, by var-name prefix, with the reason. */
export const EXCLUDED_VAR_PREFIXES: Readonly<Record<string, string>> = {
  '--radius-': 'radius scale — geometry, not palette',
  '--shadow-': 'shadow roles — already derived from the palette by color-mix',
  '--font-': 'font stacks — owned by Settings → Typography, not by theme files',
  '--color-': 'the @theme mapping layer, which re-exports tokens as utilities',
  '--animate-': 'animation registrations, not colors',
  '--run-map-': 'run-map lane geometry (lengths)',
  '--fade-':
    'fade strengths (percentages) for the tokens.css fg tiers — mode-split in app.css because alpha compositing washes out faster toward a light ground; themes tune the tiers by overriding fg-muted/subtle/hint, not these',
};

/** Individually excluded var names, with the reason. */
export const EXCLUDED_VAR_NAMES: Readonly<Record<string, string>> = {
  '--text-micro': 'type scale — shares the --text- stem with the text colors, so it is excluded by name',
  '--text-xs': 'type scale',
  '--text-sm': 'type scale',
  '--text-base': 'type scale',
  '--text-lg': 'type scale',
};

export function isExcludedVar(name: string): boolean {
  if (name in EXCLUDED_VAR_NAMES) return true;
  return Object.keys(EXCLUDED_VAR_PREFIXES).some((prefix) => name.startsWith(prefix));
}
