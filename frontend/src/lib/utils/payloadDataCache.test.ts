import { describe, expect, it } from 'vitest';
import {
  __payloadCacheStatsForTest,
  clearPayloadCacheForThread,
  payloadVersionKey,
  readPayloadCache,
  writePayloadCache,
} from './payloadDataCache';

describe('payloadDataCache', () => {
  it('round-trips an entry by (threadId, payloadId, version)', () => {
    writePayloadCache('thread-1', 'payload-a', 100, {
      chunks: ['hello world'],
      hasFullChunks: true,
      totalSize: 11,
      isComplete: true,
      loadedBytes: 11,
    });

    const got = readPayloadCache('thread-1', 'payload-a', 100);
    expect(got).toBeDefined();
    expect(got?.chunks).toEqual(['hello world']);
    expect(got?.totalSize).toBe(11);
    expect(got?.bytes).toBe(11);
  });

  it('treats a different version as a cache miss without evicting the old entry', () => {
    writePayloadCache('thread-1', 'payload-a', 1, {
      chunks: ['v1'],
      hasFullChunks: true,
      totalSize: 2,
      isComplete: true,
      loadedBytes: 2,
    });

    expect(readPayloadCache('thread-1', 'payload-a', 2)).toBeUndefined();
    // The original version is still resident — the write only invalidates
    // the (thread, payload, version) tuple, not the (thread, payload) pair.
    expect(readPayloadCache('thread-1', 'payload-a', 1)?.chunks).toEqual(['v1']);
  });

  it('distinguishes null / undefined / empty-string / "null" version keys', () => {
    writePayloadCache('thread-1', 'payload-a', undefined, {
      chunks: ['undef'], hasFullChunks: true, totalSize: 5, isComplete: true, loadedBytes: 5,
    });
    writePayloadCache('thread-1', 'payload-a', null, {
      chunks: ['nullv'], hasFullChunks: true, totalSize: 5, isComplete: true, loadedBytes: 5,
    });
    writePayloadCache('thread-1', 'payload-a', '', {
      chunks: ['empty'], hasFullChunks: true, totalSize: 5, isComplete: true, loadedBytes: 5,
    });
    writePayloadCache('thread-1', 'payload-a', 'null', {
      chunks: ['strnull'], hasFullChunks: true, totalSize: 7, isComplete: true, loadedBytes: 7,
    });

    expect(readPayloadCache('thread-1', 'payload-a', undefined)?.chunks).toEqual(['undef']);
    expect(readPayloadCache('thread-1', 'payload-a', null)?.chunks).toEqual(['nullv']);
    expect(readPayloadCache('thread-1', 'payload-a', '')?.chunks).toEqual(['empty']);
    expect(readPayloadCache('thread-1', 'payload-a', 'null')?.chunks).toEqual(['strnull']);
  });

  it('bounds large string versions before embedding them in cache keys', () => {
    const largeVersion = 'x'.repeat(10_000);
    const key = payloadVersionKey(largeVersion);

    expect(key.length).toBeLessThan(190);
    expect(key).not.toContain(largeVersion);
  });

  it('rewriting the same key replaces, not duplicates, the entry and updates byte accounting', () => {
    writePayloadCache('thread-1', 'payload-a', 1, {
      chunks: ['abc'], hasFullChunks: false, totalSize: 100, isComplete: false, loadedBytes: 3,
    });
    const after1 = __payloadCacheStatsForTest();
    expect(after1.entries).toBe(1);
    expect(after1.bytes).toBe(3);

    writePayloadCache('thread-1', 'payload-a', 1, {
      chunks: ['abcdef'], hasFullChunks: true, totalSize: 6, isComplete: true, loadedBytes: 6,
    });
    const after2 = __payloadCacheStatsForTest();
    expect(after2.entries).toBe(1);
    expect(after2.bytes).toBe(6);

    expect(readPayloadCache('thread-1', 'payload-a', 1)?.chunks).toEqual(['abcdef']);
  });

  it('does not cache a single entry larger than the cache budget', () => {
    writePayloadCache('thread-1', 'payload-huge', 1, {
      chunks: ['x'.repeat(17 * 1024 * 1024)],
      hasFullChunks: true,
      totalSize: 17 * 1024 * 1024,
      isComplete: true,
      loadedBytes: 17 * 1024 * 1024,
    });

    expect(readPayloadCache('thread-1', 'payload-huge', 1)).toBeUndefined();
    expect(__payloadCacheStatsForTest()).toEqual({ entries: 0, bytes: 0 });
  });

  it('clearPayloadCacheForThread evicts only the specified thread', () => {
    writePayloadCache('thread-1', 'payload-a', 1, {
      chunks: ['t1a'], hasFullChunks: true, totalSize: 3, isComplete: true, loadedBytes: 3,
    });
    writePayloadCache('thread-1', 'payload-b', 1, {
      chunks: ['t1b'], hasFullChunks: true, totalSize: 3, isComplete: true, loadedBytes: 3,
    });
    writePayloadCache('thread-2', 'payload-a', 1, {
      chunks: ['t2a'], hasFullChunks: true, totalSize: 3, isComplete: true, loadedBytes: 3,
    });

    clearPayloadCacheForThread('thread-1');

    expect(readPayloadCache('thread-1', 'payload-a', 1)).toBeUndefined();
    expect(readPayloadCache('thread-1', 'payload-b', 1)).toBeUndefined();
    expect(readPayloadCache('thread-2', 'payload-a', 1)?.chunks).toEqual(['t2a']);
    expect(__payloadCacheStatsForTest().entries).toBe(1);
    expect(__payloadCacheStatsForTest().bytes).toBe(3);
  });

  // Each row's first entry is owned by 'thread'; its second only looks
  // like it under a naive id join.
  it.each([
    ['does not collide threadId/payloadId boundaries', ['thread', '1'], ['thread-1', '']],
    ['does not collide when ids contain the old separator byte', ['thread', 'payload\0v1'], ['thread\0payload', 'v1']],
  ] as const)('%s', (_name, [ownThread, ownPayload], [otherThread, otherPayload]) => {
    writePayloadCache(ownThread, ownPayload, 1, {
      chunks: ['own'], hasFullChunks: true, totalSize: 3, isComplete: true, loadedBytes: 3,
    });
    writePayloadCache(otherThread, otherPayload, 1, {
      chunks: ['other'], hasFullChunks: true, totalSize: 5, isComplete: true, loadedBytes: 5,
    });

    expect(readPayloadCache(ownThread, ownPayload, 1)?.chunks).toEqual(['own']);
    expect(readPayloadCache(otherThread, otherPayload, 1)?.chunks).toEqual(['other']);

    clearPayloadCacheForThread('thread');

    expect(readPayloadCache(ownThread, ownPayload, 1)).toBeUndefined();
    expect(readPayloadCache(otherThread, otherPayload, 1)?.chunks).toEqual(['other']);
  });
});
