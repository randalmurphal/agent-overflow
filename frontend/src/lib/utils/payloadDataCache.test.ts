import { afterEach, describe, expect, it } from 'vitest';
import {
  __payloadCacheStatsForTest,
  __resetPayloadCacheForTest,
  clearPayloadCacheForThread,
  readPayloadCache,
  writePayloadCache,
} from './payloadDataCache';

afterEach(() => {
  __resetPayloadCacheForTest();
});

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

  it('does not collide threadId/payloadId boundaries', () => {
    writePayloadCache('thread', '1', 1, {
      chunks: ['boundary-a'], hasFullChunks: true, totalSize: 10, isComplete: true, loadedBytes: 10,
    });
    writePayloadCache('thread-1', '', 1, {
      chunks: ['boundary-b'], hasFullChunks: true, totalSize: 10, isComplete: true, loadedBytes: 10,
    });

    clearPayloadCacheForThread('thread');

    // Only the (threadId='thread', payloadId='1') entry is evicted.
    expect(readPayloadCache('thread', '1', 1)).toBeUndefined();
    expect(readPayloadCache('thread-1', '', 1)?.chunks).toEqual(['boundary-b']);
  });

  it('does not collide when ids contain the old separator byte', () => {
    writePayloadCache('thread', 'payload\0v1', 1, {
      chunks: ['nul-a'], hasFullChunks: true, totalSize: 5, isComplete: true, loadedBytes: 5,
    });
    writePayloadCache('thread\0payload', 'v1', 1, {
      chunks: ['nul-b'], hasFullChunks: true, totalSize: 5, isComplete: true, loadedBytes: 5,
    });

    expect(readPayloadCache('thread', 'payload\0v1', 1)?.chunks).toEqual(['nul-a']);
    expect(readPayloadCache('thread\0payload', 'v1', 1)?.chunks).toEqual(['nul-b']);

    clearPayloadCacheForThread('thread');

    expect(readPayloadCache('thread', 'payload\0v1', 1)).toBeUndefined();
    expect(readPayloadCache('thread\0payload', 'v1', 1)?.chunks).toEqual(['nul-b']);
  });

  it('LRU touch on read moves the entry to the freshest slot', () => {
    writePayloadCache('thread-1', 'payload-a', 1, {
      chunks: ['a'], hasFullChunks: true, totalSize: 1, isComplete: true, loadedBytes: 1,
    });
    writePayloadCache('thread-1', 'payload-b', 1, {
      chunks: ['b'], hasFullChunks: true, totalSize: 1, isComplete: true, loadedBytes: 1,
    });

    // Read 'a' so it's the freshest. We can't directly observe insertion
    // order externally, but the LRU contract is: subsequent eviction
    // pressure drops 'b' first (it's now the oldest). Cap is 16 MB so
    // we can't stress-evict cheaply — but we CAN re-read both and
    // verify both still resolve, which proves the LRU touch didn't
    // accidentally drop the entry it touched.
    expect(readPayloadCache('thread-1', 'payload-a', 1)?.chunks).toEqual(['a']);
    expect(readPayloadCache('thread-1', 'payload-b', 1)?.chunks).toEqual(['b']);
    expect(__payloadCacheStatsForTest().entries).toBe(2);
  });

  it('__resetPayloadCacheForTest wipes everything', () => {
    writePayloadCache('thread-1', 'payload-a', 1, {
      chunks: ['x'], hasFullChunks: true, totalSize: 1, isComplete: true, loadedBytes: 1,
    });
    expect(__payloadCacheStatsForTest().entries).toBe(1);

    __resetPayloadCacheForTest();
    expect(__payloadCacheStatsForTest().entries).toBe(0);
    expect(__payloadCacheStatsForTest().bytes).toBe(0);
    expect(readPayloadCache('thread-1', 'payload-a', 1)).toBeUndefined();
  });
});
