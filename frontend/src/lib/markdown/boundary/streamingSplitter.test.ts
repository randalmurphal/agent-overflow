import { describe, it, expect, vi } from 'vitest';
import { splitAtBoundary, type BoundarySplit } from './split';
import { StreamingBoundarySplitter } from './streamingSplitter';
import { BoundaryDetector } from './BoundaryDetector';
import { createProvenAppend } from 'svelte-streamdown';
import { expectedStreamingFenceTexts } from '../../../test/helpers/streamingFenceOracle';

function expectFenceCompletePrefix(split: BoundarySplit): void {
  expect(
    expectedStreamingFenceTexts(split.prefix).hasOpenFence,
    `committed prefix ends inside a fence: ${JSON.stringify(split.prefix.slice(-160))}`,
  ).toBe(false);
}

// Reference = the OLD ChatMarkdown behaviour: pure `splitAtBoundary`
// fed the running high-water prefix length, with the shrink/empty
// special-cases the component applied. The StreamingBoundarySplitter
// must match this byte-for-byte at EVERY growing prefix, or it would
// silently change what commits vs. stays volatile mid-stream.
function makeReference() {
  let prevPrefixLen = 0;
  return (text: string): BoundarySplit => {
    if (text.length === 0) {
      prevPrefixLen = 0;
      return { prefix: '', tail: '' };
    }
    if (prevPrefixLen > text.length) {
      prevPrefixLen = 0;
      return { prefix: '', tail: text };
    }
    const split = splitAtBoundary(text, prevPrefixLen);
    prevPrefixLen = split.prefix.length;
    return split;
  };
}

// Replay a document one character at a time through both the incremental
// splitter and the reference, asserting equality at every step. Char-by-
// char is the most thorough simulation of the streaming reveal — it
// exercises every mid-block intermediate state (partial fence line,
// partial setext underline, mid-list item, etc.).
function assertEquivalentCharByChar(doc: string) {
  const splitter = new StreamingBoundarySplitter();
  const reference = makeReference();
  for (let i = 1; i <= doc.length; i++) {
    const text = doc.slice(0, i);
    const got = splitter.split(text);
    const want = reference(text);
    // prefix+tail must always reconstruct the source.
    expect(got.prefix + got.tail).toBe(text);
    expectFenceCompletePrefix(got);
    expect(
      got,
      `divergence at prefix length ${i} of ${doc.length}\n` +
        `  source so far: ${JSON.stringify(text)}\n` +
        `  splitter prefix: ${JSON.stringify(got.prefix)}\n` +
        `  reference prefix: ${JSON.stringify(want.prefix)}`,
    ).toEqual(want);
  }
}

describe('StreamingBoundarySplitter — equivalence to splitAtBoundary', () => {
  // Each document is chosen so the committed boundary lands in a
  // NON-initial block context, which is exactly where an incorrect
  // resume context would diverge from a from-scratch scan.

  it('matches across a list with trailing non-list content (in-list context)', () => {
    assertEquivalentCharByChar(
      '# Heading\n\n- item one\n- item two\n- item three\n\nA paragraph after the list.\n\nAnother paragraph.',
    );
  });

  it('matches across a multi-line fenced code block (in-fence context)', () => {
    assertEquivalentCharByChar(
      'Intro paragraph.\n\n```ts\nconst a = 1;\nconst b = 2;\nconst c = 3;\n```\n\nClosing paragraph here.',
    );
  });

  it('matches across nested blockquotes (container/blockquote context)', () => {
    assertEquivalentCharByChar(
      'Lead-in.\n\n> outer quote\n> > inner quote\n> > more inner\n> back to outer\n\nPlain paragraph after.',
    );
  });

  it('matches across a setext heading (late-underline i-1 path)', () => {
    assertEquivalentCharByChar(
      'Title goes here\n===============\n\nBody paragraph one.\n\nBody paragraph two.',
    );
  });

  it('matches across a mixed document with several block kinds', () => {
    assertEquivalentCharByChar(
      [
        '## Section',
        '',
        'Some prose that wraps a bit and keeps going for a line or two.',
        '',
        '```js',
        'function f(x) {',
        '  return x + 1;',
        '}',
        '```',
        '',
        '- bullet a',
        '- bullet b',
        '',
        '> a quote line',
        '',
        'Final paragraph still streaming',
      ].join('\n'),
    );
  });

  it('handles a wholesale shrink by resetting (sliding-window safety)', () => {
    const splitter = new StreamingBoundarySplitter();
    // Grow past a commit point.
    splitter.split('Para one.\n\nPara two.\n\nPara three start');
    const big = splitter.split('Para one.\n\nPara two.\n\nPara three is longer now.');
    expect(big.prefix.length).toBeGreaterThan(0);
    // Source trimmed far below the committed prefix → reset, whole tail.
    const shrunk = splitter.split('short');
    expect(shrunk).toEqual({ prefix: '', tail: 'short' });
    // After reset, growth re-commits from scratch and still matches a
    // fresh from-zero split.
    const regrown = splitter.split('short now grows.\n\nA second block starts here');
    expect(regrown.prefix + regrown.tail).toBe(
      'short now grows.\n\nA second block starts here',
    );
    expect(regrown).toEqual(splitAtBoundary('short now grows.\n\nA second block starts here', 0));
  });

  it('resets detector checkpoints on a longer in-place rewrite', () => {
    // Append-only growth is the fast path, but replacements are valid inputs.
    // A rewrite that changes committed bytes must discard the old detector
    // context and match a fresh split of the new source.
    const splitter = new StreamingBoundarySplitter();
    // Commit a boundary, leaving an OPEN fence in a non-initial context.
    splitter.split('Para one.\n\nPara two.\n\n```ts\nconst x = 1;');
    // Swap to entirely different, longer content (not a prefix extension,
    // not shorter — so neither the prefix-resume nor the shrink guard is
    // the "happy" path).
    const swapped = 'Z'.repeat(40) + '\n\na wholly different paragraph follows here';
    const got = splitter.split(swapped);
    expect(got.prefix + got.tail).toBe(swapped);
    expect(got).toEqual(splitAtBoundary(swapped, 0));
  });

  it('returns empty for empty source and recovers afterwards', () => {
    const splitter = new StreamingBoundarySplitter();
    expect(splitter.split('')).toEqual({ prefix: '', tail: '' });
    const next = splitter.split('A new paragraph.\n\nSecond.');
    expect(next).toEqual(splitAtBoundary('A new paragraph.\n\nSecond.', 0));
  });
});

describe('StreamingBoundarySplitter — incremental line cache', () => {
  // The reveal-tick split now caches the previous call's line array and
  // splits only the appended delta. These cases pin the merge mechanics
  // the char-by-char corpus above exercises only at chunk size 1, plus
  // the non-append fallbacks that must reproduce the pre-cache
  // behaviour exactly.

  function assertEquivalentChunked(doc: string, chunkSizes: number[]) {
    const splitter = new StreamingBoundarySplitter();
    const reference = makeReference();
    let at = 0;
    let i = 0;
    while (at < doc.length) {
      at = Math.min(doc.length, at + chunkSizes[i % chunkSizes.length]);
      i++;
      const text = doc.slice(0, at);
      const got = splitter.split(text);
      const want = reference(text);
      expect(got.prefix + got.tail).toBe(text);
      expectFenceCompletePrefix(got);
      expect(got).toEqual(want);
    }
  }

  const mixedDoc = [
    '## Section',
    '',
    'Prose that goes on for a while, long enough to straddle chunks.',
    '',
    '```js',
    'const value = 42;',
    '```',
    '',
    '- bullet a',
    '- bullet b',
    '',
    'Final paragraph still streaming',
  ].join('\n');

  it('matches under multi-character chunked growth (partial last line grows across ticks)', () => {
    // Irregular chunk sizes so appends land mid-line, exactly on a
    // newline, just after a newline, and spanning several lines.
    assertEquivalentChunked(mixedDoc, [3, 1, 17, 2, 40, 5]);
    assertEquivalentChunked(mixedDoc, [64]);
    assertEquivalentChunked(mixedDoc, [1, 200]);
  });

  it('never commits an open fence under sparse jittered source advances', () => {
    const document = Array.from({ length: 20 }, (_, iteration) => [
      `### Working set ${iteration}`,
      '',
      `Paragraph ${iteration} remains ordinary Markdown.`,
      '',
      '```ts',
      `const sample${iteration} = true;`,
      '```',
      '',
      `> Visible progress marker ${iteration}.`,
    ].join('\n')).join('\n\n');

    for (let seed = 1; seed <= 64; seed++) {
      let state = seed;
      const sizes: number[] = [];
      for (let index = 0; index < 48; index++) {
        state = (Math.imul(state, 1_664_525) + 1_013_904_223) >>> 0;
        sizes.push(1 + (state % 600));
      }
      assertEquivalentChunked(document, sizes);
    }
  });

  it('uses trusted append lineage without rescanning the growing source', () => {
    const splitter = new StreamingBoundarySplitter();
    const reference = makeReference();
    let source = 'First paragraph';
    expect(splitter.split(source)).toEqual(reference(source));

    const startsWith = vi.spyOn(String.prototype, 'startsWith')
      .mockImplementation(() => {
        throw new Error('trusted append fell back to a prefix scan');
      });
    try {
      for (const delta of [' grows.', '\n\n', 'Second paragraph', ' continues.']) {
        const append = createProvenAppend(source, delta);
        source = append.next;
        expect(splitter.split(source, append)).toEqual(reference(source));
      }
    } finally {
      startsWith.mockRestore();
    }
  });

  it('publishes a tail suffix only while the volatile block extends in place', () => {
    const splitter = new StreamingBoundarySplitter();
    let source = '```ts\nconst first = 1;';
    let previous = splitter.split(source);
    expect(splitter.tailAppend).toBeUndefined();

    for (const delta of ['\n', 'const second', ' = 2;']) {
      const append = createProvenAppend(source, delta);
      source = append.next;
      const next = splitter.split(source, append);
      expect(splitter.tailAppend?.delta).toBe(delta);
      expect(previous.tail + delta).toBe(next.tail);
      previous = next;
    }

    // Closing the fence advances the stable boundary. The new volatile tail
    // is not an extension of the old open-fence tail, so forwarding the source
    // delta to Streamdown would violate its append contract.
    const closeAndFollow = '\n```\n\nfollowing paragraph';
    const closeAppend = createProvenAppend(source, closeAndFollow);
    source = closeAppend.next;
    const closed = splitter.split(source, closeAppend);
    expect(closed.prefix.length).toBeGreaterThan(previous.prefix.length);
    expect(splitter.tailAppend).toBeUndefined();

    const replacement = source.replace('following', 'different');
    splitter.split(replacement);
    expect(splitter.tailAppend).toBeUndefined();
  });

  it('never strands a completed fence closer at the volatile-tail boundary', () => {
    const splitter = new StreamingBoundarySplitter();
    let source = '';

    for (let iteration = 0; iteration < 12; iteration++) {
      const deltas = [
        `\n\n### Working set ${iteration}\n\n`,
        `The active pane keeps **streamed Markdown**, \`inline code\`, and [a link](https://example.test/active/${iteration}) readable. `,
        `Unicode remains intact: café, 東京, 🧪, and iteration ${iteration}.\n\n`,
        '- The parser carries state across wire chunks.\n- The reveal queue stays bounded.\n- The spring follows the live edge.\n\n',
        `| Iteration | Parser | Scroll |\n| ---: | :--- | :--- |\n| ${iteration} | active | following |\n\n`,
        '```ts\n',
        `const sample${iteration} = { pane: 1, active: true };\n`,
        `console.log(sample${iteration});\n\`\`\`\n\n`,
        `> Visible progress marker ${iteration}. The next section continues the same ordinary long turn.`,
      ];
      for (const delta of deltas) {
        const append = createProvenAppend(source, delta);
        source = append.next;
        const split = splitter.split(source, append);
        expect(
          split.tail.startsWith('```\n'),
          `iteration ${iteration}, tail ${JSON.stringify(split.tail.slice(0, 120))}`,
        ).toBe(false);
      }
    }
  });

  it('resumes at the trailing line inside an uncommitted code fence', () => {
    const starts: number[] = [];
    const original = BoundaryDetector.prototype.findStableBoundary;
    const findStableBoundary = vi
      .spyOn(BoundaryDetector.prototype, 'findStableBoundary')
      .mockImplementation(function (this: BoundaryDetector, lines, startLine, context) {
        starts.push(startLine);
        return original.call(this, lines, startLine, context);
      });
    try {
      const splitter = new StreamingBoundarySplitter();
      let source = '```ts\n';
      splitter.split(source);
      for (let line = 0; line < 100; line++) {
        const delta = `const value${line} = ${line};\n`;
        const append = createProvenAppend(source, delta);
        source = append.next;
        splitter.split(source, append);
      }

      expect(starts.at(-1)).toBeGreaterThan(95);
      const contextCache = (
        splitter as unknown as {
          detector: { contextCache: Map<number, unknown> };
        }
      ).detector.contextCache;
      expect(contextCache.size).toBeLessThanOrEqual(2);
    } finally {
      findStableBoundary.mockRestore();
    }
  });

  it('matches char-by-char on a CRLF document', () => {
    assertEquivalentCharByChar(
      'First line.\r\n\r\nSecond paragraph here.\r\n\r\n- a\r\n- b\r\n\r\nTrailing prose',
    );
  });

  it('falls back to a full re-split on a tail shrink above the committed offset', () => {
    // A shrink that stays ABOVE committedOffset does not trip the reset
    // guard; before the line cache it simply re-split the whole source.
    // The cache must detect the non-append and do the same.
    const splitter = new StreamingBoundarySplitter();
    const grown = 'Para one.\n\nPara two.\n\nPara three grows quite long here';
    const committed = splitter.split(grown);
    expect(committed.prefix.length).toBeGreaterThan(0);
    // Trim within the tail (still longer than the committed prefix).
    const trimmed = 'Para one.\n\nPara two.\n\nPara';
    expect(trimmed.length).toBeGreaterThan(committed.prefix.length);
    const got = splitter.split(trimmed);
    expect(got.prefix + got.tail).toBe(trimmed);
    // The committed region is untouched by the trim, so the fallback's
    // resumed detection must agree with a from-scratch monotonic split.
    expect(got).toEqual(splitAtBoundary(trimmed, committed.prefix.length));
    // Regrowth after the fallback keeps matching the reference exactly.
    const regrown = trimmed + ' four continues.\n\nPara five.\n\nTail';
    const after = splitter.split(regrown);
    expect(after).toEqual(splitAtBoundary(regrown, got.prefix.length));
  });

  it('falls back to a full re-split on a same-length rewrite of the tail', () => {
    const splitter = new StreamingBoundarySplitter();
    const first = 'Para one.\n\nPara two.\n\nAAAA BBBB';
    const committed = splitter.split(first);
    expect(committed.prefix.length).toBeGreaterThan(0);
    expect(first.startsWith(committed.prefix)).toBe(true);
    // Same length, different content past the committed region —
    // startsWith fails, so the cached lines must be discarded and
    // detection must see the real current lines (not 'AAAA BBBB').
    const swapped = 'Para one.\n\nPara two.\n\nCCCC\n\nDDD';
    expect(swapped.length).toBe(first.length);
    const got = splitter.split(swapped);
    expect(got.prefix + got.tail).toBe(swapped);
    expect(got).toEqual(splitAtBoundary(swapped, committed.prefix.length));
    // The rewritten tail's own boundary commits on a later call exactly
    // as the monotonic from-scratch split would find it.
    const grown = swapped + ' more prose.\n\nNext block';
    expect(splitter.split(grown)).toEqual(
      splitAtBoundary(grown, got.prefix.length),
    );
  });

  it('resets when a same-length rewrite has fewer lines than the committed source', () => {
    const splitter = new StreamingBoundarySplitter();
    const first = [
      'Paragraph one.',
      '',
      'Paragraph two.',
      '',
      'Paragraph three.',
      '',
      'Trailing paragraph.',
    ].join('\n');
    const committed = splitter.split(first);
    expect(committed.prefix.length).toBeGreaterThan(0);

    const rewritten = 'x'.repeat(first.length);
    const got = splitter.split(rewritten);
    expect(got.prefix + got.tail).toBe(rewritten);
    expect(got).toEqual(splitAtBoundary(rewritten, 0));
  });
});
