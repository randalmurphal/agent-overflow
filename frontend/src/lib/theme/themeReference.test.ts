// Freshness gate for the generated theme-reference assets.
//
// `internal/theme/assets/{theme.schema.json,TOKENS.md}` are committed because
// Go embeds them and seeds them into `<configDir>/themes/` at boot — they
// have to exist without a Node toolchain in the picture. Committed generated
// files rot silently, so this runs the generator's pure core in-process and
// diffs it against what is on disk: adding a token, renaming one, editing a
// description or changing a default in app.css all fail here until the
// generator is re-run.

import { readFileSync } from 'node:fs';
import { describe, expect, it } from 'vitest';
import {
  SCHEMA_PATH,
  TOKENS_PATH,
  buildSchema,
  generateThemeReference,
  parseCssBlocks,
} from '../../../scripts/generate-theme-reference.mjs';
import { TOKEN_REGISTRY, tokensInSection, type ThemeSection } from './tokenRegistry';
import { MAX_NAME_LENGTH, MAX_VALUE_LENGTH } from './themeParse';

const REGENERATE = 'Run: cd frontend && node scripts/generate-theme-reference.mjs';

describe('generated theme reference', () => {
  const generated = generateThemeReference() as { schema: string; tokensMd: string };

  it('matches the committed theme.schema.json', () => {
    expect(readFileSync(SCHEMA_PATH, 'utf8'), REGENERATE).toBe(generated.schema);
  });

  it('matches the committed TOKENS.md', () => {
    expect(readFileSync(TOKENS_PATH, 'utf8'), REGENERATE).toBe(generated.tokensMd);
  });

  it('enumerates every registry key in the schema, and refuses anything else', () => {
    const schema = buildSchema() as unknown as {
      additionalProperties: boolean;
      definitions: {
        variant: {
          additionalProperties: boolean;
          properties: Record<string, { additionalProperties: boolean; properties: Record<string, unknown> }>;
        };
      };
    };

    expect(schema.additionalProperties).toBe(false);
    expect(schema.definitions.variant.additionalProperties).toBe(false);

    for (const [section, block] of Object.entries(schema.definitions.variant.properties)) {
      expect(block.additionalProperties, `${section} must refuse unknown tokens`).toBe(false);
      expect(Object.keys(block.properties).sort()).toEqual(
        tokensInSection(section as ThemeSection)
          .map((token) => token.key)
          .sort(),
      );
    }
  });

  it('documents every token, with its real default', () => {
    for (const token of TOKEN_REGISTRY) {
      expect(generated.tokensMd, `${token.key} is missing from TOKENS.md`).toContain(
        `| \`${token.key}\` |`,
      );
    }
    // The defaults are READ from the stylesheets, so a doc that says a colour
    // the app does not paint is impossible by construction — pin one to prove
    // the wiring rather than the claim.
    expect(generated.tokensMd).toContain('`oklch(0.145 0.014 285.82)`');
  });

  it('states the parser caps, rather than a copy of them', () => {
    // The schema's job is to reject in the editor exactly what the parser
    // rejects at load. The script used to re-spell both numbers, where drift
    // between them was invisible to every gate — this asserts they are the
    // parser's own.
    const schema = buildSchema() as unknown as {
      properties: { name: { maxLength: number } };
      definitions: {
        variant: {
          properties: Record<string, { properties: Record<string, { maxLength: number }> }>;
        };
      };
    };
    expect(schema.properties.name.maxLength).toBe(MAX_NAME_LENGTH);
    for (const block of Object.values(schema.definitions.variant.properties)) {
      for (const [key, token] of Object.entries(block.properties)) {
        expect(token.maxLength, key).toBe(MAX_VALUE_LENGTH);
      }
    }
  });

  it('documents the two rules a theme author cannot infer from a token table', () => {
    // R5: values are concrete. A reference that does not resolve blanks every
    // consumer of the property, so it is refused rather than skipped.
    expect(generated.tokensMd).toContain('`var()`');
    expect(generated.tokensMd).toContain('`url()`');
    // R8: a token with no light default reaches both modes, so a two-variant
    // file that states one in only one block is not doing what it looks like.
    expect(generated.tokensMd).toContain(
      '**A token with no light default reaches both modes.**',
    );
  });

  it('reads only top-level rules, so nested at-rule bodies cannot leak in', () => {
    const blocks = parseCssBlocks(`
      /* comment with a } brace */
      @import "x";
      :root { --a: 1; --b: 2; }
      @media (prefers-reduced-motion: reduce) { :root { --c: 3; } }
      html.light { --a: 9; }
      :root { --d: 4; }
    `) as Map<string, Map<string, string>>;

    expect([...(blocks.get(':root') ?? new Map()).entries()]).toEqual([
      ['--a', '1'],
      ['--b', '2'],
      ['--d', '4'],
    ]);
    expect(blocks.get('html.light')?.get('--a')).toBe('9');
    // The @media body is one depth-1 block whose own declarations sit deeper.
    expect(blocks.get(':root')?.has('--c')).toBe(false);
  });
});
