import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { __resetPayloadCacheForTest, writePayloadCache } from './payloadDataCache';
import {
  createPayloadExpansion,
  formatPayloadSize,
  type LiveRevealStream,
  type PayloadExpansionHandle,
} from './payloadExpansion.svelte';

// A live reveal stream over `text` that records which ends it was read at.
function liveStream(text: string): LiveRevealStream & { text: string; reads: number[] } {
  const reads: number[] = [];
  return {
    text,
    reads,
    revealedText(end) {
      reads.push(end);
      return text.slice(0, end);
    },
  };
}

// Reveal `stream` through `end`, the delta being what follows the previous
// reveal of the same stream.
const revealedEnds = new WeakMap<LiveRevealStream, number>();
function reveal(
  expansion: PayloadExpansionHandle,
  stream: LiveRevealStream & { text: string },
  end: number,
  payloadVersion: unknown = 'streaming',
): void {
  const start = revealedEnds.get(stream) ?? 0;
  revealedEnds.set(stream, end);
  expansion.appendLiveDelta(stream, stream.text.slice(start, end), end, payloadVersion);
}

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

    reveal(expansion, liveStream(' ignored'), ' ignored'.length, 2);
    expect(expansion.displayData).toBeNull();

    await expansion.expand();
    expect(expansion.displayData).toBe('seed');

    version = 2;
    reveal(expansion, liveStream(' delta'), ' delta'.length, 2);
    expect(expansion.displayData).toBe('seed delta');
    await expansion.ensureLoaded();
    expect(data).toHaveBeenCalledTimes(1);
  });

  it('keeps expanded content visible across a streaming->settled version flip', async () => {
    // Regression: an expanded thinking block must not blink to empty when the
    // next item arrives. At settle the thinking payload version flips
    // ["id","streaming"] -> ["id",status,updatedAt] (metadata only; identical
    // bytes) and keepExpandedPayloadFresh refetches. loadPreview must keep the
    // loaded body visible and overwrite in place rather than clearing first —
    // a transient null collapses the block height, clamps the timeline
    // scrollTop, and makes the stick-to-bottom spring chase from the top.
    let payloadBytes = 'FULL THINKING TEXT';
    let version = JSON.stringify(['pid', 'streaming']);
    let settled = false;
    const data = setBindingMock('GetPayloadData', async () => ({ data: payloadBytes }));

    const expansion = createPayloadExpansion(
      () => 'pid',
      () => 'tid',
      {
        loadMode: 'full',
        payloadVersion: () => version,
        // Mirrors ThinkingBlock: cache disabled while streaming, enabled at settle.
        cacheEnabled: () => settled,
      },
    );

    await expansion.expand();
    const stream = liveStream('FULL THINKING TEXT more');
    reveal(expansion, stream, stream.text.length, version);
    expect(expansion.displayData).toBe('FULL THINKING TEXT more');

    // Settle: smoother disposes, status flips, version changes, cache opens, and
    // the backend now holds the full text the smoother had already revealed.
    payloadBytes = 'FULL THINKING TEXT more';
    settled = true;
    version = JSON.stringify(['pid', 'completed', 1234]);

    // keepExpandedPayloadFresh fires this on the version change.
    const reload = expansion.ensureLoaded();

    // No blink: the body stays at full height through the relabel and refetch.
    expect(expansion.displayData).toBe('FULL THINKING TEXT more');
    await reload;
    expect(expansion.displayData).toBe('FULL THINKING TEXT more');
    expect(data).toHaveBeenCalledTimes(2);
  });

  it('repairs a stale full payload from the revealed text before appending a delta', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: 'full before ' }));

    const expansion = createPayloadExpansion(
      'payload-live-stale',
      'thread-live-stale',
      { loadMode: 'full', payloadVersion: () => 'streaming' },
    );

    await expansion.expand();
    reveal(expansion, liveStream('live tail more'), 'live tail more'.length);

    expect(expansion.displayData).toBe('full before live tail more');
  });

  it('does not duplicate already-buffered text when the smoother reveals behind a fresh snapshot', async () => {
    // Regression: mid-stream expand. GetPayloadData flushes the live buffer
    // (app_payloads.go) so the snapshot is the FULL text received so far — a
    // longer prefix of the thinking text than the smoother has revealed. The
    // smoother then reveals already-buffered text; that revealed prefix is
    // already contained in displayData, so the merge must be a no-op. A
    // containment-blind merge would append a second copy of the
    // revealed-so-far text, duplicating the whole thinking block on
    // completion.
    const snapshot = 'Para one. Para two. Para three.';
    setBindingMock('GetPayloadData', async () => ({ data: snapshot }));

    const expansion = createPayloadExpansion(
      'payload-buffer-ahead',
      'thread-buffer-ahead',
      { loadMode: 'full', payloadVersion: () => 'streaming' },
    );

    await expansion.expand();
    expect(expansion.displayData).toBe(snapshot);

    const stream = liveStream('Para one. Para two. Para three.Para four.');
    reveal(expansion, stream, 'Para one. Para two. '.length);
    expect(expansion.displayData).toBe(snapshot);
    reveal(expansion, stream, snapshot.length);
    expect(expansion.displayData).toBe(snapshot);

    // The smoother reveals content that arrived after the snapshot.
    reveal(expansion, stream, stream.text.length);
    expect(expansion.displayData).toBe('Para one. Para two. Para three.Para four.');
  });

  it('appends an aligned reveal by offset without reading the revealed text again', async () => {
    const snapshot = 'alpha beta ';
    setBindingMock('GetPayloadData', async () => ({ data: snapshot }));
    const expansion = createPayloadExpansion('payload-offsets', 'thread-offsets', {
      loadMode: 'full',
      payloadVersion: () => 'streaming',
    });
    await expansion.expand();

    const stream = liveStream('alpha beta gamma delta epsilon');
    const words = ['alpha ', 'beta ', 'gamma ', 'delta ', 'epsilon'];
    let end = 0;
    for (const word of words) {
      end += word.length;
      reveal(expansion, stream, end);
    }

    expect(expansion.displayData).toBe('alpha beta gamma delta epsilon');
    expect(stream.reads).toEqual([6]);
  });

  it('aligns a new stream that starts inside the payload', async () => {
    // A smoother re-created mid-stream reseeds from the trimmed summary: its
    // offsets start inside the payload, not at the aligned stream's origin.
    setBindingMock('GetPayloadData', async () => ({ data: 'alpha beta gamma ' }));
    const expansion = createPayloadExpansion('payload-restream', 'thread-restream', {
      loadMode: 'full',
      payloadVersion: () => 'streaming',
    });
    await expansion.expand();

    const first = liveStream('alpha beta gamma delta ');
    reveal(expansion, first, first.text.length);
    expect(expansion.displayData).toBe('alpha beta gamma delta ');

    const resumed = liveStream('gamma delta epsilon zeta');
    reveal(expansion, resumed, 'gamma delta epsilon '.length);
    expect(expansion.displayData).toBe('alpha beta gamma delta epsilon ');
    reveal(expansion, resumed, resumed.text.length);
    expect(expansion.displayData).toBe('alpha beta gamma delta epsilon zeta');
    expect(resumed.reads).toEqual(['gamma delta epsilon '.length]);
  });

  it('aligns a stream again when its reveal no longer matches the body', async () => {
    // A short reveal can match the body at the wrong place. The next reveal
    // disagrees with the text there, and the stream is aligned from its
    // longer revealed text.
    setBindingMock('GetPayloadData', async () => ({ data: 'the cat. the dog. ' }));
    const expansion = createPayloadExpansion('payload-realign', 'thread-realign', {
      loadMode: 'full',
      payloadVersion: () => 'streaming',
    });
    await expansion.expand();

    const stream = liveStream('the dog. runs');
    reveal(expansion, stream, 'the '.length);
    expect(expansion.displayData).toBe('the cat. the dog. ');
    reveal(expansion, stream, 'the dog. '.length);
    expect(expansion.displayData).toBe('the cat. the dog. ');
    reveal(expansion, stream, stream.text.length);
    expect(expansion.displayData).toBe('the cat. the dog. runs');
    expect(stream.reads).toEqual(['the '.length, 'the dog. '.length]);
  });

  it('aligns a stream again when its reveal starts past the end of the body', async () => {
    // A reveal the handle never received leaves a gap between the body and
    // the next delta; the stream's text fills it.
    setBindingMock('GetPayloadData', async () => ({ data: 'alpha ' }));
    const expansion = createPayloadExpansion('payload-gap', 'thread-gap', {
      loadMode: 'full',
      payloadVersion: () => 'streaming',
    });
    await expansion.expand();

    const stream = liveStream('alpha beta gamma');
    reveal(expansion, stream, 'alpha '.length);
    expansion.appendLiveDelta(stream, 'gamma', stream.text.length, 'streaming');
    expect(expansion.displayData).toBe('alpha beta gamma');
    expect(stream.reads).toEqual(['alpha '.length, stream.text.length]);
  });

  it('holds only the latest reveal while the initial full payload load is pending', async () => {
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
    const stream = liveStream('seed live more');
    reveal(expansion, stream, 'seed live'.length);
    reveal(expansion, stream, stream.text.length);
    resolvePayload({ data: 'seed live' });
    await expand;

    expect(expansion.displayData).toBe('seed live more');
    expect(stream.reads).toEqual([stream.text.length]);
  });

  it('repairs a stale pending full payload from the revealed text', async () => {
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
    reveal(expansion, liveStream('live tail more'), 'live tail more'.length);
    resolvePayload({ data: 'full before ' });
    await expand;

    expect(expansion.displayData).toBe('full before live tail more');
  });

  it('does not duplicate buffered text when the pending reveals trail a fresh snapshot', async () => {
    // Several smoother reveals arrive while the initial full load is in flight,
    // then the load resolves with a flushed snapshot AHEAD of the early
    // reveals. Only the reveal that overtakes the snapshot appends its genuine
    // continuation; the text it shares with the snapshot is not re-appended.
    let resolvePayload!: (value: { data: string }) => void;
    setBindingMock('GetPayloadData', async () => (
      new Promise<{ data: string }>((resolve) => {
        resolvePayload = resolve;
      })
    ));

    const expansion = createPayloadExpansion(
      'payload-multi-replay',
      'thread-multi-replay',
      { loadMode: 'full', payloadVersion: () => 'streaming' },
    );

    const expand = expansion.expand();
    await vi.waitFor(() => expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1));

    const stream = liveStream('Para one. Para two. Para three.Para four.');
    reveal(expansion, stream, 'Para one. '.length);
    reveal(expansion, stream, 'Para one. Para two. '.length);
    reveal(expansion, stream, stream.text.length);

    resolvePayload({ data: 'Para one. Para two. Para three.' });
    await expand;

    expect(expansion.displayData).toBe('Para one. Para two. Para three.Para four.');
  });

  it('drops a held reveal whose stream ended before the body loaded', async () => {
    // An interrupted row's last reveal is held while the body loads, and its
    // smoother is disposed before the body arrives. The ended stream reads no
    // text; the body loaded for the row's new version stands, and so does
    // that version.
    let resolvePayload!: (value: { data: string }) => void;
    const firstLoad = new Promise<{ data: string }>((resolve) => {
      resolvePayload = resolve;
    });
    const data = setBindingMock('GetPayloadData', async () => (
      data.mock.calls.length === 1 ? firstLoad : { data: 'body tail' }
    ));
    const expansion = createPayloadExpansion('payload-ended', 'thread-ended', {
      loadMode: 'full',
      payloadVersion: () => 'errored',
      cacheEnabled: false,
    });

    const expand = expansion.expand();
    await vi.waitFor(() => expect(data).toHaveBeenCalledTimes(1));
    const ended: LiveRevealStream = { revealedText: () => null };
    expansion.appendLiveDelta(ended, ' tail', 'body tail'.length, 'streaming');
    resolvePayload({ data: 'body tail' });
    await expand;

    expect(expansion.displayData).toBe('body tail');
    await expansion.ensureLoaded();
    expect(data).toHaveBeenCalledTimes(1);
  });

  it('suppresses an interior-window reconnect reveal already contained in the flushed snapshot', async () => {
    // On reconnect the per-item smoother reseeds from the bounded thinking tail,
    // so its revealed window is an INTERIOR slice of the canonical reasoning
    // rather than an offset-0 prefix. alignRevealed's containment check
    // (textOverlap.ts) recognises the window is already present in the flushed
    // snapshot and appends nothing, so the live merge does not duplicate the
    // snapshot's interior while streaming. At settle the version relabel drives
    // keepExpandedPayloadFresh -> ensureLoaded -> loadPreview, whose
    // `chunks = [result.data]` reloads the complete reasoning.
    let version = 'streaming';
    let flushCall = 0;
    setBindingMock('GetPayloadData', async () => {
      flushCall += 1;
      // 1st call (expand, mid-reconnect): partial flush. 2nd call (settle
      // refetch): the completed reasoning, authoritative and clean.
      return {
        data: flushCall === 1 ? 'alpha beta gamma delta ' : 'alpha beta gamma delta epsilon ',
      };
    });

    const expansion = createPayloadExpansion('payload-heal', 'thread-heal', {
      loadMode: 'full',
      payloadVersion: () => version,
      cacheEnabled: () => version !== 'streaming',
    });

    await expansion.expand();
    expect(expansion.displayData).toBe('alpha beta gamma delta ');

    // Interior-window reveal: the stream reseeded from 'gamma '; its revealed
    // 'gamma delt' is a verbatim interior substring of the flush. It is
    // already shown, so the merge is a no-op, and so is the next reveal that
    // it aligned.
    const stream = liveStream('gamma delta epsilon ');
    reveal(expansion, stream, 'gamma delt'.length);
    expect(expansion.displayData).toBe('alpha beta gamma delta ');
    reveal(expansion, stream, 'gamma delta '.length);
    expect(expansion.displayData).toBe('alpha beta gamma delta ');

    // Turn settles: the payload version relabels. In production
    // keepExpandedPayloadFresh's $effect observes the version change and calls
    // ensureLoaded(); drive that path directly here.
    version = 'settled';
    await expansion.ensureLoaded();

    expect(flushCall).toBe(2);
    expect(expansion.displayData).toBe('alpha beta gamma delta epsilon ');
  });

  it('counts appended live text in the payload size', async () => {
    setBindingMock('GetPayloadData', async () => ({ data: 'seed ' }));
    const expansion = createPayloadExpansion('payload-copy', 'thread-copy', {
      loadMode: 'full',
      payloadVersion: () => 'streaming',
    });
    await expansion.expand();
    const stream = liveStream('seed one two three');
    for (const end of [9, 13, 18]) reveal(expansion, stream, end);

    expect(expansion.displayData).toBe('seed one two three');
    expect(expansion.fullData).toBe('seed one two three');
    expect(expansion.totalSize).toBe('seed one two three'.length);
  });

  it('keeps copies of appended live text, not the reveal slices', async () => {
    // A reveal delta can be a slice of the smoother's whole received text;
    // appendCopied keeps only the appended characters alive
    // (liveTextRetention.test.ts). The test setup has loaded this module, so
    // the recording appendCopied binds to a fresh module graph.
    vi.resetModules();
    const appendCopied = vi.fn();
    vi.doMock('./liveText', async (importOriginal) => {
      const original = await importOriginal<typeof import('./liveText')>();
      appendCopied.mockImplementation(original.appendCopied);
      return { ...original, appendCopied };
    });
    try {
      const bindings = await import('../../test/mocks/bindings-app');
      const fresh = await import('./payloadExpansion.svelte');
      bindings.setBindingMock('GetPayloadData', async () => ({ data: 'seed ' }));
      const expansion = fresh.createPayloadExpansion('payload-copied', 'thread-copied', {
        loadMode: 'full',
        payloadVersion: () => 'streaming',
      });
      await expansion.expand();
      const stream = liveStream('seed one two');
      reveal(expansion, stream, 'seed one'.length);
      reveal(expansion, stream, stream.text.length);

      expect(expansion.displayData).toBe('seed one two');
      expect(appendCopied.mock.calls).toEqual([['', 'one'], ['one', ' two']]);
    } finally {
      vi.doUnmock('./liveText');
    }
  });

  it('drops live text when a version change hydrates the body from the cache', async () => {
    let version = 'streaming';
    setBindingMock('GetPayloadData', async () => ({ data: 'body ' }));
    const expansion = createPayloadExpansion('payload-hydrate-live', 'thread-hydrate-live', {
      loadMode: 'full',
      payloadVersion: () => version,
      cacheEnabled: () => version !== 'streaming',
    });
    await expansion.expand();
    const stream = liveStream('body live');
    reveal(expansion, stream, stream.text.length);
    expect(expansion.displayData).toBe('body live');

    // Another view of the payload already cached its settled body.
    writePayloadCache('thread-hydrate-live', 'payload-hydrate-live', 'settled', {
      chunks: ['body live'],
      hasFullChunks: true,
      totalSize: 9,
      isComplete: true,
      loadedBytes: 9,
    });
    version = 'settled';
    await expansion.ensureLoaded();

    expect(expansion.displayData).toBe('body live');
    expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1);
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
