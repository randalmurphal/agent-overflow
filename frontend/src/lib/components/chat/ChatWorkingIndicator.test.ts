import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ChatWorkingIndicator from './ChatWorkingIndicator.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import {
  getQueueForThread,
  replaceQueueForThread,
  resetSendQueueForTest,
  type QueueItem,
} from '../../stores/sendQueue.svelte';
import {
  clearPendingSend,
  projectSendStarted,
  resetForTest as resetThreadStatuses,
} from '../../stores/threadStatuses.svelte';

function enqueueSimple(threadId: string, message: string): void {
  const item: QueueItem = {
    id: `queue:${message}-${Math.random()}`,
    threadId,
    message,
    attachmentIds: [],
    sourceProposedPlan: null,
    revisionSourceProposedPlan: null,
    enqueuedAt: Date.now(),
  };
  const current = getQueueForThread(threadId);
  replaceQueueForThread(threadId, [...current, item]);
}

function popQueueFront(threadId: string): QueueItem | undefined {
  const current = getQueueForThread(threadId);
  if (current.length === 0) return undefined;
  const [head, ...rest] = current;
  replaceQueueForThread(threadId, rest);
  return head;
}

describe('<ChatWorkingIndicator>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(10_000));
    resetBindingMocks();
    resetSendQueueForTest();
    resetThreadStatuses();
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

  // Bridge predicate: keep the spinner up across the gap between a
  // round completing and the next arming when a queued user message
  // is about to drain. Without this, the indicator would flicker for
  // 50–200ms (the SendMessageWithOptions RPC roundtrip).
  describe('drain bridge', () => {
    it('stays visible when a queue item is present and no turn is active', async () => {
      const pane = await buildPane();
      enqueueSimple(pane.threadId ?? 'thread-1', 'queued follow-up');

      const { getByTestId, queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });
      await tick();

      // Indicator still shows even though activeTurn is null.
      expect(getByTestId('chat-working-indicator')).toBeInTheDocument();
      // Elapsed-counter span is hidden during the bridge moment to
      // avoid the "Working for 0s" flash.
      expect(queryByTestId('chat-working-indicator-elapsed')).toBeNull();
      expect(getByTestId('chat-working-indicator-bridge').textContent).toBe('Working');
    });

    it('stays visible when pendingSend is set during the drain RPC roundtrip', async () => {
      // Simulates the moment between popFront and the next
      // turn_started arriving: queue is empty (popped), pendingSend
      // is true (we just dispatched), activeTurn is null.
      const pane = await buildPane();
      const tid = pane.threadId ?? 'thread-1';
      enqueueSimple(tid, 'q1');
      popQueueFront(tid);
      projectSendStarted(tid);

      const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });
      await tick();

      expect(getByTestId('chat-working-indicator')).toBeInTheDocument();
      expect(getByTestId('chat-working-indicator-bridge')).toBeInTheDocument();
    });

    it('keeps the indicator visible after a drain failure (queue restored, pendingSend cleared)', async () => {
      // The drain failure path: SendMessageWithOptions threw, the
      // helper restored the popped item to the front and called
      // clearPendingSend so the pill collapses to running-via-queue
      // only. The indicator MUST stay visible because the queue is
      // still non-empty.
      const pane = await buildPane();
      const tid = pane.threadId ?? 'thread-1';
      enqueueSimple(tid, 'q1');
      const popped = popQueueFront(tid);
      projectSendStarted(tid);
      // Failure: restore + clear pendingSend. In the new architecture
      // the backend owns drain failure recovery; this test only
      // exercises the bridge predicate, so we seed the restored
      // state directly via the snapshot replace API.
      if (popped) {
        const current = getQueueForThread(tid);
        replaceQueueForThread(tid, [popped, ...current]);
      }
      clearPendingSend(tid);

      const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });
      await tick();

      expect(getByTestId('chat-working-indicator')).toBeInTheDocument();
      expect(getByTestId('chat-working-indicator-bridge')).toBeInTheDocument();
    });

    it('elapsed counter resumes from the new round startedAt after the bridge', async () => {
      const pane = await buildPane();
      const tid = pane.threadId ?? 'thread-1';

      // Round 1 in flight.
      pane.setActiveTurn({ turnId: 'round-1', turnIndex: 0, startedAt: 9_000 });
      const { getByTestId, queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });
      await tick();
      expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('1s');

      // Round 1 ends, we enqueue and popFront simulating drain start.
      // pendingSend is the only signal still active.
      pane.settleTurn({
        turnId: 'round-1',
        turnIndex: 0,
        startedAt: 9_000,
        completedAt: 10_000,
        tokenUsage: null,
        assistantMessageId: null,
        aborted: false,
        stopReason: '',
        errorMessage: '',
      });
      enqueueSimple(tid, 'queued follow-up');
      popQueueFront(tid);
      projectSendStarted(tid);
      await tick();
      // Bridge moment: indicator visible but elapsed span replaced by
      // plain "Working".
      expect(queryByTestId('chat-working-indicator-elapsed')).toBeNull();
      expect(getByTestId('chat-working-indicator-bridge').textContent).toBe('Working');

      // Round 2 arms with a fresh startedAt (clock is at 10_000).
      vi.advanceTimersByTime(2_000);
      pane.setActiveTurn({ turnId: 'round-2', turnIndex: 0, startedAt: 12_000 });
      await tick();
      expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('0s');
      expect(queryByTestId('chat-working-indicator-bridge')).toBeNull();
    });

    it('hides only after both activeTurn is null and the queue / pendingSend bridge clears', async () => {
      const pane = await buildPane();
      const tid = pane.threadId ?? 'thread-1';
      enqueueSimple(tid, 'q1');

      const { queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });
      await tick();
      expect(queryByTestId('chat-working-indicator')).toBeInTheDocument();

      // Queue empties (drain succeeded and round 2 hasn't armed yet,
      // but pendingSend is also cleared, e.g. session terminated). At
      // that point the indicator should hide — there's nothing to
      // bridge against.
      popQueueFront(tid);
      await tick();
      expect(queryByTestId('chat-working-indicator')).toBeNull();
    });

    it('disables the interrupt button during the bridge (no active turn to interrupt)', async () => {
      const pane = await buildPane();
      enqueueSimple(pane.threadId ?? 'thread-1', 'q1');

      const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });
      await tick();

      const button = getByTestId('chat-working-indicator-interrupt') as HTMLButtonElement;
      expect(button.disabled).toBe(true);
    });
  });
});
