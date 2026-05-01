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

  // Pins the per-wire-round emission cadence (see
  // internal/triage/AGENTS.md "Wire-round vs logical-turn"): the
  // elapsed-time counter resets on each new round because each round
  // gets a fresh `startedAt`. Verifies the indicator naturally tracks
  // round-level state — round 1 ends, indicator hides; round 2
  // starts at a new time, elapsed counter restarts at the round-2
  // anchor.
  it('elapsed timer resets per wire round', async () => {
    const pane = await buildPane();

    // Round 1 begins at t=7s; current clock is t=10s → 3s elapsed.
    pane.setActiveTurn({ turnId: 'round-1', turnIndex: 0, startedAt: 7_000 });
    const { getByTestId, queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });
    await tick();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('3s');

    // Round 1 settles. Indicator hides.
    pane.settleTurn({
      turnId: 'round-1',
      turnIndex: 0,
      startedAt: 7_000,
      completedAt: 11_000,
      tokenUsage: null,
      assistantMessageId: null,
      aborted: false,
      stopReason: '',
      errorMessage: '',
    });
    await tick();
    expect(queryByTestId('chat-working-indicator')).toBeNull();

    // Time passes (model is idle while bg task settles). Clock now t=15s.
    vi.advanceTimersByTime(5_000);

    // Round 2 begins at t=15s. Indicator re-appears with a fresh 0s
    // anchor — NOT the cumulative 8s since the round 1 startedAt.
    pane.setActiveTurn({ turnId: 'round-2', turnIndex: 0, startedAt: 15_000 });
    await tick();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('0s');

    // The data-round-id attribute carries the round id so component
    // tests can pin which round is currently rendered.
    expect(getByTestId('chat-working-indicator').dataset.roundId).toBe('round-2');
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
