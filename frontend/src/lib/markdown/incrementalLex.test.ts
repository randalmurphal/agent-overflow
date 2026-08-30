// Equivalence proof for the svelte-streamdown incremental-lex patch hunk.
//
// `incrementalLex` promises IDENTICAL output to a fresh `lex` at every
// streamed prefix — the fast paths (re-lex from the last list item or table
// source row, splice onto sealed reference-identical tokens) must be
// invisible in the token tree. This suite streams a corpus of shapes
// through the cache under several chunkings and deep-compares against the
// full lex at every step, then separately asserts the two properties that
// make the patch worth carrying: sealed tokens keep their object
// references (what lets Svelte skip their subtrees), and the fast paths
// actually engage (`lastPath` breadcrumb), so a silent regression to full
// re-lexing fails loudly.
//
// Reference-link definitions and footnote definitions inside the streamed
// block are handled exactly, not excluded: a definition DECLARED in the
// live tail bails that tick to a full re-lex (keeping marked's first-wins
// table current while the definition line itself streams), and sealed
// definitions are carried into tail re-lexes by seeding the merge lexer.
// The corpus contains both patterns on purpose. Cross-BLOCK definitions
// never resolved mid-stream in the first place (each Block lexes its
// string in isolation) — that pre-existing upstream property is out of
// scope here.
import { describe, expect, it, vi } from 'vitest';
import { expectCleanTransitions } from '../../test/helpers/transitions';
import { describeFirstDivergence } from '../../test/helpers/firstDivergence';
import {
  assertTimingContract,
  type PerfContractContext,
} from '../../test/helpers/perfContract';
import {
  createIncrementalLexCache,
  createMaterializedProvenAppend,
  createParseBlocksCache,
  createProvenAppend,
  incrementalLex,
  lex,
  parseBlocks,
  parseIncompleteMarkdown,
  updateParseBlockStringMaterialization,
} from './index';

interface CorpusDoc {
  name: string;
  text: string;
  /** Which fast path this shape must engage over a long stream; null for
   *  shapes that only promise equivalence (fallback-heavy or too small). */
  descent: 'list-append' | 'table-append' | null;
}

const bullets = (n: number, line: (i: number) => string): string =>
  Array.from({ length: n }, (_, i) => line(i)).join('\n');

const tableOf = (n: number, row: (i: number) => string, header = '| Item | Detail | Pass |\n| --- | :---: | ---: |'): string =>
  `${header}\n${Array.from({ length: n }, (_, i) => row(i)).join('\n')}`;

const CORPUS: CorpusDoc[] = [
  {
    name: 'tight bullets with inline markup',
    text: bullets(24, (i) => `- Item ${i}: the \`resolver\` keeps a **steady** cadence on [pass ${i}](https://example.com/${i}).`),
    descent: 'list-append',
  },
  {
    name: 'loose bullets (blank-line separated)',
    text: bullets(16, (i) => `- Loose item ${i} with *emphasis* and detail.`).replaceAll('\n', '\n\n'),
    descent: 'list-append',
  },
  {
    name: 'tight list flipping loose midway',
    text: `${bullets(8, (i) => `- Early tight item ${i}.`)}\n- Item eight.\n\n- Item nine after a blank.\n- Item ten.`,
    descent: 'list-append',
  },
  {
    name: 'nested list',
    text: bullets(12, (i) =>
      i % 3 === 0
        ? `- Parent ${i} with children:\n  - child ${i}.a has \`code\`\n  - child ${i}.b`
        : `- Plain parent ${i}`),
    descent: 'list-append',
  },
  {
    name: 'ordered decimal sequential',
    text: bullets(15, (i) => `${i + 1}. Step ${i + 1} of the procedure runs cleanly.`),
    descent: 'list-append',
  },
  {
    name: 'ordered with skipped values',
    text: '1. first\n2. second\n9. ninth out of order\n10. tenth\n4. fourth again',
    descent: 'list-append',
  },
  {
    name: 'lower-alpha ordered',
    text: bullets(6, (i) => `${String.fromCharCode(97 + i)}. option ${i}`),
    descent: 'list-append',
  },
  {
    name: 'task list',
    text: bullets(10, (i) => `- [${i % 2 === 0 ? 'x' : ' '}] task ${i} with a [link](https://example.com)`),
    descent: 'list-append',
  },
  {
    name: 'item containing a fenced code block',
    text: '- first item\n- second item with code:\n  ```ts\n  const x = 1;\n  const y = 2;\n  ```\n- third item after the fence\n- fourth item',
    descent: 'list-append',
  },
  {
    name: 'items with lazy continuation lines',
    text: '- first item\ncontinues lazily on the next line\n- second item\nalso continues\n- third',
    descent: 'list-append',
  },
  {
    name: 'list closed by a trailing paragraph',
    text: `${bullets(8, (i) => `- item ${i}`)}\n\nA closing paragraph after the list ends it for good.`,
    descent: 'list-append',
  },
  {
    name: 'paragraph, list, paragraph',
    text: `An opening paragraph.\n\n${bullets(10, (i) => `- point ${i}`)}\n\nAnd a closer.`,
    descent: null,
  },
  {
    name: 'blockquote inside an item',
    text: '- first\n- second holds a quote:\n  > quoted line one\n  > quoted line two\n- third',
    descent: 'list-append',
  },
  {
    name: 'loose list defining a reference link other items use',
    text: [
      '- First item references [the docs][d] before the definition exists.',
      '',
      '- Second item carries the definition:',
      '',
      '  [d]: https://example.com/docs "Docs"',
      '',
      '- Third item uses [the docs][d] after it sealed.',
      '',
      '- Fourth item links [again][d] and once [more][d].',
      '',
      '- Fifth item closes the stream with plain text.',
    ].join('\n'),
    descent: 'list-append',
  },
  {
    name: 'footnote defined inside an item, cited before and after',
    text: [
      '- First item cites[^a] before the definition arrives.',
      '- Second item holds it:',
      '  [^a]: The footnote text streams in inside this item.',
      '- Third item cites[^a] again afterwards.',
      '- Fourth item is plain.',
    ].join('\n'),
    descent: 'list-append',
  },
  {
    name: 'aligned table with inline markup cells',
    text: tableOf(20, (i) => `| Item ${i} | the \`resolver\` stays **steady** | [pass ${i}](https://example.com/${i}) |`),
    descent: 'table-append',
  },
  {
    name: 'table with escaped pipes and code-span pipes',
    text: tableOf(14, (i) => `| ${i} | escaped \\| pipe ${i} | \`a | b\` in code |`),
    descent: 'table-append',
  },
  {
    name: 'table with colspan rows',
    text: tableOf(12, (i) => (i % 4 === 0 ? `| spanning ${i} || tail ${i} |` : `| ${i} | mid ${i} | end ${i} |`)),
    descent: 'table-append',
  },
  {
    name: 'multi-row header table',
    text: tableOf(
      12,
      (i) => `| ${i} | body ${i} | tail ${i} |`,
      '| Region | Q1 | Q2 |\n| Detail | a | b |\n| --- | --- | --- |',
    ),
    descent: 'table-append',
  },
  {
    name: 'table with rowspan carets (append-unsafe rows)',
    text: tableOf(8, (i) => (i % 3 === 1 ? `| ^ | merge ${i} | ${i} |` : `| cell ${i} | keep ${i} | ${i} |`)),
    descent: null,
  },
  {
    name: 'table with a footer marker row',
    text: `${tableOf(6, (i) => `| ${i} | val ${i} | ok |`)}\n| --- | --- | --- |\n| totals | 21 | done |`,
    descent: null,
  },
  {
    name: 'small no-alignment table',
    text: '| 1 | 2 |\n| 3 | 4 |\n| 5 | 6 |',
    descent: null,
  },
  {
    name: 'footnote refs inside table cells',
    text: tableOf(8, (i) => `| ${i} | cites[^t] here | ${i * 2} |`),
    descent: 'table-append',
  },
  {
    name: 'blockquote block (fallback shape)',
    text: '> line one of the quote\n> line two of the quote\n> line three keeps going',
    descent: null,
  },
  {
    name: 'unclosed bold cut mid-stream',
    text: bullets(8, (i) => `- item ${i} has **bold ${i}** and math $x_${i}$ inline`),
    descent: 'list-append',
  },
];

const CHUNKINGS: Array<{ name: string; sizes: (docLen: number) => number[] }> = [
  { name: 'reveal-tick (21 chars)', sizes: () => [21] },
  { name: 'small (7 chars)', sizes: () => [7] },
  { name: 'bursty (250 chars)', sizes: () => [250] },
  {
    name: 'seeded random 3..80',
    sizes: () => {
      // Deterministic LCG so failures reproduce.
      let s = 42;
      const out: number[] = [];
      for (let i = 0; i < 4096; i++) {
        s = (s * 1103515245 + 12345) % 2147483648;
        out.push(3 + (s % 78));
      }
      return out;
    },
  },
];

function* prefixes(text: string, sizes: number[]): Generator<string> {
  let at = 0;
  let i = 0;
  while (at < text.length) {
    at = Math.min(text.length, at + sizes[i % sizes.length]);
    i += 1;
    yield text.slice(0, at);
  }
}

const fullReference = (prefix: string, complete: boolean) =>
  lex(complete ? parseIncompleteMarkdown(prefix.trim()) : prefix);

describe('incrementalLex completion-mode cache transitions', () => {
  // One cache serves both modes: the live tail lexes through
  // `parseIncompleteMarkdown`, a sealed block lexes raw. `cache.completeKey`
  // is what keeps the two apart, and the failure mode is silent — a stale
  // key reuses tokens completed under the other mode. Drive the flips.
  it('carries no state across a completion-mode flip', () => {
    const source = '- alpha item one\n- bravo item two\n- charlie **bold** and `code';
    const cache = createIncrementalLexCache();
    const raw = () => incrementalLex(source, [], cache, null);
    const completing = () => incrementalLex(source, [], cache, parseIncompleteMarkdown);

    // Enter disengaged: raw is the resting mode this subject returns to.
    raw();

    expectCleanTransitions('incrementalLex completion mode', {
      on: () => { completing(); },
      off: () => { raw(); },
      whileOn: () => {
        expect(cache.completeKey).not.toBeNull();
        expect(incrementalLex(source, [], cache, parseIncompleteMarkdown))
          .toEqual(fullReference(source, true));
      },
      onAgain: () => { completing(); },
      inFlight: () => {
        // A longer prefix arriving while completion is engaged, so the
        // flip back lands on a cache holding tail state from the other mode.
        incrementalLex(`${source} tail`, [], cache, parseIncompleteMarkdown);
      },
      read: () => ({
        completeKey: cache.completeKey,
        tokens: incrementalLex(source, [], cache, null),
        reference: fullReference(source, false),
      }),
    });

    expect(raw()).toEqual(fullReference(source, false));
  });
});

describe('incrementalLex streamed equivalence', () => {
  it('reports only calls that perform parser work', () => {
    const observed: Array<{ path: string; inputLength: number }> = [];
    const cache = createIncrementalLexCache((path, inputLength) => {
      observed.push({ path, inputLength });
    });
    const initial = 'A paragraph';
    incrementalLex(initial, [], cache, parseIncompleteMarkdown);
    incrementalLex(initial, [], cache, parseIncompleteMarkdown);
    incrementalLex(`${initial} grows`, [], cache, parseIncompleteMarkdown);

    expect(observed).toEqual([
      { path: 'full', inputLength: initial.length },
      { path: 'full', inputLength: `${initial} grows`.length },
    ]);
  });

  for (const doc of CORPUS) {
    for (const chunking of CHUNKINGS) {
      for (const complete of [true, false]) {
        it(`${doc.name} / ${chunking.name} / complete=${complete}`, () => {
          const cache = createIncrementalLexCache();
          let descents = 0;
          let steps = 0;
          for (const prefix of prefixes(doc.text, chunking.sizes(doc.text.length))) {
            const incremental = incrementalLex(
              prefix,
              [],
              cache,
              complete ? parseIncompleteMarkdown : null,
            );
            const reference = fullReference(prefix, complete);
            if (JSON.stringify(incremental) !== JSON.stringify(reference)) {
              // The raw trees run to megabytes; report only the window.
              expect.fail(
                `token divergence at prefix length ${prefix.length} (path=${cache.lastPath})\n` +
                `stream tail: ${JSON.stringify(prefix.slice(-80))}\n` +
                describeFirstDivergence(incremental, reference),
              );
            }
            if (doc.descent && cache.lastPath === doc.descent) descents += 1;
            steps += 1;
          }
          // A doc consumed in a couple of chunks never has an append landing
          // on an established sealed prefix, so engagement is only
          // assertable on streams with real step counts.
          if (doc.descent && steps >= 4) {
            expect(descents, 'the fast path must actually engage').toBeGreaterThan(0);
          }
        });
      }
    }
  }

  it('reuses sealed item token references across appends', () => {
    const text = bullets(30, (i) => `- Item ${i} with \`code\` and **bold** content here.`);
    const cache = createIncrementalLexCache();
    let previousItems: unknown[] | null = null;
    let reuseChecked = 0;
    for (const prefix of prefixes(text, [21])) {
      const tokens = incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
      if (cache.lastPath === 'list-append' && previousItems) {
        const items = (tokens[0] as { tokens: unknown[] }).tokens;
        // Every item sealed at the previous step (all but its last) must
        // survive by reference — that identity is what lets Svelte skip
        // the subtree. The previous LAST item may be re-minted.
        for (let i = 0; i < previousItems.length - 1; i++) {
          expect(items[i]).toBe(previousItems[i]);
        }
        reuseChecked += 1;
      }
      previousItems = tokens[0]?.type === 'list'
        ? [...(tokens[0] as { tokens: unknown[] }).tokens]
        : null;
    }
    expect(reuseChecked).toBeGreaterThan(20);
  });

  it('reuses sealed table row references across appends', () => {
    const text = tableOf(30, (i) => `| Item ${i} | keeps \`code\` and **bold** | ${i * 3} |`);
    const cache = createIncrementalLexCache();
    const rowsOf = (tokens: unknown[]): unknown[] | null => {
      const table = tokens[0] as { type?: string; tokens?: Array<{ type: string; tokens: unknown[] }> };
      if (table?.type !== 'table') return null;
      const tbody = table.tokens?.find((section) => section.type === 'tbody');
      return tbody ? tbody.tokens : null;
    };
    let previousRows: unknown[] | null = null;
    let reuseChecked = 0;
    for (const prefix of prefixes(text, [21])) {
      const tokens = incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
      const rows = rowsOf(tokens);
      if (cache.lastPath === 'table-append' && previousRows && rows) {
        // Sealed rows = every previous row except the last (the last source
        // line stays volatile until a newline commits it).
        for (let i = 0; i < previousRows.length - 1; i++) {
          expect(rows[i]).toBe(previousRows[i]);
        }
        reuseChecked += 1;
      }
      previousRows = rows ? [...rows] : null;
    }
    expect(reuseChecked).toBeGreaterThan(20);
  });

  it('handles grow → replace → grow transitions', () => {
    const listA = bullets(10, (i) => `- alpha ${i}`);
    const listB = bullets(10, (i) => `1. beta ${i}`);
    const cache = createIncrementalLexCache();
    for (const prefix of prefixes(listA, [21])) {
      incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
    }
    // Wholesale replacement (thread switch, correction) must fall back to a
    // full lex, then resume descending on the new stream.
    let descentsAfterReplace = 0;
    for (const prefix of prefixes(listB, [21])) {
      const tokens = incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
      expect(JSON.stringify(tokens)).toBe(JSON.stringify(fullReference(prefix, true)));
      if (cache.lastPath === 'list-append') descentsAfterReplace += 1;
    }
    expect(descentsAfterReplace).toBeGreaterThan(0);
  });

  it('handles a list replaced by a table and back', () => {
    const cache = createIncrementalLexCache();
    const table = tableOf(10, (i) => `| ${i} | value ${i} | ok |`);
    for (const prefix of prefixes(bullets(10, (i) => `- alpha ${i}`), [21])) {
      incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
    }
    let tableDescents = 0;
    for (const prefix of prefixes(table, [21])) {
      const tokens = incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
      expect(JSON.stringify(tokens)).toBe(JSON.stringify(fullReference(prefix, true)));
      if (cache.lastPath === 'table-append') tableDescents += 1;
    }
    expect(tableDescents).toBeGreaterThan(0);
  });

  it('is idempotent for an unchanged input', () => {
    const cache = createIncrementalLexCache();
    const text = '- one\n- two\n- three';
    const first = incrementalLex(text, [], cache, parseIncompleteMarkdown);
    const second = incrementalLex(text, [], cache, parseIncompleteMarkdown);
    expect(second).toBe(first);
  });

  it('keeps an open fenced-code stream equivalent through partial and real closers', () => {
    const text = [
      '````ts title="stream"',
      'const first = `literal`;',
      '```',
      'const second = 2;',
      '``',
      'const third = 3;',
      '`````',
      '',
      'Paragraph after the closed fence.',
    ].join('\n');
    const cache = createIncrementalLexCache();
    let previous = '';
    let codeAppends = 0;
    for (const prefix of prefixes(text, [1, 2, 7, 19])) {
      const delta = prefix.slice(previous.length);
      const append = createProvenAppend(previous, delta);
      const incremental = incrementalLex(
        prefix,
        [],
        cache,
        parseIncompleteMarkdown,
        append,
      );
      expect(incremental).toEqual(fullReference(prefix, true));
      if (cache.lastPath === 'code-append') codeAppends += 1;
      previous = prefix;
    }
    expect(codeAppends, 'the open-fence append path must engage').toBeGreaterThan(5);
  });

  it('falls back when a valid fence closer gains trailing tabs', () => {
    const seed = '```ts\nconst value = 1;\n';
    const partial = `${seed}\`\``;
    const complete = `${partial}\`\t\n\nParagraph after the fence.`;
    const cache = createIncrementalLexCache();
    incrementalLex(seed, [], cache, parseIncompleteMarkdown);
    incrementalLex(
      partial,
      [],
      cache,
      parseIncompleteMarkdown,
      createProvenAppend(seed, partial.slice(seed.length)),
    );
    expect(cache.lastPath).toBe('code-append');

    const append = createProvenAppend(partial, complete.slice(partial.length));
    expect(incrementalLex(
      complete,
      [],
      cache,
      parseIncompleteMarkdown,
      append,
    )).toEqual(fullReference(complete, true));
    expect(cache.lastPath).toBe('full');

    const blockCache = createParseBlocksCache();
    parseBlocks(seed, [], blockCache);
    parseBlocks(
      partial,
      [],
      blockCache,
      createProvenAppend(seed, partial.slice(seed.length)),
    );
    expect(parseBlocks(
      complete,
      [],
      blockCache,
      createProvenAppend(partial, complete.slice(partial.length)),
    )).toEqual(parseBlocks(complete, []));
    expect(blockCache.trailingBlock?.kind).not.toBe('fence');
  });

  it('rejects stale and fabricated append proofs across source replacements', () => {
    const first = '```ts\nalpha line';
    const replacement = '```ts\nbeta! line';

    const blockCache = createParseBlocksCache();
    parseBlocks(first, [], blockCache);
    const stale = createProvenAppend('unrelated source', replacement);
    expect(parseBlocks(replacement, [], blockCache, stale)).toEqual(
      parseBlocks(replacement, []),
    );
    expect(blockCache.lastBlockAppend).toBeUndefined();

    const fabricated = {
      previous: first,
      delta: replacement.slice(first.length),
      next: replacement,
    } as unknown as ReturnType<typeof createProvenAppend>;
    parseBlocks(first, [], blockCache);
    expect(parseBlocks(replacement, [], blockCache, fabricated)).toEqual(
      parseBlocks(replacement, []),
    );
    expect(blockCache.lastBlockAppend).toBeUndefined();

    const lexCache = createIncrementalLexCache();
    incrementalLex(first, [], lexCache, parseIncompleteMarkdown);
    const tokens = incrementalLex(
      replacement,
      [],
      lexCache,
      parseIncompleteMarkdown,
      fabricated,
    );
    expect(tokens).toEqual(fullReference(replacement, true));
    expect(lexCache.lastPath).toBe('full');
  });

  it('accepts an independently materialized append on both incremental layers', () => {
    const previous = '- alpha\n- beta';
    const deltas = ['\n- gamma', '\n- delta'];
    const append = createMaterializedProvenAppend(previous, deltas);
    expect(append).toEqual({
      previous,
      delta: deltas.join(''),
      next: previous + deltas.join(''),
    });
    expect(Object.isFrozen(append)).toBe(true);

    const blockCache = createParseBlocksCache();
    parseBlocks(previous, [], blockCache);
    expect(parseBlocks(append.next, [], blockCache, append)).toEqual(
      parseBlocks(append.next, []),
    );

    const lexCache = createIncrementalLexCache();
    incrementalLex(previous, [], lexCache, parseIncompleteMarkdown);
    expect(incrementalLex(
      append.next,
      [],
      lexCache,
      parseIncompleteMarkdown,
      append,
    )).toEqual(fullReference(append.next, true));
    expect(lexCache.lastPath).toBe('list-append');
  });

  it('materializes many pending deltas without a spread-sized append operation', () => {
    const deltas = Array.from({ length: 20_000 }, (_, index) =>
      String.fromCharCode(97 + (index % 26)),
    );
    const append = createMaterializedProvenAppend('seed:', deltas);
    expect(append.previous).toBe('seed:');
    expect(append.delta).toBe(deltas.join(''));
    expect(append.next).toBe(`seed:${deltas.join('')}`);
  });

  it('detaches cached block strings across append and replacement transitions', () => {
    const first = [
      'Paragraph one.',
      '',
      'Paragraph two.',
      '',
      'Paragraph three.',
      '',
      'Paragraph four.',
      '',
      'Paragraph five with unicode 🚀 and an unpaired surrogate \ud800.',
    ].join('\n');
    const cache = createParseBlocksCache();
    const blocks = parseBlocks(first, [], cache);
    expect(cache.materializationEnabled).toBe(false);

    const initialJoin = vi.spyOn(Array.prototype, 'join');
    updateParseBlockStringMaterialization(cache, true);
    expect(initialJoin).toHaveBeenCalledTimes(blocks.length);
    initialJoin.mockRestore();
    expect(cache.blocks).toBe(blocks);
    expect(cache.materializationEnabled).toBe(true);
    expect(cache.materializedBlocks).toEqual(cache.blocks);
    expect(cache.dirtyBlockStart).toBe(cache.blocks.length);
    expect(cache.raws.join('')).toBe(first);
    expect(cache.blocks).toEqual(parseBlocks(first, []));

    // A repeated call is a no-op. An append reuses the independent value for
    // an unchanged reparsed block and copies only the block that really grew.
    const unchangedJoin = vi.spyOn(Array.prototype, 'join');
    updateParseBlockStringMaterialization(cache, true);
    expect(unchangedJoin).not.toHaveBeenCalled();
    unchangedJoin.mockRestore();
    const delta = ' keeps growing.';
    const second = first + delta;
    expect(parseBlocks(
      second,
      [],
      cache,
      createProvenAppend(first, delta),
    )).toEqual(parseBlocks(second, []));
    expect(cache.dirtyBlockStart).toBeLessThan(cache.blocks.length);
    const appendJoin = vi.spyOn(Array.prototype, 'join');
    updateParseBlockStringMaterialization(cache, true);
    expect(appendJoin).toHaveBeenCalledTimes(1);
    appendJoin.mockRestore();
    expect(cache.dirtyBlockStart).toBe(cache.blocks.length);
    expect(cache.raws.join('')).toBe(second);

    // A non-append replacement rebuilds the cache, after which the same
    // operation detaches every new block without stale history.
    const replacement = '# Replacement\n\nFresh body.';
    expect(parseBlocks(replacement, [], cache)).toEqual(
      parseBlocks(replacement, []),
    );
    expect(cache.dirtyBlockStart).toBe(0);
    updateParseBlockStringMaterialization(cache, true);
    expect(cache.materializedBlocks).toEqual(cache.blocks);
    expect(cache.raws.join('')).toBe(replacement);

    // Mode transitions release the independent history. Re-enabling with an
    // unchanged parse must revisit every block rather than trusting stale state.
    updateParseBlockStringMaterialization(cache, false);
    updateParseBlockStringMaterialization(cache, false);
    expect(cache.materializationEnabled).toBe(false);
    expect(cache.materializedBlocks).toEqual([]);
    expect(cache.dirtyBlockStart).toBe(0);
    const reenabledJoin = vi.spyOn(Array.prototype, 'join');
    updateParseBlockStringMaterialization(cache, true);
    expect(reenabledJoin).toHaveBeenCalledTimes(cache.blocks.length);
    reenabledJoin.mockRestore();
    expect(cache.materializationEnabled).toBe(true);
  });

  it('rejects an empty materialized append', () => {
    expect(() => createMaterializedProvenAppend('seed', [])).toThrow(
      'materialized append needs one or more non-empty deltas',
    );
    expect(() => createMaterializedProvenAppend('seed', ['next', ''])).toThrow(
      'materialized append needs one or more non-empty deltas',
    );
  });
});

describe('incrementalLex token identity', () => {
  it('preserves ECMAScript trim semantics across proven whitespace appends', () => {
    const whitespace = [
      '\u0009', '\u000A', '\u000B', '\u000C', '\u000D', '\u0020', '\u00A0',
      '\u1680', '\u2000', '\u2001', '\u2002', '\u2003', '\u2004', '\u2005',
      '\u2006', '\u2007', '\u2008', '\u2009', '\u200A', '\u2028', '\u2029',
      '\u202F', '\u205F', '\u3000', '\uFEFF',
    ];

    for (const edge of whitespace) {
      const cache = createIncrementalLexCache();
      let source = edge;
      expect(incrementalLex(source, [], cache, parseIncompleteMarkdown))
        .toEqual(fullReference(source, true));
      for (const delta of [`value${edge}`, `${edge}more${edge}`, 'end']) {
        const append = createProvenAppend(source, delta);
        source = append.next;
        expect(incrementalLex(source, [], cache, parseIncompleteMarkdown, append))
          .toEqual(fullReference(source, true));
      }
    }

    const formerWhitespace = '\u180E';
    const cache = createIncrementalLexCache();
    expect(incrementalLex(
      `${formerWhitespace}value${formerWhitespace}`,
      [],
      cache,
      parseIncompleteMarkdown,
    )).toEqual(fullReference(`${formerWhitespace}value${formerWhitespace}`, true));
  });

  it('invalidates cached tokens when incomplete-markdown mode changes', () => {
    const cache = createIncrementalLexCache();
    const source = '```ts\nconst value = 1;';

    expect(incrementalLex(
      source,
      [],
      cache,
      parseIncompleteMarkdown,
    )).toEqual(lex(parseIncompleteMarkdown(source.trim())));
    expect(incrementalLex(source, [], cache, null)).toEqual(lex(source));
    expect(cache.lastPath).toBe('full');
    expect(incrementalLex(
      source,
      [],
      cache,
      parseIncompleteMarkdown,
    )).toEqual(lex(parseIncompleteMarkdown(source.trim())));
    expect(cache.lastPath).toBe('full');
  });

  it('reuses unchanged inline subtrees through a paragraph fallback', () => {
    const cache = createIncrementalLexCache();
    const first = incrementalLex(
      'Prefix **stable bold** before the live tail',
      [],
      cache,
      parseIncompleteMarkdown,
    );
    const paragraph = first[0] as { tokens: Array<{ type: string }> };
    const stable = paragraph.tokens.find((token) => token.type === 'strong');
    expect(stable).toBeDefined();

    const second = incrementalLex(
      'Prefix **stable bold** before the live tail grows.',
      [],
      cache,
      parseIncompleteMarkdown,
    );
    const nextParagraph = second[0] as { tokens: Array<{ type: string }> };

    expect(cache.lastPath).toBe('full');
    expect(nextParagraph).not.toBe(paragraph);
    expect(nextParagraph.tokens.find((token) => token.type === 'strong')).toBe(stable);
  });
});

describe('incremental lexing performance contract', () => {
  // The reason the patch hunk exists: an append to a large trailing list or
  // table must cost O(new content), not O(block). Relative bounds (against
  // the full path measured in the same warmed process) keep the assertion
  // robust to machine speed; the generous absolute cap is a second line
  // against a pathological regression that slows both paths together.
  // Pre-fix reference points on the profiling machine: full lex 27ms and
  // block-level append 5.9ms at a 120KB list — the 5× margins are far
  // outside noise in both directions.
  //
  // Robust to machine speed is not robust to a machine under LOAD: beside
  // the soak rig or a perf profile both paths stall unevenly and the ratio
  // fails while the code is fine. The measurement always runs; the
  // wall-clock assertion is gated on AO_PERF_CONTRACT=1 (set by `make
  // test`). The path breadcrumbs and the largest-input bound below are
  // deterministic work counts and stay unconditional.
  const bigList = bullets(660, (i) => `- Item ${i}: the \`resolver\` keeps a **steady** cadence while pass ${i} holds the viewport across its flush.`);
  const bigTable = tableOf(660, (i) => `| Item ${i} | the \`resolver\` keeps a **steady** cadence on pass ${i} | ${i * 7} |`);
  const bigFence = `\`\`\`ts\n${bullets(1600, (i) => `const value${i} = computeThing(alpha, beta); // streamed code line ${i}`)}`;

  const median = (samples: number[]): number => {
    const sorted = [...samples].sort((a, b) => a - b);
    return sorted[Math.floor(sorted.length / 2)];
  };

  const lexAppendContract = async (
    ctx: PerfContractContext,
    text: string,
    path: 'list-append' | 'table-append',
  ): Promise<void> => {
    const cache = createIncrementalLexCache();
    // Establish the stream mid-document, then measure steady-state appends.
    let previous = text.slice(0, text.length - 2100);
    incrementalLex(previous, [], cache, parseIncompleteMarkdown);
    const appendTimes: number[] = [];
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const proof = createProvenAppend(
        previous,
        text.slice(previous.length, text.length - cut),
      );
      const prefix = proof.next;
      const t0 = performance.now();
      incrementalLex(prefix, [], cache, parseIncompleteMarkdown, proof);
      appendTimes.push(performance.now() - t0);
      expect(cache.lastPath).toBe(path);
      previous = prefix;
    }
    const fullTimes: number[] = [];
    for (let i = 0; i < 5; i++) {
      const t0 = performance.now();
      lex(parseIncompleteMarkdown(text.trim()));
      fullTimes.push(performance.now() - t0);
    }
    const append = median(appendTimes);
    const full = median(fullTimes);
    await assertTimingContract(
      ctx,
      `append=${append.toFixed(3)}ms full=${full.toFixed(3)}ms`,
      () => {
        expect(
          append,
          `append=${append.toFixed(3)}ms full=${full.toFixed(3)}ms`,
        ).toBeLessThan(full / 5);
        expect(append).toBeLessThan(10);
      },
    );
  };

  it('incrementalLex list append costs far less than a full re-lex', async (ctx) => {
    await lexAppendContract(ctx, bigList, 'list-append');
  });

  it('incrementalLex table append costs far less than a full re-lex', async (ctx) => {
    await lexAppendContract(ctx, bigTable, 'table-append');
  });

  it('incrementalLex open-fence append takes the dedicated path', () => {
    const cache = createIncrementalLexCache();
    const initial = bigFence.slice(0, bigFence.length - 2100);
    incrementalLex(initial, [], cache, parseIncompleteMarkdown);
    let previous = initial;
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const prefix = bigFence.slice(0, bigFence.length - cut);
      const append = createProvenAppend(previous, prefix.slice(previous.length));
      incrementalLex(
        prefix,
        [],
        cache,
        parseIncompleteMarkdown,
        append,
      );
      expect(cache.lastPath).toBe('code-append');
      previous = prefix;
    }
  });

  it('does not slice the whole open-fence body for an ordinary append', () => {
    const cache = createIncrementalLexCache();
    const initial = `\`\`\`ts\n${'const value = 1;\n'.repeat(800)}partial`;
    incrementalLex(initial, [], cache, parseIncompleteMarkdown);
    const delta = ' suffix';
    const next = initial + delta;
    const append = createProvenAppend(initial, delta);
    const bodyStart = '```ts\n'.length;
    const originalSlice = String.prototype.slice;
    const slice = vi.spyOn(String.prototype, 'slice').mockImplementation(function (
      this: string,
      start?: number,
      end?: number,
    ): string {
      if (String(this) === next && start === bodyStart && end === next.length) {
        throw new Error('open fence body was sliced from its first line');
      }
      return Reflect.apply(originalSlice, this, [start, end]) as string;
    });
    try {
      incrementalLex(next, [], cache, parseIncompleteMarkdown, append);
    } finally {
      slice.mockRestore();
    }
    expect(cache.lastPath).toBe('code-append');
  });

  const parseBlocksAppendContract = async (
    ctx: PerfContractContext,
    doc: string,
    kind: 'list' | 'table',
  ): Promise<void> => {
    let maxLexInput = 0;
    const cache = createParseBlocksCache((_path, inputLength) => {
      maxLexInput = Math.max(maxLexInput, inputLength);
    });
    parseBlocks(doc.slice(0, doc.length - 2100), [], cache);
    maxLexInput = 0;
    const appendTimes: number[] = [];
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const prefix = doc.slice(0, doc.length - cut);
      const t0 = performance.now();
      parseBlocks(prefix, [], cache);
      appendTimes.push(performance.now() - t0);
    }
    expect(cache.trailingBlock?.kind, 'descent record must be live at scale').toBe(kind);
    const append = median(appendTimes);
    // Deterministic work bound: unconditional.
    expect(maxLexInput, `largest marked input was ${maxLexInput} of ${doc.length} code units`)
      .toBeLessThan(doc.length / 10);
    await assertTimingContract(
      ctx,
      `append=${append.toFixed(3)}ms`,
      () => { expect(append).toBeLessThan(10); },
    );
  };

  it('parseBlocks append with a trailing list costs far less than a fresh parse', async (ctx) => {
    await parseBlocksAppendContract(ctx, `Intro paragraph.\n\n${bigList}`, 'list');
  });

  it('parseBlocks append with a trailing table costs far less than a fresh parse', async (ctx) => {
    await parseBlocksAppendContract(ctx, `Intro paragraph.\n\n${bigTable}`, 'table');
  });

  it('parseBlocks descends into an open trailing fence', () => {
    const cache = createParseBlocksCache();
    const initial = bigFence.slice(0, bigFence.length - 2100);
    parseBlocks(initial, [], cache);
    let previous = initial;
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const prefix = bigFence.slice(0, bigFence.length - cut);
      const append = createProvenAppend(previous, prefix.slice(previous.length));
      const blocks = parseBlocks(
        prefix,
        [],
        cache,
        append,
      );
      expect(blocks).toEqual(parseBlocks(prefix, []));
      expect(cache.trailingBlock?.kind).toBe('fence');
      previous = prefix;
    }
  });

  it('updates one block array instead of copying every sealed block per append', () => {
    const sealed = Array.from(
      { length: 2_000 },
      (_, index) => `Paragraph ${index} stays sealed.`,
    ).join('\n\n');
    const firstSource = `${sealed}\n\nTrailing paragraph`;
    const cache = createParseBlocksCache();
    const blocks = parseBlocks(firstSource, [], cache);
    const nextSource = `${firstSource} keeps growing.`;
    const next = parseBlocks(
      nextSource,
      [],
      cache,
      createProvenAppend(firstSource, ' keeps growing.'),
    );

    expect(next).toBe(blocks);
    expect(next).toEqual(parseBlocks(nextSource, []));
    let offset = 0;
    for (let index = 0; index < cache.raws.length; index++) {
      expect(cache.rawStarts[index]).toBe(offset);
      const blockIndex = cache.blockIndexes[index];
      expect(blockIndex < 0 ? false : cache.blocks[blockIndex] === cache.raws[index])
        .toBe(cache.keep[index]);
      if (blockIndex >= 0) {
        expect(cache.blockRawIndexes[blockIndex]).toBe(index);
      }
      offset += cache.raws[index].length;
    }
    expect(offset).toBe(nextSource.length);
  });
});

describe('parseBlocks trailing-block descent', () => {
  const streamedBlocks = (text: string, chunk: number, kind: 'list' | 'table'): void => {
    const cache = createParseBlocksCache();
    let sawDescentState = false;
    for (const prefix of prefixes(text, [chunk])) {
      const incremental = parseBlocks(prefix, [], cache);
      const reference = parseBlocks(prefix, []);
      expect(incremental).toEqual(reference);
      if (cache.trailingBlock?.kind === kind) sawDescentState = true;
    }
    expect(sawDescentState, `descent record must engage on a trailing ${kind}`).toBe(true);
  };

  it('matches the fresh parse at every step of a growing list document', () => {
    streamedBlocks(
      `An opening paragraph that commits.\n\n${bullets(40, (i) => `- Item ${i} with \`inline\` and **markup** to lex.`)}`,
      21,
      'list',
    );
  });

  it('matches the fresh parse at every step of a growing table document', () => {
    streamedBlocks(
      `An opening paragraph that commits.\n\n${tableOf(30, (i) => `| Item ${i} | detail \`${i}\` | ${i * 2} |`)}`,
      21,
      'table',
    );
  });

  it('matches when the list closes and new blocks follow', () => {
    streamedBlocks(
      `${bullets(20, (i) => `- item ${i}`)}\n\nA paragraph after.\n\n\`\`\`ts\nconst x = 1;\n\`\`\`\n\n${bullets(6, (i) => `1. later ${i}`)}`,
      17,
      'list',
    );
  });

  it('matches when the table closes and new blocks follow', () => {
    streamedBlocks(
      `${tableOf(16, (i) => `| ${i} | mid ${i} | end ${i} |`)}\n\nA paragraph after the table.\n\n${bullets(6, (i) => `- later ${i}`)}`,
      17,
      'table',
    );
  });

  it('recovers across a wholesale content replacement', () => {
    const cache = createParseBlocksCache();
    const a = bullets(12, (i) => `- alpha ${i}`);
    const b = `Fresh start.\n\n${tableOf(12, (i) => `| ${i} | beta ${i} | ok |`)}`;
    for (const prefix of prefixes(a, [21])) parseBlocks(prefix, [], cache);
    for (const prefix of prefixes(b, [21])) {
      expect(parseBlocks(prefix, [], cache)).toEqual(parseBlocks(prefix, []));
    }
  });
});

describe('open-fence append differential fuzz', () => {
  const nextRandom = (state: { value: number }): number => {
    state.value = (Math.imul(state.value, 1_664_525) + 1_013_904_223) >>> 0;
    return state.value;
  };

  const makeFenceDoc = (index: number): string => {
    const state = { value: (0x9e37_79b9 ^ index) >>> 0 };
    const char = index % 2 === 0 ? '`' : '~';
    const length = 3 + (nextRandom(state) % 6);
    const fence = char.repeat(length);
    const lines = [`${' '.repeat(index % 4)}${fence}${char === '`' ? 'ts title=x' : 'python title=x'}`];
    const lineCount = 3 + (nextRandom(state) % 10);
    for (let line = 0; line < lineCount; line++) {
      const shape = nextRandom(state) % 7;
      if (shape === 0) lines.push(`${char.repeat(Math.max(1, length - 1))}`);
      else if (shape === 1) lines.push(`${' '.repeat(nextRandom(state) % 4)}${char.repeat(length)}x`);
      else if (shape === 2) lines.push(`${char === '`' ? '~' : '`'}`.repeat(length + 2));
      else if (shape === 3) lines.push(`const emoji_${line} = "😀";`);
      else if (shape === 4) lines.push(`  nested ${char.repeat(2)} literal ${line}`);
      else if (shape === 5) lines.push('');
      else lines.push(`value_${line} = compute(${nextRandom(state) % 10_000})`);
    }
    if (index % 3 !== 0) {
      const trailing = index % 4 === 0
        ? '  '
        : index % 4 === 1
          ? '\t '
          : '';
      lines.push(`${' '.repeat(nextRandom(state) % 4)}${char.repeat(length + (index % 2))}${trailing}`);
      lines.push('');
      lines.push(`paragraph after fence ${index}`);
    }
    const separator = index % 11 === 0 ? '\r\n' : '\n';
    return lines.join(separator);
  };

  for (let index = 0; index < 64; index++) {
    it(`matches fresh block and token parses for generated fence ${index}`, () => {
      const source = makeFenceDoc(index);
      const blockCache = createParseBlocksCache();
      const lexCache = createIncrementalLexCache();
      const state = { value: (0xa511_e9b3 ^ index) >>> 0 };
      let previous = '';
      let offset = 0;
      while (offset < source.length) {
        offset = Math.min(source.length, offset + 1 + (nextRandom(state) % 17));
        const prefix = source.slice(0, offset);
        const append = createProvenAppend(
          previous,
          prefix.slice(previous.length),
        );
        expect(parseBlocks(prefix, [], blockCache, append)).toEqual(
          parseBlocks(prefix, []),
        );
        expect(
          incrementalLex(
            prefix,
            [],
            lexCache,
            parseIncompleteMarkdown,
            append,
          ),
        ).toEqual(fullReference(prefix, true));
        previous = prefix;
      }
    });
  }
});
