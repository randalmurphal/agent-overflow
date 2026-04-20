import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import ChatWorkingIndicator from './ChatWorkingIndicator.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks } from '../../../test/mocks/bindings-app';

describe('<ChatWorkingIndicator>', () => {
  beforeEach(() => {
    resetBindingMocks();
    // Pin the clock so elapsed-seconds math is deterministic. The component
    // seeds `now = Date.now()` and ticks via setInterval; fake timers let
    // us advance the clock by known amounts.
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('is hidden when the pane has no active turn', async () => {
    const pane = await buildPane(undefined, [
      // One completed assistant item — isTurnActive === false.
      makeItem({ id: 'text:0:0', kind: 'assistant_text', status: 'completed' }),
    ]);

    const { queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('renders when a streaming assistant message is active', async () => {
    vi.setSystemTime(new Date(1_000_000));
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'streaming',
        createdAt: 1_000_000,
      }),
    ]);

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    const node = getByTestId('chat-working-indicator');
    expect(node.textContent).toMatch(/Working/);
    expect(node.textContent).toMatch(/Esc to interrupt/);
  });

  it('counts elapsed seconds from the earliest streaming/running item', async () => {
    vi.setSystemTime(new Date(5_000));
    const pane = await buildPane(undefined, [
      // Earliest is the tool_call at t=1_000ms; so at t=5_000ms we expect 4s.
      makeItem({
        id: 'tool:0:0',
        kind: 'tool_call',
        status: 'running',
        isBackground: false,
        createdAt: 1_000,
      }),
      makeItem({
        id: 'text:0:1',
        kind: 'assistant_text',
        status: 'streaming',
        createdAt: 3_000,
        itemIndex: 1,
      }),
    ]);

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('4s');

    // Advance 5 more seconds; counter should tick up.
    vi.advanceTimersByTime(5_000);
    await Promise.resolve();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('9s');
  });

  it('ignores backgrounded tool calls when selecting the anchor', async () => {
    vi.setSystemTime(new Date(10_000));
    const pane = await buildPane(undefined, [
      // Backgrounded tools don't count as "active turn", so this alone
      // should keep isTurnActive === false and hide the indicator.
      makeItem({
        id: 'tool:bg',
        kind: 'tool_call',
        status: 'running',
        isBackground: true,
        createdAt: 0,
      }),
    ]);

    const { queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('treats a pending approval as active even without running items', async () => {
    vi.setSystemTime(new Date(2_000));
    const pane = await buildPane();
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      kind: 'tool',
      summary: 'approve?',
    } as unknown as Parameters<typeof pane.addApproval>[0]);

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    // No anchor item → elapsed reads 0s, but the indicator is visible
    // because isTurnActive is derived from pendingApprovals too.
    expect(getByTestId('chat-working-indicator')).toBeInTheDocument();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('0s');
  });
});
