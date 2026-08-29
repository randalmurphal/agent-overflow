import { Lexer } from 'marked';
import { describe, expect, it, vi } from 'vitest';
import {
  createParseBlocksCache,
  createProvenAppend,
  lex,
  parseBlocks,
  type Extension,
  type ParseBlocksCache,
} from 'svelte-streamdown';
import {
  markedListBlock,
  parseListSource,
} from '../../../vendor/svelte-streamdown/dist/marked/marked-list.js';
import { parseBlockquoteSource } from '../../../vendor/svelte-streamdown/dist/marked/marked-blockquote-source.js';
import { markedDl } from '../../../vendor/svelte-streamdown/dist/marked/marked-dl.js';
import { markedFootnote } from '../../../vendor/svelte-streamdown/dist/marked/marked-footnotes.js';
import { markedBr } from '../../../vendor/svelte-streamdown/dist/marked/marked-br.js';
import { markedCitations } from '../../../vendor/svelte-streamdown/dist/marked/marked-citations.js';
import { markedMath } from '../../../vendor/svelte-streamdown/dist/marked/marked-math.js';
import { parseMdxSource } from '../../../vendor/svelte-streamdown/dist/marked/marked-mdx.js';
import { markedSub, markedSup } from '../../../vendor/svelte-streamdown/dist/marked/marked-subsup.js';
import { markedTableBlock } from '../../../vendor/svelte-streamdown/dist/marked/marked-table-source.js';

type ParseBlocksPath = ParseBlocksCache['lastPath'];

interface AppendDifferentialResult {
  paragraphAppends: number;
  paths: ReadonlySet<ParseBlocksPath>;
}

function withoutRaw(value: unknown): unknown {
  return JSON.parse(JSON.stringify(value, (key, nested) => (
    key === 'raw' ? undefined : nested
  )));
}

function prefixes(text: string, chunks: readonly number[]): string[] {
  const values: string[] = [];
  let offset = 0;
  let chunkIndex = 0;
  while (offset < text.length) {
    offset = Math.min(text.length, offset + chunks[chunkIndex % chunks.length]);
    values.push(text.slice(0, offset));
    chunkIndex++;
  }
  return values;
}

function expectAppendEquivalent(
  text: string,
  chunks: readonly number[],
  extensions: Extension[] = [],
): AppendDifferentialResult {
  const cache = createParseBlocksCache();
  let previous = '';
  let paragraphAppends = 0;
  const paths = new Set<ParseBlocksPath>();
  for (const prefix of prefixes(text, chunks)) {
    const append = createProvenAppend(previous, prefix.slice(previous.length));
    const actual = parseBlocks(prefix, extensions, cache, append);
    const expected = parseBlocks(prefix, extensions);
    expect(actual).toEqual(expected);
    if (cache.lastPath === 'paragraph-append') paragraphAppends++;
    paths.add(cache.lastPath);
    previous = prefix;
  }
  return { paragraphAppends, paths };
}

function expectFullLexEquivalent(text: string): void {
  const blockTokens = parseBlocks(text).flatMap((block) => lex(block));
  expect(withoutRaw(blockTokens)).toEqual(withoutRaw(lex(text)));
}

const TRANSITION_CORPUS = [
  'The renderer keeps **streamed Markdown**, `code`, and [links](https://example.test) readable.',
  'Visible progress remains on one ordinary source line.',
  '東京 remains intact while more text arrives.',
  'a. alphabetic list item\nb. second item',
  'IV. Roman list item\nV. second item',
  '123. numeric list item\n124. second item',
  '# ATX heading\n\nParagraph after it.',
  '> Blockquote text that grows.\n> Another line.',
  '```ts\nconst value = 1;\n```\n\nParagraph after it.',
  '---\n\nParagraph after the rule.',
  'Setext heading\n---\n\nParagraph after it.',
  '| Header | Value |\n| --- | ---: |\n| row | 1 |',
  '[ref]: https://example.test\n\nA [reference][ref].',
  '[^note]: Footnote body\n\nA footnote[^note].',
  ':term: detail text',
  '$$ x + y $$',
  '<Component value="x" />',
  '<Component value="x">Child **Markdown**</Component>',
  '<Component><Component label="> inside" /></Component>',
  '<div>HTML block</div>',
  '[center]\nCentered text\n[/center]',
  'The first line is stable.\r\n\r\n## A CRLF heading',
  'The first paragraph is sealed.\n\nNext block starts here.',
];

describe('parseBlocks append differential', () => {
  it('keeps block-only list and table tokens free of render child arrays', () => {
    const list = '- first\n- second\n- third';
    const listToken = markedListBlock.tokenizer.call(
      { lexer: { options: { gfm: true, pedantic: false } } } as never,
      list,
      [],
    );
    expect(listToken).toEqual({
      type: 'list',
      raw: list,
      sealedLen: '- first\n- second\n'.length,
    });

    const table = '| H | V |\n| --- | --- |\n| a | 1 |\n| b | 2 |';
    const tableToken = markedTableBlock.tokenizer.call(
      {} as never,
      table,
      [],
    );
    expect(tableToken).toEqual({
      type: 'table',
      raw: table,
      headerRowCount: 1,
      bodyRowCount: 2,
      hasFooter: false,
    });
  });

  it('finds a blockquote boundary without tokenizing its children', () => {
    const blockTokens = vi.spyOn(Lexer.prototype, 'blockTokens');
    try {
      expect(parseBlocks('> quoted **Markdown**\n> continues')).toEqual([
        '> quoted **Markdown**\n> continues',
      ]);
      expect(blockTokens).toHaveBeenCalledOnce();
    } finally {
      blockTokens.mockRestore();
    }
  });

  it('finds default extension boundaries without building render children', () => {
    const blockTokens = vi.spyOn(Lexer.prototype, 'blockTokens');
    const inlineTokens = vi.spyOn(Lexer.prototype, 'inlineTokens');
    let blockCalls = -1;
    let inlineCalls = -1;
    try {
      expect(parseBlocks(':term: detail')).toEqual([':term: detail']);
      expect(parseBlocks('[center]\nCentered text\n[/center]')).toEqual([
        '[center]\nCentered text\n[/center]',
      ]);
      expect(parseBlocks('<Component>body</Component>')).toEqual([
        '<Component>body</Component>',
      ]);
      blockCalls = blockTokens.mock.calls.length;
      inlineCalls = inlineTokens.mock.calls.length;
    } finally {
      blockTokens.mockRestore();
      inlineTokens.mockRestore();
    }
    expect(blockCalls).toBe(3);
    expect(inlineCalls).toBe(0);
  });

  it.each([
    ['self-closing', '<Card label="value" count={2} enabled={true} />'],
    ['paired', '<Card>Child **Markdown**</Card>'],
    ['nested', '<Card><Card>inner</Card><Card /></Card>'],
    ['greater-than in a nested string attribute', '<Card><Card label="> inside" /></Card>'],
    ['greater-than in a nested expression', '<Card><Card value={"a>b"} /></Card>'],
  ])('keeps the lightweight MDX boundary exact for %s components', (_label, source) => {
    expect(parseMdxSource(source)?.raw).toBe(source);
    expect(parseBlocks(source)).toEqual([source]);
    expectAppendEquivalent(source, [1, 2, 5]);
    expectFullLexEquivalent(source);
  });

  it('rejects impossible extension starts before their full grammars', () => {
    const exec = vi.spyOn(RegExp.prototype, 'exec');
    let grammarCalls = -1;
    try {
      expect(parseListSource(
        'Ordinary prose cannot open a list.',
        { gfm: true, pedantic: false },
        true,
      )).toBeUndefined();
      expect(parseBlockquoteSource('Ordinary prose cannot open a quote.'))
        .toBeUndefined();
      const footnotes = markedFootnote();
      const footnote = footnotes.find((extension) => extension.level === 'block');
      expect(footnote?.tokenizer.call(
        { lexer: {} } as never,
        'Ordinary prose cannot open a footnote.',
        [],
      )).toBeUndefined();
      const footnoteRef = footnotes.find((extension) => extension.level === 'inline');
      expect(footnoteRef?.tokenizer.call(
        { lexer: {} } as never,
        'Ordinary prose cannot reference a footnote.',
        [],
      )).toBeUndefined();
      const blockMath = markedMath.find((extension) => extension.level === 'block');
      const inlineMath = markedMath.find((extension) => extension.level === 'inline');
      for (const extension of [blockMath, inlineMath, markedSub, markedSup, markedBr, markedCitations]) {
        expect(extension?.tokenizer.call(
          { lexer: {} } as never,
          'Ordinary prose cannot open an inline extension.',
          [],
        )).toBeUndefined();
      }
      expect(markedDl.tokenizer.call(
        {} as never,
        'Ordinary prose cannot open a description list.',
        [],
      )).toBeUndefined();
      grammarCalls = exec.mock.calls.length;
    } finally {
      exec.mockRestore();
    }
    expect(grammarCalls).toBe(0);
  });

  it('initializes safe fresh boundaries without entering marked', () => {
    const blockTokens = vi.spyOn(Lexer.prototype, 'blockTokens');
    try {
      for (const source of [
        'Ordinary prose starts here',
        '# Heading starts here',
        '> Quoted prose starts here',
        '> Quoted prose starts here\n> and continues',
        '- A list item starts here',
        '| A table row starts | here |',
        '```ts\nconst value = 1;',
      ]) {
        const cache = createParseBlocksCache();
        expect(parseBlocks(source, [], cache)).toEqual([source]);
        expect(cache.lastPath).toBe('initial-boundary');
      }
      expect(blockTokens).not.toHaveBeenCalled();
    } finally {
      blockTokens.mockRestore();
    }
  });

  it.each([
    ['one code unit', [1]],
    ['mixed small chunks', [1, 2, 3, 5, 8]],
    ['reveal-sized chunks', [7, 19, 31]],
  ] as const)('matches a fresh parse through block-shape transitions with %s', (_label, chunks) => {
    let paragraphAppends = 0;
    for (const document of TRANSITION_CORPUS) {
      paragraphAppends += expectAppendEquivalent(document, chunks).paragraphAppends;
    }
    expect(paragraphAppends).toBeGreaterThan(10);
  });

  it.each([
    ['heading', '# Streaming heading keeps **rich text** readable.', 'line-block-append'],
    ['blockquote', '> Streaming quote keeps `code` readable.', 'line-block-append'],
    ['bullet list', '- Streaming list item keeps **rich text** readable.', 'list-line-append'],
    ['ordered list', '1. Streaming list item keeps **rich text** readable.', 'list-line-append'],
    [
      'table row',
      '| Header | Value |\n| --- | ---: |\n| Streaming row keeps growing | 123 |',
      'table-line-append',
    ],
  ] as const)('takes the %s direct boundary path without changing blocks', (_label, text, path) => {
    const result = expectAppendEquivalent(text, [1]);
    expect(result.paths).toContain(path);
  });

  it('keeps isolated block rendering aligned with the full lexer when no definition crosses blocks', () => {
    for (const document of TRANSITION_CORPUS) {
      if (document.includes(']:')) continue;
      for (const prefix of prefixes(document, [1, 2, 5, 13])) {
        expectFullLexEquivalent(prefix);
      }
    }
  });

  it('keeps a CRLF aligned table in one rendered block', () => {
    const table = '| Header | Value |\r\n| --- | ---: |\r\n| row | 1 |\r\n\r\n';
    const result = expectAppendEquivalent(table, [1, 2, 5]);
    const blocks = parseBlocks(table);

    expect(blocks).toHaveLength(1);
    expectFullLexEquivalent(table);
    expect(result.paths).toContain('table-line-append');
  });

  it.each([
    ['task items', '- [ ] pending\n- [x] done\n- final item'],
    ['loose items', '- first\n\n  continuation\n\n- second\n\n  more'],
    ['nested items', '- outer\n  - child one\n  - child two\n- outer two'],
    ['alphabetic items', 'a. alpha\nb. beta\nc. gamma'],
    ['Roman items', 'IV. four\nV. five\nVI. six'],
    ['CRLF items', '- first\r\n- second\r\n- third'],
  ])('keeps lightweight list boundaries exact for %s', (_label, list) => {
    const result = expectAppendEquivalent(list, [1, 3, 11]);
    const cache = createParseBlocksCache();
    parseBlocks(list, [], cache);

    expect(cache.trailingBlock?.kind).toBe('list');
    expect(result.paths).toContain('list-descent');
    expectFullLexEquivalent(list);
  });

  it.each([
    [
      'one body row',
      '| H | V |\n| --- | --- |\n| a | 1 |',
      'table-line',
    ],
    [
      'two body rows',
      '| H | V |\n| --- | --- |\n| a | 1 |\n| b | 2 |',
      'table',
    ],
    [
      'footer marker',
      '| H | V |\n| --- | --- |\n| a | 1 |\n| --- | --- |\n| total | 1 |',
      'table-line',
    ],
    [
      'CRLF rows',
      '| H | V |\r\n| --- | --- |\r\n| a | 1 |\r\n| b | 2 |',
      'table',
    ],
    [
      'simple rows without alignment',
      '| a | 1 |\n| b | 2 |\n| c | 3 |',
      null,
    ],
  ] as const)('keeps lightweight table geometry exact for %s', (_label, table, kind) => {
    expectAppendEquivalent(table, [1, 3, 11]);
    const cache = createParseBlocksCache();
    parseBlocks(table, [], cache);

    expect(cache.trailingBlock?.kind ?? null).toBe(kind);
    expectFullLexEquivalent(table);
  });

  it('matches fresh block boundaries for deterministic mixed-markdown streams', () => {
    let state = 0x5eed1234;
    const random = (limit: number): number => {
      state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0;
      return state % limit;
    };
    const blocks = [
      (n: number) => `Ordinary paragraph ${n} with **bold**, \`code\`, and 東京.`,
      (n: number) => `${'#'.repeat(1 + (n % 6))} Heading ${n}`,
      (n: number) => `> Quote ${n} with [a link](https://example.test/${n}).`,
      (n: number) => `- item ${n}\n- item ${n + 1}`,
      (n: number) => `${n + 1}. ordered ${n}\n${n + 2}. ordered ${n + 1}`,
      (n: number) => `| H${n} | Value |\n| --- | ---: |\n| row ${n} | ${n * 3} |`,
      (n: number) => `\`\`\`ts\nconst value${n} = ${n};\n\`\`\``,
      (n: number) => `Setext ${n}\n${n % 2 === 0 ? '---' : '==='}`,
      (n: number) => `<div data-row="${n}">HTML ${n}</div>`,
      (n: number) => `[center]\nCentered ${n}\n[/center]`,
      (n: number) => `[^note${n}]: footnote ${n}\n\nUses note[^note${n}].`,
      (n: number) => `:term${n}: description ${n}`,
    ];

    for (let documentIndex = 0; documentIndex < 48; documentIndex++) {
      const parts: string[] = [];
      const blockCount = 2 + random(5);
      for (let blockIndex = 0; blockIndex < blockCount; blockIndex++) {
        parts.push(blocks[random(blocks.length)](documentIndex * 10 + blockIndex));
      }
      const document = parts.join(random(3) === 0 ? '\r\n\r\n' : '\n\n');
      const chunks = [1 + random(7), 1 + random(19), 1 + random(37)];
      expectAppendEquivalent(document, chunks);
      if (!document.includes(']:')) {
        for (const prefix of prefixes(document, chunks)) {
          expectFullLexEquivalent(prefix);
        }
      }
    }
  });

  it('invalidates sealed boundaries when a block extension changes', () => {
    const customBlock: Extension = {
      name: 'split-alpha',
      level: 'block',
      applyInBlockParsing: true,
      tokenizer(source) {
        if (!source.startsWith('Alpha!')) return undefined;
        return { type: 'split-alpha', raw: 'Alpha!' };
      },
    };
    const cache = createParseBlocksCache();
    const initial = 'Alpha';
    parseBlocks(initial, [], cache);

    const next = 'Alpha!Beta';
    const append = createProvenAppend(initial, next.slice(initial.length));
    expect(parseBlocks(next, [customBlock], cache, append)).toEqual([
      'Alpha!',
      'Beta',
    ]);
    expect(cache.lastPath).toBe('full');
    expect(cache.extKey).toEqual([customBlock]);
  });

  it('does no block-parser work when a reactive rerun repeats the same source', () => {
    const observeLex = vi.fn();
    const cache = createParseBlocksCache(observeLex);
    const source = 'Ordinary paragraph text.\n\n- first item\n- second item';
    const first = parseBlocks(source, [], cache);
    const callsAfterFirstParse = observeLex.mock.calls.length;

    const repeated = parseBlocks(source, [], cache);

    expect(repeated).toBe(first);
    expect(observeLex).toHaveBeenCalledTimes(callsAfterFirstParse);
  });

  it('keeps outer block boundaries when only an inline extension identity changes', () => {
    const inlineExtension = (name: string): Extension => ({
      name,
      level: 'inline',
      tokenizer() {
        return undefined;
      },
    });
    const observeLex = vi.fn();
    const cache = createParseBlocksCache(observeLex);
    const source = 'One paragraph.\n\nAnother paragraph.';
    const first = parseBlocks(source, [inlineExtension('first')], cache);
    const callsAfterFirstParse = observeLex.mock.calls.length;

    const repeated = parseBlocks(source, [inlineExtension('second')], cache);

    expect(repeated).toBe(first);
    expect(observeLex).toHaveBeenCalledTimes(callsAfterFirstParse);
  });

  it('recovers from replacement before taking the paragraph append path again', () => {
    const cache = createParseBlocksCache();
    const first = 'The first paragraph keeps growing.';
    expectAppendEquivalent(first, [3, 5, 7]);
    parseBlocks(first, [], cache);

    const replacement = 'Fresh replacement text';
    expect(parseBlocks(replacement, [], cache)).toEqual(parseBlocks(replacement, []));
    expect(cache.lastPath).toBe('full');

    const next = `${replacement} keeps growing.`;
    expect(parseBlocks(
      next,
      [],
      cache,
      createProvenAppend(replacement, ' keeps growing.'),
    )).toEqual(parseBlocks(next, []));
    expect(cache.lastPath).toBe('paragraph-append');
  });

  it('does not append across an omitted blank-line token', () => {
    const cache = createParseBlocksCache();
    const initial = 'The first paragraph.\n\n';
    parseBlocks(initial, [], cache);
    expect(cache.trailingBlock).toBeNull();

    const next = `${initial}Next block.`;
    expect(parseBlocks(
      next,
      [],
      cache,
      createProvenAppend(initial, 'Next block.'),
    )).toEqual(['The first paragraph.', 'Next block.']);
    expect(cache.lastPath).toBe('append-tail');
  });
});
