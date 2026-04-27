import { beforeEach, describe, expect, it } from 'vitest';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { createPayloadExpansion, formatPayloadSize } from './payloadExpansion.svelte';

describe('payloadExpansion', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('loads preview before full payload and discards both on collapse', async () => {
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
    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledTimes(2);
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
});
