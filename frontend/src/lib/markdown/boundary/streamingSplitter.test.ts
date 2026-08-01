import { describe, it, expect } from 'vitest';
import { splitAtBoundary, type BoundarySplit } from './split';
import { StreamingBoundarySplitter } from './streamingSplitter';

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

  it('never drops or duplicates text on an out-of-contract in-place swap', () => {
    // The contract is append-only (+ wholesale shrink). A same-length-or-
    // longer, different-content replacement is OUT of contract: the resume
    // uses the detector's cached block context for the OLD content, which
    // can pick a mid-block split point for the NEW content. The hard
    // invariant that must survive even then is `prefix + tail === text`
    // (no dropped or duplicated characters) — worst case is a transient
    // mis-split that self-corrects once a real boundary commits, never
    // data loss. The only same-id re-stream path in production resets the
    // summary to empty first (emitStreamingBlockStart emits an empty-
    // summary upsert), which the shrink guard catches; this pins the floor
    // for the input the contract formally excludes.
    const splitter = new StreamingBoundarySplitter();
    // Commit a boundary, leaving an OPEN fence in a non-initial context.
    splitter.split('Para one.\n\nPara two.\n\n```ts\nconst x = 1;');
    // Swap to entirely different, longer content (not a prefix extension,
    // not shorter — so neither the prefix-resume nor the shrink guard is
    // the "happy" path).
    const swapped = 'Z'.repeat(40) + '\n\na wholly different paragraph follows here';
    const got = splitter.split(swapped);
    expect(got.prefix + got.tail).toBe(swapped);
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
});
