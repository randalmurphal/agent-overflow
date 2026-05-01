import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import ThinkingBlock from './ThinkingBlock.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

describe('<ThinkingBlock>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
  });

  it('does not render a copy button while collapsed', () => {
    const { container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });
    expect(container.querySelector('[aria-label="Copy thinking"]')).toBeNull();
  });

  it('shows a copy button once expanded with loaded content', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'full reasoning text',
      totalSize: 19,
      isComplete: true,
    }));

    const { getByRole, getByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => expect(getByLabelText('Copy thinking')).toBeInTheDocument());
  });

  it('hides the copy button while the payload is loading', async () => {
    let resolvePreview: ((value: { data: string; totalSize: number; isComplete: boolean }) => void) = () => {};
    setBindingMock('GetPayloadPreview', () => new Promise((resolve) => {
      resolvePreview = resolve;
    }));

    const { getByRole, container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    // While the preview is in-flight the button must not appear yet.
    expect(container.querySelector('[aria-label="Copy thinking"]')).toBeNull();
    resolvePreview({ data: 'loaded', totalSize: 6, isComplete: true });
  });

  it('writes the loaded payload to the clipboard on click', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'loaded reasoning text',
      totalSize: 21,
      isComplete: true,
    }));
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    const { getByRole, getByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    const copyButton = await waitFor(() => getByLabelText('Copy thinking'));
    await fireEvent.click(copyButton);
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('loaded reasoning text'));
  });
});
