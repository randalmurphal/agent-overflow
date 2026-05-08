import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import ActivityRail from './ActivityRail.svelte';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetForTest as resetThreadStatuses } from '../../stores/threadStatuses.svelte';
import { resetForTest as resetSendQueue, replaceQueueForThread } from '../../stores/sendQueue.svelte';
import { __resetActivityRailUiPrefsForTest, __resetLiveTodoUiPrefsForTest, LIVE_TODO_AUTOHIDE_MS } from '../../stores/thread.svelte';
import type { QueueItem } from '../../stores/sendQueue.svelte';

function backgroundLaunch(overrides = {}) {
  return makeItem({
    id: 'bg-launch',
    kind: 'tool_call',
    role: 'assistant',
    isBackground: true,
    status: 'running',
    summary: 'Bash: sleep 30',
    toolName: 'Bash',
    payloadKind: 'command_output',
    payloadId: 'pay-bg-1',
    payloadMeta: JSON.stringify({ exitCode: 0, lineCount: 0 }),
    createdAt: Date.now() - 1_000,
    updatedAt: Date.now() - 1_000,
    ...overrides,
  });
}

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
  replaceQueueForThread(threadId, [item]);
}

describe('<ActivityRail>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetThreadStatuses();
    resetSendQueue();
    __resetLiveTodoUiPrefsForTest();
    __resetActivityRailUiPrefsForTest();
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    setBindingMock('InterruptTurn', async () => {});
    setBindingMock('StopClaudeTask', async () => {});
    setBindingMock('CleanCodexBackgroundTerminals', async () => {});
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('renders nothing when idle, no todos, no background', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    expect(queryByTestId('activity-rail')).toBeNull();
  });

  it('shows the working segment with elapsed timer when a turn is active', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working-elapsed')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-working-bridge')).toBeNull();
    // Shimmer mounts whenever a turn is active.
    expect(await findByTestId('activity-rail-shimmer')).toBeInTheDocument();
  });

  it('mounts the shimmer in the bridge state too (queue item, no active turn)', async () => {
    const pane = await buildPane();
    enqueueSimple(pane.threadId!, 'queued bridge');
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail-shimmer')).toBeInTheDocument();
  });

  it('does not mount the shimmer when only todos are visible (rail visible, isWorking false)', async () => {
    // Guards against a regression that hangs the shimmer off
    // `railVisible` instead of the stricter `isWorking` predicate.
    const pane = await buildPane();
    pane.setLiveTodo([{ step: 'one', status: 'inProgress' }]);
    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-shimmer')).toBeNull();
  });

  it('shows the Todos toggle and counts when a liveTodo snapshot is present', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'Refactor send pipeline', status: 'inProgress' },
      { step: 'Migrate dispatcher tests', status: 'inProgress' },
      { step: 'Update wire contract docs', status: 'pending' },
      { step: 'Read flush_queue.go for context', status: 'completed' },
    ]);
    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    const toggle = await findByTestId('activity-rail-todos-toggle');
    expect(toggle).toBeInTheDocument();
    expect((await findByTestId('activity-rail-todos-count')).textContent?.trim()).toBe('2/4');
    // Body collapsed by default.
    expect(queryByTestId('activity-rail-todos-body')).toBeNull();

    await fireEvent.click(toggle);
    await tick();
    expect(await findByTestId('activity-rail-todos-body')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-todos-list')).toBeInTheDocument();
  });

  it('shows the Background toggle and expanded body with rows when tasks are running', async () => {
    const launch = backgroundLaunch();
    setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
    const pane = await buildPane();
    pane.upsertItem(launch);

    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await tick();

    const toggle = await findByTestId('activity-rail-background-toggle');
    expect(toggle).toBeInTheDocument();
    expect((await findByTestId('activity-rail-background-count')).textContent?.trim()).toBe('1');
    expect(queryByTestId('activity-rail-background-body')).toBeNull();

    await fireEvent.click(toggle);
    await tick();
    expect(await findByTestId('activity-rail-background-body')).toBeInTheDocument();
    expect(await findByTestId('background-task-tray-row')).toBeInTheDocument();
  });

  it('opens Todos and Background independently', async () => {
    const launch = backgroundLaunch();
    setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
    const pane = await buildPane();
    pane.upsertItem(launch);
    pane.setLiveTodo([{ step: 'one', status: 'inProgress' }]);

    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await tick();

    await fireEvent.click(await findByTestId('activity-rail-todos-toggle'));
    await tick();
    expect(queryByTestId('activity-rail-todos-body')).not.toBeNull();
    expect(queryByTestId('activity-rail-background-body')).toBeNull();

    await fireEvent.click(await findByTestId('activity-rail-background-toggle'));
    await tick();
    expect(queryByTestId('activity-rail-todos-body')).not.toBeNull();
    expect(queryByTestId('activity-rail-background-body')).not.toBeNull();

    // Closing Todos leaves Background open.
    await fireEvent.click(await findByTestId('activity-rail-todos-toggle'));
    await tick();
    expect(queryByTestId('activity-rail-todos-body')).toBeNull();
    expect(queryByTestId('activity-rail-background-body')).not.toBeNull();
  });

  it('hides the working timer when neither active nor bridging', async () => {
    // Only a liveTodo set — rail visible, but the Working segment must
    // not render.
    const pane = await buildPane();
    pane.setLiveTodo([{ step: 'one', status: 'inProgress' }]);
    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-working')).toBeNull();
    expect(queryByTestId('activity-rail-interrupt')).toBeNull();
  });

  it('per-thread expansion state survives a thread switch', async () => {
    const threadA = makeThread({ id: 'thread-A' });
    const threadB = makeThread({ id: 'thread-B' });
    setBindingMock('SwitchThread', async (id: unknown) => {
      const target = id === 'thread-B' ? threadB : threadA;
      return target;
    });
    const pane = await buildPane(threadA);
    pane.setLiveTodo([{ step: 'one', status: 'inProgress' }]);

    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();

    // Open Todos on thread A.
    await fireEvent.click(await findByTestId('activity-rail-todos-toggle'));
    await tick();
    expect(queryByTestId('activity-rail-todos-body')).not.toBeNull();

    // Switch to thread B — fresh per-thread default is collapsed.
    await pane.switchThread(threadB);
    pane.setLiveTodo([{ step: 'other', status: 'inProgress' }]);
    await tick();
    expect(queryByTestId('activity-rail-todos-body')).toBeNull();

    // Switch back to thread A — Todos remembered as open.
    await pane.switchThread(threadA);
    pane.setLiveTodo([{ step: 'one', status: 'inProgress' }]);
    await tick();
    expect(queryByTestId('activity-rail-todos-body')).not.toBeNull();
  });

  it('clicking interrupt dispatches an InterruptTurn RPC', async () => {
    let called = 0;
    setBindingMock('InterruptTurn', async () => {
      called++;
    });
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() });
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-interrupt'));
    await tick();
    expect(called).toBe(1);
  });

  it('shows the bridge label when a queue item is present without an active turn', async () => {
    const pane = await buildPane();
    enqueueSimple(pane.threadId!, 'queued follow-up');

    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working-bridge')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-working-elapsed')).toBeNull();
  });

  it('disables the interrupt button during the bridge (no active turn)', async () => {
    const pane = await buildPane();
    enqueueSimple(pane.threadId!, 'queued');
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    const btn = await findByTestId('activity-rail-interrupt');
    expect((btn as HTMLButtonElement).disabled).toBe(true);
  });

  it('passes the active threadId to the InterruptTurn RPC', async () => {
    const seen: unknown[] = [];
    setBindingMock('InterruptTurn', async (id: unknown) => {
      seen.push(id);
    });
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() });
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-interrupt'));
    await tick();
    expect(seen).toEqual([pane.thread!.id]);
  });

  it('sorts todo steps in-progress -> pending -> completed and preserves wire order within buckets', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'one', status: 'completed' },
      { step: 'two', status: 'pending' },
      { step: 'three', status: 'inProgress' },
      { step: 'four', status: 'completed' },
      { step: 'five', status: 'pending' },
    ]);
    pane.toggleActivityRailTodos();

    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();

    const list = await findByTestId('activity-rail-todos-list');
    const labels = Array.from(list.querySelectorAll('li')).map((li) => li.textContent?.trim() ?? '');
    // inProgress first; then pending in wire order; then completed in wire order.
    expect(labels).toEqual(['three', 'two', 'five', 'one', 'four']);
  });

  it('truncates the todo list at 5 entries and reveals the rest via show-more', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 's1', status: 'pending' },
      { step: 's2', status: 'pending' },
      { step: 's3', status: 'pending' },
      { step: 's4', status: 'pending' },
      { step: 's5', status: 'pending' },
      { step: 's6', status: 'pending' },
      { step: 's7', status: 'pending' },
    ]);
    pane.toggleActivityRailTodos();

    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();

    const list = await findByTestId('activity-rail-todos-list');
    expect(list.querySelectorAll('li').length).toBe(5 + 1); // 5 steps + show-more row
    const showMore = await findByTestId('activity-rail-todos-show-more');
    expect(showMore.textContent?.trim()).toBe('Show 2 more…');

    await fireEvent.click(showMore);
    await tick();
    expect(list.querySelectorAll('li').length).toBe(7);
    expect(queryByTestId('activity-rail-todos-show-more')).toBeNull();
  });

  it('renders the in-progress preview alongside the Todos toggle', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'finished prep', status: 'completed' },
      { step: 'rebalance loader windows', status: 'inProgress' },
      { step: 'queued cleanup', status: 'pending' },
    ]);
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    const preview = await findByTestId('activity-rail-todos-preview');
    expect(preview.textContent?.trim()).toBe('rebalance loader windows');
  });

  it('auto-hides the Todos segment after every step completes', async () => {
    vi.useFakeTimers();
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'a', status: 'completed' },
      { step: 'b', status: 'completed' },
    ]);

    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail-todos-toggle')).toBeInTheDocument();

    vi.advanceTimersByTime(LIVE_TODO_AUTOHIDE_MS - 1);
    await tick();
    expect(queryByTestId('activity-rail-todos-toggle')).not.toBeNull();

    vi.advanceTimersByTime(2);
    await tick();
    expect(queryByTestId('activity-rail-todos-toggle')).toBeNull();
  });

  it('per-thread Background expansion state survives a thread switch', async () => {
    const launchA = backgroundLaunch({ id: 'a', threadId: 'thread-A' });
    const threadA = makeThread({ id: 'thread-A' });
    const threadB = makeThread({ id: 'thread-B' });
    setBindingMock('SwitchThread', async (id: unknown) => (id === 'thread-B' ? threadB : threadA));
    setBindingMock('ListLiveBackgroundTasks', async (id: unknown) => (id === 'thread-A' ? [launchA] : []));

    const pane = await buildPane(threadA);
    pane.upsertItem(launchA);

    const { findByTestId, queryByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await tick();

    await fireEvent.click(await findByTestId('activity-rail-background-toggle'));
    await tick();
    expect(queryByTestId('activity-rail-background-body')).not.toBeNull();

    // Switch to thread B (no background tasks). Whole rail collapses.
    await pane.switchThread(threadB);
    await tick();
    await tick();
    expect(queryByTestId('activity-rail')).toBeNull();

    // Back to A — rail re-renders and Background body remembered as open.
    await pane.switchThread(threadA);
    pane.upsertItem(launchA);
    await tick();
    await tick();
    expect(queryByTestId('activity-rail-background-body')).not.toBeNull();
  });

  it('renders a per-row Stop button and dispatches StopClaudeTask with the resolved task_id', async () => {
    const launch = backgroundLaunch({
      id: 'launch-with-id',
      meta: JSON.stringify({ task_id: 'tsk-99' }),
    });
    setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
    const calls: unknown[][] = [];
    setBindingMock('StopClaudeTask', async (...args: unknown[]) => {
      calls.push(args);
    });

    const pane = await buildPane();
    pane.upsertItem(launch);
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-background-toggle'));
    await tick();
    await fireEvent.click(await findByTestId('background-task-tray-row-stop'));
    await tick();

    expect(calls).toEqual([[pane.thread!.id, 'tsk-99']]);
  });

  it('Stop-all on a Claude thread fans out StopClaudeTask per task_id', async () => {
    const a = backgroundLaunch({ id: 'a', meta: JSON.stringify({ task_id: 'tsk-A' }) });
    const b = backgroundLaunch({ id: 'b', meta: JSON.stringify({ task_id: 'tsk-B' }) });
    setBindingMock('ListLiveBackgroundTasks', async () => [a, b]);
    const calls: unknown[][] = [];
    setBindingMock('StopClaudeTask', async (...args: unknown[]) => {
      calls.push(args);
    });

    const pane = await buildPane();
    pane.upsertItem(a);
    pane.upsertItem(b);
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-background-toggle'));
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-background-stop-all'));
    await tick();

    expect(calls.length).toBe(2);
    const ids = calls.map((c) => c[1]).sort();
    expect(ids).toEqual(['tsk-A', 'tsk-B']);
    for (const c of calls) expect(c[0]).toBe(pane.thread!.id);
  });

  it('Stop-all on a Codex thread calls CleanCodexBackgroundTerminals once and never StopClaudeTask', async () => {
    const exec = backgroundLaunch({
      id: 'exec',
      summary: 'exec_command',
      toolName: 'exec_command',
      payloadKind: 'command_output',
      meta: JSON.stringify({ source: 'unifiedExecStartup' }),
    });
    setBindingMock('ListLiveBackgroundTasks', async () => [exec]);
    let claudeCalls = 0;
    let codexCalls = 0;
    setBindingMock('StopClaudeTask', async () => { claudeCalls++; });
    setBindingMock('CleanCodexBackgroundTerminals', async () => { codexCalls++; });

    const pane = await buildPane(makeThread({ provider: 'codex' }));
    pane.upsertItem(exec);
    const { findByTestId } = render(ActivityRail, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-background-toggle'));
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-background-stop-all'));
    await tick();

    expect(codexCalls).toBe(1);
    expect(claudeCalls).toBe(0);
  });

  it('upserts that are neither background nor a completion do not re-fetch the tray', async () => {
    let fetches = 0;
    const launch = backgroundLaunch();
    setBindingMock('ListLiveBackgroundTasks', async () => {
      fetches++;
      return [launch];
    });
    const pane = await buildPane();
    pane.upsertItem(launch);
    render(ActivityRail, { props: { pane } });
    await tick();
    await tick();
    const baseline = fetches;

    // A non-background, non-completion upsert (regular assistant text).
    pane.upsertItem(
      makeItem({ id: 'plain', kind: 'assistant_text', role: 'assistant', summary: 'hi' }),
    );
    // Nudge the debounce window with a microtask + tick; the listener
    // ignores this upsert before the debounce starts.
    await Promise.resolve();
    await tick();
    expect(fetches).toBe(baseline);
  });
});
