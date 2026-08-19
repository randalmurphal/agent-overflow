#!/usr/bin/env node
// Generate the theme-file reference assets from the frontend token registry.
//
//   cd frontend
//   node scripts/generate-theme-reference.mjs            # always WRITES
//   node scripts/generate-theme-reference.mjs --check    # writes nothing
//   pnpm run generate:theme-reference
//
// Plain invocation is unconditional: it rewrites both assets whether or not
// they had drifted. `--check` is the read-only half — it reports which assets
// differ and exits 1, for a shell that wants the gate without the side effect.
//
// Writes two files into `internal/theme/assets/`, where Go embeds them and
// seeds/refreshes them into `<configDir>/themes/` at boot:
//
//   theme.schema.json  every registry key enumerated, `additionalProperties`
//                      false at every level, so an editor completes token
//                      names and flags typos before the app ever reads them
//   TOKENS.md          the human/agent reference: the format rules, then one
//                      table per section with each token's role and its
//                      CURRENT default in both modes
//
// ── One source of truth, no build step ────────────────────────────────────
//
// The token list is authored once, in TypeScript, at
// `src/lib/theme/tokenRegistry.ts`, and this script IMPORTS it directly.
// Node ≥22.18 strips types from an imported `.ts` file on its own, so there
// is no compile step, no generated JSON mirror to keep in sync, and no second
// place a token can be spelled. (The registry is written in erasable syntax
// only — no enums, no namespaces, no parameter properties — which is what
// keeps that true. `verbatimModuleSyntax` in tsconfig already forces the
// `import type` discipline the stripper needs.)
//
// The DEFAULT VALUES are not authored anywhere: they are read out of
// `app.css`, `styles/tokens.css` and `styles/syntax.css` at generation time,
// so the reference cannot document a palette the app does not have.
//
// The generation core is pure and exported (`generateThemeReference`), and
// `src/lib/theme/themeReference.test.ts` runs it in-process and diffs against
// the committed assets — so drift fails the gate instead of shipping a
// reference that lies.

import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import {
  MAX_NAME_LENGTH,
  MAX_VALUE_LENGTH,
  SECTION_AXIS,
  SECTION_TITLES,
  THEME_SECTIONS,
  TOKEN_REGISTRY,
  EXCLUDED_VAR_NAMES,
  EXCLUDED_VAR_PREFIXES,
  tokensInSection,
} from '../src/lib/theme/tokenRegistry.ts';

const HERE = path.dirname(fileURLToPath(import.meta.url));

/** `frontend/src`. */
export const SRC_ROOT = path.resolve(HERE, '..', 'src');

/** Where the Go side embeds the generated assets from. */
export const ASSETS_DIR = path.resolve(HERE, '..', '..', 'internal', 'theme', 'assets');

export const SCHEMA_PATH = path.join(ASSETS_DIR, 'theme.schema.json');
export const TOKENS_PATH = path.join(ASSETS_DIR, 'TOKENS.md');

// The value and name caps are IMPORTED, never re-spelled: the schema's job is
// to reject in the editor what the parser would reject at load, and a
// hand-copied number makes parser-vs-schema drift invisible to every gate.
// They live in `tokenRegistry.ts` (which `themeParse.ts` re-exports) because
// Node's type stripping resolves no extensionless specifiers, so the only
// theme module this script can reach is the one that imports nothing.

// ---------------------------------------------------------------------------
// CSS reading
// ---------------------------------------------------------------------------

const CSS_FILES = {
  'app.css': 'app.css',
  'tokens.css': path.join('styles', 'tokens.css'),
  'syntax.css': path.join('styles', 'syntax.css'),
};

/**
 * Top-level rule blocks of one stylesheet, as `selector → { --var: value }`.
 *
 * Brace-matched rather than regexed, and only DEPTH-1 blocks are collected,
 * so an at-rule's nested rules (the reduced-motion block) cannot be mistaken
 * for top-level declarations and `@theme` is just another selector to ignore.
 * Repeated selectors merge, last declaration winning, which is what the
 * cascade does with them anyway.
 */
export function parseCssBlocks(css) {
  const clean = css.replace(/\/\*[\s\S]*?\*\//g, ' ');
  const blocks = new Map();

  let depth = 0;
  let selectorStart = 0;
  let bodyStart = 0;
  let selector = '';

  for (let i = 0; i < clean.length; i += 1) {
    const ch = clean[i];
    if (ch === '{') {
      if (depth === 0) {
        selector = clean.slice(selectorStart, i).trim().replace(/\s+/g, ' ');
        bodyStart = i + 1;
      }
      depth += 1;
    } else if (ch === '}') {
      depth -= 1;
      if (depth === 0) {
        const body = clean.slice(bodyStart, i);
        const existing = blocks.get(selector) ?? new Map();
        for (const match of body.matchAll(/(--[A-Za-z0-9-]+)\s*:\s*([^;{}]+);/g)) {
          existing.set(match[1], match[2].trim().replace(/\s+/g, ' '));
        }
        blocks.set(selector, existing);
        selectorStart = i + 1;
      }
    } else if (depth === 0 && ch === ';') {
      // A top-level statement (`@import …;`) — the next selector starts after it.
      selectorStart = i + 1;
    }
  }

  return blocks;
}

/**
 * `{ 'app.css': { root: Map, light: Map }, … }` for the three token files.
 * `root` is the `:root` block, `light` the `html.light` block; either may be
 * empty (tokens.css declares only derived roles and so has no light block).
 */
export function readCssVars(srcRoot = SRC_ROOT) {
  const out = {};
  for (const [name, relative] of Object.entries(CSS_FILES)) {
    const blocks = parseCssBlocks(readFileSync(path.join(srcRoot, relative), 'utf8'));
    out[name] = {
      root: blocks.get(':root') ?? new Map(),
      light: blocks.get('html.light') ?? new Map(),
    };
  }
  return out;
}

/**
 * Registry key → `{ dark, light }` as declared today. `light` is `null` when
 * the token has no mode-specific declaration, which is a real statement about
 * the token (it is mode-invariant, or it derives from something that is not).
 */
export function readTokenDefaults(cssVars = readCssVars()) {
  const defaults = new Map();
  for (const token of TOKEN_REGISTRY) {
    const file = cssVars[token.cssFile];
    if (!file) throw new Error(`registry token "${token.key}" names unknown file ${token.cssFile}`);
    const dark = file.root.get(`--${token.key}`);
    if (dark === undefined) {
      throw new Error(
        `registry token "${token.key}" is not declared in ${token.cssFile} :root — the registry and the stylesheets have drifted`,
      );
    }
    defaults.set(token.key, { dark, light: file.light.get(`--${token.key}`) ?? null });
  }
  return defaults;
}

// ---------------------------------------------------------------------------
// JSON Schema
// ---------------------------------------------------------------------------

function sectionSchema(section) {
  const properties = {};
  for (const token of tokensInSection(section)) {
    properties[token.key] = {
      type: 'string',
      minLength: 1,
      maxLength: MAX_VALUE_LENGTH,
      description: token.derived
        ? `${token.description} Derived by default — override only to stop it tracking.`
        : token.description,
    };
  }
  return {
    type: 'object',
    description: `${SECTION_TITLES[section]}. Sparse: name only the tokens you are changing.`,
    additionalProperties: false,
    properties,
  };
}

export function buildSchema() {
  const variantProperties = {};
  for (const section of THEME_SECTIONS) variantProperties[section] = sectionSchema(section);

  return {
    $schema: 'http://json-schema.org/draft-07/schema#',
    $id: 'https://agent-overflow.dev/schemas/theme.schema.json',
    title: 'Agent Overflow theme',
    description:
      'A theme file states one or two per-mode palettes as sparse overrides of the built-in palette of that polarity. The theme id is the filename stem; there is no "extends" key, because the variant name IS the base.',
    type: 'object',
    additionalProperties: false,
    properties: {
      $schema: { type: 'string', description: 'Path to this schema, for editor completion.' },
      name: {
        type: 'string',
        minLength: 1,
        maxLength: MAX_NAME_LENGTH,
        description: 'Display name. Defaults to the filename stem.',
      },
      dark: {
        $ref: '#/definitions/variant',
      },
      light: {
        $ref: '#/definitions/variant',
      },
    },
    definitions: {
      variant: {
        type: 'object',
        description:
          'One mode palette. Every section is optional and every section is sparse; a token you do not name keeps the built-in value.',
        additionalProperties: false,
        properties: variantProperties,
      },
    },
  };
}

// ---------------------------------------------------------------------------
// TOKENS.md
// ---------------------------------------------------------------------------

function cell(value) {
  return value === null || value === undefined ? '—' : `\`${value}\``;
}

function sectionTable(section, defaults) {
  const lines = [
    `### \`${section}\` — ${SECTION_AXIS[section] === 'ui' ? 'UI axis' : 'code axis'}`,
    '',
    '| Token | What it paints | Dark default | Light default |',
    '| --- | --- | --- | --- |',
  ];
  for (const token of tokensInSection(section)) {
    const value = defaults.get(token.key);
    const description = token.derived
      ? `${token.description} **Derived** — override only to stop it tracking.`
      : token.description;
    lines.push(`| \`${token.key}\` | ${description} | ${cell(value.dark)} | ${cell(value.light)} |`);
  }
  lines.push('');
  return lines.join('\n');
}

function excludedList() {
  const rows = [];
  for (const [prefix, why] of Object.entries(EXCLUDED_VAR_PREFIXES)) {
    rows.push(`| \`${prefix}*\` | ${why} |`);
  }
  for (const [name, why] of Object.entries(EXCLUDED_VAR_NAMES)) {
    rows.push(`| \`${name}\` | ${why} |`);
  }
  return rows.join('\n');
}

export function buildTokensMd(defaults) {
  const counts = THEME_SECTIONS.map(
    (section) => `${tokensInSection(section).length} in \`${section}\``,
  ).join(', ');

  const header = `# Theme tokens

<!-- GENERATED FILE — do not edit.
     Source: frontend/src/lib/theme/tokenRegistry.ts + the app stylesheets.
     Regenerate: cd frontend && node scripts/generate-theme-reference.mjs -->

Every color this app paints, and the name a theme file calls it by. ${TOKEN_REGISTRY.length} tokens: ${counts}.

## Where theme files live

\`\`\`
<configDir>/themes/
  appearance.json      the selection: mode, uiTheme, codeTheme
  my-theme.json        a theme; its id is the filename stem ("my-theme")
  theme.schema.json    this file's machine-readable twin (generated)
  TOKENS.md            this file (generated)
\`\`\`

Edit a file and the app reloads it — no restart. A file that is broken does
not break the app: what could not be understood is reported as a warning and
skipped, per token, and everything else still applies.

## The format

\`\`\`json
{
  "$schema": "./theme.schema.json",
  "name": "Display Name",
  "dark":  { "colors": {}, "syntax": {}, "ansi": {}, "code": {} },
  "light": { "colors": {} }
}
\`\`\`

- **Sparse, and there is no \`extends\`.** A variant block overrides the
  built-in palette **of its own polarity** — that is what \`"dark"\` and
  \`"light"\` MEAN here. Name only the tokens you are changing; everything you
  leave out keeps the built-in value, including tokens added by a future
  version of the app. A materialized copy of every value would go stale the
  day a token is added; a sparse file never does.
- **One theme per axis.** The UI axis (\`colors\`) and the code axis
  (\`syntax\` + \`ansi\` + \`code\`) are selected independently, so a UI theme
  and a code theme can come from different files. A file that defines
  \`colors\` in any variant is offered on the UI axis; a file that defines any
  code section is offered on the code axis; one file may serve both.
- **Missing variants behave differently per axis, on purpose.**
  - *UI axis*: chrome must match the mode. A theme with only a \`dark\` block
    applies in dark mode and steps aside entirely in light mode, where the
    built-in light palette renders instead.
  - *Code axis*: code surfaces own their own grounds (\`code-block\`,
    \`code-inline-bg\`, \`terminal-bg\`), so a theme with only a \`dark\`
    block **stays itself in light mode** — a dark code island on a light page,
    the familiar docs-site pattern, rather than unreadable dark-on-light text.
- **Built-in ids are \`default\` (UI) and \`github\` (code).** Both are
  identity themes: they name the palette the app ships with rather than
  restating it. A file of your own with one of those names shadows the
  built-in.
- **Values are CONCRETE CSS colors**, as a string — any color syntax the
  browser accepts, \`color-mix()\` included. Up to ${MAX_VALUE_LENGTH}
  characters, and restricted to the characters a color value needs, so a value
  cannot carry a second declaration or a comment. A value that is not a valid
  color is skipped with a warning; the rest of the theme still applies.
  \`url()\`, \`image-set()\` and \`src()\` are refused — a palette needs no
  network fetch — and so are \`var()\`, \`attr()\` and \`env()\`: a reference
  that does not resolve blanks every property that reads it, which would let
  one bad token take out the whole app ground instead of costing one color.
  "Follow another token" is what the derived rows below already do.
- **Derived tokens follow their base.** The rows marked **Derived** below
  default to an expression over another token — the foreground hierarchy over
  the text color, the subtle border over the border, the card and code-block
  grounds over the first elevation tier. Move the base and they all move with
  it. Override one only when you want it to stop tracking.
- **A token with no light default reaches both modes.** The rows showing
  **—** in the light column have no light-mode declaration to out-cascade the
  dark one, so in a file that states BOTH \`"dark"\` and \`"light"\`, naming
  one of them in only one block still paints it in both. State it in both
  blocks when you want two colors; leave it in one when one color is what you
  meant. Either way the app says which tokens you did that to.

## Not themeable

| Variable | Why |
| --- | --- |
${excludedList()}

These are structure rather than palette. Shadows are already mixed from the
palette, and letting a theme move radii or type sizes turns "pick a palette"
into "rebuild the layout".

## Tokens

A **—** in the light column means the token has no light-mode declaration: the
dark value applies in both modes (for a derived token, it re-derives from
whatever base the mode has).
`;

  const sections = THEME_SECTIONS.map((section) => sectionTable(section, defaults));
  return `${header}\n${sections.join('\n')}`;
}

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

/** The two asset bodies, exactly as they should exist on disk. */
export function generateThemeReference(srcRoot = SRC_ROOT) {
  const defaults = readTokenDefaults(readCssVars(srcRoot));
  return {
    schema: `${JSON.stringify(buildSchema(), null, 2)}\n`,
    tokensMd: buildTokensMd(defaults),
  };
}

function isMain() {
  const invoked = process.argv[1];
  return invoked !== undefined && path.resolve(invoked) === fileURLToPath(import.meta.url);
}

function readIfPresent(file) {
  try {
    return readFileSync(file, 'utf8');
  } catch {
    return undefined;
  }
}

if (isMain()) {
  const { schema, tokensMd } = generateThemeReference();
  const targets = [
    [SCHEMA_PATH, schema],
    [TOKENS_PATH, tokensMd],
  ];

  if (process.argv.includes('--check')) {
    const drifted = targets.filter(([file, want]) => readIfPresent(file) !== want);
    if (drifted.length === 0) {
      process.stdout.write('theme reference assets are up to date\n');
    } else {
      for (const [file] of drifted) process.stderr.write(`stale: ${file}\n`);
      process.stderr.write(
        'Run: cd frontend && node scripts/generate-theme-reference.mjs\n',
      );
      process.exitCode = 1;
    }
  } else {
    mkdirSync(ASSETS_DIR, { recursive: true });
    for (const [file, body] of targets) writeFileSync(file, body);
    process.stdout.write(`wrote ${SCHEMA_PATH}\nwrote ${TOKENS_PATH}\n`);
  }
}
