import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import BackgroundTaskTray from './BackgroundTaskTray.svelte';
import type { Item, Thread } from '../../types/models';
import { buildPane, emitItemEventUpsert, makeThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import { setupEventListeners } from '../../stores/events';
import { getToasts } from '../../stores/toast.svelte';

/**
 * Test fixtures mirror the real `Item` shape produced by the backend:
 * launches land as `tool_call`, completions as `tool_completion` with
 * `completionOf` pointing at the launch id. `status` uses the real
 * vocabulary from the store (`streaming | running | completed |
 * errored | declined | killed`).
 */
function item(overrides: Partial<Item> & { id: string }): Item {
  return {
    threadId: 'thread-1',
    turnIndex: 0,
    itemIndex: 0,
    kind: 'tool_call',
    role: 'assistant',
    status: 'running',
    summary: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

async function makePaneWithBackground(items: Item[], thread?: Thread) {
  const pane = await buildPane(thread);
  setBindingMock('ListLiveBackgroundTasks', async () => items);
  return pane;
}

/**
 * Render the tray and flush the ListLiveBackgroundTasks fetch + Svelte
 * reactions until the derived task list has settled. Two ticks covers
 * the async binding result + the $derived pass that reads it.
 *
 * The tray defaults to collapsed in production, so tests that need to
 * inspect or interact with row elements should pass `{ expand: true }`
 * to click the header after mount.
 */
async function renderTray(
  pane: Awaited<ReturnType<typeof buildPane>>,
  options: { expand?: boolean } = {},
) {
  const result = render(BackgroundTaskTray, { props: { pane } });
  await tick();
  await tick();
  if (options.expand) {
    const header = result.queryByTestId('background-task-tray-header');
    if (!header) {
      throw new Error(
        'renderTray({ expand: true }) was called but the tray header is not rendered (the tray hides itself when no background tasks are present).',
      );
    }
    await fireEvent.click(header);
    await tick();
  }
  return result;
}

describe('<BackgroundTaskTray>', () => {
  let cleanupEvents: () => void;

  beforeEach(async () => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(1_000_000));
    resetWailsMocks();
    resetBindingMocks();
    cleanupEvents = setupEventListeners();
    // Default the two stop primitives to resolved no-ops; tests that
    // care about specific call shapes or error surfacing override
    // these.
    setBindingMock('StopClaudeTask', async () => {});
    setBindingMock('CleanCodexBackgroundTerminals', async () => {});
  });

  afterEach(() => {
    cleanup();
    cleanupEvents?.();
    vi.useRealTimers();
  });

  it('renders nothing when the backend returns no live background tasks', async () => {
    const pane = await makePaneWithBackground([]);
    const { container, queryByTestId } = await renderTray(pane);

    expect(queryByTestId('background-task-tray')).toBeNull();
    expect(container.textContent?.trim() ?? '').toBe('');
  });

  it('renders the header with a count when there is at least one running task', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 5_000,
      }),
      item({
        id: 'launch-b',
        isBackground: true,
        status: 'running',
        summary: 'Grep',
        createdAt: 1_000_000 - 2_000,
      }),
    ]);

    const { getByTestId } = await renderTray(pane);
    expect(getByTestId('background-task-tray')).toBeInTheDocument();
    expect(getByTestId('background-task-tray-header').textContent).toContain('Background');
    expect(getByTestId('background-task-tray-count').textContent).toBe('2');
  });

  it('defaults collapsed when tasks first appear', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 5_000,
      }),
    ]);
    const rendered = await renderTray(pane);

    expect(rendered.getByTestId('background-task-tray-header').getAttribute('aria-expanded')).toBe('false');
    expect(rendered.queryByTestId('background-task-tray-body')).toBeNull();
  });

  it('defaults collapsed even when many tasks appear at once', async () => {
    const tasks = Array.from({ length: 5 }, (_, index) => item({
      id: `launch-${index}`,
      isBackground: true,
      status: 'running',
      summary: `Task ${index}`,
      createdAt: 1_000_000 - index * 1_000,
    }));
    const pane = await makePaneWithBackground(tasks);
    const rendered = await renderTray(pane);

    expect(rendered.getByTestId('background-task-tray-header').getAttribute('aria-expanded')).toBe('false');
    expect(rendered.queryByTestId('background-task-tray-body')).toBeNull();
  });

  it('clicking the header toggles the body open and closed', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 5_000,
      }),
    ]);
    const rendered = await renderTray(pane);
    const header = rendered.getByTestId('background-task-tray-header');

    await fireEvent.click(header);
    await tick();
    expect(header.getAttribute('aria-expanded')).toBe('true');
    expect(rendered.getByTestId('background-task-tray-body')).toBeInTheDocument();

    await fireEvent.click(header);
    await tick();
    expect(header.getAttribute('aria-expanded')).toBe('false');
    expect(rendered.queryByTestId('background-task-tray-body')).toBeNull();
  });

  it('re-collapses when tasks drain and a fresh task arrives, even after the user expanded', async () => {
    // The tray's component instance survives count transitions (the
    // inner `{#if count > 0}` only hides DOM, not the component). When
    // a new burst of background work arrives after the previous burst
    // drained, the user's prior expansion must not carry over —
    // matches "default collapsed" framing on every refill.
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 5_000,
        updatedAt: 1_000_000 - 5_000,
      }),
      item({
        id: 'done-a',
        kind: 'tool_completion',
        isBackground: true,
        status: 'completed',
        completionOf: 'launch-a',
        summary: 'Bash -> done',
        createdAt: 1_000_000,
        updatedAt: 1_000_000,
      }),
    ]);
    const rendered = await renderTray(pane, { expand: true });
    expect(rendered.getByTestId('background-task-tray-header').getAttribute('aria-expanded')).toBe('true');

    // Drain: advance past retention window so the tray hides.
    await vi.advanceTimersByTimeAsync(2_500);
    expect(rendered.queryByTestId('background-task-tray')).toBeNull();

    // Refill: a new background launch lands.
    setBindingMock('ListLiveBackgroundTasks', async () => [
      item({
        id: 'launch-b',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: Date.now(),
        updatedAt: Date.now(),
      }),
    ]);
    emitItemEventUpsert({
      id: 'launch-b',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      summary: 'Bash',
      isBackground: true,
      createdAt: Date.now(),
      updatedAt: Date.now(),
    });
    // Debounced refresh + reactive flush.
    await vi.advanceTimersByTimeAsync(150);
    await tick();
    await tick();

    expect(rendered.getByTestId('background-task-tray-header').getAttribute('aria-expanded')).toBe('false');
    expect(rendered.queryByTestId('background-task-tray-body')).toBeNull();
  });

  it('stays collapsed across thread switches', async () => {
    // Both threads' trays default to collapsed; the test pins that
    // switching threads does not transiently render the new thread's
    // tray expanded.
    const pane = await buildPane(makeThread({ id: 'thread-1' }));
    setBindingMock('ListLiveBackgroundTasks', async (threadId: string) => {
      if (threadId === 'thread-1') {
        return [
          item({
            id: 'one',
            threadId: 'thread-1',
            isBackground: true,
            status: 'running',
            summary: 'One',
            createdAt: 1_000_000 - 1_000,
          }),
        ];
      }
      return Array.from({ length: 5 }, (_, index) => item({
        id: `many-${index}`,
        threadId: 'thread-2',
        isBackground: true,
        status: 'running',
        summary: `Many ${index}`,
        createdAt: 1_000_000 - index * 1_000,
      }));
    });

    const rendered = await renderTray(pane);
    expect(rendered.getByTestId('background-task-tray-header').getAttribute('aria-expanded')).toBe('false');

    await pane.switchThread(makeThread({ id: 'thread-2' }));
    await tick();
    await tick();
    await tick();

    expect(rendered.getByTestId('background-task-tray-count').textContent).toBe('5');
    expect(rendered.getByTestId('background-task-tray-header').getAttribute('aria-expanded')).toBe('false');
    expect(rendered.queryByTestId('background-task-tray-body')).toBeNull();
  });

  it('shows the pulsing dot when at least one task is running and hides it for a standalone completion', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 1_000,
      }),
    ]);
    const { getByTestId } = await renderTray(pane);
    expect(getByTestId('background-task-tray-pulse')).toBeInTheDocument();

    // Swap the binding to return only a lone completion; unmount and
    // remount a fresh tray so the mount-time fetch picks up the new
    // backing set.
    cleanup();
    setBindingMock('ListLiveBackgroundTasks', async () => [
      item({
        id: 'done-a',
        kind: 'tool_completion',
        isBackground: true,
        status: 'completed',
        completionOf: 'launch-a',
        summary: 'Bash',
        createdAt: 1_000_000 - 500,
      }),
    ]);
    const refreshed = await renderTray(pane);
    expect(refreshed.queryByTestId('background-task-tray-pulse')).toBeNull();
    // The completion is still within the 2 s retention window, so the
    // tray keeps rendering a row.
    expect(refreshed.getByTestId('background-task-tray-count').textContent).toBe('1');
  });

  it('prunes a standalone completion 2 seconds after its createdAt', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'done-a',
        kind: 'tool_completion',
        isBackground: true,
        status: 'completed',
        completionOf: 'ghost-launch',
        summary: 'Bash',
        createdAt: 1_000_000,
      }),
    ]);
    const { getByTestId, queryByTestId } = await renderTray(pane);
    expect(getByTestId('background-task-tray')).toBeInTheDocument();
    expect(getByTestId('background-task-tray-count').textContent).toBe('1');

    // Advance past the 2 s retention window — the 1 s interval fires
    // twice, flushing the derivation twice with new `now` values.
    await vi.advanceTimersByTimeAsync(2_500);

    expect(queryByTestId('background-task-tray')).toBeNull();
  });

  it('removes a launch+completion pair 2 seconds after the completion lands', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 10_000,
        updatedAt: 1_000_000 - 10_000,
      }),
      item({
        id: 'done-a',
        kind: 'tool_completion',
        isBackground: true,
        status: 'completed',
        completionOf: 'launch-a',
        summary: 'Bash -> done',
        createdAt: 1_000_000,
        updatedAt: 1_000_000,
      }),
    ]);
    const { getByTestId, queryByTestId, getAllByTestId } = await renderTray(pane, { expand: true });
    const [row] = getAllByTestId('background-task-tray-row');
    expect(row.getAttribute('data-row-id')).toBe('launch-a');
    expect(getByTestId('background-task-tray-row-status').getAttribute('data-status')).toBe('completed');

    await vi.advanceTimersByTimeAsync(2_500);

    expect(queryByTestId('background-task-tray')).toBeNull();
  });

  it('renders a declined completion with error styling, not as a green checkmark', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 5_000,
        updatedAt: 1_000_000 - 5_000,
      }),
      item({
        id: 'done-a',
        kind: 'tool_completion',
        isBackground: true,
        status: 'declined',
        completionOf: 'launch-a',
        summary: 'Bash -> declined',
        createdAt: 1_000_000 - 100,
        updatedAt: 1_000_000 - 100,
      }),
    ]);
    const { getByTestId } = await renderTray(pane, { expand: true });
    const status = getByTestId('background-task-tray-row-status');
    expect(status.getAttribute('data-status')).toBe('declined');
    expect(status.className).toContain('text-error');
    expect(status.textContent).toContain('declined');
  });

  it('renders a killed completion with gray "Stopped" styling, distinct from failed', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 5_000,
        updatedAt: 1_000_000 - 5_000,
      }),
      item({
        id: 'done-a',
        kind: 'tool_completion',
        isBackground: true,
        status: 'killed',
        completionOf: 'launch-a',
        summary: 'Bash -> stopped',
        createdAt: 1_000_000 - 100,
        updatedAt: 1_000_000 - 100,
      }),
    ]);
    const { getByTestId } = await renderTray(pane, { expand: true });
    const status = getByTestId('background-task-tray-row-status');
    expect(status.getAttribute('data-status')).toBe('killed');
    expect(status.textContent).toContain('stopped');
    // Must NOT read as an error — red palette is reserved for actual
    // failures, and killed is a user-initiated stop.
    expect(status.className).not.toContain('text-error');
    expect(status.className).not.toContain('text-success');
    expect(status.className).toContain('text-text-secondary');
  });

  it('suppresses the elapsed label for an orphan completion (no launch)', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'done-a',
        kind: 'tool_completion',
        isBackground: true,
        status: 'completed',
        completionOf: 'ghost-launch',
        summary: 'Bash',
        createdAt: 1_000_000 - 100,
      }),
    ]);
    const { getByTestId, queryByTestId } = await renderTray(pane, { expand: true });
    expect(getByTestId('background-task-tray-row')).toBeInTheDocument();
    expect(queryByTestId('background-task-tray-row-elapsed')).toBeNull();
  });

  it('renders all rows when expanded', async () => {
    const pane = await makePaneWithBackground([
      item({ id: 'a', isBackground: true, status: 'running', summary: 'A', createdAt: 1_000_000 - 1_000 }),
      item({ id: 'b', isBackground: true, status: 'running', summary: 'B', createdAt: 1_000_000 - 2_000 }),
      item({ id: 'c', isBackground: true, status: 'running', summary: 'C', createdAt: 1_000_000 - 3_000 }),
      item({ id: 'd', isBackground: true, status: 'running', summary: 'D', createdAt: 1_000_000 - 4_000 }),
    ]);

    const { getAllByTestId, queryByTestId } = await renderTray(pane, { expand: true });
    expect(getAllByTestId('background-task-tray-row')).toHaveLength(4);
    expect(queryByTestId('background-task-tray-more')).toBeNull();
  });

  it('refreshes on upsert of a background launch (isBackground=true, completionOf empty)', async () => {
    // Regression pin for the `isBackground || completionOf` filter in
    // the provider:item_event handler. A background launch that has
    // not yet been paired with a completion has `completionOf` empty;
    // the handler must still trigger a refresh — flipping the `||`
    // to `&&` would silently stop all launches from refreshing the
    // tray.
    vi.useRealTimers();
    let fetchCalls = 0;
    const pane = await buildPane();
    setBindingMock('ListLiveBackgroundTasks', async () => {
      fetchCalls += 1;
      return [];
    });
    render(BackgroundTaskTray, { props: { pane } });
    // Flush the mount fetch.
    await tick();
    await tick();
    const mountCalls = fetchCalls;

    emitItemEventUpsert({
      id: 'fresh-launch',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      summary: 'Bash: sleep 1',
      isBackground: true,
      // completionOf deliberately empty — this is a pure launch row.
      createdAt: 0,
      updatedAt: 0,
    });
    // Debounce window is 100 ms; give it room.
    await new Promise((r) => setTimeout(r, 150));
    expect(fetchCalls).toBeGreaterThan(mountCalls);
  });

  it('ignores upserts with neither isBackground nor completionOf', async () => {
    // Inverse of the test above: a plain diff-only upsert must NOT
    // trigger a tray refresh. The filter keeps background-adjacent
    // work out of the hot path — if it misfires on every upsert the
    // debounced fetch still runs 10x/sec on a streaming turn.
    vi.useRealTimers();
    let fetchCalls = 0;
    const pane = await buildPane();
    setBindingMock('ListLiveBackgroundTasks', async () => {
      fetchCalls += 1;
      return [];
    });
    render(BackgroundTaskTray, { props: { pane } });
    await tick();
    await tick();
    const mountCalls = fetchCalls;

    emitItemEventUpsert({
      id: 'diff-only',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      summary: 'Edit: foo.ts',
      // isBackground + completionOf both falsy
      createdAt: 0,
      updatedAt: 0,
    });
    await new Promise((r) => setTimeout(r, 150));
    expect(fetchCalls).toBe(mountCalls);
  });

  it('orders rows by oldest created task first', async () => {
    const pane = await makePaneWithBackground([
      item({ id: 'older', isBackground: true, status: 'running', summary: 'Older', createdAt: 1_000_000 - 4_000, updatedAt: 1_000_000 - 4_000 }),
      item({ id: 'newer', isBackground: true, status: 'running', summary: 'Newer', createdAt: 1_000_000 - 1_000, updatedAt: 1_000_000 - 1_000 }),
    ]);

    const { getAllByTestId } = await renderTray(pane, { expand: true });
    const rows = getAllByTestId('background-task-tray-row');
    expect(rows[0]?.getAttribute('data-row-id')).toBe('older');
    expect(rows[1]?.getAttribute('data-row-id')).toBe('newer');
  });

  it('does not render a per-row Stop button when the launch has no task_id', async () => {
    // Codex launches have no task_id in meta — so the per-row Stop
    // button must hide. Codex's stop path is thread-wide (via the
    // header Stop-all button), not per-row.
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-codex',
        isBackground: true,
        status: 'running',
        summary: 'exec_command',
        toolName: 'exec_command',
        createdAt: 1_000_000 - 1_000,
      }),
    ], makeThread({ provider: 'codex' }));

    const { queryByTestId } = await renderTray(pane, { expand: true });
    expect(queryByTestId('background-task-tray-row-stop')).toBeNull();
  });

  it('does not render a per-row Stop button for a Codex row with stray task_id metadata', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-codex-weird',
        isBackground: true,
        status: 'running',
        summary: 'exec_command',
        toolName: 'exec_command',
        meta: JSON.stringify({ task_id: 'not-a-claude-task' }),
        createdAt: 1_000_000 - 1_000,
      }),
    ], makeThread({ provider: 'codex' }));

    const { queryByTestId } = await renderTray(pane, { expand: true });
    expect(queryByTestId('background-task-tray-row-stop')).toBeNull();
  });

  it('renders a per-row Stop button on a Claude launch with a task_id', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-bash',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-1' }),
        createdAt: 1_000_000 - 1_000,
      }),
    ]);

    const { getByTestId } = await renderTray(pane, { expand: true });
    expect(getByTestId('background-task-tray-row-stop')).toBeInTheDocument();
  });

  it('clicking a per-row Stop button calls StopClaudeTask with the resolved task_id', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-bash',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-99' }),
        createdAt: 1_000_000 - 1_000,
      }),
    ]);
    const stopMock = setBindingMock('StopClaudeTask', async () => {});

    const { getByTestId } = await renderTray(pane, { expand: true });
    await fireEvent.click(getByTestId('background-task-tray-row-stop'));
    await tick();

    expect(stopMock).toHaveBeenCalledTimes(1);
    expect(stopMock).toHaveBeenCalledWith(pane.thread!.id, 'tsk-99');
  });

  it('surfaces an error toast when StopClaudeTask rejects on a per-row click', async () => {
    vi.useRealTimers();
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-bash',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-42' }),
        createdAt: Date.now() - 1_000,
      }),
    ]);
    setBindingMock('StopClaudeTask', async () => {
      throw new Error('session closed');
    });

    const before = getToasts().length;
    const { getByTestId } = render(BackgroundTaskTray, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(getByTestId('background-task-tray-header'));
    await tick();
    await fireEvent.click(getByTestId('background-task-tray-row-stop'));
    // Let the promise rejection settle.
    await new Promise((r) => setTimeout(r, 10));

    const after = getToasts();
    expect(after.length).toBe(before + 1);
    const last = after[after.length - 1];
    expect(last.type).toBe('error');
    expect(last.message).toContain('session closed');
  });

  it('Stop-all on a Claude thread fans out StopClaudeTask per resolvable task_id', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-bash-1',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-a' }),
        createdAt: 1_000_000 - 1_000,
      }),
      item({
        id: 'launch-bash-2',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-b' }),
        createdAt: 1_000_000 - 2_000,
      }),
    ]);
    const stopMock = setBindingMock('StopClaudeTask', async () => {});

    const { getByTestId } = await renderTray(pane);
    await fireEvent.click(getByTestId('background-task-tray-stop-all'));
    await tick();

    expect(stopMock).toHaveBeenCalledTimes(2);
    const taskIDs = stopMock.mock.calls.map((c) => c[1]);
    expect(taskIDs).toEqual(expect.arrayContaining(['tsk-a', 'tsk-b']));
  });

  it('Stop-all on a Codex thread calls CleanCodexBackgroundTerminals once', async () => {
    const pane = await makePaneWithBackground(
      [
        item({
          id: 'launch-exec',
          isBackground: true,
          status: 'running',
          summary: 'exec_command',
          toolName: 'exec_command',
          createdAt: 1_000_000 - 1_000,
        }),
      ],
      makeThread({ provider: 'codex' }),
    );
    const cleanMock = setBindingMock('CleanCodexBackgroundTerminals', async () => {});
    const stopMock = setBindingMock('StopClaudeTask', async () => {});

    const { getByTestId } = await renderTray(pane);
    await fireEvent.click(getByTestId('background-task-tray-stop-all'));
    await tick();

    expect(cleanMock).toHaveBeenCalledTimes(1);
    expect(cleanMock).toHaveBeenCalledWith(pane.thread!.id);
    expect(stopMock).not.toHaveBeenCalled();
  });

  it('shows pending Codex unifiedExec rows but hides Stop-all until they are backgrounded', async () => {
    const pane = await makePaneWithBackground(
      [
        item({
          id: 'launch-pending',
          isBackground: false,
          status: 'running',
          summary: 'exec_command',
          toolName: 'exec_command',
          createdAt: 1_000_000 - 1_000,
        }),
      ],
      makeThread({ provider: 'codex' }),
    );

    const { getByTestId, queryByTestId } = await renderTray(pane, { expand: true });
    expect(getByTestId('background-task-tray-row')).toBeInTheDocument();
    expect(getByTestId('background-task-tray-row-status').textContent).toContain('running');
    expect(queryByTestId('background-task-tray-stop-all')).toBeNull();
  });

  it('hides the Stop-all button on a Codex thread even when a row somehow carries a task_id in meta', async () => {
    // Defensive: if a Codex row ever surfaced `task_id` in item.meta
    // (triage bug, replay corruption), the old `hasClaudeStoppable`
    // gate would have shown Stop-all and then silently no-op'd on
    // click (the dispatch only branches on `provider === 'claude'`
    // or `'codex'`). The provider gate prevents that fragile state —
    // Stop-all is hidden on a Codex thread regardless of row meta.
    const pane = await makePaneWithBackground(
      [
        item({
          id: 'launch-weird',
          isBackground: true,
          status: 'running',
          summary: 'mystery',
          // Codex subagent rows are the only "no-kill" shape in the
          // Codex tray surface, so a non-subagent launch with a
          // task_id would otherwise trip hasClaudeStoppable.
          toolName: 'exec_command',
          meta: JSON.stringify({ task_id: 'should-be-ignored' }),
          createdAt: 1_000_000 - 1_000,
        }),
      ],
      makeThread({ provider: 'codex' }),
    );

    const { getByTestId } = await renderTray(pane);
    // Stop-all should appear because the row is a running unifiedExec
    // (non-subagent), which Codex CAN stop thread-wide. Clicking it
    // must route through CleanCodexBackgroundTerminals, NOT fan out
    // StopClaudeTask with the stray task_id.
    const cleanMock = setBindingMock('CleanCodexBackgroundTerminals', async () => {});
    const stopMock = setBindingMock('StopClaudeTask', async () => {});
    await fireEvent.click(getByTestId('background-task-tray-stop-all'));
    await tick();

    expect(cleanMock).toHaveBeenCalledTimes(1);
    expect(stopMock).not.toHaveBeenCalled();
  });

  it('Stop-all stays visible on a Codex tray that mixes unifiedExec and subagent rows, and clicking it fires CleanCodexBackgroundTerminals exactly once', async () => {
    // Regression pin for the Codex stop-all visibility predicate: the
    // tray filters out pure-subagent trays (no kill path) but MUST
    // still show Stop-all when at least one unifiedExec row is
    // present. The RPC is thread-wide and terminates every bg PTY for
    // the thread — subagents are unaffected but the unifiedExec kills
    // still land. A regression that required every row to be
    // cleanable would hide Stop-all whenever a subagent coexisted,
    // leaving the user without any primitive for the terminals.
    const pane = await makePaneWithBackground(
      [
        item({
          id: 'launch-exec',
          isBackground: true,
          status: 'running',
          summary: 'pnpm run server',
          toolName: 'exec_command',
          createdAt: 1_000_000 - 1_000,
        }),
        item({
          id: 'launch-subagent',
          isBackground: true,
          status: 'running',
          summary: 'spawn_agent: helper',
          toolName: 'collab_agent',
          createdAt: 1_000_000 - 2_000,
        }),
      ],
      makeThread({ provider: 'codex' }),
    );
    const cleanMock = setBindingMock('CleanCodexBackgroundTerminals', async () => {});
    const stopMock = setBindingMock('StopClaudeTask', async () => {});

    const { getByTestId } = await renderTray(pane);
    await fireEvent.click(getByTestId('background-task-tray-stop-all'));
    await tick();

    // Thread-wide primitive fires exactly once — not one call per
    // unifiedExec row.
    expect(cleanMock).toHaveBeenCalledTimes(1);
    expect(cleanMock).toHaveBeenCalledWith(pane.thread!.id);
    // Claude stop primitive MUST NOT fire on a Codex thread, even with
    // a subagent row present.
    expect(stopMock).not.toHaveBeenCalled();
  });

  it('hides the Stop-all button when the Codex tray only contains subagent rows', async () => {
    // `collab_agent` spawn children have no client-side kill path —
    // CleanCodexBackgroundTerminals only touches unifiedExec PTYs, so
    // rendering Stop-all would promise an action we cannot perform.
    const pane = await makePaneWithBackground(
      [
        item({
          id: 'launch-subagent',
          isBackground: true,
          status: 'running',
          summary: 'spawn_agent',
          toolName: 'collab_agent',
          createdAt: 1_000_000 - 1_000,
        }),
      ],
      makeThread({ provider: 'codex' }),
    );

    const { queryByTestId } = await renderTray(pane);
    expect(queryByTestId('background-task-tray-stop-all')).toBeNull();
  });

  it('Stop-all keeps dispatching every task even when earlier ones reject (Promise.allSettled, not Promise.all)', async () => {
    // Regression pin: a plain `Promise.all` short-circuits on the first
    // rejection, so an early failure would leave later backgrounded
    // tasks running. Phase 5 uses allSettled + per-rejection toast so
    // the user sees each failure and every stop attempt still fires.
    // If someone regresses this to `Promise.all`, the second rejection
    // wouldn't register and this test would fail on the call-count
    // assertion (and the double-toast assertion).
    vi.useRealTimers();
    const now = Date.now();
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-bash-1',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-a' }),
        createdAt: now - 1_000,
      }),
      item({
        id: 'launch-bash-2',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-b' }),
        createdAt: now - 2_000,
      }),
    ]);
    const calls: string[] = [];
    const stopMock = setBindingMock('StopClaudeTask', async (_tid: unknown, taskID: unknown) => {
      calls.push(String(taskID));
      throw new Error(`lost: ${String(taskID)}`);
    });

    const before = getToasts().length;
    const { getByTestId } = render(BackgroundTaskTray, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(getByTestId('background-task-tray-stop-all'));
    await new Promise((r) => setTimeout(r, 10));

    // Both task_ids got a StopClaudeTask call despite the first failing.
    expect(stopMock).toHaveBeenCalledTimes(2);
    expect(calls).toEqual(expect.arrayContaining(['tsk-a', 'tsk-b']));
    // And both failures surfaced as distinct toasts — not a single
    // "one of them failed" toast — so the user knows exactly which
    // stops blew up.
    const after = getToasts();
    expect(after.length).toBe(before + 2);
    expect(after.some((t) => t.type === 'error' && t.message.includes('lost: tsk-a'))).toBe(true);
    expect(after.some((t) => t.type === 'error' && t.message.includes('lost: tsk-b'))).toBe(true);
  });

  it('surfaces an error toast when a Stop-all fan-out call rejects', async () => {
    vi.useRealTimers();
    const now = Date.now();
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-bash-1',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-a' }),
        createdAt: now - 1_000,
      }),
    ]);
    setBindingMock('StopClaudeTask', async () => {
      throw new Error('stream lost');
    });

    const before = getToasts().length;
    const { getByTestId } = render(BackgroundTaskTray, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(getByTestId('background-task-tray-stop-all'));
    await new Promise((r) => setTimeout(r, 10));

    const after = getToasts();
    expect(after.length).toBeGreaterThan(before);
    expect(after.some((t) => t.type === 'error' && t.message.includes('stream lost'))).toBe(true);
  });

  it('does not render the per-row Stop button on a running Codex subagent row', async () => {
    const pane = await makePaneWithBackground(
      [
        item({
          id: 'launch-subagent',
          isBackground: true,
          status: 'running',
          summary: 'spawn_agent',
          toolName: 'collab_agent',
          createdAt: 1_000_000 - 1_000,
        }),
      ],
      makeThread({ provider: 'codex' }),
    );

    const { queryByTestId } = await renderTray(pane, { expand: true });
    expect(queryByTestId('background-task-tray-row-stop')).toBeNull();
  });

  it('non-output rows do not request timeline scroll when clicked', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 1_000,
      }),
    ]);
    const scrollSpy = vi.spyOn(pane, 'requestScrollToItem');

    const { getByTestId } = await renderTray(pane, { expand: true });
    const row = getByTestId('background-task-tray-row');
    expect(row.tagName).not.toBe('BUTTON');
    await fireEvent.click(row);
    expect(scrollSpy).not.toHaveBeenCalled();
  });

  it('command output rows expand a preview without requesting timeline scroll or full payload chunks', async () => {
    const preview = setBindingMock('GetPayloadPreview', async () => ({
      data: 'preview output',
      totalSize: 200_000,
      isComplete: false,
      nextOffset: 14,
    }));
    const chunk = setBindingMock('GetPayloadChunk', async () => {
      throw new Error('tray should not fetch full output');
    });
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        payloadKind: 'command_output',
        payloadId: 'payload-a',
        payloadMeta: JSON.stringify({
          command: 'sleep 1; echo done',
          exitCode: 0,
          lineCount: 1,
          preview: 'preview output',
        }),
        createdAt: 1_000_000 - 1_000,
      }),
    ]);
    const scrollSpy = vi.spyOn(pane, 'requestScrollToItem');

    const rendered = await renderTray(pane, { expand: true });
    expect(rendered.queryByTestId('background-task-tray-row-output')).toBeNull();

    await fireEvent.click(rendered.getByRole('button', { name: /Toggle command output/i }));
    await tick();
    await tick();

    expect(rendered.getByTestId('background-task-tray-row-output')).toBeInTheDocument();
    const outputToggles = rendered.getAllByRole('button', { name: /Toggle command output: sleep 1; echo done/i });
    await fireEvent.click(outputToggles[outputToggles.length - 1]!);
    await tick();
    await tick();

    expect(preview).toHaveBeenCalledWith('thread-1', 'payload-a', expect.any(Number));
    expect(chunk).not.toHaveBeenCalled();
    expect(rendered.queryByTestId('command-output-show-full')).toBeNull();
    expect(scrollSpy).not.toHaveBeenCalled();
  });

  it('disables the per-row Stop button while the StopClaudeTask call is in-flight', async () => {
    vi.useRealTimers();
    const pending: Array<() => void> = [];
    setBindingMock('StopClaudeTask', () => new Promise<void>((r) => {
      pending.push(() => r());
    }));

    const now = Date.now();
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-bash',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        toolName: 'Bash',
        meta: JSON.stringify({ task_id: 'tsk-slow' }),
        createdAt: now - 1_000,
      }),
    ]);

    const { getByTestId } = render(BackgroundTaskTray, { props: { pane } });
    await tick();
    await tick();
    await fireEvent.click(getByTestId('background-task-tray-header'));
    await tick();
    const btn = getByTestId('background-task-tray-row-stop') as HTMLButtonElement;
    expect(btn.disabled).toBe(false);
    await fireEvent.click(btn);
    await tick();
    expect(btn.disabled).toBe(true);
    // Resolve the pending StopClaudeTask so the tray clears its
    // "in-flight" flag and re-renders the button as enabled.
    const done = pending.shift();
    if (!done) throw new Error('StopClaudeTask was not called');
    done();
    // Let the promise settle + re-render.
    await new Promise((r) => setTimeout(r, 10));
    expect(btn.disabled).toBe(false);
  });
});
