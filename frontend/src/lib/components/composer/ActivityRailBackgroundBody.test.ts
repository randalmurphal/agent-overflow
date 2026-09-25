import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ActivityRailBackgroundBody from './ActivityRailBackgroundBody.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { deriveTrayTasks } from '../../utils/backgroundTray';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import type { Item } from '../../types/models';

function shell(id: string, overrides: Partial<Item> = {}): Item {
  return makeItem({ id, threadId: 'thread', kind: 'tool_call', toolName: 'Bash', isBackground: true,
    status: 'running', summary: `Bash: ${id}`, createdAt: Date.now(), meta: JSON.stringify({ task_id: `tsk-${id}` }), ...overrides });
}
function killed(launch: Item): Item {
  return makeItem({ id: `${launch.id}:done`, threadId: 'thread', kind: 'tool_completion', status: 'killed',
    completionOf: launch.id, createdAt: Date.now() });
}
function trayTasks(items: Item[]) {
  return deriveTrayTasks(items, Date.now(), 200);
}
function renderBody(items: Item[]) {
  return render(ActivityRailBackgroundBody, { tasks: trayTasks(items), provider: 'claude', threadId: 'thread', runningCount: items.length });
}
function stopError(view: ReturnType<typeof renderBody>, rowId: string): string | null {
  const row = view.container.querySelector(`[data-testid="background-task-tray-row"][data-row-id="${rowId}"]`);
  return row?.querySelector('[data-testid="background-task-tray-row-stop-error"]')?.textContent?.trim() ?? null;
}
function rowStop(view: ReturnType<typeof renderBody>, rowId: string): HTMLButtonElement {
  return view.container.querySelector(`[data-row-stop-id="${rowId}"]`) as HTMLButtonElement;
}
function toasts() {
  return getToasts().map((toast) => [toast.type, toast.message]);
}

describe('Stop All in the background tray', () => {
  beforeEach(() => {
    resetBindingMocks();
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  it('renders each launch\'s result: a failed stop on its row, an ended task in a notice', async () => {
    const [a, b, c, d] = ['a', 'b', 'c', 'd'].map((id) => shell(id));
    const stopAll = setBindingMock('StopBackgroundTasks', async () => [
      { launchItemId: 'a', outcome: 'failed', error: 'refused' },
      { launchItemId: 'b', outcome: 'stopping' },
      { launchItemId: 'c', outcome: 'ended' },
    ]);
    setBindingMock('StopClaudeTask', async () => {});
    const view = renderBody([a, b, c, d]);

    await fireEvent.click(view.getByRole('button', { name: 'Stop All Running Background Tasks' }));
    await waitFor(() => expect(stopError(view, 'a')).toBe('Stop failed: refused'));
    expect(stopAll).toHaveBeenCalledTimes(1);
    expect(stopAll).toHaveBeenCalledWith('thread', ['a', 'b', 'c', 'd']);
    expect(stopError(view, 'b')).toBeNull();
    expect(stopError(view, 'c')).toBeNull();
    expect(stopError(view, 'd')).toBe('Stop failed: the stop returned no result for this task');
    expect(toasts()).toEqual([
      ['error', 'Failed to stop 2 tasks: refused'],
      ['info', 'A task had already ended.'],
    ]);
    for (const id of ['a', 'b', 'c', 'd']) expect(rowStop(view, id)).toHaveTextContent(/^Stop$/);

    // Stopping the row again clears its error; settling clears the rest.
    await fireEvent.click(rowStop(view, 'd'));
    await waitFor(() => expect(stopError(view, 'd')).toBeNull());
    expect(stopError(view, 'a')).toBe('Stop failed: refused');
    await view.rerender({ tasks: trayTasks([a, killed(a), b, c, d]), provider: 'claude', threadId: 'thread', runningCount: 3 });
    expect(stopError(view, 'a')).toBeNull();
  });

  it('marks the named rows while the call runs and reports a failed call once', async () => {
    const [a, b] = ['a', 'b'].map((id) => shell(id));
    let fail!: (err: Error) => void;
    setBindingMock('StopBackgroundTasks', () => new Promise((_resolve, reject) => { fail = reject; }));
    const view = renderBody([a, b]);

    await fireEvent.click(view.getByRole('button', { name: 'Stop All Running Background Tasks' }));
    await waitFor(() => expect(rowStop(view, 'a')).toHaveTextContent('Stopping…'));
    expect(rowStop(view, 'b')).toBeDisabled();
    fail(new Error('computer offline'));
    await waitFor(() => expect(rowStop(view, 'a')).toHaveTextContent(/^Stop$/));
    expect(rowStop(view, 'b')).toBeEnabled();
    expect(stopError(view, 'a')).toBeNull();
    expect(toasts()).toEqual([['error', 'Failed to stop tasks: computer offline']]);
  });

  it('names no row without a stop of its own', async () => {
    const stopAll = setBindingMock('StopBackgroundTasks', async () => [{ launchItemId: 'a', outcome: 'stopping' }]);
    const view = renderBody([shell('a'), shell('no-id', { meta: '{}' }), shell('done', { status: 'completed' })]);

    await fireEvent.click(view.getByRole('button', { name: 'Stop All Running Background Tasks' }));
    await waitFor(() => expect(stopAll).toHaveBeenCalledTimes(1));
    expect(stopAll).toHaveBeenCalledWith('thread', ['a']);
    expect(toasts()).toEqual([]);
  });
});
