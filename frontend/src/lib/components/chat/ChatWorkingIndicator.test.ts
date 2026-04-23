import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatWorkingIndicator from './ChatWorkingIndicator.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

describe('<ChatWorkingIndicator>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(10_000));
    resetBindingMocks();
    setBindingMock('InterruptTurn', async () => {});
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('is hidden when no turn is active', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });
    await tick();

    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('renders turn-start elapsed time while active and hides after settlement', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 7_000 });

    const { getByTestId, queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });
    await tick();

    expect(getByTestId('chat-working-indicator')).toBeInTheDocument();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('3s');

    vi.advanceTimersByTime(2_000);
    await tick();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('5s');

    pane.settleTurn({
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 7_000,
      completedAt: 12_000,
      tokenUsage: null,
      assistantMessageId: null,
      aborted: false,
      stopReason: '',
      errorMessage: '',
    });
    await tick();

    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('routes button clicks through InterruptTurn for the active thread', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 7_000 });
    const interrupt = setBindingMock('InterruptTurn', async () => {});

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });
    await tick();
    await fireEvent.click(getByTestId('chat-working-indicator-interrupt'));
    await tick();

    expect(interrupt).toHaveBeenCalledTimes(1);
    expect(interrupt).toHaveBeenCalledWith('thread-1');
  });
});
