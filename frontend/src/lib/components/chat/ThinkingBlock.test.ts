import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import ThinkingBlock from './ThinkingBlock.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
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

  it('renders the brain icon in the gutter', () => {
    // The think row used to share the checklist icon, which read as
    // "todo list" next to actual TodoWrite rows. The brain icon is a
    // distinct visual that doesn't collide with any other tool kind.
    const { container } = render(ThinkingBlock, {
      props: {
        item: makeItem({
          kind: 'thinking',
          summary: 'reasoning content',
          payloadId: 'thinking-payload',
        }),
      },
    });
    const icon = container.querySelector('svg[data-icon]');
    expect(icon?.getAttribute('data-icon')).toBe('brain');
    expect(icon?.getAttribute('aria-label')).toBe('think');
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
    setBindingMock('GetPayloadData', async () => ({ data: 'full reasoning text' }));
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
    setBindingMock('GetPayloadData', async () => ({ data: 'loaded reasoning text' }));
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

  it('streams into the expanded full body and refetches current payload after collapse', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'seed',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    const payloads = ['seed', 'seed live collapsed'];
    const getPayloadData = setBindingMock('GetPayloadData', async () => ({
      data: payloads.shift() ?? 'seed live collapsed',
    }));

    const { container, getByRole, rerender } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed'));

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: ' live',
      updatedAt: 2,
    });
    await rerender({ pane, item: pane.items[0] });
    await tick();
    expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed live');
    expect(getPayloadData).toHaveBeenCalledTimes(1);

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await tick();
    expect(container.querySelector('[data-testid="thinking-body"]')?.className).toMatch(/max-h-\[3lh\]/);

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: ' collapsed',
      updatedAt: 3,
    });
    await rerender({ pane, item: pane.items[0] });
    await tick();
    expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed live collapsed');

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => expect(container.querySelector('[data-testid="thinking-body"]')?.textContent).toBe('seed live collapsed'));
    expect(getPayloadData).toHaveBeenCalledTimes(2);
  });

  it('keeps the live tail visible when an expanded streaming payload read is stale', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'live tail',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    setBindingMock('GetPayloadData', async () => ({ data: 'full payload before ' }));

    const { container, getByRole } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));

    await waitFor(() => {
      expect(container.querySelector('[data-testid="thinking-body"]')?.textContent)
        .toBe('full payload before live tail');
    });
  });

  it('repairs a stale expanded streaming payload before appending the next live delta', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'live tail',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    setBindingMock('GetPayloadData', async () => ({ data: 'full payload before ' }));

    const { container, getByRole, rerender } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => {
      expect(container.querySelector('[data-testid="thinking-body"]')?.textContent)
        .toBe('full payload before live tail');
    });

    pane.applyItemDelta({
      threadId: 'thread-1',
      itemId: 'think:0:0',
      kind: 'thinking',
      delta: ' more',
      updatedAt: 2,
    });
    await rerender({ pane, item: pane.items[0] });
    await tick();

    expect(container.querySelector('[data-testid="thinking-body"]')?.textContent)
      .toBe('full payload before live tail more');
  });

  it('copies the refreshed completed payload when a row settles while expanded', async () => {
    const thinking = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      summary: 'seed',
      payloadId: 'thinking-payload',
      updatedAt: 1,
    });
    const pane = await buildPane(makeThread({ id: 'thread-1' }), [thinking]);
    const payloads = ['seed', 'seed final'];
    setBindingMock('GetPayloadData', async () => ({
      data: payloads.shift() ?? 'seed final',
    }));
    const writeText = vi.fn(async () => {});
    Object.defineProperty(navigator, 'clipboard', {
      value: { writeText },
      configurable: true,
      writable: true,
    });

    const { getByRole, getByLabelText, rerender } = render(ThinkingBlock, {
      props: { pane, item: pane.items[0] },
    });

    await fireEvent.click(getByRole('button', { name: /toggle thinking block/i }));
    await waitFor(() => expect(getByRole('button', { name: /toggle thinking block/i }).getAttribute('aria-expanded')).toBe('true'));

    pane.upsertItem({
      ...pane.items[0],
      status: 'completed',
      summary: 'seed final',
      updatedAt: 2,
    });
    await rerender({ pane, item: pane.items[0] });
    await fireEvent.click(getByLabelText('Copy thinking'));

    await waitFor(() => expect(writeText).toHaveBeenCalledWith('seed final'));
  });
});
