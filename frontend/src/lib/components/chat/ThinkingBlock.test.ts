import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
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

  it('renders body text and timestamp inline; no standalone region', () => {
    const { container, queryByRole } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
          createdAt: Date.UTC(2026, 0, 1, 14, 32, 0),
        }),
      },
    });
    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.textContent).toBe('reasoning content');
    expect(queryByRole('region', { name: 'Thinking Content' })).toBeNull();
    expect(container.querySelector('time[datetime]')).not.toBeNull();
  });

  it('tail-clamps the body to 3 lines via max-height when collapsed', () => {
    const { container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'completed',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });
    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.className).toMatch(/max-h-\[3lh\]/);
    expect(body?.className).toMatch(/overflow-hidden/);
  });

  it('removes the max-height cap once expanded', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'full reasoning text',
      totalSize: 19,
      isComplete: true,
    }));
    const { container, getByRole } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'completed',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await tick();

    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.className).not.toMatch(/max-h-\[3lh\]/);
    expect(body?.className).not.toMatch(/overflow-hidden/);
  });

  it('stays tail-clamped through the streaming → settled boundary', async () => {
    const baseItem = makeItem({
      kind: 'thinking',
      status: 'streaming',
      summary: 'live delta text',
      payloadId: 'thinking-payload',
    });
    const { container, rerender } = render(ThinkingBlock, {
      props: { item: baseItem },
    });

    let body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.className).toMatch(/max-h-\[3lh\]/);

    await rerender({ item: { ...baseItem, status: 'completed' } });
    await tick();

    body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.className).toMatch(/max-h-\[3lh\]/);
  });

  it('renders the live item summary as the body content during streaming', () => {
    const { container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'streaming',
          summary: 'live delta text',
          payloadId: 'thinking-payload',
        }),
      },
    });
    const body = container.querySelector('[data-testid="thinking-body"]');
    expect(body?.textContent).toContain('live delta text');
  });

  it('exposes the copy button when there is non-empty content', () => {
    const { getByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });
    expect(getByLabelText('Copy thinking')).toBeInTheDocument();
  });

  it('omits the copy button while streaming', () => {
    const { queryByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          status: 'streaming',
          summary: 'live partial reasoning',
          payloadId: 'thinking-payload',
        }),
      },
    });
    expect(queryByLabelText('Copy thinking')).toBeNull();
  });

  it('copies the full payload via the getter, even without an explicit expand', async () => {
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

    const { getByLabelText } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'preview only',
          payloadId: 'thinking-payload',
        }),
      },
    });

    await fireEvent.click(getByLabelText('Copy thinking'));
    await waitFor(() => expect(writeText).toHaveBeenCalledWith('loaded reasoning text'));
  });
});
