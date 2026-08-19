// Resolve an appearance selection plus the theme files on disk into ONE set of
// declarations, a palette identity, and a window ground color.
//
// Pure: no DOM, no `CSS.supports`, no probe. The caller filters the
// declarations through the checks that need a browser, runs the survivors back
// through `serializeThemeCss`, and writes the result into the single
// `<style id="user-theme">` element (one style invalidation per change — see
// docs/specs/theme-system.md §1.4, and the measured cost of the N-setProperty
// alternative); `windowBackground` it normalizes through
// `utils/cssColorProbe.ts`.
//
// ── Emission model ─────────────────────────────────────────────────────────
//
// Two blocks, `:root` and `html.light`, mirroring exactly how app.css states
// the built-in palettes. That is what makes "absent = cascade default" fall
// out for free: a token the theme does not mention is simply not in the
// block, so the app's own declaration keeps winning. There is no reset value
// to invent and no default palette duplicated on this side.
//
// The two axes read the mode differently, and the difference is deliberate
// (§7 phase 2):
//
//   UI axis — chrome must match the mode. Both variants present: dark goes to
//     `:root`, light to `html.light`, and a mode flip re-resolves to
//     BYTE-IDENTICAL CSS, so the style rewrite is a no-op. Only one variant
//     present: it is emitted only when it matches the resolved mode, and the
//     other mode falls back to the built-in default in full. That last rule
//     is why the resolved mode is an input rather than something the CSS
//     handles on its own — leaving a dark-only theme's `:root` block standing
//     in light mode would half-apply it, since the tokens app.css declares as
//     mode-INVARIANT (the accent foreground, the media-overlay pair, the
//     brand marks) have no light-mode declaration to out-cascade it.
//
//   Code axis — code surfaces own their grounds, so a code theme with a sole
//     variant is emitted into BOTH blocks: a dark palette on a light UI
//     renders as a dark island, the familiar docs-site pattern, rather than
//     as unreadable dark-on-light text.
//
// ── What this deliberately does NOT decide ─────────────────────────────────
//
// Whether a value is a COLOR. That is `CSS.supports('color', v)`, which needs
// a browser, so it is apply-time work. `declarations` carries every emitted
// value per token with its provenance for exactly that: the applier filters
// the list, warns per rejected token in the same `ThemeWarning` shape, and
// re-serializes what survives through `serializeThemeCss`. One bad value
// costs one token, never the theme, and the escaping rules stay in one place.
//
// ── Safety ────────────────────────────────────────────────────────────────
//
// Theme files are local and user-authored, and this function must still be
// safe on ANY input, including a file an agent wrote wrong and a file that is
// hostile. Two independent gates, because they fail differently:
//
//   Keys never come from the file. A declaration is only ever written for a
//   key that is already in the token registry, and the name shape is checked
//   again at serialization — so there is no path by which file text becomes a
//   property name.
//
//   Values are checked against a conservative shape: a printable subset with
//   no `;`, no braces, no newline, no quote, no backslash, no `<`/`>`, no
//   `*` (so no comment can be opened), plus BALANCED PARENTHESES. The last
//   one is not paranoia: CSS's tokenizer lets an unclosed function consume to
//   EOF, so a value ending in a dangling `(` would swallow the closing brace
//   of its own block and everything after it. A value that fails costs
//   exactly that one token and produces a warning.
//
//   On top of the charset, SIX FUNCTIONS ARE REFUSED OUTRIGHT, in two groups
//   that fail differently:
//
//     `url()`, `image-set()`, `src()` — EGRESS. A theme file is local text; a
//     value carrying one of these turns it into a network beacon the moment
//     the property is consumed. Nothing about a color needs a fetch, and the
//     apply-time `CSS.supports('color', …)` gate that would otherwise reject
//     them is SKIPPED on an engine that cannot discriminate colors, so this
//     has to hold unconditionally and without a browser.
//
//     `var()`, `attr()`, `env()` — REFERENCES. A theme file states CONCRETE
//     colors; "follow another token" is what the registry's derived roles
//     already mean, and they do it in the stylesheet where the fallback is
//     ours. A reference here is a blanking primitive: `var(--nope)` passes
//     every shape check and makes the custom property invalid at
//     computed-value time, which unsets EVERY property that consumes it — so
//     one bad token takes out the app ground, and "one bad value costs one
//     token" stops being true.
//
//   Both groups are matched after case-folding and whitespace-stripping, so
//   `URL (`, `url/**/(` (already dead — `*` is refused) and `-webkit-image-set(`
//   land on the same rule. Escape forms need `\`, which the charset refuses.

import { BUILTIN_CODE_THEME_ID, BUILTIN_THEMES, BUILTIN_UI_THEME_ID } from './builtins';
import type { ParsedTheme, ThemeVariant, ThemeVariantName, ThemeWarning } from './themeParse';
import {
  THEME_SECTIONS,
  TOKEN_REGISTRY,
  WINDOW_GROUND_KEY,
  cssVarName,
  isSharedDefaultToken,
  type ThemeAxis,
  type ThemeSection,
} from './tokenRegistry';

/** The mode actually in effect. `system` has already been resolved away. */
export type ResolvedMode = 'dark' | 'light';

/** The selection an appearance file carries, minus the mode. */
export interface ThemeSelection {
  readonly uiTheme: string;
  readonly codeTheme: string;
}

export interface ResolveInput {
  readonly mode: ResolvedMode;
  readonly appearance: ThemeSelection;
  /** Themes read from `<configDir>/themes/*.json`, already parsed. */
  readonly themes: readonly ParsedTheme[];
  /** Overridable for tests; defaults to {@link BUILTIN_THEMES}. */
  readonly builtins?: readonly ParsedTheme[];
  /**
   * Bumps on ANY theme-file change. Part of the palette identity so consumers
   * that cache rendered output (mermaid's SVG cache, the xterm bridge) redraw
   * when a file is edited under a selection that did not change.
   */
  readonly revision: number;
}

export interface ResolvedThemeRef {
  readonly id: string;
  readonly name: string;
  readonly source: 'user' | 'builtin';
  /** True when the requested id was missing or unusable on this axis. */
  readonly fallback: boolean;
}

export interface ResolvedTheme {
  /**
   * The resolved content per token, with provenance, and the ONLY form the
   * content is returned in. The applier filters this through
   * `CSS.supports('color', …)` — the one check that needs a browser — adds the
   * derived accent foreground, and serializes the survivors with
   * {@link serializeThemeCss}. There is deliberately no `cssText` here beside
   * it: every production caller enriches the set first, so a text field would
   * be built on every resolve and discarded on every resolve, and a second
   * spelling of the same content is a second thing that can be stale.
   */
  readonly declarations: ResolvedDeclarations;
  /**
   * `uiTheme|codeTheme|revision`. Consumers that must redraw on a palette
   * change key on this.
   *
   * The MODE is deliberately absent. It is not forgotten: it already flows to
   * every one of those consumers through `getResolvedTheme()`, which they
   * read anyway to pick a light/dark posture, so folding it in here would
   * make one of the two spellings redundant and let them disagree. The rule
   * is "identity plus mode", and the pairing is the consumer's.
   */
  readonly paletteIdentity: string;
  /**
   * Raw, unnormalized value the theme gives the window ground for this mode,
   * or `undefined` when it does not set one (the caller then probes the live
   * DOM, which is the built-in answer).
   */
  readonly windowBackground: string | undefined;
  readonly warnings: readonly ThemeWarning[];
  readonly ui: ResolvedThemeRef;
  readonly code: ResolvedThemeRef;
  readonly mode: ResolvedMode;
}

// ---------------------------------------------------------------------------
// Serialization gates
// ---------------------------------------------------------------------------

/** Registry key shape, re-checked at the boundary that writes a property name. */
const SAFE_KEY = /^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/;

/** The same shape with the leading dashes, for the serializer's own re-check. */
const SAFE_CSS_VAR = /^--[a-z][a-z0-9]*(?:-[a-z0-9]+)*$/;

/**
 * Characters a declaration value may contain. Everything a concrete color
 * value needs (functions, channels, percentages, slashes, hex) and nothing
 * that can end a declaration, open a block, open a comment, escape, or close a
 * `<style>` element.
 */
const SAFE_VALUE_CHARS = /^[-#()%,.\/+_ a-zA-Z0-9]+$/;

/**
 * Function names a theme value may never open. See the module header: the
 * first three are network egress, the last three are references that blank
 * every consumer of the property when they fail to resolve.
 */
const REFUSED_FUNCTIONS: readonly string[] = ['url(', 'image-set(', 'src(', 'var(', 'attr(', 'env('];

/** Deep enough for any real color expression; a wall against pathological nesting. */
const MAX_PAREN_DEPTH = 8;

export function isSafeTokenKey(key: string): boolean {
  return SAFE_KEY.test(key);
}

export function isSafeDeclarationValue(value: string): boolean {
  if (!SAFE_VALUE_CHARS.test(value)) return false;
  // Case-folded and whitespace-free, so `URL (` and `Var(` cannot slip past a
  // literal-substring test. Comment forms need `*` and escapes need `\`, both
  // already refused by the charset above.
  const folded = value.toLowerCase().replace(/\s+/g, '');
  for (const fn of REFUSED_FUNCTIONS) {
    if (folded.includes(fn)) return false;
  }
  let depth = 0;
  for (const ch of value) {
    if (ch === '(') {
      depth += 1;
      if (depth > MAX_PAREN_DEPTH) return false;
    } else if (ch === ')') {
      depth -= 1;
      if (depth < 0) return false;
    }
  }
  return depth === 0;
}

// ---------------------------------------------------------------------------
// Catalog
// ---------------------------------------------------------------------------

export interface ThemeCatalogEntry {
  readonly theme: ParsedTheme;
  readonly source: 'user' | 'builtin';
}

/**
 * Built-ins first, user files layered over them: a file whose stem is a
 * reserved id SHADOWS the built-in and is listed as user-sourced. Later user
 * files win over earlier ones, which cannot happen in practice (ids are
 * filename stems) but keeps the function total.
 */
export function buildThemeCatalog(
  themes: readonly ParsedTheme[],
  builtins: readonly ParsedTheme[] = BUILTIN_THEMES,
): ReadonlyMap<string, ThemeCatalogEntry> {
  const catalog = new Map<string, ThemeCatalogEntry>();
  for (const theme of builtins) catalog.set(theme.id, { theme, source: 'builtin' });
  for (const theme of themes) catalog.set(theme.id, { theme, source: 'user' });
  return catalog;
}

/** The themes selectable on one axis, in id order. */
export function themesForAxis(
  catalog: ReadonlyMap<string, ThemeCatalogEntry>,
  axis: ThemeAxis,
): readonly ThemeCatalogEntry[] {
  return [...catalog.values()]
    .filter((entry) => (axis === 'ui' ? entry.theme.axes.ui : entry.theme.axes.code))
    .sort((a, b) => a.theme.id.localeCompare(b.theme.id));
}

interface AxisPick {
  readonly entry: ThemeCatalogEntry;
  readonly ref: ResolvedThemeRef;
  /**
   * The selected theme when it EXISTS but could not be used on this axis. Its
   * own parse warnings are the explanation for the fallback — a file that is
   * not JSON at all is unusable on both axes, and reporting only "defines no
   * colors section" while withholding "not valid JSON" tells the user the one
   * thing they cannot act on. The caller propagates them, deduplicated,
   * because the same file can be the unusable pick on both axes.
   */
  readonly unusable?: ThemeCatalogEntry;
}

function pickAxisTheme(
  catalog: ReadonlyMap<string, ThemeCatalogEntry>,
  requestedId: string,
  axis: ThemeAxis,
  fallbackId: string,
  warnings: ThemeWarning[],
): AxisPick {
  const found = catalog.get(requestedId);
  const usable = found && (axis === 'ui' ? found.theme.axes.ui : found.theme.axes.code);

  if (found && usable) {
    return {
      entry: found,
      ref: { id: found.theme.id, name: found.theme.name, source: found.source, fallback: false },
    };
  }

  // `unknown-theme` means "does not exist". A file that exists and cannot
  // serve the axis is a different fact with a different fix, so it gets its
  // own code.
  warnings.push(
    found
      ? {
          code: 'axis-unusable',
          themeId: found.theme.id,
          path: axis === 'ui' ? 'uiTheme' : 'codeTheme',
          message: `Theme "${requestedId}" defines no ${axis === 'ui' ? 'colors' : 'code'} section, so it cannot be used on the ${axis} axis. Falling back to "${fallbackId}".`,
        }
      : {
          code: 'unknown-theme',
          path: axis === 'ui' ? 'uiTheme' : 'codeTheme',
          message: `No theme named "${requestedId}". Falling back to "${fallbackId}".`,
        },
  );

  const fallback = catalog.get(fallbackId);
  if (!fallback) {
    // Only reachable when a caller passes its own builtin list and omits the
    // reserved id. Answer with an empty identity rather than throwing: a
    // broken selection must never be able to take the app's chrome down.
    const empty: ParsedTheme = {
      id: fallbackId,
      name: fallbackId,
      variants: {},
      axes: { ui: axis === 'ui', code: axis === 'code' },
      warnings: [],
      builtin: true,
    };
    return {
      entry: { theme: empty, source: 'builtin' },
      ref: { id: fallbackId, name: fallbackId, source: 'builtin', fallback: true },
      unusable: found,
    };
  }
  return {
    entry: fallback,
    ref: {
      id: fallback.theme.id,
      name: fallback.theme.name,
      source: fallback.source,
      fallback: true,
    },
    unusable: found,
  };
}

// ---------------------------------------------------------------------------
// Variant picking
// ---------------------------------------------------------------------------

/**
 * UI axis: the variant matching the mode, or nothing. A theme that does not
 * speak this mode does not get to speak in it at all.
 */
export function pickUiVariant(
  theme: ParsedTheme,
  mode: ResolvedMode,
): { dark?: ThemeVariant; light?: ThemeVariant } {
  const { dark, light } = theme.variants;
  if (dark && light) return { dark, light };
  if (mode === 'dark' && dark) return { dark };
  if (mode === 'light' && light) return { light };
  return {};
}

/**
 * Code axis: the matching variant, else the sole variant in BOTH blocks —
 * the dark island. A code theme with both variants behaves like the UI axis.
 */
export function pickCodeVariant(theme: ParsedTheme): { dark?: ThemeVariant; light?: ThemeVariant } {
  const { dark, light } = theme.variants;
  if (dark && light) return { dark, light };
  const sole = dark ?? light;
  if (!sole) return {};
  return { dark: sole, light: sole };
}

// ---------------------------------------------------------------------------
// Resolve
// ---------------------------------------------------------------------------

const SECTIONS_BY_AXIS: Readonly<Record<ThemeAxis, readonly ThemeSection[]>> = {
  ui: THEME_SECTIONS.filter((section) => section === 'colors'),
  code: THEME_SECTIONS.filter((section) => section !== 'colors'),
};

interface Contribution {
  readonly value: string;
  readonly themeId: string;
  readonly section: ThemeSection;
}

function collect(
  target: Map<string, Contribution>,
  variant: ThemeVariant | undefined,
  sections: readonly ThemeSection[],
  themeId: string,
): void {
  if (!variant) return;
  for (const section of sections) {
    const values = variant[section];
    if (!values) continue;
    for (const [key, value] of Object.entries(values)) {
      target.set(key, { value, themeId, section });
    }
  }
}

/**
 * One declaration the resolver is willing to emit, with the provenance an
 * apply-time check needs to report on it.
 *
 * This is the seam for the value validation that CANNOT happen here: whether
 * a string is a color is `CSS.supports('color', v)`'s question, and that
 * needs a browser. The applier filters this list, warns per rejected token
 * using the same `ThemeWarning` shape, and re-serializes what survives with
 * {@link serializeThemeCss} — so one bad value costs exactly one token and
 * the CSS text is still built by the code that owns the escaping rules.
 */
export interface ResolvedDeclaration {
  /** Registry key, e.g. `surface-1`. */
  readonly key: string;
  /** The custom property this writes, e.g. `--surface-1`. */
  readonly cssVar: string;
  readonly value: string;
  readonly section: ThemeSection;
  /** Which block it belongs to, and therefore which mode it paints. */
  readonly variant: ThemeVariantName;
  /** The theme that contributed it, for warning attribution. */
  readonly themeId: string;
}

export interface ResolvedDeclarations {
  /** The `:root` block — the dark palette. */
  readonly root: readonly ResolvedDeclaration[];
  /** The `html.light` block. */
  readonly light: readonly ResolvedDeclaration[];
}

function declarationsFor(
  contributions: ReadonlyMap<string, Contribution>,
  variant: ThemeVariantName,
  warnings: ThemeWarning[],
): ResolvedDeclaration[] {
  const out: ResolvedDeclaration[] = [];
  // Registry order, not insertion order: the CSS text is compared, cached and
  // diffed, so it must not depend on the key order of a JSON file.
  for (const token of TOKEN_REGISTRY) {
    const contribution = contributions.get(token.key);
    if (contribution === undefined) continue;
    if (!isSafeTokenKey(token.key)) continue;
    if (!isSafeDeclarationValue(contribution.value)) {
      warnings.push({
        code: 'unsafe-value',
        themeId: contribution.themeId,
        path: `${variant}.${contribution.section}.${token.key}`,
        message: `The value for "${token.key}" contains characters a CSS declaration cannot carry; ignoring it.`,
      });
      continue;
    }
    out.push({
      key: token.key,
      cssVar: cssVarName(token.key),
      value: contribution.value,
      section: contribution.section,
      variant,
      themeId: contribution.themeId,
    });
  }
  return out;
}

/**
 * The `<style id="user-theme">` body for a set of declarations. Total and
 * idempotent: it re-checks both gates, so a caller that filtered the list
 * cannot hand back something the resolver would not have written itself, and
 * an empty side simply contributes no block.
 */
export function serializeThemeCss(declarations: ResolvedDeclarations): string {
  const block = (selector: string, decls: readonly ResolvedDeclaration[]): string => {
    const lines = decls
      // The gate is on `cssVar` rather than on `key`, because `cssVar` is what
      // actually gets written: re-deriving it from the key would leave the
      // field the declaration carries permanently unread, and checking one
      // while emitting the other is how a property name escapes.
      .filter((decl) => SAFE_CSS_VAR.test(decl.cssVar) && isSafeDeclarationValue(decl.value))
      .map((decl) => `  ${decl.cssVar}: ${decl.value};`);
    return lines.length === 0 ? '' : `${selector} {\n${lines.join('\n')}\n}`;
  };
  return [block(':root', declarations.root), block('html.light', declarations.light)]
    .filter((text) => text.length > 0)
    .join('\n');
}

/**
 * The mode-invariant leak (see `tokenRegistry.ts` § MODE-INVARIANT ROLES).
 *
 * A two-variant theme reads as "here is my dark palette and here is my light
 * one", and for every token with an `html.light` default that is exactly what
 * it gets. For a token whose only app declaration lives in `:root`, a value in
 * the `dark` block alone lands in `:root` with nothing in `html.light` to
 * out-cascade it, so it paints in BOTH modes — the theme's light palette
 * quietly inherits a colour its author only wrote once.
 *
 * TWO classes of token are stranded that way, and both are warned:
 *
 *   - the mode-invariant roles (`token.modeInvariant`) — no light default
 *     exists at all;
 *   - every token whose default is declared in `tokens.css` — the derived
 *     roles (`--fg-muted`, `--card`, the `--md-*` prose roles, …) live in a
 *     single `:root` block whose `var()` carries the mode for the DEFAULT,
 *     but a theme's literal replaces the derivation, so a dark-only statement
 *     strands in light mode exactly the same way. (Found live: a light
 *     variant omitting `md-bold` while its dark variant stated one rendered
 *     the dark mustard on the light ground.) `tokenRegistry.test.ts` pins
 *     tokens.css's light block empty, so `cssFile` is the whole predicate.
 *
 * Both shapes are one question — "is the default declared once for both
 * modes?" — asked through `isSharedDefaultToken`, which the curated
 * split-statement test in `builtins.test.ts` shares.
 *
 * Stating in one variant only can be a legitimate thing to want (the
 * media-overlay pair is mode-invariant by design), so it is a warning and not
 * a refusal: state it in both variants to be explicit, or leave it and accept
 * the reach. A one-variant theme is not ambiguous at all — it never claims to
 * speak for the other mode — so it is silent here.
 */
function warnModeInvariantSplits(
  theme: ParsedTheme,
  sections: readonly ThemeSection[],
  warnings: ThemeWarning[],
): void {
  const { dark, light } = theme.variants;
  if (!dark || !light) return;

  const statedIn = (variant: ThemeVariant, key: string): ThemeSection | undefined =>
    sections.find((section) => variant[section]?.[key] !== undefined);

  for (const token of TOKEN_REGISTRY) {
    if (!isSharedDefaultToken(token)) continue;
    if (!sections.includes(token.section)) continue;
    const inDark = statedIn(dark, token.key);
    const inLight = statedIn(light, token.key);
    if ((inDark === undefined) === (inLight === undefined)) continue;
    const variant: ThemeVariantName = inDark ? 'dark' : 'light';
    const other: ThemeVariantName = inDark ? 'light' : 'dark';
    const why = token.modeInvariant
      ? `has no ${other}-mode default`
      : `is a derived role declared once for both modes`;
    warnings.push({
      code: 'mode-invariant',
      themeId: theme.id,
      path: `${variant}.${(inDark ?? inLight)!}.${token.key}`,
      message: `"${token.key}" ${why}, so the value in "${variant}" applies in both modes. State it in "${other}" too, or leave it if that is what you meant.`,
    });
  }
}

/**
 * The whole frontend theme core in one call. See the module header for the
 * emission model and the safety gates.
 */
export function resolveTheme(input: ResolveInput): ResolvedTheme {
  const { mode, appearance, themes, revision } = input;
  const warnings: ThemeWarning[] = [];
  const catalog = buildThemeCatalog(themes, input.builtins ?? BUILTIN_THEMES);

  const ui = pickAxisTheme(catalog, appearance.uiTheme, 'ui', BUILTIN_UI_THEME_ID, warnings);
  const code = pickAxisTheme(catalog, appearance.codeTheme, 'code', BUILTIN_CODE_THEME_ID, warnings);

  // A selected theme's own parse warnings are part of the user-facing state:
  // a file with a typo'd key is only interesting while it is the one applied.
  // A theme that was SELECTED and turned out unusable counts as applied for
  // this purpose — its warnings are the reason it is unusable — and the Set is
  // what keeps a file selected on both axes from reporting itself twice.
  const reported = new Set<ThemeCatalogEntry>();
  for (const entry of [ui.entry, code.entry, ui.unusable, code.unusable]) {
    if (entry) reported.add(entry);
  }
  for (const entry of reported) warnings.push(...entry.theme.warnings);

  warnModeInvariantSplits(ui.entry.theme, SECTIONS_BY_AXIS.ui, warnings);
  warnModeInvariantSplits(code.entry.theme, SECTIONS_BY_AXIS.code, warnings);

  const uiVariants = pickUiVariant(ui.entry.theme, mode);
  const codeVariants = pickCodeVariant(code.entry.theme);

  const rootContributions = new Map<string, Contribution>();
  const lightContributions = new Map<string, Contribution>();
  collect(rootContributions, uiVariants.dark, SECTIONS_BY_AXIS.ui, ui.ref.id);
  collect(lightContributions, uiVariants.light, SECTIONS_BY_AXIS.ui, ui.ref.id);
  collect(rootContributions, codeVariants.dark, SECTIONS_BY_AXIS.code, code.ref.id);
  collect(lightContributions, codeVariants.light, SECTIONS_BY_AXIS.code, code.ref.id);

  const declarations: ResolvedDeclarations = {
    root: declarationsFor(rootContributions, 'dark', warnings),
    light: declarationsFor(lightContributions, 'light', warnings),
  };

  // The ground the window, the page and the app shell are all painted with.
  // Read from the block the mode will actually apply, and only from what we
  // were willing to emit.
  const windowBackground = (mode === 'dark' ? declarations.root : declarations.light).find(
    (decl) => decl.key === WINDOW_GROUND_KEY,
  )?.value;

  return {
    declarations,
    paletteIdentity: `${ui.ref.id}|${code.ref.id}|${revision}`,
    windowBackground,
    warnings,
    ui: ui.ref,
    code: code.ref,
    mode,
  };
}
