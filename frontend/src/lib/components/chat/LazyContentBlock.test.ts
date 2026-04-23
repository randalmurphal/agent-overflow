import { describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import LazyContentBlock from './LazyContentBlock.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { MAX_INLINE_BYTES } from '../../utils/inlineThreshold';
import { DEFAULT_PAYLOAD_CHUNK_BYTES, DEFAULT_PAYLOAD_PREVIEW_BYTES } from './payloadExpansion.svelte';

describe('<LazyContentBlock>', () => {
  it('renders the preview verbatim and no toggle when preview is short', () => {
    const { getByTestId, queryByTestId } = render(LazyContentBlock, {
      props: { payloadId: 'p1', preview: 'small preview' },
    });
    expect(getByTestId('lazy-content-preview').textContent).toBe('small preview');
    expect(queryByTestId('lazy-content-toggle')).toBeNull();
  });

  it('omits the toggle when the preview is short and payloadId is absent', () => {
    const { queryByTestId } = render(LazyContentBlock, {
      props: { payloadId: undefined, preview: 'small preview' },
    });
    expect(queryByTestId('lazy-content-toggle')).toBeNull();
  });

  it('truncates an oversized preview to MAX_INLINE_BYTES plus ellipsis', () => {
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 500);
    const { getByTestId } = render(LazyContentBlock, {
      props: { payloadId: 'p1', preview },
    });
    const node = getByTestId('lazy-content-preview');
    expect(node.textContent?.length).toBe(MAX_INLINE_BYTES + 1);
    expect(node.textContent?.endsWith('…')).toBe(true);
  });

  it('shows the "Show all" button when preview is oversized and payloadId is present', () => {
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId } = render(LazyContentBlock, {
      props: { payloadId: 'p1', preview },
    });
    const toggle = getByTestId('lazy-content-toggle');
    expect(toggle.textContent?.trim()).toBe('Show all');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');
  });

  it('keeps the button hidden when preview is oversized but payloadId is missing', () => {
    // Without a payloadId there's nothing to fetch, so the component falls
    // back to pure truncation and hides the affordance.
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { queryByTestId } = render(LazyContentBlock, {
      props: { payloadId: undefined, preview },
    });
    expect(queryByTestId('lazy-content-toggle')).toBeNull();
  });

  it('uses the custom label for the expand button', () => {
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId } = render(LazyContentBlock, {
      props: { payloadId: 'p1', preview, label: 'Expand output' },
    });
    expect(getByTestId('lazy-content-toggle').textContent?.trim()).toBe('Expand output');
  });

  it('does not call payload bindings on mount — only on click', () => {
    const preview = setBindingMock('GetPayloadPreview', async () => ({
      data: 'PREVIEW',
      html: 'PREVIEW',
      totalSize: 12,
      isComplete: true,
    }));
    const full = setBindingMock('GetPayloadChunk', async () => ({
      data: 'FULL BODY',
      html: 'FULL BODY',
      offset: 7,
      nextOffset: 9,
      totalSize: 9,
      isComplete: true,
    }));
    render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview: 'a'.repeat(MAX_INLINE_BYTES + 1) },
    });
    expect(preview).not.toHaveBeenCalled();
    expect(full).not.toHaveBeenCalled();
  });

  it('fetches the preview on expand and the full body only when requested', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'PREVIEW BODY',
      html: 'PREVIEW BODY',
      totalSize: 64 * 1024,
      isComplete: false,
    }));
    setBindingMock('GetPayloadChunk', async () => ({
      data: 'FULL BODY CONTENT',
      html: 'FULL BODY CONTENT',
      offset: 'PREVIEW BODY'.length,
      nextOffset: 'FULL BODY CONTENT'.length,
      totalSize: 'FULL BODY CONTENT'.length,
      isComplete: true,
    }));
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId } = render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview },
    });

    const toggle = getByTestId('lazy-content-toggle');
    await fireEvent.click(toggle);
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledWith('thread-1', 'p1', DEFAULT_PAYLOAD_PREVIEW_BYTES);
    expect(getBindingMock('GetPayloadChunk')).not.toHaveBeenCalled();
    expect(getByTestId('lazy-content-preview').textContent).toBe('PREVIEW BODY');
    expect(getByTestId('lazy-content-show-full').textContent).toContain('64.0 KB');

    await fireEvent.click(getByTestId('lazy-content-show-full'));
    await Promise.resolve();
    await Promise.resolve();

    expect(getBindingMock('GetPayloadChunk')).toHaveBeenCalledWith(
      'thread-1',
      'p1',
      'PREVIEW BODY'.length,
      DEFAULT_PAYLOAD_CHUNK_BYTES,
    );
    expect(getByTestId('lazy-content-full').textContent).toBe('FULL BODY CONTENT');
  });

  it('discarding on collapse causes re-expand to refetch the preview', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'PREVIEW',
      html: 'PREVIEW',
      totalSize: 8 * 1024,
      isComplete: true,
    }));
    setBindingMock('GetPayloadChunk', async () => ({
      data: 'FULL',
      html: 'FULL',
      offset: 7,
      nextOffset: 4,
      totalSize: 4,
      isComplete: true,
    }));
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId, queryByTestId } = render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview },
    });

    const toggle = getByTestId('lazy-content-toggle');
    await fireEvent.click(toggle);
    await Promise.resolve();
    await Promise.resolve();
    expect(getByTestId('lazy-content-preview').textContent).toBe('PREVIEW');

    await fireEvent.click(toggle);
    expect(queryByTestId('lazy-content-full')).toBeNull();
    expect(getByTestId('lazy-content-preview')).toBeInTheDocument();

    await fireEvent.click(toggle);
    await Promise.resolve();
    await Promise.resolve();
    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledTimes(2);
  });

  it('surfaces preview fetch errors inline without swallowing them', async () => {
    setBindingMock('GetPayloadPreview', async () => {
      throw new Error('preview boom');
    });
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId } = render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview },
    });
    await fireEvent.click(getByTestId('lazy-content-toggle'));
    await Promise.resolve();
    await Promise.resolve();
    const errorNode = getByTestId('lazy-content-error');
    expect(errorNode.textContent).toContain('preview boom');
    expect(errorNode.getAttribute('role')).toBe('alert');
  });
});
