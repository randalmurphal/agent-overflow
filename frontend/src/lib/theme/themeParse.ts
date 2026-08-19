// Theme-file parsing and STRUCTURAL validation. Pure, synchronous, and
// deliberately incurious about color.
//
// A theme file is user- or agent-authored local JSON, which means two things
// at once: it is trusted enough to be applied, and untrusted enough that no
// input may be able to produce output the resolver would not have written on
// its own. So parsing is total — every reachable malformation lands as a
// WARNING plus a skip, never as a throw and never as a silent drop. Warnings
// are DATA (a code, a path, a sentence), because "errors are user-facing
// state, not log entries": the surface that renders them is the point of
// producing them, and a console line would be indistinguishable from nothing.
//
// What is NOT decided here: whether a value is a color. That needs
// `CSS.supports('color', v)`, which needs a browser, which makes it
// apply-time work. Values therefore pass through as OPAQUE STRINGS with only
// a length cap applied. The resolver puts a conservative structural shape on
// top before emitting anything (`themeResolve.ts#isSafeDeclarationValue`),
// and the apply step rejects per token on top of that. Three layers, each
// answering the question it can actually answer, and a value that fails any
// of them costs exactly one token.
//
// ERASABLE SYNTAX ONLY. This module is read by `scripts/generate-theme-
// reference.mjs` through Node's type stripping, so no enums, no namespaces, no
// parameter properties, and `import type` for every type-only import — the
// same discipline `tokenRegistry.ts` carries, and for the same reason.

import {
  MAX_NAME_LENGTH,
  MAX_VALUE_LENGTH,
  THEME_SECTIONS,
  isThemeSection,
  tokenKeysInSection,
  type ThemeSection,
} from './tokenRegistry';

// The caps are DEFINED in `tokenRegistry.ts` — the one theme module that
// imports nothing, and therefore the only one the reference generator can read
// under Node type stripping (which resolves no extensionless specifiers). The
// parser is still where they are enforced and where callers expect to find
// them, so they are re-exported rather than moved out of view.
export { MAX_NAME_LENGTH, MAX_VALUE_LENGTH };

export type ThemeVariantName = 'dark' | 'light';

export const THEME_VARIANTS: readonly ThemeVariantName[] = ['dark', 'light'];

export type ThemeWarningCode =
  /** The file is not valid JSON at all. */
  | 'invalid-json'
  /** A place that must hold an object holds something else. */
  | 'not-an-object'
  /** A key at the file root that is not `name`, a variant, or `$schema`. */
  | 'unknown-root-key'
  /** A key inside a variant block that is not one of the four sections. */
  | 'unknown-section'
  /** A key inside a section that no registry token claims. */
  | 'unknown-key'
  /** A token's value is not a string. */
  | 'non-string-value'
  /** A token's value is empty once trimmed. */
  | 'empty-value'
  /** A token's value is longer than {@link MAX_VALUE_LENGTH}. */
  | 'value-too-long'
  /** A token's value contains characters a CSS declaration may not carry. */
  | 'unsafe-value'
  /** A token's value is well-shaped but is not a color this browser understands. */
  | 'not-a-color'
  /** `name` is present but unusable; the id is used instead. */
  | 'invalid-name'
  /** The file defines no token in any variant. */
  | 'empty-theme'
  /** An appearance selection names a theme that does not exist. */
  | 'unknown-theme'
  /**
   * An appearance selection names a theme that EXISTS but defines nothing the
   * axis can use. Distinct from `unknown-theme` on purpose: the file is on
   * disk and its own parse warnings are what explain why.
   */
  | 'axis-unusable'
  /**
   * A two-variant theme states a mode-invariant token (one with no
   * `html.light` default to out-cascade `:root`) in exactly one variant, so
   * the value silently applies to both modes.
   */
  | 'mode-invariant';

export interface ThemeWarning {
  readonly code: ThemeWarningCode;
  /** Theme id the warning came from, when it came from a file. */
  readonly themeId?: string;
  /** Dotted path inside the file, e.g. `dark.colors.surface-1`. Empty at the root. */
  readonly path: string;
  /** One sentence, safe to render to a user as-is. */
  readonly message: string;
}

/** One variant's sparse overrides, section → (registry key → raw value). */
export type ThemeVariant = {
  readonly [S in ThemeSection]?: Readonly<Record<string, string>>;
};

export interface ParsedTheme {
  /** Filename stem for a user file; the reserved id for a built-in. */
  readonly id: string;
  /** Display name; defaults to the id. */
  readonly name: string;
  readonly variants: {
    readonly dark?: ThemeVariant;
    readonly light?: ThemeVariant;
  };
  /** Which axes this file can be SELECTED on. */
  readonly axes: { readonly ui: boolean; readonly code: boolean };
  readonly warnings: readonly ThemeWarning[];
  /** False for the identity built-ins, which exist to name the cascade default. */
  readonly builtin: boolean;
}

function warn(
  themeId: string,
  code: ThemeWarningCode,
  path: string,
  message: string,
): ThemeWarning {
  return { code, themeId, path, message };
}

function isPlainObject(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/**
 * Reads one section object into a key → value record, dropping and reporting
 * anything the registry does not claim or that cannot become a declaration.
 */
function parseSection(
  themeId: string,
  variant: ThemeVariantName,
  section: ThemeSection,
  raw: unknown,
  warnings: ThemeWarning[],
): Record<string, string> {
  const out: Record<string, string> = {};
  if (!isPlainObject(raw)) {
    warnings.push(
      warn(
        themeId,
        'not-an-object',
        `${variant}.${section}`,
        `"${section}" must be an object of token names to color values; ignoring it.`,
      ),
    );
    return out;
  }

  const known = tokenKeysInSection(section);
  for (const [key, value] of Object.entries(raw)) {
    const path = `${variant}.${section}.${key}`;
    if (!known.has(key)) {
      warnings.push(
        warn(
          themeId,
          'unknown-key',
          path,
          `"${key}" is not a token in the "${section}" section; ignoring it. See TOKENS.md for the list.`,
        ),
      );
      continue;
    }
    if (typeof value !== 'string') {
      warnings.push(
        warn(themeId, 'non-string-value', path, `"${key}" must be a string color value; ignoring it.`),
      );
      continue;
    }
    const trimmed = value.trim();
    if (trimmed.length === 0) {
      warnings.push(warn(themeId, 'empty-value', path, `"${key}" is empty; ignoring it.`));
      continue;
    }
    if (trimmed.length > MAX_VALUE_LENGTH) {
      warnings.push(
        warn(
          themeId,
          'value-too-long',
          path,
          `"${key}" is longer than ${MAX_VALUE_LENGTH} characters; ignoring it.`,
        ),
      );
      continue;
    }
    out[key] = trimmed;
  }
  return out;
}

function parseVariant(
  themeId: string,
  variant: ThemeVariantName,
  raw: unknown,
  warnings: ThemeWarning[],
): ThemeVariant | undefined {
  if (!isPlainObject(raw)) {
    warnings.push(
      warn(
        themeId,
        'not-an-object',
        variant,
        `"${variant}" must be an object of sections (${THEME_SECTIONS.join(', ')}); ignoring it.`,
      ),
    );
    return undefined;
  }

  const parsed: { -readonly [S in ThemeSection]?: Record<string, string> } = {};
  for (const [key, value] of Object.entries(raw)) {
    if (!isThemeSection(key)) {
      warnings.push(
        warn(
          themeId,
          'unknown-section',
          `${variant}.${key}`,
          `"${key}" is not a theme section; expected one of ${THEME_SECTIONS.join(', ')}.`,
        ),
      );
      continue;
    }
    const section = parseSection(themeId, variant, key, value, warnings);
    if (Object.keys(section).length > 0) parsed[key] = section;
  }

  return Object.keys(parsed).length > 0 ? parsed : undefined;
}

const ROOT_KEYS = new Set<string>(['$schema', 'name', ...THEME_VARIANTS]);

/**
 * Which axes a set of variants makes this file selectable on. `colors` in any
 * variant is the UI axis; any code-axis section in any variant is the code
 * axis; a file may serve both.
 */
function axesOf(variants: ParsedTheme['variants']): ParsedTheme['axes'] {
  let ui = false;
  let code = false;
  for (const variant of Object.values(variants)) {
    if (!variant) continue;
    for (const section of THEME_SECTIONS) {
      if (!variant[section]) continue;
      if (section === 'colors') ui = true;
      else code = true;
    }
  }
  return { ui, code };
}

/**
 * Parses one theme file. `id` is the filename stem and is authoritative — a
 * file never names itself, so two files can never claim one id and a rename
 * is a re-id.
 *
 * Total by construction: a file that is not JSON, not an object, or entirely
 * unknown keys yields an empty theme plus the warnings explaining it, and the
 * caller renders those rather than the app rendering nothing.
 */
export function parseTheme(id: string, raw: string): ParsedTheme {
  let doc: unknown;
  try {
    doc = JSON.parse(raw) as unknown;
  } catch (err) {
    return {
      id,
      name: id,
      variants: {},
      axes: { ui: false, code: false },
      builtin: false,
      warnings: [
        warn(
          id,
          'invalid-json',
          '',
          `Not valid JSON: ${err instanceof Error ? err.message : String(err)}`,
        ),
      ],
    };
  }
  return parseThemeDoc(id, doc);
}

/**
 * `parseTheme` for a caller that already HAS the document — the curated
 * built-ins, which are authored as object literals in TypeScript.
 *
 * Same validation, same warnings, same total-by-construction contract; the
 * only thing skipped is the JSON round trip. Built-ins used to reach the
 * parser by `JSON.stringify`ing nine specs and re-parsing ~31KB at module
 * init, on the eager startup path, purely to reuse the file entry point.
 */
export function parseThemeDoc(id: string, doc: unknown): ParsedTheme {
  const warnings: ThemeWarning[] = [];

  if (!isPlainObject(doc)) {
    return {
      id,
      name: id,
      variants: {},
      axes: { ui: false, code: false },
      builtin: false,
      warnings: [warn(id, 'not-an-object', '', 'A theme file must be a JSON object.')],
    };
  }

  for (const key of Object.keys(doc)) {
    if (ROOT_KEYS.has(key)) continue;
    warnings.push(
      warn(
        id,
        'unknown-root-key',
        key,
        `"${key}" is not a theme file key; expected "name", "dark" or "light". The theme's id comes from the filename.`,
      ),
    );
  }

  let name = id;
  if ('name' in doc && doc.name !== undefined) {
    const rawName = doc.name;
    if (typeof rawName !== 'string' || rawName.trim().length === 0) {
      warnings.push(warn(id, 'invalid-name', 'name', '"name" must be a non-empty string; using the file name.'));
    } else if (rawName.trim().length > MAX_NAME_LENGTH) {
      warnings.push(
        warn(
          id,
          'invalid-name',
          'name',
          `"name" is longer than ${MAX_NAME_LENGTH} characters; using the file name.`,
        ),
      );
    } else {
      name = rawName.trim();
    }
  }

  const variants: { dark?: ThemeVariant; light?: ThemeVariant } = {};
  for (const variant of THEME_VARIANTS) {
    if (!(variant in doc) || doc[variant] === undefined) continue;
    const parsed = parseVariant(id, variant, doc[variant], warnings);
    if (parsed) variants[variant] = parsed;
  }

  const axes = axesOf(variants);
  if (!axes.ui && !axes.code) {
    warnings.push(
      warn(
        id,
        'empty-theme',
        '',
        'This theme defines no tokens, so it cannot be selected on either axis.',
      ),
    );
  }

  return { id, name, variants, axes, warnings, builtin: false };
}
