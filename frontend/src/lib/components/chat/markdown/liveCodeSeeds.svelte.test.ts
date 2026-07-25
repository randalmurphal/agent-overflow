import { beforeEach, describe, expect, it } from 'vitest';
import { contentKey } from '../../../utils/fnv1a';
import {
  MAX_LIVE_CODE_SEEDS,
  __liveCodeSeedStatsForTest,
  lineHashChain,
  liveCodeSeedGeneration,
  matchLiveCodeSeed,
  putLiveCodeSeed,
  resetLiveCodeSeedsForTest,
} from './liveCodeSeeds.svelte';

// The chain is one entry per line; spans line up 1:1 for these tests.
function linesFor(chain: number[]) {
  return chain.map((_, i) => ({ r: [1, i + 1] }));
}

function put(threadId: string, lang: string, text: string, itemId = 'i1') {
  const chain = lineHashChain(text);
  putLiveCodeSeed(threadId, itemId, lang, chain, linesFor(chain));
  return chain;
}

beforeEach(() => {
  resetLiveCodeSeedsForTest();
});

describe('lineHashChain', () => {
  it('ends with the whole-string hash contentKey() is built from', () => {
    // FrontendContentKey (Go) and contentKey (TS) both encode the
    // full-string fnv1a; the chain's final entry must be that hash so
    // a final seed's chain and its contentKey agree.
    for (const text of ['abc', 'def route():\n    pass', '🎉 emoji\ncafé ☕', '\n\n']) {
      const chain = lineHashChain(text);
      const hash = chain[chain.length - 1]!;
      expect(contentKey(text)).toBe(`${text.length}:${hash.toString(36)}`);
      expect(chain.length).toBe(text.split('\n').length);
    }
  });
});

describe('matchLiveCodeSeed', () => {
  it('matches the entire text exactly when the chain covers it', () => {
    put('t1', 'python', 'def f():\n    pass');
    const match = matchLiveCodeSeed('python', 'def f():\n    pass');
    expect(match).not.toBeNull();
    expect(match!.exact).toBe(true);
    expect(match!.covered).toBe('def f():\n    pass');
    expect(match!.spans).toHaveLength(2);
  });

  it('verifies a line prefix when the text has grown past the seed', () => {
    put('t1', 'python', 'a = 1\nb = 2');
    const match = matchLiveCodeSeed('python', 'a = 1\nb = 2\nc = 3');
    expect(match).not.toBeNull();
    expect(match!.exact).toBe(false);
    // Both seed lines verify: line 0 at its newline, line 1 by the
    // chain's final whole-prefix entry at the next newline.
    expect(match!.covered).toBe('a = 1\nb = 2');
    expect(match!.spans).toHaveLength(2);
  });

  it('treats a seed AHEAD of the text as prefix coverage, not exact', () => {
    // The reveal smoother renders a paced prefix of received text, so
    // a seed computed at a flush boundary usually describes MORE lines
    // than are displayed. The displayed whole lines verify and paint —
    // but the match is NOT exact: the seed's spans were parsed with a
    // suffix this block may never contain, so an exact adoption (which
    // cancels the block's own RPC) must wait until the text consumes
    // the seed's entire chain.
    put('t1', 'python', 'a = 1\nb = 2\nc = 3');
    const match = matchLiveCodeSeed('python', 'a = 1\nb = 2');
    expect(match).not.toBeNull();
    expect(match!.exact).toBe(false);
    expect(match!.covered).toBe('a = 1\nb = 2');
    expect(match!.spans).toHaveLength(2);
  });

  it('does not verify a partially streamed final line', () => {
    put('t1', 'python', 'a = 1\nb = 2');
    const match = matchLiveCodeSeed('python', 'a = 1\nb =');
    expect(match).not.toBeNull();
    expect(match!.exact).toBe(false);
    expect(match!.covered).toBe('a = 1');
    expect(match!.spans).toHaveLength(1);
  });

  it('stops the walk at the first diverged line', () => {
    // The chain is cumulative: once a line diverges, later entries
    // cannot match, even if a later line's text is identical.
    put('t1', 'python', 'a = 1\nb = 2\nc = 3');
    const match = matchLiveCodeSeed('python', 'a = 1\nX = 9\nc = 3');
    expect(match).not.toBeNull();
    expect(match!.covered).toBe('a = 1');
    expect(match!.spans).toHaveLength(1);
  });

  it('returns null when nothing matches', () => {
    put('t1', 'python', 'a = 1');
    expect(matchLiveCodeSeed('python', 'completely different')).toBeNull();
    expect(matchLiveCodeSeed('go', 'a = 1')).toBeNull();
    expect(matchLiveCodeSeed('', 'a = 1')).toBeNull();
    expect(matchLiveCodeSeed('python', '')).toBeNull();
  });

  it('hashes UTF-16 code units, surrogate pairs included', () => {
    const text = '🎉 emoji\ncafé ☕';
    put('t1', 'markdown', text);
    const match = matchLiveCodeSeed('markdown', text);
    expect(match).not.toBeNull();
    expect(match!.exact).toBe(true);
    expect(match!.spans).toHaveLength(2);
  });

  it('picks the seed with the longest verified prefix across threads', () => {
    put('t1', 'python', 'a = 1');
    put('t2', 'python', 'a = 1\nb = 2');
    const match = matchLiveCodeSeed('python', 'a = 1\nb = 2\nc = 3');
    expect(match!.covered).toBe('a = 1\nb = 2');
  });
});

describe('seed lifecycle', () => {
  it('replaces the seed for the same (thread, item, lang) key latest-wins', () => {
    put('t1', 'python', 'old = 1');
    put('t1', 'python', 'new = 2');
    expect(__liveCodeSeedStatsForTest().entries).toBe(1);
    expect(matchLiveCodeSeed('python', 'old = 1')).toBeNull();
    expect(matchLiveCodeSeed('python', 'new = 2')!.exact).toBe(true);
  });

  it('retains concurrent same-language seeds from different items', () => {
    // Subagent fan-out: two rows in one thread stream python fences at
    // the same time. Each keeps its own slot; neither evicts the other.
    put('t1', 'python', 'row_a = 1', 'item-a');
    put('t1', 'python', 'row_b = 2', 'item-b');
    expect(__liveCodeSeedStatsForTest().entries).toBe(2);
    expect(matchLiveCodeSeed('python', 'row_a = 1')!.exact).toBe(true);
    expect(matchLiveCodeSeed('python', 'row_b = 2')!.exact).toBe(true);
  });

  it('caps retained seeds LRU', () => {
    for (let i = 0; i < MAX_LIVE_CODE_SEEDS + 2; i += 1) {
      put(`t${i}`, 'python', `x = ${i}`);
    }
    expect(__liveCodeSeedStatsForTest().entries).toBe(MAX_LIVE_CODE_SEEDS);
    expect(matchLiveCodeSeed('python', 'x = 0')).toBeNull();
    expect(matchLiveCodeSeed('python', `x = ${MAX_LIVE_CODE_SEEDS + 1}`)).not.toBeNull();
  });

  it('ignores empty-lang and empty-chain puts', () => {
    putLiveCodeSeed('t1', 'i1', '', [1], [{ r: [1, 1] }]);
    putLiveCodeSeed('t1', 'i1', 'python', [], []);
    expect(__liveCodeSeedStatsForTest().entries).toBe(0);
    expect(liveCodeSeedGeneration()).toBe(0);
  });

  it('bumps the generation on every retained put', () => {
    expect(liveCodeSeedGeneration()).toBe(0);
    put('t1', 'python', 'a = 1');
    expect(liveCodeSeedGeneration()).toBe(1);
    put('t1', 'python', 'a = 1\nb = 2');
    expect(liveCodeSeedGeneration()).toBe(2);
  });
});
