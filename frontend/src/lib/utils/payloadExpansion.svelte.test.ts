import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { __resetPayloadCacheForTest, writePayloadCache } from './payloadDataCache';
import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';

describe('payloadExpansion', () => {
  beforeEach(() => {
    resetBindingMocks();
    __resetPayloadCacheForTest();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('loads preview before full payload and hydrates from cache after collapse', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'PREVIEW ',
      nextOffset: 8,
      totalSize: 40_960,
      isComplete: false,
    }));
    setBindingMock('GetPayloadChunk', async () => ({
      data: 'FULL PAYLOAD',
      offset: 8,
      nextOffset: 20,
      totalSize: 40_960,
      isComplete: true,
    }));

    const expansion = createPayloadExpansion('payload-1', 'thread-1');
    await expansion.expand();

    expect(expansion.expanded).toBe(true);
    expect(expansion.previewData).toBe('PREVIEW ');
    expect(expansion.hasMore).toBe(true);
    expect(getBindingMock('GetPayloadChunk')).not.toHaveBeenCalled();

    await expansion.showFull();
    expect(expansion.fullData).toBe('PREVIEW FULL PAYLOAD');
    expect(expansion.displayData).toBe('PREVIEW FULL PAYLOAD');

    expansion.collapse();
    expect(expansion.expanded).toBe(false);
    expect(expansion.previewData).toBeNull();
    expect(expansion.fullData).toBeNull();

    await expansion.expand();
    expect(expansion.displayData).toBe('PREVIEW FULL PAYLOAD');
    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledTimes(1);
  });

  it('uses backend byte offsets instead of UTF-16 string length', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'éé',
      nextOffset: 4,
      totalSize: 8,
      isComplete: false,
    }));
    setBindingMock('GetPayloadChunk', async () => ({
      data: ' tail',
      offset: 4,
      nextOffset: 9,
      totalSize: 9,
      isComplete: true,
    }));

    const expansion = createPayloadExpansion('payload-1', 'thread-1');
    await expansion.expand();
    await expansion.showFull();

    expect(getBindingMock('GetPayloadChunk')).toHaveBeenCalledWith(
      'thread-1',
      'payload-1',
      4,
      256 * 1024,
    );
    expect(expansion.fullData).toBe('éé tail');
  });

  it('formats byte sizes for preview footer labels', () => {
    expect(formatPayloadSize(512)).toBe('512 B');
    expect(formatPayloadSize(2_048)).toBe('2.0 KB');
    expect(formatPayloadSize(2_097_152)).toBe('2.0 MB');
  });

  it('can load the full payload on expand', async () => {
    const data = setBindingMock('GetPayloadData', async () => ({ data: 'FULL PAYLOAD' }));
    const preview = setBindingMock('GetPayloadPreview', async () => {
      throw new Error('full mode should not fetch a preview');
    });
    const chunk = setBindingMock('GetPayloadChunk', async () => {
      throw new Error('full mode should not fetch chunks');
    });

    const expansion = createPayloadExpansion(
      'payload-full',
      'thread-full',
      { loadMode: 'full' },
    );

    await expansion.expand();

    expect(expansion.displayData).toBe('FULL PAYLOAD');
    expect(expansion.fullData).toBe('FULL PAYLOAD');
    expect(expansion.hasMore).toBe(false);
    expect(data).toHaveBeenCalledWith('thread-full', 'payload-full');
    expect(preview).not.toHaveBeenCalled();
    expect(chunk).not.toHaveBeenCalled();
  });

  it('appends live deltas only after a full payload is expanded', async () => {
    let version = 1;
    const data = setBindingMock('GetPayloadData', async () => ({ data: 'seed' }));

    const expansion = createPayloadExpansion(
      'payload-live',
      'thread-live',
      { loadMode: 'full', payloadVersion: () => version },
    );

    expansion.appendLiveDelta(' ignored', 2);
    expect(expansion.displayData).toBeNull();

    await expansion.expand();
    expect(expansion.displayData).toBe('seed');

    version = 2;
    expansion.appendLiveDelta(' delta', 2);
    expect(expansion.displayData).toBe('seed delta');
    await expansion.ensureLoaded();
    expect(data).toHaveBeenCalledTimes(1);
  });

  it('repairs a stale full payload from the previous live tail before appending a delta', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: 'full before ' }));

    const expansion = createPayloadExpansion(
      'payload-live-stale',
      'thread-live-stale',
      { loadMode: 'full', payloadVersion: () => 'streaming' },
    );

    await expansion.expand();
    expansion.appendLiveDelta(' more', 'streaming', 'live tail');

    expect(expansion.displayData).toBe('full before live tail more');
  });

  it('queues live deltas while the initial full payload load is pending', async () => {
    let resolvePayload!: (value: { data: string }) => void;
    setBindingMock('GetPayloadData', async () => (
      new Promise<{ data: string }>((resolve) => {
        resolvePayload = resolve;
      })
    ));

    const expansion = createPayloadExpansion(
      'payload-live-pending',
      'thread-live-pending',
      { loadMode: 'full', payloadVersion: () => 'streaming' },
    );

    const expand = expansion.expand();
    await vi.waitFor(() => expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1));
    expansion.appendLiveDelta(' live', 'streaming');
    resolvePayload({ data: 'seed live' });
    await expand;

    expect(expansion.displayData).toBe('seed live');
  });

  it('repairs a stale pending full payload from the previous live tail before queued deltas', async () => {
    let resolvePayload!: (value: { data: string }) => void;
    setBindingMock('GetPayloadData', async () => (
      new Promise<{ data: string }>((resolve) => {
        resolvePayload = resolve;
      })
    ));

    const expansion = createPayloadExpansion(
      'payload-live-pending-stale',
      'thread-live-pending-stale',
      { loadMode: 'full', payloadVersion: () => 'streaming' },
    );

    const expand = expansion.expand();
    await vi.waitFor(() => expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1));
    expansion.appendLiveDelta(' more', 'streaming', 'live tail');
    resolvePayload({ data: 'full before ' });
    await expand;

    expect(expansion.displayData).toBe('full before live tail more');
  });


  it('skips cache reads and writes when cache is disabled', async () => {
    writePayloadCache('thread-cache-off', 'payload-cache-off', 1, {
      chunks: ['stale cached'],
      hasFullChunks: true,
      totalSize: 12,
      isComplete: true,
      loadedBytes: 12,
    });
    const data = setBindingMock('GetPayloadData', async () => ({ data: 'fresh payload' }));

    const expansion = createPayloadExpansion(
      'payload-cache-off',
      'thread-cache-off',
      { loadMode: 'full', payloadVersion: () => 1, cacheEnabled: false },
    );

    expect(expansion.displayData).toBeNull();
    await expansion.expand();
    expect(expansion.displayData).toBe('fresh payload');

    expansion.collapse();
    await expansion.expand();

    expect(data).toHaveBeenCalledTimes(2);
  });

  it('surfaces non-string payload data as a load error instead of caching it', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: { text: 'not a string' } as unknown as string,
      nextOffset: 1,
      totalSize: 1,
      isComplete: true,
    }));

    const expansion = createPayloadExpansion('payload-1', 'thread-1');
    await expansion.expand();

    expect(expansion.expanded).toBe(true);
    expect(expansion.displayData).toBeNull();
    expect(expansion.error).toContain('GetPayloadPreview returned non-string payload data');
  });

  it('times out a stuck preview request and can retry', async () => {
    vi.useFakeTimers();
    let call = 0;
    setBindingMock('GetPayloadPreview', () => {
      call += 1;
      if (call === 1) return new Promise(() => {});
      return Promise.resolve({
        data: 'retry ok',
        nextOffset: 8,
        totalSize: 8,
        isComplete: true,
      });
    });

    const expansion = createPayloadExpansion(
      'payload-timeout',
      'thread-timeout',
      { requestTimeoutMs: 5 },
    );
    const first = expansion.expand();
    await vi.advanceTimersByTimeAsync(5);
    await first;

    expect(expansion.expanded).toBe(true);
    expect(expansion.loading).toBe(false);
    expect(expansion.error).toContain('timed out');

    await expansion.retry();
    expect(expansion.error).toBeNull();
    expect(expansion.displayData).toBe('retry ok');
    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledTimes(2);
  });

  it('loads when a payload id appears after the handle was expanded', async () => {
    let payloadId: string | undefined;
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'late payload',
      nextOffset: 12,
      totalSize: 12,
      isComplete: true,
    }));

    const expansion = createPayloadExpansion(
      () => payloadId,
      'thread-late-payload',
    );

    await expansion.expand();
    expect(expansion.expanded).toBe(true);
    expect(expansion.displayData).toBeNull();
    expect(getBindingMock('GetPayloadPreview')).not.toHaveBeenCalled();

    payloadId = 'payload-late';
    await expansion.expand();

    expect(expansion.displayData).toBe('late payload');
    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledWith(
      'thread-late-payload',
      'payload-late',
      32 * 1024,
    );
  });

  it('reloads an expanded handle when the payload version changes', async () => {
    let version = 1;
    const preview = setBindingMock('GetPayloadPreview', async () => ({
      data: version === 1 ? 'first snapshot' : 'second snapshot',
      nextOffset: 14,
      totalSize: 14,
      isComplete: true,
    }));

    const expansion = createPayloadExpansion(
      'payload-versioned',
      'thread-versioned',
      { payloadVersion: () => version },
    );

    await expansion.expand();
    expect(expansion.displayData).toBe('first snapshot');

    version = 2;
    await expansion.ensureLoaded();

    expect(expansion.displayData).toBe('second snapshot');
    expect(preview).toHaveBeenCalledTimes(2);
  });

  it('concatenates correctly across multiple showFull() calls (chunk-buffer regression test)', async () => {
    // Pin the chunk-buffer refactor: showFull() called repeatedly
    // should accumulate chunks via a `chunks: string[]` buffer and
    // join lazily, not via O(N²) cumulative string concat. The
    // wire-correctness lives in the join order.
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'P',
      nextOffset: 1,
      totalSize: 4,
      isComplete: false,
    }));
    let chunkCall = 0;
    setBindingMock('GetPayloadChunk', async (_thread: string, _payload: string, offset: number) => {
      chunkCall += 1;
      // Three sequential chunks: A at 1, B at 2, C at 3.
      const data = chunkCall === 1 ? 'A' : chunkCall === 2 ? 'B' : 'C';
      return {
        data,
        offset,
        nextOffset: offset + 1,
        totalSize: 4,
        isComplete: chunkCall === 3,
      };
    });

    const expansion = createPayloadExpansion('payload-multi', 'thread-multi');
    await expansion.expand();
    expect(expansion.previewData).toBe('P');
    expect(expansion.fullData).toBeNull();

    await expansion.showFull();
    expect(expansion.displayData).toBe('PA');
    expect(expansion.previewData).toBeNull();
    expect(expansion.fullData).toBe('PA');
    expect(expansion.hasMore).toBe(true);

    await expansion.showFull();
    expect(expansion.displayData).toBe('PAB');
    expect(expansion.hasMore).toBe(true);

    await expansion.showFull();
    expect(expansion.displayData).toBe('PABC');
    expect(expansion.hasMore).toBe(false);
    expect(expansion.fullData).toBe('PABC');

    // Collapse drops the buffer entirely.
    expansion.collapse();
    expect(expansion.displayData).toBeNull();
    expect(expansion.previewData).toBeNull();
    expect(expansion.fullData).toBeNull();
  });

  it('hydrates synchronously from the module cache without touching the binding', () => {
    // Pre-populate the cache as if a prior expansion had loaded this
    // payload. The new handle must surface `displayData` at construction
    // — no async fetch, no binding call. This is the architectural
    // contract that prevents the empty-then-loaded oscillation on
    // thread re-entry.
    writePayloadCache('thread-cache', 'payload-cached', 99, {
      chunks: ['cached chunk'],
      hasFullChunks: true,
      totalSize: 12,
      isComplete: true,
      loadedBytes: 12,
    });
    const preview = setBindingMock('GetPayloadPreview', async () => {
      throw new Error('cache hydration must not refetch');
    });

    const expansion = createPayloadExpansion(
      'payload-cached',
      'thread-cache',
      { payloadVersion: () => 99 },
    );

    expect(expansion.displayData).toBe('cached chunk');
    expect(expansion.fullData).toBe('cached chunk');
    expect(expansion.totalSize).toBe(12);
    expect(expansion.isComplete).toBe(true);
    // `expanded` stays false unless loadOnMount is set — toggle-style
    // consumers expect their thread-switch reset of `expanded=false` to
    // survive the cache hit.
    expect(expansion.expanded).toBe(false);
    expect(preview).not.toHaveBeenCalled();
  });

  it('cache hit with loadOnMount also flips expanded=true synchronously', () => {
    writePayloadCache('thread-cache', 'payload-cached', 7, {
      chunks: ['hit'],
      hasFullChunks: true,
      totalSize: 3,
      isComplete: true,
      loadedBytes: 3,
    });
    const preview = setBindingMock('GetPayloadPreview', async () => {
      throw new Error('loadOnMount cache hit must not refetch');
    });

    // loadOnMount registers a $effect, so the handle must be created
    // inside an effect root in tests (mimics component instantiation).
    const dispose = $effect.root(() => {
      const expansion = createPayloadExpansion(
        'payload-cached',
        'thread-cache',
        { payloadVersion: () => 7, loadOnMount: true },
      );

      expect(expansion.expanded).toBe(true);
      expect(expansion.displayData).toBe('hit');
    });
    dispose();
    expect(preview).not.toHaveBeenCalled();
  });

  it('loadOnMount reloads the same payload id when the payload version changes', async () => {
    let fetchCount = 0;
    const preview = setBindingMock('GetPayloadPreview', async () => {
      fetchCount += 1;
      return {
        data: fetchCount === 1 ? 'payload v1' : 'payload v2',
        nextOffset: 10,
        totalSize: 10,
        isComplete: true,
      };
    });

    let expansion!: ReturnType<typeof createPayloadExpansion>;
    const dispose = $effect.root(() => {
      expansion = createPayloadExpansion(
        'payload-auto',
        'thread-auto',
        { loadOnMount: true },
      );
    });

    await vi.waitFor(() => expect(expansion.displayData).toBe('payload v1'));
    expansion.setPayloadVersion(2);
    await vi.waitFor(() => expect(expansion.displayData).toBe('payload v2'));

    expect(preview).toHaveBeenCalledTimes(2);
    dispose();
  });

  it('loadOnMount does not consume a newer version while an older request is loading', async () => {
    const resolvers: Array<(value: {
      data: string;
      nextOffset: number;
      totalSize: number;
      isComplete: boolean;
    }) => void> = [];
    const preview = setBindingMock('GetPayloadPreview', async () => (
      new Promise((resolve) => {
        resolvers.push(resolve);
      })
    ));

    let expansion!: ReturnType<typeof createPayloadExpansion>;
    const dispose = $effect.root(() => {
      expansion = createPayloadExpansion(
        'payload-race',
        'thread-race',
        { payloadVersion: () => 1, loadOnMount: true },
      );
    });

    await vi.waitFor(() => expect(preview).toHaveBeenCalledTimes(1));
    expansion.setPayloadVersion(2);

    resolvers[0]!({
      data: 'payload v1',
      nextOffset: 10,
      totalSize: 10,
      isComplete: true,
    });
    await vi.waitFor(() => expect(preview).toHaveBeenCalledTimes(2));
    resolvers[1]!({
      data: 'payload v2',
      nextOffset: 10,
      totalSize: 10,
      isComplete: true,
    });

    await vi.waitFor(() => expect(expansion.displayData).toBe('payload v2'));
    dispose();
  });

  it('setPayloadVersion hydrates a cached replacement without refetching', async () => {
    writePayloadCache('thread-cache', 'payload-cached', 1, {
      chunks: ['old cached'],
      hasFullChunks: true,
      totalSize: 10,
      isComplete: true,
      loadedBytes: 10,
    });
    writePayloadCache('thread-cache', 'payload-cached', 2, {
      chunks: ['new cached'],
      hasFullChunks: true,
      totalSize: 10,
      isComplete: true,
      loadedBytes: 10,
    });
    const preview = setBindingMock('GetPayloadPreview', async () => {
      throw new Error('version cache hit must not refetch');
    });

    const expansion = createPayloadExpansion(
      'payload-cached',
      'thread-cache',
      { payloadVersion: () => 1 },
    );
    expect(expansion.displayData).toBe('old cached');

    expansion.setPayloadVersion(2);
    expect(expansion.displayData).toBe('new cached');

    await expansion.expand();
    expect(expansion.displayData).toBe('new cached');
    expect(preview).not.toHaveBeenCalled();
  });

  it('setPayloadVersion prevents an older preview request from overwriting a cached replacement', async () => {
    let resolvePreview!: (value: {
      data: string;
      nextOffset: number;
      totalSize: number;
      isComplete: boolean;
    }) => void;
    setBindingMock('GetPayloadPreview', async () => (
      new Promise((resolve) => {
        resolvePreview = resolve;
      })
    ));
    writePayloadCache('thread-race', 'payload-race', 2, {
      chunks: ['new cached'],
      hasFullChunks: true,
      totalSize: 10,
      isComplete: true,
      loadedBytes: 10,
    });

    const expansion = createPayloadExpansion(
      'payload-race',
      'thread-race',
      { payloadVersion: () => 1 },
    );
    const firstExpand = expansion.expand();
    await vi.waitFor(() => expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledTimes(1));

    expansion.setPayloadVersion(2);
    expect(expansion.displayData).toBe('new cached');

    resolvePreview({
      data: 'old preview',
      nextOffset: 11,
      totalSize: 11,
      isComplete: true,
    });
    await firstExpand;

    expect(expansion.payloadVersion).toBe(2);
    expect(expansion.displayData).toBe('new cached');
  });

  it('setPayloadVersion prevents an older full chunk from overwriting a cached replacement', async () => {
    let resolveChunk!: (value: {
      data: string;
      offset: number;
      nextOffset: number;
      totalSize: number;
      isComplete: boolean;
    }) => void;
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'preview v1',
      nextOffset: 10,
      totalSize: 30,
      isComplete: false,
    }));
    setBindingMock('GetPayloadChunk', async () => (
      new Promise((resolve) => {
        resolveChunk = resolve;
      })
    ));
    writePayloadCache('thread-race', 'payload-race', 2, {
      chunks: ['new cached'],
      hasFullChunks: true,
      totalSize: 10,
      isComplete: true,
      loadedBytes: 10,
    });

    const expansion = createPayloadExpansion(
      'payload-race',
      'thread-race',
      { payloadVersion: () => 1 },
    );
    await expansion.expand();
    expect(expansion.displayData).toBe('preview v1');

    const fullLoad = expansion.showFull();
    await vi.waitFor(() => expect(getBindingMock('GetPayloadChunk')).toHaveBeenCalledTimes(1));

    expansion.setPayloadVersion(2);
    expect(expansion.displayData).toBe('new cached');

    resolveChunk({
      data: ' old full chunk',
      offset: 10,
      nextOffset: 25,
      totalSize: 25,
      isComplete: true,
    });
    await fullLoad;

    expect(expansion.payloadVersion).toBe(2);
    expect(expansion.displayData).toBe('new cached');
  });

  it('expand waits for an existing full-payload load to finish', async () => {
    let resolvePayload!: (value: { data: string }) => void;
    const data = setBindingMock('GetPayloadData', async () => (
      new Promise<{ data: string }>((resolve) => {
        resolvePayload = resolve;
      })
    ));

    let expansion!: ReturnType<typeof createPayloadExpansion>;
    const dispose = $effect.root(() => {
      expansion = createPayloadExpansion(
        'payload-full-pending',
        'thread-full-pending',
        { loadMode: 'full', loadOnMount: true },
      );
    });

    await vi.waitFor(() => expect(data).toHaveBeenCalledTimes(1));
    const manualExpand = expansion.expand();
    resolvePayload({ data: 'FULL PAYLOAD' });
    await manualExpand;

    expect(expansion.displayData).toBe('FULL PAYLOAD');
    expect(data).toHaveBeenCalledTimes(1);
    dispose();
  });

  it('cache miss on version mismatch falls through to a fresh fetch', async () => {
    writePayloadCache('thread-cache', 'payload-cached', 1, {
      chunks: ['stale'],
      hasFullChunks: true,
      totalSize: 5,
      isComplete: true,
      loadedBytes: 5,
    });
    const preview = setBindingMock('GetPayloadPreview', async () => ({
      data: 'fresh',
      nextOffset: 5,
      totalSize: 5,
      isComplete: true,
    }));

    const expansion = createPayloadExpansion(
      'payload-cached',
      'thread-cache',
      { payloadVersion: () => 2 },
    );

    // Pre-expand: cache miss ⇒ no synchronous chunks.
    expect(expansion.displayData).toBeNull();

    await expansion.expand();
    expect(expansion.displayData).toBe('fresh');
    expect(preview).toHaveBeenCalledTimes(1);
  });
});
