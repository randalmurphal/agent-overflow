// Equivalence proof for the svelte-streamdown incremental-lex patch hunk.
//
// `incrementalLex` promises IDENTICAL output to a fresh `lex` at every
// streamed prefix — the fast path (re-lex from the last list item, splice
// onto sealed reference-identical items) must be invisible in the token
// tree. This suite streams a corpus of list shapes through the cache under
// several chunkings and deep-compares against the full lex at every step,
// then separately asserts the two properties that make the patch worth
// carrying: sealed items keep their object references (what lets Svelte
// skip their subtrees), and the fast path actually engages (`lastPath`
// breadcrumb), so a silent regression to full re-lexing fails loudly.
//
// Known, deliberate divergences (documented in the patch): reference-link
// definitions and footnote definitions arriving in the live tail do not
// resolve usages inside already-sealed items mid-stream; the settled full
// parse renders them correctly. The corpus therefore contains neither.
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
  /** Docs whose shape can never take the descent (tables, blockquotes,
   *  fences) still assert equivalence — just not engagement. */
  expectDescent: boolean;
}

const bullets = (n: number, line: (i: number) => string): string =>
  Array.from({ length: n }, (_, i) => line(i)).join('\n');

const CORPUS: CorpusDoc[] = [
  {
    name: 'tight bullets with inline markup',
    text: bullets(24, (i) => `- Item ${i}: the \`resolver\` keeps a **steady** cadence on [pass ${i}](https://example.com/${i}).`),
    expectDescent: true,
  },
  {
    name: 'loose bullets (blank-line separated)',
    text: bullets(16, (i) => `- Loose item ${i} with *emphasis* and detail.`).replaceAll('\n', '\n\n'),
    expectDescent: true,
  },
  {
    name: 'tight list flipping loose midway',
    text: `${bullets(8, (i) => `- Early tight item ${i}.`)}\n- Item eight.\n\n- Item nine after a blank.\n- Item ten.`,
    expectDescent: true,
  },
  {
    name: 'nested list',
    text: bullets(12, (i) =>
      i % 3 === 0
        ? `- Parent ${i} with children:\n  - child ${i}.a has \`code\`\n  - child ${i}.b`
        : `- Plain parent ${i}`),
    expectDescent: true,
  },
  {
    name: 'ordered decimal sequential',
    text: bullets(15, (i) => `${i + 1}. Step ${i + 1} of the procedure runs cleanly.`),
    expectDescent: true,
  },
  {
    name: 'ordered with skipped values',
    text: '1. first\n2. second\n9. ninth out of order\n10. tenth\n4. fourth again',
    expectDescent: true,
  },
  {
    name: 'lower-alpha ordered',
    text: bullets(6, (i) => `${String.fromCharCode(97 + i)}. option ${i}`),
    expectDescent: true,
  },
  {
    name: 'task list',
    text: bullets(10, (i) => `- [${i % 2 === 0 ? 'x' : ' '}] task ${i} with a [link](https://example.com)`),
    expectDescent: true,
  },
  {
    name: 'item containing a fenced code block',
    text: '- first item\n- second item with code:\n  ```ts\n  const x = 1;\n  const y = 2;\n  ```\n- third item after the fence\n- fourth item',
    expectDescent: true,
  },
  {
    name: 'items with lazy continuation lines',
    text: '- first item\ncontinues lazily on the next line\n- second item\nalso continues\n- third',
    expectDescent: true,
  },
  {
    name: 'list closed by a trailing paragraph',
    text: `${bullets(8, (i) => `- item ${i}`)}\n\nA closing paragraph after the list ends it for good.`,
    expectDescent: true,
  },
  {
    name: 'paragraph, list, paragraph',
    text: `An opening paragraph.\n\n${bullets(10, (i) => `- point ${i}`)}\n\nAnd a closer.`,
    expectDescent: false,
  },
  {
    name: 'blockquote inside an item',
    text: '- first\n- second holds a quote:\n  > quoted line one\n  > quoted line two\n- third',
    expectDescent: true,
  },
  {
    name: 'table (no descent shape)',
    text: '| a | b |\n| - | - |\n| 1 | 2 |\n| 3 | 4 |\n| 5 | 6 |',
    expectDescent: false,
  },
  {
    name: 'blockquote block (no descent shape)',
    text: '> line one of the quote\n> line two of the quote\n> line three keeps going',
    expectDescent: false,
  },
  {
    name: 'unclosed bold cut mid-stream',
    text: bullets(8, (i) => `- item ${i} has **bold ${i}** and math $x_${i}$ inline`),
    expectDescent: true,
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
            expect(JSON.stringify(incremental)).toBe(JSON.stringify(reference));
            if (cache.lastPath === 'list-append') descents += 1;
            steps += 1;
          }
          // A doc consumed in a couple of chunks never has an append landing
          // on an established ≥2-item list, so engagement is only assertable
          // on streams with real step counts.
          if (doc.expectDescent && steps >= 4) {
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

  it('is idempotent for an unchanged input', () => {
    const cache = createIncrementalLexCache();
    const text = '- one\n- two\n- three';
    const first = incrementalLex(text, [], cache, parseIncompleteMarkdown);
    const second = incrementalLex(text, [], cache, parseIncompleteMarkdown);
    expect(second).toBe(first);
  });
});

describe('incremental lexing performance contract', () => {
  // The reason the patch hunk exists: an append to a large trailing list
  // must cost O(new content), not O(list). Relative bounds (against the
  // full path measured in the same warmed process) keep the assertion
  // robust to machine speed; the generous absolute cap is a second line
  // against a pathological regression that slows both paths together.
  // Pre-fix reference points on the profiling machine: full lex 27ms and
  // block-level append 5.9ms at a 120KB list — the 5× margins are far
  // outside noise in both directions.
  const bigList = bullets(660, (i) => `- Item ${i}: the \`resolver\` keeps a **steady** cadence while pass ${i} holds the viewport across its flush.`);

  const median = (samples: number[]): number => {
    const sorted = [...samples].sort((a, b) => a - b);
    return sorted[Math.floor(sorted.length / 2)];
  };

  it('incrementalLex append costs far less than a full re-lex', () => {
    const cache = createIncrementalLexCache();
    // Establish the stream mid-list, then measure steady-state appends.
    incrementalLex(bigList.slice(0, bigList.length - 2100), [], cache, parseIncompleteMarkdown);
    const appendTimes: number[] = [];
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const prefix = bigList.slice(0, bigList.length - cut);
      const t0 = performance.now();
      incrementalLex(prefix, [], cache, parseIncompleteMarkdown);
      appendTimes.push(performance.now() - t0);
      expect(cache.lastPath).toBe('list-append');
    }
    const fullTimes: number[] = [];
    for (let i = 0; i < 5; i++) {
      const t0 = performance.now();
      lex(parseIncompleteMarkdown(bigList.trim()));
      fullTimes.push(performance.now() - t0);
    }
    const append = median(appendTimes);
    const full = median(fullTimes);
    expect(append, `append=${append.toFixed(3)}ms full=${full.toFixed(3)}ms`).toBeLessThan(full / 5);
    expect(append).toBeLessThan(10);
  });

  it('parseBlocks append with a trailing list costs far less than a fresh parse', () => {
    const doc = `Intro paragraph.\n\n${bigList}`;
    const cache = createParseBlocksCache();
    parseBlocks(doc.slice(0, doc.length - 2100), [], cache);
    const appendTimes: number[] = [];
    for (let cut = 2100 - 21; cut >= 0; cut -= 21) {
      const prefix = doc.slice(0, doc.length - cut);
      const t0 = performance.now();
      parseBlocks(prefix, [], cache);
      appendTimes.push(performance.now() - t0);
    }
    expect(cache.trailingList, 'descent record must be live at scale').not.toBeNull();
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
  });
});

describe('parseBlocks trailing-list descent', () => {
  const streamedBlocks = (text: string, chunk: number): void => {
    const cache = createParseBlocksCache();
    let sawDescentState = false;
    for (const prefix of prefixes(text, [chunk])) {
      const incremental = parseBlocks(prefix, [], cache);
      const reference = parseBlocks(prefix, []);
      expect(incremental).toEqual(reference);
      if (cache.trailingList !== null) sawDescentState = true;
    }
    expect(sawDescentState, 'descent record must engage on a trailing list').toBe(true);
  };

  it('matches the fresh parse at every step of a growing list document', () => {
    streamedBlocks(
      `An opening paragraph that commits.\n\n${bullets(40, (i) => `- Item ${i} with \`inline\` and **markup** to lex.`)}`,
      21,
    );
  });

  it('matches when the list closes and new blocks follow', () => {
    streamedBlocks(
      `${bullets(20, (i) => `- item ${i}`)}\n\nA paragraph after.\n\n\`\`\`ts\nconst x = 1;\n\`\`\`\n\n${bullets(6, (i) => `1. later ${i}`)}`,
      17,
    );
  });

  it('recovers across a wholesale content replacement', () => {
    const cache = createParseBlocksCache();
    const a = bullets(12, (i) => `- alpha ${i}`);
    const b = `Fresh start.\n\n${bullets(12, (i) => `- beta ${i}`)}`;
    for (const prefix of prefixes(a, [21])) parseBlocks(prefix, [], cache);
    for (const prefix of prefixes(b, [21])) {
      expect(parseBlocks(prefix, [], cache)).toEqual(parseBlocks(prefix, []));
    }
  });
});
