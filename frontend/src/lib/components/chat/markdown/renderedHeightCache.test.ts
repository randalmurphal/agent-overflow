import { afterEach, describe, expect, it } from 'vitest';
import {
  readMathRenderedHeight,
  readMermaidRenderedHeight,
  writeMathRenderedHeight,
  writeMermaidRenderedHeight,
  __renderedHeightCacheSizesForTest,
  __resetRenderedHeightCachesForTest,
} from './renderedHeightCache';

// Unit coverage for the read/write API used by both StreamdownMathHost
// and StreamdownMermaidHost. The hosts read this cache via `$derived`
// at construction and write to it after the renderer-content
// MutationObserver fires; the LRU contract here is what bounds memory
// under heavy thread scrollback (many distinct math/mermaid sources
// mounted and unmounted over time).
//
// Public surface verified:
//   - writeXxx stores under the source key
//   - readXxx returns the stored value (or undefined for misses)
//   - re-writing the same key updates the value and bumps its LRU
//     position
//   - exceeding the cap evicts the least-recently observed entry, not
//     the least-recently inserted one
//   - math and mermaid caches do not interfere
//
// Eviction tests rely on knowing the cap. Rather than expose the cap
// constant (would re-leak an implementation detail), they fill the
// cache past its cap (200 / 100 — both small enough to fill in a
// fraction of a millisecond) and check the eviction shape.

describe('renderedHeightCache (math)', () => {
  afterEach(() => {
    __resetRenderedHeightCachesForTest();
  });

  it('writes and reads a height for a given source key', () => {
    writeMathRenderedHeight('src-A', 240);
    expect(readMathRenderedHeight('src-A')).toBe(240);
    expect(__renderedHeightCacheSizesForTest().math).toBe(1);
  });

  it('returns undefined for a key that has not been written', () => {
    expect(readMathRenderedHeight('never-written')).toBeUndefined();
  });

  it('overwrites an existing entry with the new value', () => {
    writeMathRenderedHeight('src-A', 240);
    writeMathRenderedHeight('src-A', 480);
    expect(readMathRenderedHeight('src-A')).toBe(480);
    expect(__renderedHeightCacheSizesForTest().math).toBe(1);
  });

  it('evicts the least-recently-touched entry when filled past the cap', () => {
    // Fill past the cap by writing CAP+2 distinct keys (cap is 200; use
    // a deterministic key sequence so we can name the survivors).
    for (let i = 0; i < 202; i++) {
      writeMathRenderedHeight(`fill-${i}`, i + 1);
    }
    expect(__renderedHeightCacheSizesForTest().math).toBe(200);
    // The two oldest insertions should be gone.
    expect(readMathRenderedHeight('fill-0')).toBeUndefined();
    expect(readMathRenderedHeight('fill-1')).toBeUndefined();
    // The most recent insertion is still around.
    expect(readMathRenderedHeight('fill-201')).toBe(202);
  });

  it('re-touched entries survive an eviction pass', () => {
    // Fill exactly to the cap.
    for (let i = 0; i < 200; i++) {
      writeMathRenderedHeight(`fill-${i}`, i + 1);
    }
    // Re-touch the oldest entry — moves it to the newest LRU position.
    writeMathRenderedHeight('fill-0', 999);
    // Add one more — now 'fill-1' is the oldest and should be evicted,
    // while 'fill-0' survives.
    writeMathRenderedHeight('fill-200', 1000);
    expect(readMathRenderedHeight('fill-0')).toBe(999);
    expect(readMathRenderedHeight('fill-1')).toBeUndefined();
    expect(readMathRenderedHeight('fill-200')).toBe(1000);
  });
});

describe('renderedHeightCache (mermaid)', () => {
  afterEach(() => {
    __resetRenderedHeightCachesForTest();
  });

  it('writes and reads independently of the math cache', () => {
    writeMathRenderedHeight('mathsrc', 100);
    writeMermaidRenderedHeight('mermaidsrc', 500);

    expect(readMathRenderedHeight('mermaidsrc')).toBeUndefined();
    expect(readMermaidRenderedHeight('mathsrc')).toBeUndefined();
    expect(readMathRenderedHeight('mathsrc')).toBe(100);
    expect(readMermaidRenderedHeight('mermaidsrc')).toBe(500);
  });

  it('honors its own cap (smaller than math) for eviction', () => {
    for (let i = 0; i < 102; i++) {
      writeMermaidRenderedHeight(`d-${i}`, i + 1);
    }
    expect(__renderedHeightCacheSizesForTest().mermaid).toBe(100);
    expect(readMermaidRenderedHeight('d-0')).toBeUndefined();
    expect(readMermaidRenderedHeight('d-1')).toBeUndefined();
    expect(readMermaidRenderedHeight('d-101')).toBe(102);
  });
});

describe('renderedHeightCache (test reset)', () => {
  it('__resetRenderedHeightCachesForTest clears both caches', () => {
    writeMathRenderedHeight('a', 1);
    writeMermaidRenderedHeight('b', 2);
    expect(__renderedHeightCacheSizesForTest()).toEqual({ math: 1, mermaid: 1 });

    __resetRenderedHeightCachesForTest();
    expect(__renderedHeightCacheSizesForTest()).toEqual({ math: 0, mermaid: 0 });
    expect(readMathRenderedHeight('a')).toBeUndefined();
    expect(readMermaidRenderedHeight('b')).toBeUndefined();
  });
});
