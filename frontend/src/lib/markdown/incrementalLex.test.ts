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
import { describe, expect, it } from 'vitest';
import {
  createIncrementalLexCache,
  createParseBlocksCache,
  incrementalLex,
  lex,
  parseBlocks,
  parseIncompleteMarkdown,
} from 'svelte-streamdown';

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

describe('incrementalLex streamed equivalence', () => {
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
            const incStr = JSON.stringify(incremental);
            const refStr = JSON.stringify(reference);
            if (incStr !== refStr) {
              // The raw strings run to megabytes and the reporter truncates
              // them into uselessness — fail with the divergence window.
              let d = 0;
              while (d < Math.min(incStr.length, refStr.length) && incStr[d] === refStr[d]) d++;
              expect.fail(
                `token divergence at prefix length ${prefix.length} (path=${cache.lastPath})\n` +
                `stream tail: ${JSON.stringify(prefix.slice(-80))}\n` +
                `expected …${refStr.slice(Math.max(0, d - 200), d + 240)}…\n` +
                `received …${incStr.slice(Math.max(0, d - 200), d + 240)}…`,
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
  const bigList = bullets(660, (i) => `- Item ${i}: the \`resolver\` keeps a **steady** cadence while pass ${i} holds the viewport across its flush.`);
  const bigTable = tableOf(660, (i) => `| Item ${i} | the \`resolver\` keeps a **steady** cadence on pass ${i} | ${i * 7} |`);

  const median = (samples: number[]): number => {
    const sorted = [...samples].sort((a, b) => a - b);
    return sorted[Math.floor(sorted.length / 2)];
  };

  const lexAppendContract = (text: string, path: 'list-append' | 'table-append'): void => {
    const cache = createIncrementalLexCache();
    // Establish the stream mid-document, then measure steady-state appends.
    incrementalLex(text.slice(0, text.length - 2100), [], cache, parseIncompleteMarkdown);
    const appendTimes: number[] = [];
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const prefix = text.slice(0, text.length - cut);
      const t0 = performance.now();
      incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
      appendTimes.push(performance.now() - t0);
      expect(cache.lastPath).toBe(path);
    }
    const fullTimes: number[] = [];
    for (let i = 0; i < 5; i++) {
      const t0 = performance.now();
      lex(parseIncompleteMarkdown(text.trim()));
      fullTimes.push(performance.now() - t0);
    }
    const append = median(appendTimes);
    const full = median(fullTimes);
    expect(append, `append=${append.toFixed(3)}ms full=${full.toFixed(3)}ms`).toBeLessThan(full / 5);
    expect(append).toBeLessThan(10);
  };

  it('incrementalLex list append costs far less than a full re-lex', () => {
    lexAppendContract(bigList, 'list-append');
  });

  it('incrementalLex table append costs far less than a full re-lex', () => {
    lexAppendContract(bigTable, 'table-append');
  });

  const parseBlocksAppendContract = (doc: string, kind: 'list' | 'table'): void => {
    const cache = createParseBlocksCache();
    parseBlocks(doc.slice(0, doc.length - 2100), [], cache);
    const appendTimes: number[] = [];
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const prefix = doc.slice(0, doc.length - cut);
      const t0 = performance.now();
      parseBlocks(prefix, [], cache);
      appendTimes.push(performance.now() - t0);
    }
    expect(cache.trailingBlock?.kind, 'descent record must be live at scale').toBe(kind);
    const fullTimes: number[] = [];
    for (let i = 0; i < 5; i++) {
      const t0 = performance.now();
      parseBlocks(doc, []);
      fullTimes.push(performance.now() - t0);
    }
    const append = median(appendTimes);
    const full = median(fullTimes);
    expect(append, `append=${append.toFixed(3)}ms full=${full.toFixed(3)}ms`).toBeLessThan(full / 5);
    expect(append).toBeLessThan(10);
  };

  it('parseBlocks append with a trailing list costs far less than a fresh parse', () => {
    parseBlocksAppendContract(`Intro paragraph.\n\n${bigList}`, 'list');
  });

  it('parseBlocks append with a trailing table costs far less than a fresh parse', () => {
    parseBlocksAppendContract(`Intro paragraph.\n\n${bigTable}`, 'table');
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
