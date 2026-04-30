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

  // Regression: previously the per-pane activeTurn was cleared on
  // switchThread while the global active-turn registry survived. After
  // switching back to the original thread the working indicator stayed
  // dark even though the turn was still in flight backend-side. Now
  // both surfaces read from the same global registry, so a switch
  // away-and-back does not lose the indicator.
  it('survives thread-switch when the global active-turn registry still has a record', async () => {
    const { makeThread } = await import('../../../test/helpers/chat');
    const otherThread = makeThread({ id: 'thread-other', title: 'Other' });
    setBindingMock('SwitchThread', async () => otherThread);

    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: 7_000 });

    const { getByTestId, queryByTestId, rerender } = render(ChatWorkingIndicator, { props: { pane } });
    await tick();
    expect(getByTestId('chat-working-indicator')).toBeInTheDocument();

    // Switch away to a different thread — the indicator should hide.
    await pane.switchThread(otherThread);
    await rerender({ pane });
    await tick();
    expect(queryByTestId('chat-working-indicator')).toBeNull();

    // Switch back to the original thread. Backend still has the turn
    // open; the global registry remembers it, so the indicator must
    // re-render even though the per-pane state was cleared on switch.
    setBindingMock('SwitchThread', async () => makeThread({ id: 'thread-1' }));
    await pane.switchThread(makeThread({ id: 'thread-1' }));
    await rerender({ pane });
    await tick();
    expect(getByTestId('chat-working-indicator')).toBeInTheDocument();
  });
});
