import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
// The rail's visibility predicate + background controller live in its
// host (Composer, via createActivityRailHost); the fixture wires the
// rail exactly like Composer does so these tests exercise the
// production predicate.
import ActivityRailHost from '../../../test/mocks/ActivityRailTestHost.svelte';
import type { UserInputRequest } from '../../types/events';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { getSettings, resetSettingsForTest } from '../../stores/settings.svelte';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import { resetForTest as resetThreadStatuses } from '../../stores/threadStatuses.svelte';
import { applyCompactingState, resetForTest as resetCompactingState } from '../../stores/compactingState.svelte';
import { BUILTIN_SPINNER_VERBS } from '../../spinners/builtinVerbs';
import { BUILTIN_SPRITES } from '../../spinners/catalog';
import { __resetCustomSpinnersForTest } from '../../stores/spinners.svelte';
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
    for (const toast of [...getToasts()]) removeToast(toast.id);
    setBindingMock('ListLiveBackgroundTasks', async () => []);
    setBindingMock('StopClaudeTask', async () => {});
    setBindingMock('CleanCodexBackgroundTerminals', async () => {});
    setBindingMock('TerminateCodexBackgroundTerminal', async () => true);
  });

  afterEach(() => {
    vi.useRealTimers();
    resetSettingsForTest();
  });

  it('renders nothing when idle, no todos, no background', async () => {
    const pane = await buildPane();
    const { queryByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect(queryByTestId('activity-rail')).toBeNull();
  });

  it('shows the working segment with elapsed timer when a turn is active', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working-elapsed')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-working-bridge')).toBeNull();
    // Hairline + LED chase mount whenever a turn is active.
    expect(await findByTestId('activity-rail-hairline')).toBeInTheDocument();
    const leds = await findByTestId('activity-rail-working-leds');
    expect(leds.classList.contains('working-leds')).toBe(true);
    expect(leds.querySelectorAll('.working-led')).toHaveLength(3);
  });

  it('mounts the hairline and LEDs in the bridge state too (queue item, no active turn)', async () => {
    const pane = await buildPane();
    enqueueSimple(pane.threadId!, 'queued bridge');
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail-hairline')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working-leds')).toBeInTheDocument();
  });

  it('renders static LEDs in low power mode (hairline still shows — it never animates)', async () => {
    // The LED chase is the working segment's only standing animation;
    // low-power mode drops the chase class so the LEDs rest at base
    // opacity. The hairline is static CSS, so unlike the old shimmer
    // it is NOT suppressed by low-power mode.
    getSettings().lowPowerMode = true;
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail-working')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-hairline')).toBeInTheDocument();
    const leds = await findByTestId('activity-rail-working-leds');
    expect(leds.classList.contains('working-leds')).toBe(false);
    expect(leds.querySelectorAll('.working-led')).toHaveLength(3);
  });

  it('does not mount the hairline when only todos are visible (rail visible, isWorking false)', async () => {
    // Guards against a regression that hangs the working indicator off
    // `railVisible` instead of the stricter `isWorking` predicate.
    const pane = await buildPane();
    pane.setLiveTodo([{ step: 'one', status: 'inProgress' }]);
    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-hairline')).toBeNull();
    expect(queryByTestId('activity-rail-working-leds')).toBeNull();
  });

  it('shows the Todos toggle and counts when a liveTodo snapshot is present', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'Refactor send pipeline', status: 'inProgress' },
      { step: 'Migrate dispatcher tests', status: 'inProgress' },
      { step: 'Update wire contract docs', status: 'pending' },
      { step: 'Read flush_queue.go for context', status: 'completed' },
    ]);
    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    const toggle = await findByTestId('activity-rail-todos-toggle');
    expect(toggle).toBeInTheDocument();
    // Pill is `completed/total`, so 1 completed of 4 total → "1/4".
    expect((await findByTestId('activity-rail-todos-count')).textContent?.trim()).toBe('1/4');
    // Body collapsed by default.
    expect(queryByTestId('activity-rail-todos-body')).toBeNull();

    await fireEvent.click(toggle);
    await tick();
    expect(await findByTestId('activity-rail-todos-body')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-todos-list')).toBeInTheDocument();
  });

  it('shows N/N in the count pill when every step is completed', async () => {
    // Regression: the numerator counts completed steps, so "all done"
    // reads N/N — not 0/N, which was the symptom when the pill was
    // wired to in-progress count.
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'one', status: 'completed' },
      { step: 'two', status: 'completed' },
      { step: 'three', status: 'completed' },
    ]);
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect((await findByTestId('activity-rail-todos-count')).textContent?.trim()).toBe('3/3');
  });

  it('renders the stepped spinner (not animate-spin) on the in-progress row only', async () => {
    // The in-progress indicator is a standing animation — it lives for
    // the whole todo, not a transient loading flash — so it must be the
    // spoked steps(12) spinner, never a continuous animate-spin (see
    // primitives/SteppedSpinner.svelte and the app.css --animate-pulse
    // note for the frame-production incident behind this).
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'active', status: 'inProgress' },
      { step: 'queued', status: 'pending' },
      { step: 'done', status: 'completed' },
    ]);
    const { container, findByTestId, queryAllByTestId } = render(ActivityRailHost, {
      props: { pane },
    });
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-todos-toggle'));
    await tick();

    const spinners = queryAllByTestId('stepped-spinner');
    expect(spinners).toHaveLength(1);
    expect(spinners[0].classList.contains('stepped-spin')).toBe(true);
    expect(container.querySelector('.animate-spin')).toBeNull();
  });

  it('renders the in-progress spinner static in low power mode', async () => {
    // Same contract as the working-LED chase: low-power mode drops
    // standing animations. The glyph stays (the row still reads as
    // active); only the rotation stops.
    getSettings().lowPowerMode = true;
    const pane = await buildPane();
    pane.setLiveTodo([{ step: 'active', status: 'inProgress' }]);
    const { findByTestId, queryAllByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-todos-toggle'));
    await tick();

    const spinners = queryAllByTestId('stepped-spinner');
    expect(spinners).toHaveLength(1);
    expect(spinners[0].classList.contains('stepped-spin')).toBe(false);
  });

  it('renders the owner badge only on Task* rows that were claimed', async () => {
    // The Claude Code 2.1.150+ Task* family threads an `owner` field
    // through the snapshot. The visual is added-only: empty owner
    // matches the pre-Task* TodoWrite rendering (no badge);
    // non-empty owner surfaces as a pill so multi-agent / teammate
    // ownership is visible at a glance.
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'Solo task', status: 'inProgress' },
      { step: 'Claimed task', status: 'pending', owner: 'helper-agent' },
    ]);
    const { findByTestId, findAllByTestId } = render(ActivityRailHost, {
      props: { pane },
    });
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-todos-toggle'));
    await tick();

    const badges = await findAllByTestId('activity-rail-todos-owner');
    expect(badges).toHaveLength(1);
    expect(badges[0].textContent?.trim()).toBe('(helper-agent)');
  });

  it('renders no owner badge when every step has an empty/missing owner', async () => {
    // Parity with the legacy TodoWrite + Codex update_plan rendering:
    // an entry without an owner must not produce a chip, layout
    // shift, or empty wrapper. This is the load-bearing "match the
    // previous behavior" guarantee.
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'first', status: 'inProgress' },
      { step: 'second', status: 'pending', owner: '' },
    ]);
    const { findByTestId, queryAllByTestId } = render(ActivityRailHost, {
      props: { pane },
    });
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-todos-toggle'));
    await tick();

    expect(queryAllByTestId('activity-rail-todos-owner')).toHaveLength(0);
  });

  it('shows the Background toggle and expanded body with rows when tasks are running', async () => {
    const launch = backgroundLaunch();
    setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
    const pane = await buildPane();
    pane.upsertItem(launch);

    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
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

    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
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
    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-working')).toBeNull();
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

    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
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

  it('keeps one width-reserved label when the pending-send bridge becomes an active turn', async () => {
    const pane = await buildPane();
    enqueueSimple(pane.threadId!, 'queued follow-up');

    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    expect(await findByTestId('activity-rail')).toBeInTheDocument();
    expect(await findByTestId('activity-rail-working')).toBeInTheDocument();
    const label = await findByTestId('activity-rail-working-label');
    const elapsed = await findByTestId('activity-rail-working-elapsed');
    expect(elapsed).toHaveClass('invisible');
    expect(elapsed).toHaveAttribute('aria-hidden', 'true');

    pane.setActiveTurn({ turnId: 'turn-1', turnIndex: 0, startedAt: Date.now() });
    await tick();

    expect(await findByTestId('activity-rail-working-label')).toBe(label);
    expect(await findByTestId('activity-rail-working-elapsed')).toBe(elapsed);
    expect(elapsed).not.toHaveClass('invisible');
    expect(elapsed).toHaveAttribute('aria-hidden', 'false');
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

    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();

    const list = await findByTestId('activity-rail-todos-list');
    const labels = Array.from(list.querySelectorAll('li')).map((li) => li.textContent?.trim() ?? '');
    // inProgress first; then pending in wire order; then completed in wire order.
    expect(labels).toEqual(['three', 'two', 'five', 'one', 'four']);
  });

  it('truncates the todo list at 5 entries and toggles the rest via show-more/show-less', async () => {
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

    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();

    const list = await findByTestId('activity-rail-todos-list');
    expect(list.querySelectorAll('li').length).toBe(5 + 1); // 5 steps + show-more row
    const showMore = await findByTestId('activity-rail-todos-show-more');
    expect(showMore.textContent?.trim()).toBe('Show 2 more…');

    await fireEvent.click(showMore);
    await tick();
    expect(list.querySelectorAll('li').length).toBe(7 + 1); // 7 steps + show-less row
    expect(queryByTestId('activity-rail-todos-show-more')).toBeNull();
    const showLess = await findByTestId('activity-rail-todos-show-less');
    expect(showLess.textContent?.trim()).toBe('Show less');

    await fireEvent.click(showLess);
    await tick();
    expect(list.querySelectorAll('li').length).toBe(5 + 1); // 5 steps + show-more row
    expect(queryByTestId('activity-rail-todos-show-less')).toBeNull();
    expect((await findByTestId('activity-rail-todos-show-more')).textContent?.trim()).toBe('Show 2 more…');
  });

  it('renders the in-progress preview alongside the Todos toggle', async () => {
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'finished prep', status: 'completed' },
      { step: 'rebalance loader windows', status: 'inProgress' },
      { step: 'queued cleanup', status: 'pending' },
    ]);
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    const preview = await findByTestId('activity-rail-todos-preview');
    expect(preview.textContent?.trim()).toBe('rebalance loader windows');
  });

  it('keeps the row single-line: fixed segments are shrink-0 and the preview ellipsizes via CSS', async () => {
    // jsdom has no layout engine, so assert the CSS contract directly.
    // The composer reserves exactly one row of height for this rail
    // (composer-activity-reserve in Composer.svelte), so the row must
    // never wrap; narrow panes squeeze the todos preview instead. The
    // preview must also not hide behind a viewport breakpoint (sm:) —
    // pane width and viewport width are independent in split layouts.
    const launch = backgroundLaunch();
    setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
    pane.setLiveTodo([
      { step: 'a very long in-progress step that would otherwise wrap the rail', status: 'inProgress' },
    ]);
    pane.upsertItem(launch);

    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    await tick();

    const row = (await findByTestId('activity-rail')).querySelector(':scope > div');
    expect(row).not.toBeNull();
    expect(row!.classList.contains('flex')).toBe(true);
    expect(row!.classList.contains('flex-wrap')).toBe(false);

    expect((await findByTestId('activity-rail-working')).classList.contains('shrink-0')).toBe(true);
    expect((await findByTestId('activity-rail-background-toggle')).classList.contains('shrink-0')).toBe(true);

    // The todos toggle is the one segment allowed to shrink; its preview
    // gives up width first via text-overflow ellipsis.
    const todosToggle = await findByTestId('activity-rail-todos-toggle');
    expect(todosToggle.classList.contains('shrink-0')).toBe(false);
    expect(todosToggle.classList.contains('min-w-0')).toBe(true);
    expect(todosToggle.classList.contains('overflow-hidden')).toBe(true);
    const preview = await findByTestId('activity-rail-todos-preview');
    expect(preview.classList.contains('truncate')).toBe(true);
    expect(preview.classList.contains('hidden')).toBe(false);
  });

  it('input-requested chip stays shrink-0 alongside todos', async () => {
    // Working and input-requested are mutually exclusive, so the narrow-pane
    // squeeze with a pending input is chip + todos: the chip must hold its
    // width while the todos toggle absorbs the deficit.
    const pane = await buildPane();
    pane.setLiveTodo([{ step: 'in flight', status: 'inProgress' }]);
    const { findByTestId } = render(ActivityRailHost, {
      props: { pane, inputRequest: userInputRequest() },
    });
    await tick();
    const chip = await findByTestId('activity-rail-input-toggle');
    expect(chip.classList.contains('shrink-0')).toBe(true);
    expect((await findByTestId('activity-rail-todos-toggle')).classList.contains('shrink-0')).toBe(false);
  });

  it('auto-hides the Todos segment after every step completes', async () => {
    vi.useFakeTimers();
    const pane = await buildPane();
    pane.setLiveTodo([
      { step: 'a', status: 'completed' },
      { step: 'b', status: 'completed' },
    ]);

    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
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

    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
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
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
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
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
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
    const { findByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-background-toggle'));
    await tick();
    await fireEvent.click(await findByTestId('activity-rail-background-stop-all'));
    await tick();

    expect(codexCalls).toBe(1);
    expect(claudeCalls).toBe(0);
  });

  // --- Codex per-row stop (thread/backgroundTerminals/terminate) ---
  //
  // Parity with the Claude per-row stop above: the same tray button, on
  // the same rows, over a different primitive. The id namespace differs
  // (PTY process id, not task id) and so does the binding, so these
  // tests pin that the process_id reaches the right RPC and that no
  // Claude call is made on a Codex thread.

  function codexBackgroundLaunch(overrides = {}) {
    return backgroundLaunch({
      id: 'codex-exec',
      summary: 'exec_command',
      toolName: 'exec_command',
      payloadKind: 'command_output',
      meta: JSON.stringify({ source: 'unifiedExecStartup', process_id: '1734029' }),
      ...overrides,
    });
  }

  async function openCodexBackgroundTray(launch: ReturnType<typeof codexBackgroundLaunch>) {
    setBindingMock('ListLiveBackgroundTasks', async () => [launch]);
    const pane = await buildPane(makeThread({ provider: 'codex' }));
    pane.upsertItem(launch);
    const view = render(ActivityRailHost, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(await view.findByTestId('activity-rail-background-toggle'));
    await tick();
    return { pane, ...view };
  }

  it('renders a per-row Stop on a Codex background terminal and dispatches TerminateCodexBackgroundTerminal with the process_id', async () => {
    const calls: unknown[][] = [];
    setBindingMock('TerminateCodexBackgroundTerminal', async (...args: unknown[]) => {
      calls.push(args);
      return true;
    });
    let claudeCalls = 0;
    setBindingMock('StopClaudeTask', async () => { claudeCalls++; });

    const { pane, findByTestId } = await openCodexBackgroundTray(codexBackgroundLaunch());
    await fireEvent.click(await findByTestId('background-task-tray-row-stop'));
    await tick();

    expect(calls).toEqual([[pane.thread!.id, '1734029']]);
    expect(claudeCalls).toBe(0);
  });

  it('renders no per-row Stop for a Codex background row the wire has not named a process for', async () => {
    // Stop-all still applies (it is thread-wide and needs no id), but a
    // per-row button would have nothing to terminate.
    let terminateCalls = 0;
    setBindingMock('TerminateCodexBackgroundTerminal', async () => { terminateCalls++; return true; });

    const { findByTestId, queryByTestId } = await openCodexBackgroundTray(
      codexBackgroundLaunch({ meta: JSON.stringify({ source: 'unifiedExecStartup' }) }),
    );

    expect(await findByTestId('background-task-tray-row')).toBeInTheDocument();
    expect(queryByTestId('background-task-tray-row-stop')).toBeNull();
    expect(await findByTestId('activity-rail-background-stop-all')).toBeInTheDocument();
    expect(terminateCalls).toBe(0);
  });

  it('surfaces terminated:false as state instead of swallowing it', async () => {
    // `terminated:false` means the RPC matched no running process, so no
    // item/completed follows and the row will not change on its own. A
    // silent no-op would read as a broken button.
    setBindingMock('TerminateCodexBackgroundTerminal', async () => false);

    const { findByTestId } = await openCodexBackgroundTray(codexBackgroundLaunch());
    const before = getToasts().length;
    await fireEvent.click(await findByTestId('background-task-tray-row-stop'));
    await tick();

    const added = getToasts().slice(before);
    expect(added).toHaveLength(1);
    expect(added[0].type).toBe('info');
    expect(added[0].message).toMatch(/already exited/i);
  });

  it('stays quiet when the terminate actually killed something', async () => {
    setBindingMock('TerminateCodexBackgroundTerminal', async () => true);

    const { findByTestId } = await openCodexBackgroundTray(codexBackgroundLaunch());
    const before = getToasts().length;
    await fireEvent.click(await findByTestId('background-task-tray-row-stop'));
    await tick();

    expect(getToasts().slice(before)).toHaveLength(0);
  });

  it('surfaces a failed Codex terminate as an error toast carrying the backend message', async () => {
    setBindingMock('TerminateCodexBackgroundTerminal', async () => {
      throw new Error('codex: thread/backgroundTerminals/terminate 1734029: thread not found');
    });

    const { findByTestId } = await openCodexBackgroundTray(codexBackgroundLaunch());
    const before = getToasts().length;
    await fireEvent.click(await findByTestId('background-task-tray-row-stop'));
    await tick();

    const added = getToasts().slice(before);
    expect(added).toHaveLength(1);
    expect(added[0].type).toBe('error');
    expect(added[0].message).toContain('thread not found');
  });

  it('shows active Codex subagents in Background without stop controls', async () => {
    const spawn = backgroundLaunch({
      id: 'spawn-agent',
      summary: 'spawn_agent: worker',
      toolName: 'collab_agent',
      payloadKind: undefined,
      payloadId: undefined,
      payloadMeta: '',
      meta: JSON.stringify({
        input: {
          tool: 'spawn_agent',
          receiverThreadIds: ['child-1'],
          newAgentNickname: 'worker',
        },
      }),
    });
    setBindingMock('ListLiveBackgroundTasks', async () => [spawn]);
    let codexCalls = 0;
    setBindingMock('CleanCodexBackgroundTerminals', async () => { codexCalls++; });

    const pane = await buildPane(makeThread({ provider: 'codex' }));
    const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
    await tick();
    await tick();

    expect(await findByTestId('activity-rail-background-toggle')).toBeInTheDocument();
    expect((await findByTestId('activity-rail-background-count')).textContent?.trim()).toBe('1');

    await fireEvent.click(await findByTestId('activity-rail-background-toggle'));
    await tick();

    expect(await findByTestId('background-task-tray-row')).toBeInTheDocument();
    expect(queryByTestId('activity-rail-background-stop-all')).toBeNull();
    expect(queryByTestId('background-task-tray-row-stop')).toBeNull();
    expect(codexCalls).toBe(0);
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
    render(ActivityRailHost, { props: { pane } });
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

  function userInputRequest(): UserInputRequest {
    return {
      requestId: 'req-input-1',
      threadId: 'thread-1',
      toolName: 'request_user_input',
      title: 'Input',
      questions: [{ id: 'q', header: 'Q', question: 'Pick', options: [{ label: 'A', description: '' }] }],
    };
  }

  it('renders the input-requested chip and toggles via onToggleInput', async () => {
    const pane = await buildPane();
    const onToggleInput = vi.fn();
    const { getByTestId, rerender } = render(ActivityRailHost, {
      props: { pane, inputRequest: userInputRequest(), inputCollapsed: false, onToggleInput },
    });
    await tick();

    const chip = getByTestId('activity-rail-input-toggle');
    expect(chip.getAttribute('aria-expanded')).toBe('true');

    await fireEvent.click(chip);
    expect(onToggleInput).toHaveBeenCalledTimes(1);

    await rerender({ pane, inputRequest: userInputRequest(), inputCollapsed: true, onToggleInput });
    expect(getByTestId('activity-rail-input-toggle').getAttribute('aria-expanded')).toBe('false');
  });

  it('hides the working segment while an input request is pending', async () => {
    const pane = await buildPane();
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
    const { getByTestId, queryByTestId } = render(ActivityRailHost, {
      props: { pane, inputRequest: userInputRequest() },
    });
    await tick();

    expect(getByTestId('activity-rail-input-toggle')).toBeTruthy();
    expect(queryByTestId('activity-rail-working')).toBeNull();
    expect(queryByTestId('activity-rail-hairline')).toBeNull();
  });

  describe('spinner verbs + sprites', () => {
    it('shows one built-in verb per turn by default (verbs default on)', async () => {
      const pane = await buildPane();
      pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
      const { findByTestId } = render(ActivityRailHost, { props: { pane } });
      await tick();
      const working = await findByTestId('activity-rail-working');
      const label = working.textContent ?? '';
      expect(label).not.toContain('Working');
      expect(BUILTIN_SPINNER_VERBS.some((verb) => label.includes(verb))).toBe(true);
    });

    it('falls back to the plain Working label when verbs are off', async () => {
      getSettings().spinnerVerbsEnabled = false;
      const pane = await buildPane();
      pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
      const { findByTestId } = render(ActivityRailHost, { props: { pane } });
      await tick();
      expect((await findByTestId('activity-rail-working')).textContent).toContain('Working');
    });

    it('draws only custom verbs when built-ins are disabled', async () => {
      getSettings().spinnerCustomVerbs = ['Vibing'];
      getSettings().spinnerBuiltinVerbsDisabled = true;
      const pane = await buildPane();
      pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
      const { findByTestId } = render(ActivityRailHost, { props: { pane } });
      await tick();
      expect((await findByTestId('activity-rail-working')).textContent).toContain('Vibing');
    });

    it('the compacting label beats the verb', async () => {
      const pane = await buildPane();
      pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
      applyCompactingState({ threadId: pane.threadId!, active: true, sinceUnixMs: Date.now() });
      const { findByTestId } = render(ActivityRailHost, { props: { pane } });
      await tick();
      expect((await findByTestId('activity-rail-working')).textContent).toContain('Compacting');
      resetCompactingState();
    });

    it('replaces the LED cluster with a sprite when animations are on', async () => {
      setBindingMock('GetSpinnerFiles', async () => ({ dir: '/cfg', sprites: [], warnings: [] }));
      getSettings().spinnerAnimationsEnabled = true;
      const pane = await buildPane();
      pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
      const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
      await tick();
      const slot = await findByTestId('activity-rail-sprite');
      expect(slot.querySelector('.working-sprite')).not.toBeNull();
      expect(queryByTestId('activity-rail-working-leds')).toBeNull();
      __resetCustomSpinnersForTest();
    });

    it('keeps the LED cluster when animations are on but every sprite is unchecked', async () => {
      setBindingMock('GetSpinnerFiles', async () => ({ dir: '/cfg', sprites: [], warnings: [] }));
      getSettings().spinnerAnimationsEnabled = true;
      getSettings().spinnerDisabledAnimations = BUILTIN_SPRITES.map((sprite) => sprite.id);
      getSettings().spinnerCompactionAnimation = 'none';
      const pane = await buildPane();
      pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: Date.now() - 3_000 });
      const { findByTestId, queryByTestId } = render(ActivityRailHost, { props: { pane } });
      await tick();
      expect(await findByTestId('activity-rail-working-leds')).toBeInTheDocument();
      expect(queryByTestId('activity-rail-sprite')).toBeNull();
      __resetCustomSpinnersForTest();
    });
  });
});
