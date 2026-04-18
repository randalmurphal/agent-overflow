import { describe, expect, it } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import LazyContentBlock from './LazyContentBlock.svelte';
import { getBindingMock, setBindingMock } from '../../../test/mocks/bindings-app';
import { MAX_INLINE_BYTES } from '../../utils/inlineThreshold';

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

  it('does not call GetPayloadData on mount — only on click', () => {
    const fullBody = 'FULL BODY';
    const mock = setBindingMock('GetPayloadData', async () => fullBody);
    render(LazyContentBlock, {
      props: { payloadId: 'p1', preview: 'a'.repeat(MAX_INLINE_BYTES + 1) },
    });
    expect(mock).not.toHaveBeenCalled();
  });

  it('fetches and swaps in the full body when "Show all" is clicked', async () => {
    const fullBody = 'FULL BODY CONTENT';
    setBindingMock('GetPayloadData', async () => fullBody);
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId } = render(LazyContentBlock, {
      props: { payloadId: 'p1', preview },
    });

    const toggle = getByTestId('lazy-content-toggle');
    await fireEvent.click(toggle);
    // Let the microtask chain for the await resolve.
    await Promise.resolve();
    await Promise.resolve();

    expect(getByTestId('lazy-content-full').textContent).toBe(fullBody);
    expect(toggle.textContent?.trim()).toBe('Show less');
    expect(toggle.getAttribute('aria-expanded')).toBe('true');
    // GetPayloadData was called with the payloadId exactly once.
    const mock = getBindingMock('GetPayloadData');
    expect(mock).toHaveBeenCalledTimes(1);
    expect(mock?.mock.calls[0]).toEqual(['p1']);
  });

  it('collapses back to the preview on "Show less" without re-fetching', async () => {
    setBindingMock('GetPayloadData', async () => 'FULL');
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId, queryByTestId } = render(LazyContentBlock, {
      props: { payloadId: 'p1', preview },
    });

    const toggle = getByTestId('lazy-content-toggle');
    await fireEvent.click(toggle);
    await Promise.resolve();
    await Promise.resolve();
    expect(getByTestId('lazy-content-full')).toBeInTheDocument();

    await fireEvent.click(toggle);
    expect(queryByTestId('lazy-content-full')).toBeNull();
    expect(getByTestId('lazy-content-preview')).toBeInTheDocument();
    // Re-expanding shouldn't fire a second fetch — the cached body wins.
    await fireEvent.click(toggle);
    await Promise.resolve();
    await Promise.resolve();
    expect(getBindingMock('GetPayloadData')).toHaveBeenCalledTimes(1);
  });

  it('surfaces fetch errors inline without swallowing them', async () => {
    setBindingMock('GetPayloadData', async () => {
      throw new Error('network boom');
    });
    const preview = 'a'.repeat(MAX_INLINE_BYTES + 1);
    const { getByTestId } = render(LazyContentBlock, {
      props: { payloadId: 'p1', preview },
    });
    await fireEvent.click(getByTestId('lazy-content-toggle'));
    await Promise.resolve();
    await Promise.resolve();
    const errorNode = getByTestId('lazy-content-error');
    expect(errorNode.textContent).toContain('network boom');
    expect(errorNode.getAttribute('role')).toBe('alert');
  });
});
