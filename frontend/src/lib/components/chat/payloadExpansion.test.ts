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
});
