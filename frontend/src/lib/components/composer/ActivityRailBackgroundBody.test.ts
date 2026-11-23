import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import ActivityRailBackgroundBody from './ActivityRailBackgroundBody.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { normalizedTextContent } from '../../../test/helpers/textContent';
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
    setBindingMock('StopClaudeTask', async () => true);
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
    for (const id of ['a', 'b', 'c', 'd']) expect(normalizedTextContent(rowStop(view, id))).toMatch(/^Stop$/);

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
    await waitFor(() => expect(normalizedTextContent(rowStop(view, 'a'))).toMatch(/^Stop$/));
    expect(rowStop(view, 'b')).toBeEnabled();
    expect(stopError(view, 'a')).toBeNull();
    expect(toasts()).toEqual([['error', 'Failed to stop tasks: computer offline']]);
    expect(getToasts()[0].captured?.report.chain).toEqual(['computer offline']);
  });

  it('names no row without a stop of its own', async () => {
    const stopAll = setBindingMock('StopBackgroundTasks', async () => [{ launchItemId: 'a', outcome: 'stopping' }]);
    const view = renderBody([shell('a'), shell('no-id', { meta: '{}' }), shell('done', { status: 'completed' })]);

    await fireEvent.click(view.getByRole('button', { name: 'Stop All Running Background Tasks' }));
    await waitFor(() => expect(stopAll).toHaveBeenCalledTimes(1));
    expect(stopAll).toHaveBeenCalledWith('thread', ['a']);
    expect(toasts()).toEqual([]);
  });

  // A computer whose rows stop by item id stops each named command alone in
  // Stop All, so an agent pane's tray names its commands in the bulk call,
  // and a row's own Stop names the command by item id, the agent's included.
  it('stops a scoped Codex tray’s item-id commands in the bulk call and by item id from the row', async () => {
    const agentCommand = shell('agent-cmd', { parentId: 'agent', meta: JSON.stringify({ stop_by_item: true }) });
    const rootCommand = shell('root-cmd', { meta: JSON.stringify({ stop_by_item: true, process_id: 'pid-9' }) });
    const stopAll = setBindingMock('StopBackgroundTasks', async (_thread: string, ids: string[]) =>
      ids.map((launchItemId) => ({ launchItemId, outcome: 'stopping' })));
    const stopCommand = setBindingMock('StopCodexBackgroundCommand', async () => true);
    const terminate = setBindingMock('TerminateCodexBackgroundTerminal', async () => true);
    const props = { tasks: trayTasks([agentCommand, rootCommand]), provider: 'codex' as const, threadId: 'thread', runningCount: 2 };

    const scopedView = render(ActivityRailBackgroundBody, { ...props, scoped: true, idPrefix: 'agent-pane' });
    await fireEvent.click(scopedView.getByTestId('agent-pane-background-stop-all'));
    await waitFor(() => expect(stopAll).toHaveBeenCalledWith('thread', ['agent-cmd', 'root-cmd']));
    expect(stopCommand).not.toHaveBeenCalled();
    scopedView.unmount();

    const threadView = render(ActivityRailBackgroundBody, props);
    for (const id of ['agent-cmd', 'root-cmd']) {
      await fireEvent.click(rowStop(threadView, id));
      await waitFor(() => expect(stopCommand).toHaveBeenLastCalledWith('thread', id));
    }
    expect(terminate).not.toHaveBeenCalled();
    expect(toasts()).toEqual([]);
  });

  it('says so when a Codex command stopped by item id had already ended', async () => {
    setBindingMock('StopCodexBackgroundCommand', async () => false);
    const view = render(ActivityRailBackgroundBody, {
      tasks: trayTasks([shell('agent-cmd', { meta: JSON.stringify({ stop_by_item: true }) })]),
      provider: 'codex', threadId: 'thread', runningCount: 1,
    });
    await fireEvent.click(rowStop(view, 'agent-cmd'));
    await waitFor(() => expect(toasts()).toEqual([['info', 'That background terminal had already exited.']]));
  });

  // Stop All acts on the handle each row's Stop shows. A backgrounded command
  // inside a code review has neither handle, so it has no Stop of its own,
  // and Stop All neither names it nor sends a stop with no target for it.
  it('leaves a Codex command with no Stop of its own out of Stop All', async () => {
    const reviewCommand = shell('review-cmd', { parentId: 'review', meta: JSON.stringify({ source: 'unifiedExecStartup' }) });
    const command = shell('cmd', { meta: JSON.stringify({ stop_by_item: true }) });
    const stopAll = setBindingMock('StopBackgroundTasks', async (_thread: string, ids: string[]) =>
      ids.map((launchItemId) => ({ launchItemId, outcome: 'stopping' })));
    const terminate = setBindingMock('TerminateCodexBackgroundTerminal', async () => true);
    const stopCommand = setBindingMock('StopCodexBackgroundCommand', async () => true);
    const props = { tasks: trayTasks([reviewCommand, command]), provider: 'codex' as const, threadId: 'thread', runningCount: 2 };

    const threadView = render(ActivityRailBackgroundBody, props);
    expect(rowStop(threadView, 'review-cmd')).toBeNull();
    await fireEvent.click(threadView.getByTestId('activity-rail-background-stop-all'));
    await waitFor(() => expect(stopAll).toHaveBeenCalledWith('thread', ['cmd']));
    threadView.unmount();

    const scopedView = render(ActivityRailBackgroundBody, { ...props, scoped: true, idPrefix: 'agent-pane' });
    await fireEvent.click(scopedView.getByTestId('agent-pane-background-stop-all'));
    await waitFor(() => expect(stopAll).toHaveBeenCalledTimes(2));
    expect(stopAll).toHaveBeenLastCalledWith('thread', ['cmd']);
    expect(terminate).not.toHaveBeenCalled();
    expect(stopCommand).not.toHaveBeenCalled();
    expect(toasts()).toEqual([]);
  });

  // A computer older than the item-id Stop cleans terminals thread-wide in
  // Stop All, so an agent pane's tray must not name its terminals in the
  // bulk call: that would stop the thread's other terminals too.
  it('stops a scoped Codex tray’s terminals one by one and names only its subagents', async () => {
    const terminal = shell('term', { meta: JSON.stringify({ process_id: 'pid-7' }) });
    const agent = shell('agent', { toolName: 'collab_agent', meta: JSON.stringify({ input: { tool: 'spawn_agent' } }) });
    const stopAll = setBindingMock('StopBackgroundTasks', async (_thread: string, ids: string[]) =>
      ids.map((launchItemId) => ({ launchItemId, outcome: 'stopping' })));
    const terminate = setBindingMock('TerminateCodexBackgroundTerminal', async () => true);
    const tasks = trayTasks([terminal, agent]);
    const props = { tasks, provider: 'codex' as const, threadId: 'thread', runningCount: 2 };

    const scopedView = render(ActivityRailBackgroundBody, { ...props, scoped: true, idPrefix: 'agent-pane' });
    await fireEvent.click(scopedView.getByTestId('agent-pane-background-stop-all'));
    await waitFor(() => expect(terminate).toHaveBeenCalledWith('thread', 'pid-7'));
    expect(stopAll).toHaveBeenCalledTimes(1);
    expect(stopAll).toHaveBeenCalledWith('thread', ['agent']);
    scopedView.unmount();

    const threadView = render(ActivityRailBackgroundBody, props);
    await fireEvent.click(threadView.getByTestId('activity-rail-background-stop-all'));
    await waitFor(() => expect(stopAll).toHaveBeenCalledTimes(2));
    expect(stopAll).toHaveBeenLastCalledWith('thread', ['term', 'agent']);
    expect(terminate).toHaveBeenCalledTimes(1);
    expect(toasts()).toEqual([]);
  });
});
