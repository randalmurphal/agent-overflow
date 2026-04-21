import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, fireEvent, cleanup } from '@testing-library/svelte';
import { tick } from 'svelte';
import BackgroundTaskTray from './BackgroundTaskTray.svelte';
import type { Item } from '../../types/models';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

/**
 * Test fixtures mirror the real `Item` shape produced by the backend:
 * launches land as `tool_call`, completions as `tool_completion` with
 * `completionOf` pointing at the launch id. `status` uses the real
 * vocabulary from the store (`streaming | running | completed |
 * errored | declined`).
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
    highlightedContent: '',
    createdAt: 0,
    updatedAt: 0,
    ...overrides,
  };
}

async function makePaneWithBackground(items: Item[]) {
  const pane = await buildPane();
  setBindingMock('ListLiveBackgroundTasks', async () => items);
  return pane;
}

/**
 * Render the tray and flush the ListLiveBackgroundTasks fetch + Svelte
 * reactions until the derived task list has settled. Two ticks covers
 * the async binding result + the $derived pass that reads it.
 */
async function renderTray(pane: Awaited<ReturnType<typeof buildPane>>) {
  const result = render(BackgroundTaskTray, { props: { pane } });
  await tick();
  await tick();
  return result;
}

describe('<BackgroundTaskTray>', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(1_000_000));
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
    vi.useRealTimers();
  });

  it('renders nothing when the backend returns no live background tasks', async () => {
    const pane = await makePaneWithBackground([]);
    const { container, queryByTestId } = await renderTray(pane);

    expect(queryByTestId('background-task-tray')).toBeNull();
    expect(container.textContent?.trim() ?? '').toBe('');
  });

  it('renders the header with a count when there is at least one background task', async () => {
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
    expect(getByTestId('background-task-tray-count').textContent).toBe('2');
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
    const { getByTestId, queryByTestId, getAllByTestId } = await renderTray(pane);
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
    const { getByTestId } = await renderTray(pane);
    const status = getByTestId('background-task-tray-row-status');
    expect(status.getAttribute('data-status')).toBe('declined');
    expect(status.className).toContain('text-error');
    expect(status.textContent).toContain('Declined');
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
    const { getByTestId, queryByTestId } = await renderTray(pane);
    expect(getByTestId('background-task-tray-row')).toBeInTheDocument();
    expect(queryByTestId('background-task-tray-row-elapsed')).toBeNull();
  });

  it('clicking a row publishes a scroll-to-item request on the pane', async () => {
    const pane = await makePaneWithBackground([
      item({
        id: 'launch-a',
        isBackground: true,
        status: 'running',
        summary: 'Bash',
        createdAt: 1_000_000 - 3_000,
      }),
    ]);
    const spy = vi.spyOn(pane, 'requestScrollToItem');
    const { getAllByTestId } = await renderTray(pane);

    const rows = getAllByTestId('background-task-tray-row');
    expect(rows).toHaveLength(1);
    await fireEvent.click(rows[0]);

    expect(spy).toHaveBeenCalledTimes(1);
    expect(spy).toHaveBeenCalledWith('launch-a');
  });

  it('caps visible rows at three and shows the hidden count', async () => {
    const pane = await makePaneWithBackground([
      item({ id: 'a', isBackground: true, status: 'running', summary: 'A', createdAt: 1_000_000 - 1_000 }),
      item({ id: 'b', isBackground: true, status: 'running', summary: 'B', createdAt: 1_000_000 - 2_000 }),
      item({ id: 'c', isBackground: true, status: 'running', summary: 'C', createdAt: 1_000_000 - 3_000 }),
      item({ id: 'd', isBackground: true, status: 'running', summary: 'D', createdAt: 1_000_000 - 4_000 }),
    ]);

    const { getAllByTestId, getByTestId } = await renderTray(pane);
    expect(getAllByTestId('background-task-tray-row')).toHaveLength(3);
    expect(getByTestId('background-task-tray-more').textContent).toContain('+1 more');
  });

  it('refreshes on upsert of a background launch (isBackground=true, completionOf empty)', async () => {
    // Regression pin for the `isBackground || completionOf` filter in
    // the provider:item_upsert handler. A background launch that has
    // not yet been paired with a completion has `completionOf` empty;
    // the handler must still trigger a refresh — flipping the `||`
    // to `&&` would silently stop all launches from refreshing the
    // tray.
    vi.useRealTimers();
    const wailsioMock = await import('../../../test/mocks/wailsio-runtime');
    const { emitWailsEvent } = wailsioMock;
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

    emitWailsEvent('provider:item_upsert', {
      id: 'fresh-launch',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      summary: 'Bash: sleep 1',
      highlightedContent: '',
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
    const wailsioMock = await import('../../../test/mocks/wailsio-runtime');
    const { emitWailsEvent } = wailsioMock;
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

    emitWailsEvent('provider:item_upsert', {
      id: 'diff-only',
      threadId: pane.thread!.id,
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      summary: 'Edit: foo.ts',
      highlightedContent: '',
      // isBackground + completionOf both falsy
      createdAt: 0,
      updatedAt: 0,
    });
    await new Promise((r) => setTimeout(r, 150));
    expect(fetchCalls).toBe(mountCalls);
  });

  it('orders rows by latest activity first', async () => {
    const pane = await makePaneWithBackground([
      item({ id: 'older', isBackground: true, status: 'running', summary: 'Older', createdAt: 1_000_000 - 4_000, updatedAt: 1_000_000 - 4_000 }),
      item({ id: 'newer', isBackground: true, status: 'running', summary: 'Newer', createdAt: 1_000_000 - 1_000, updatedAt: 1_000_000 - 1_000 }),
    ]);

    const { getAllByTestId } = await renderTray(pane);
    const rows = getAllByTestId('background-task-tray-row');
    expect(rows[0]?.getAttribute('data-row-id')).toBe('newer');
    expect(rows[1]?.getAttribute('data-row-id')).toBe('older');
  });
});
