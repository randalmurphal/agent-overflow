import { describe, expect, it } from 'vitest';
import { chunkCut, fakeDiff } from './streamedPatch';
import cutVectors from '../fixtures/chunkCutVectors.json';

// The fake backend cuts reads where the Go backend does: Go's
// TestChunkCutMatchesTheSharedVectors (internal/gitdiff) holds
// gitdiff.ChunkCut to the same vectors.

function rawBytes(hex: string): Uint8Array {
  return Uint8Array.from(hex.match(/../g) ?? [], (pair) => Number.parseInt(pair, 16));
}

describe('the fake review diff backend', () => {
  it('cuts a read where the backend does', () => {
    expect(cutVectors.length).toBeGreaterThan(0);
    for (const vector of cutVectors) {
      expect(chunkCut(rawBytes(vector.rawHex), vector.offset, vector.window), vector.name).toBe(vector.cut);
    }
  });

  it('reads with the backend\'s cut once the window holds the character at the offset', () => {
    for (const vector of cutVectors) {
      const bytes = rawBytes(vector.rawHex);
      const read = fakeDiff(bytes).read(vector.offset, vector.window, false);
      // The backend's window is never shorter than the character at its
      // offset (MinChunkBytes).
      if ('shorterThanItsCharacter' in vector) expect(read.nextOffset, vector.name).toBeGreaterThan(vector.cut);
      else expect(read.nextOffset, vector.name).toBe(vector.cut);
    }
  });

  it('widens a window shorter than its character to the character', () => {
    const euro = new TextEncoder().encode('€ab');
    const astral = new TextEncoder().encode('a\u{1F600}b');
    expect([1, 2, 3].map((window) => fakeDiff(euro).read(0, window, false).nextOffset)).toEqual([3, 3, 3]);
    expect([1, 2, 3, 4].map((window) => fakeDiff(astral).read(1, window, false).nextOffset)).toEqual([5, 5, 5, 5]);
    expect(fakeDiff(euro).read(0, 1, false).data).toBe('€');
  });
});
