// Coverage for the svelte-streamdown patch hunk that stops LLM "aligned
// value" bullets from rendering as code blocks.
//
// CommonMark reads five or more columns between a list marker and its
// content as "list item starting with indented code", so `-     $499 per
// month` is spec-correctly a code block. Agent prose uses that spacing to
// align values, never to open a code block, so the patch rewrites exactly
// that shape (`marked-list.js`, `tokenizeListItemContent`) and nothing
// else. This suite pins both halves: the artifact shapes that must become
// prose, and the neighbouring shapes whose code blocks are deliberate and
// must survive untouched.
//
// The rewrite lives in the ONE function that turns a list item's text into
// its child tokens, which the tight pass, the loose re-tokenize and
// `incrementalLex`'s tail loosener all call — so the streaming-parity
// block at the bottom is not a formality: a rewrite that fired on only one
// path would make a streamed render disagree with its settled render.
import { describe, expect, it } from 'vitest';
import {
  createIncrementalLexCache,
  incrementalLex,
  lex,
  parseIncompleteMarkdown,
} from 'svelte-streamdown';

interface Token {
  type: string;
  text?: string;
  lang?: string;
  codeBlockStyle?: string;
  tokens?: Token[];
}

/** Child tokens of the list item at `index` of the document's first list. */
const itemTokens = (markdown: string, index = 0): Token[] => {
  const list = (lex(markdown) as Token[]).find((token) => token.type === 'list');
  expect(list, `expected a list in ${JSON.stringify(markdown)}`).toBeDefined();
  const item = list?.tokens?.[index];
  expect(item?.type, 'expected a list_item').toBe('list_item');
  return item?.tokens ?? [];
};

const types = (tokens: Token[]): string[] => tokens.map((token) => token.type);

/** Concatenated prose of an item's text/paragraph children. */
const prose = (tokens: Token[]): string =>
  tokens
    .filter((token) => token.type === 'text' || token.type === 'paragraph')
    .map((token) => token.text ?? '')
    .join('\n');

describe('marker-line alignment is not an indented code block', () => {
  it('rewrites a single aligned bullet to prose', () => {
    const tokens = itemTokens('-     $499 per month');
    expect(types(tokens)).toEqual(['text']);
    expect(prose(tokens)).toBe('$499 per month');
  });

  it('rewrites an aligned bullet that follows a normal one', () => {
    const source = '- normal\n-     aligned value';
    expect(prose(itemTokens(source, 0))).toBe('normal');
    const aligned = itemTokens(source, 1);
    expect(types(aligned)).toEqual(['text']);
    expect(prose(aligned)).toBe('aligned value');
  });

  it('rewrites an ordered list item with the same shape', () => {
    const tokens = itemTokens('1.     $499 per month');
    expect(types(tokens)).toEqual(['text']);
    expect(prose(tokens)).toBe('$499 per month');
  });

  it('rewrites a nested list item, not just top-level ones', () => {
    const outer = itemTokens('- outer\n  -     $499 nested');
    const nested = outer.find((token) => token.type === 'list');
    expect(nested).toBeDefined();
    const item = nested?.tokens?.[0]?.tokens ?? [];
    expect(types(item)).toEqual(['text']);
    expect(prose(item)).toBe('$499 nested');
  });

  it('gives the recovered content a real inline pass', () => {
    const tokens = itemTokens(
      '-       **important** [docs](https://example.com) uses `inline code`',
    );
    expect(types(tokens)).toEqual(['text']);
    expect(types(tokens[0]?.tokens ?? [])).toEqual([
      'strong',
      'text',
      'link',
      'text',
      'codespan',
    ]);
  });

  it('rejoins content that follows the marker line instead of splitting it', () => {
    const tokens = itemTokens('-     $499 per month\n  more text');
    expect(types(tokens)).toEqual(['text']);
    expect(prose(tokens)).toBe('$499 per month\nmore text');
  });

  it('recovers every block when the aligned run spans a blank line', () => {
    const tokens = itemTokens(
      '-       **first block**\n\n        [second block](https://example.com)',
    );
    expect(types(tokens)).toEqual(['paragraph', 'space', 'paragraph']);
    expect(types(tokens[0]?.tokens ?? [])).toEqual(['strong']);
    expect(types(tokens[2]?.tokens ?? [])).toEqual(['link']);
  });

  it('leaves nothing re-indented enough to fall back into code', () => {
    // Nine columns of alignment survives marked's own four-space strip;
    // only the per-line dedent keeps the recovered content out of code.
    const tokens = itemTokens('-         $499 per month');
    expect(types(tokens)).toEqual(['text']);
    expect(prose(tokens)).toBe('$499 per month');
  });
});

describe('deliberate code blocks in lists stay code', () => {
  it('keeps an indented code block that starts below the marker line', () => {
    // Spec-correct and intended: the code is not the item's first child.
    const tokens = itemTokens('- item one\n\n        deep indent');
    expect(types(tokens)).toEqual(['paragraph', 'space', 'code']);
    expect(tokens[2]?.codeBlockStyle).toBe('indented');
    expect(tokens[2]?.text).toBe('  deep indent');
  });

  it('keeps code under an empty marker line', () => {
    // The blank marker line closes the item, so the code is a sibling of
    // the list rather than a child of the item — the rewrite never sees it.
    const tokens = lex('-\n\n    code') as Token[];
    expect(types(tokens)).toEqual(['list', 'code']);
    expect(tokens[0]?.tokens?.[0]?.tokens ?? []).toEqual([]);
    expect(tokens[1]?.codeBlockStyle).toBe('indented');
    expect(tokens[1]?.text).toBe('code');
  });

  it('keeps a fenced code block as the first child', () => {
    const tokens = itemTokens('- ```js\n  const a = 1;\n  ```');
    expect(types(tokens)).toEqual(['code']);
    expect(tokens[0]?.lang).toBe('js');
    expect(tokens[0]?.codeBlockStyle).toBeUndefined();
    expect(tokens[0]?.text).toBe('const a = 1;');
  });

  it('keeps a deep-indented block that follows recovered marker-line prose', () => {
    const tokens = itemTokens('-     aligned value\n\n          deep code');
    expect(types(tokens)).toEqual(['paragraph', 'space', 'code']);
    expect(prose(tokens)).toBe('aligned value');
    expect(tokens[2]?.text).toBe('deep code');
  });

  it('leaves an indented first line a block extension claimed', () => {
    // The gate tests the TOKEN, not the raw indentation: block extensions
    // tokenize ahead of marked's built-ins, so an aligned `$$…$$` is math,
    // not this artifact, and must not be dedented out from under it.
    expect(types(itemTokens('-     $$x^2$$'))).toEqual(['math']);
  });

  it('leaves a plain indented continuation alone', () => {
    expect(prose(itemTokens('- item one\n    continuation text'))).toBe(
      'item one\n  continuation text',
    );
    expect(types(itemTokens('- item one\n\n    continuation text'))).toEqual([
      'paragraph',
      'space',
      'paragraph',
    ]);
  });
});

describe('streaming parity', () => {
  const CHUNK_SIZES = [3, 7, 21];

  const ARTIFACT_DOCS = [
    '-     $499 per month',
    '- normal\n-     aligned value\n- trailing normal item',
    '- Plan\n-     $499 per month\n-     $4,990 per year\n- Cancel any time.',
    '-       **first block**\n\n        [second block](https://example.com)',
    // Long enough that the append fast path seals items behind the tail,
    // so the loosener and the merge both run over rewritten items.
    `${Array.from({ length: 24 }, (_, i) => `- Tier ${i}\n-     $${i}99 per month`).join('\n')}`,
  ];

  for (const doc of ARTIFACT_DOCS) {
    for (const size of CHUNK_SIZES) {
      for (const complete of [true, false]) {
        it(`${JSON.stringify(doc.slice(0, 40))} / ${size}-char chunks / complete=${complete}`, () => {
          const cache = createIncrementalLexCache();
          for (let at = size; ; at += size) {
            const prefix = doc.slice(0, Math.min(doc.length, at));
            const streamed = incrementalLex(
              prefix,
              [],
              cache,
              complete ? parseIncompleteMarkdown : null,
            );
            const settled = complete
              ? lex(parseIncompleteMarkdown(prefix.trim()))
              : lex(prefix);
            expect(
              JSON.stringify(streamed),
              `divergence at prefix length ${prefix.length} (path=${cache.lastPath})`,
            ).toBe(JSON.stringify(settled));
            if (prefix.length === doc.length) break;
          }
          // The whole point: the settled render carries no code block.
          expect(JSON.stringify(lex(doc))).not.toContain('"codeBlockStyle":"indented"');
        });
      }
    }
  }

  it('engages the list-append fast path over a rewritten list', () => {
    const doc = Array.from({ length: 40 }, (_, i) => `- Tier ${i}\n-     $${i}99 per month`).join('\n');
    const cache = createIncrementalLexCache();
    let appends = 0;
    for (let at = 21; at <= doc.length + 20; at += 21) {
      incrementalLex(doc.slice(0, Math.min(doc.length, at)), [], cache, parseIncompleteMarkdown);
      if (cache.lastPath === 'list-append') appends += 1;
    }
    // Without this the parity above could hold trivially by never leaving
    // the full-lex path, leaving the merge/loosener rewrite untested.
    expect(appends).toBeGreaterThan(10);
  });
});
