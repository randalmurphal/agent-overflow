import { beforeEach, describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import LazyContentBlock from './LazyContentBlock.svelte';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { MAX_INLINE_BYTES } from '../../utils/inlineThreshold';
import { __resetPayloadCacheForTest } from '../../utils/payloadDataCache';
import { DEFAULT_PAYLOAD_CHUNK_BYTES, DEFAULT_PAYLOAD_PREVIEW_BYTES } from '../../utils/payloadExpansion.svelte';

describe('<LazyContentBlock>', () => {
  beforeEach(() => {
    resetBindingMocks();
    __resetPayloadCacheForTest();
  });

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

  it('links the toggle to the body it reveals (aria-controls)', async () => {
    // The disclosure contract (utils/activityRunClip.ts): an enclosing
    // activity run finds expandable bodies through `aria-expanded` +
    // `aria-controls`, so a toggle without the link would silently opt this
    // body out of the run's height cap. The body wraps BOTH states — the
    // preview is its collapsed baseline.
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'PREVIEW BODY',
      totalSize: 12,
      isComplete: true,
    }));
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { container, findByTestId, getByTestId } = render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview },
    });

    const toggle = getByTestId('lazy-content-toggle');
    const controls = toggle.getAttribute('aria-controls');
    expect(controls).toBeTruthy();
    const body = container.querySelector(`[id="${controls}"]`);
    expect(body).not.toBeNull();
    expect(body!.contains(getByTestId('lazy-content-preview'))).toBe(true);

    await fireEvent.click(toggle);
    expect(body!.contains(await findByTestId('lazy-content-preview'))).toBe(true);
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
      totalSize: 12,
      isComplete: true,
    }));
    const full = setBindingMock('GetPayloadChunk', async () => ({
      data: 'FULL BODY',
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
      nextOffset: 'PREVIEW BODY'.length,
      totalSize: 64 * 1024,
      isComplete: false,
    }));
    setBindingMock('GetPayloadChunk', async () => ({
      data: 'FULL BODY CONTENT',
      offset: 'PREVIEW BODY'.length,
      nextOffset: 'FULL BODY CONTENT'.length,
      totalSize: 'FULL BODY CONTENT'.length,
      isComplete: true,
    }));
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { findByTestId, getByTestId } = render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview },
    });

    const toggle = getByTestId('lazy-content-toggle');
    await fireEvent.click(toggle);

    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledWith('thread-1', 'p1', DEFAULT_PAYLOAD_PREVIEW_BYTES);
    expect(getBindingMock('GetPayloadChunk')).not.toHaveBeenCalled();
    expect((await findByTestId('lazy-content-preview')).textContent).toBe('PREVIEW BODY');
    expect(getByTestId('lazy-content-show-full').textContent).toContain('64.0 KB');

    await fireEvent.click(getByTestId('lazy-content-show-full'));

    expect(getBindingMock('GetPayloadChunk')).toHaveBeenCalledWith(
      'thread-1',
      'p1',
      'PREVIEW BODY'.length,
      DEFAULT_PAYLOAD_CHUNK_BYTES,
    );
    expect((await findByTestId('lazy-content-full')).textContent).toBe('PREVIEW BODYFULL BODY CONTENT');
  });

  it('discarding local state on collapse re-expands from the payload cache', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'PREVIEW',
      totalSize: 8 * 1024,
      isComplete: true,
    }));
    setBindingMock('GetPayloadChunk', async () => ({
      data: 'FULL',
      offset: 7,
      nextOffset: 4,
      totalSize: 4,
      isComplete: true,
    }));
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { findByTestId, getByTestId, queryByTestId } = render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview },
    });

    const toggle = getByTestId('lazy-content-toggle');
    await fireEvent.click(toggle);
    expect((await findByTestId('lazy-content-preview')).textContent).toBe('PREVIEW');

    await fireEvent.click(toggle);
    expect(queryByTestId('lazy-content-full')).toBeNull();
    expect(getByTestId('lazy-content-preview')).toBeInTheDocument();

    await fireEvent.click(toggle);
    expect((await findByTestId('lazy-content-preview')).textContent).toBe('PREVIEW');
    expect(getBindingMock('GetPayloadPreview')).toHaveBeenCalledTimes(1);
  });

  it('surfaces preview fetch errors inline without swallowing them', async () => {
    setBindingMock('GetPayloadPreview', async () => {
      throw new Error('preview boom');
    });
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { findByTestId, getByTestId } = render(LazyContentBlock, {
      props: { threadId: 'thread-1', payloadId: 'p1', preview },
    });
    await fireEvent.click(getByTestId('lazy-content-toggle'));
    const errorNode = await findByTestId('lazy-content-error');
    expect(errorNode.textContent).toContain('preview boom');
    expect(errorNode.getAttribute('role')).toBe('alert');
  });
});
