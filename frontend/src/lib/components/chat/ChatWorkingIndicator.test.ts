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
      // One completed assistant item — no activeTurn push, so the
      // indicator stays hidden regardless of item state.
      makeItem({ id: 'text:0:0', kind: 'assistant_text', status: 'completed' }),
    ]);

    const { queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('renders when activeTurn is set via the wire push path', async () => {
    vi.setSystemTime(new Date(1_000_000));
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 1_000_000 });

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    const node = getByTestId('chat-working-indicator');
    expect(node.textContent).toMatch(/Working/);
    expect(node.textContent).toMatch(/Esc to interrupt/);
  });

  it('anchors elapsed seconds to activeTurn.startedAt, not item.createdAt', async () => {
    // Deliberately mismatch item createdAt (1_000) with activeTurn.startedAt
    // (3_000) so the test fails if the component regresses to reading
    // items again. At now=5_000 the correct anchor is 3_000 → 2s elapsed.
    vi.setSystemTime(new Date(5_000));
    const pane = await buildPane(undefined, [
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
        createdAt: 2_000,
        itemIndex: 1,
      }),
    ]);
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 3_000 });

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('2s');

    // Advance 5 more seconds; counter should tick up via the interval.
    vi.advanceTimersByTime(5_000);
    await Promise.resolve();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('7s');
  });

  it('invariant 22: hidden even when a running tool_call exists, if activeTurn is null', async () => {
    vi.setSystemTime(new Date(10_000));
    // Stuck running tool_call with no activeTurn push. A pre-refactor
    // items-derived implementation would have lit up the indicator here.
    // Under invariant 22 it must stay hidden.
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool:stuck',
        kind: 'tool_call',
        status: 'running',
        isBackground: false,
        createdAt: 0,
      }),
    ]);

    const { queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('stays hidden when approvals are pending without an active turn push', async () => {
    // Pre-refactor, pending approvals were folded into isTurnActive via
    // the items-derived path. Post-refactor (invariant 22), pending
    // approvals ride INSIDE an active turn — the provider emits
    // turn_started before any approval request — so `isTurnActive` stays
    // false until the wire push arrives. A synthetic approval without a
    // matching activeTurn is an unusual state; the indicator should not
    // fire on its own.
    vi.setSystemTime(new Date(2_000));
    const pane = await buildPane();
    pane.addApproval({
      requestId: 'req-1',
      threadId: 'thread-1',
      kind: 'tool',
      summary: 'approve?',
    } as unknown as Parameters<typeof pane.addApproval>[0]);

    const { queryByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('stops ticking when activeTurn flips to null', async () => {
    vi.setSystemTime(new Date(0));
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });

    const { getByTestId, queryByTestId } = render(ChatWorkingIndicator, {
      props: { pane },
    });

    vi.advanceTimersByTime(3_000);
    await Promise.resolve();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('3s');

    // Turn settles → activeTurn = null. The indicator unmounts entirely,
    // and the self-owned interval is cleared by the effect cleanup.
    pane.clearTurnState();
    await Promise.resolve();
    expect(queryByTestId('chat-working-indicator')).toBeNull();

    // Advance the clock a long way. If the interval leaked, it would
    // keep firing on the detached component, but since the DOM node is
    // gone there's nothing to assert against directly — the contract
    // we can observe is that the indicator stays hidden.
    vi.advanceTimersByTime(30_000);
    await Promise.resolve();
    expect(queryByTestId('chat-working-indicator')).toBeNull();
  });

  it('re-seeds the counter on a fresh turn (different startedAt)', async () => {
    // First turn runs for 4s then settles.
    vi.setSystemTime(new Date(0));
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    vi.advanceTimersByTime(4_000);
    await Promise.resolve();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('4s');

    pane.clearTurnState();
    await Promise.resolve();

    // A fresh turn starts 10s later with a new startedAt. The effect
    // should re-seed `now` to the current wall clock and anchor to the
    // new startedAt, so the displayed elapsed is 0s at mount time.
    vi.setSystemTime(new Date(14_000));
    pane.setActiveTurn({ turnId: 't2', turnIndex: 1, startedAt: 14_000 });
    await Promise.resolve();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('0s');

    // And the counter ticks from that anchor, not from the first turn's.
    vi.advanceTimersByTime(2_000);
    await Promise.resolve();
    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('2s');
  });

  it('clamps a backend clock-skew (startedAt in the future) to 0s', async () => {
    vi.setSystemTime(new Date(1_000));
    const pane = await buildPane();
    // startedAt ahead of the wall clock — rendering -4s would be wrong;
    // the component clamps to zero.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 5_000 });

    const { getByTestId } = render(ChatWorkingIndicator, { props: { pane } });

    expect(getByTestId('chat-working-indicator-elapsed').textContent).toBe('0s');
  });
});
