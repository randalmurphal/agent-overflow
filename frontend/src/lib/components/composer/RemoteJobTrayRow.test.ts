import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import RemoteJobTrayRow from './RemoteJobTrayRow.svelte';
import ActivityRailBackgroundBody from './ActivityRailBackgroundBody.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { deriveTrayTasks, trayRemoteJob } from '../../utils/backgroundTray';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';

const job = { computerId: 'nexus', requestId: 'job-1', workspace: '/workspace', error: '', warning: '', notification: '' };
// The projection the desktop writes (app_remote_watch.go remoteTrayItems):
// the remote_run call's presentation plus the tray's own handle.
function remoteItem(input: Record<string, unknown>, remoteJob = job, overrides: Partial<Parameters<typeof makeItem>[0]> = {}) {
  return makeItem({ id: `remote-job:nexus:${remoteJob.requestId}`, kind: 'tool_call', toolName: 'MCP/remote_run', isBackground: true,
    status: 'running', summary: 'Nexus · python train.py', createdAt: Date.now(),
    meta: JSON.stringify({ mcp: { server: 'ao-remote-tools', tool: 'remote_run' },
      input: { computer_id: 'nexus', computer_name: 'Nexus', request_id: remoteJob.requestId, label: 'python train.py', command: 'python train.py', ...input },
      remoteJob }), ...overrides });
}
function remoteTask(input: Record<string, unknown> = {}, remoteJob = job) {
  return deriveTrayTasks([remoteItem(input, remoteJob)], Date.now(), 200)[0];
}
const chunk = { text: 'epoch 10 complete', error: '', offset: 100, nextOffset: 117, expired: false };

describe('remote jobs in the background tray', () => {
  beforeEach(resetBindingMocks);
  afterEach(resetStagedBackends);

  it('presents the job as the remote_run call it came from and holds Stop while the computer is offline', async () => {
    const staged = stageBackend({ id: 'nexus', name: 'Nexus', status: 'reconnecting' });
    const view = render(RemoteJobTrayRow, { task: remoteTask(), job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(view.getByTestId('remote-job-tray-row').dataset.toolKind).toBe('monitor');
    expect(view.getByTestId('remote-job-tray-row-label')).toHaveTextContent('run');
    expect(view.getByTestId('remote-job-tray-row-where').dataset.machine).toBe('Nexus');
    expect(view.getByTestId('remote-job-tray-row-command')).toHaveTextContent('python train.py');
    expect(view.queryByTestId('remote-job-tray-row-job-label')).toBeNull();
    expect(view.getByTestId('remote-job-tray-row-status').dataset.state).toBe('backgrounded');
    const stop = view.getByRole('button', { name: 'Stop Remote Job' });
    expect(stop).toBeDisabled();
    expect(stop).toHaveAttribute('title', 'Offline');
    staged.setStatus('connected');
    await waitFor(() => expect(stop).toBeEnabled());
    expect(stop).not.toHaveAttribute('title');
  });

  it('shows the job label beside the command when the caller gave one', () => {
    const view = render(RemoteJobTrayRow, { task: remoteTask({ label: 'Train image model' }), job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(view.getByTestId('remote-job-tray-row-command')).toHaveTextContent('python train.py');
    expect(view.getByTestId('remote-job-tray-row-job-label')).toHaveTextContent('Train image model');
  });

  it('shows a receipt warning beside the row', () => {
    const warned = { ...job, warning: 'The command left background processes running in its process group; they were stopped when it exited.' };
    const task = remoteTask({}, warned);
    expect(trayRemoteJob(task)).toEqual(warned);
    const view = render(RemoteJobTrayRow, { task, job: warned, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(view.getByText(/left background processes/)).toBeInTheDocument();
  });

  it('names the computer as the desktop knew it when this client is not attached there', () => {
    const view = render(RemoteJobTrayRow, { task: remoteTask({ computer_name: 'Macaroni-air' }), job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(view.getByTestId('remote-job-tray-row-where').dataset.machine).toBe('Macaroni-air');
    expect(view.getByRole('button', { name: 'Stop Remote Job' })).toBeEnabled();
  });

  it('reports the outcome under the row once the receipt settles', () => {
    const launch = remoteItem({});
    const done = remoteItem({}, job, { id: `${launch.id}:done`, kind: 'tool_completion', completionOf: launch.id, status: 'killed' });
    const task = deriveTrayTasks([launch, done], Date.now(), 200)[0];
    const view = render(RemoteJobTrayRow, { task, job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(view.queryByRole('button', { name: 'Stop Remote Job' })).toBeNull();
    expect(view.getByTestId('remote-job-tray-row-status').dataset.state).toBe('error');
    expect(view.getByText('Tool call stopped')).toBeInTheDocument();
  });

  it('loads only a bounded tail on expansion and discards a read closed before completion', async () => {
    let finish!: (value: typeof chunk) => void;
    const read = setBindingMock('ReadThreadRemoteLog', () => new Promise((resolve) => { finish = resolve; }));
    const { getByRole, queryByText } = render(RemoteJobTrayRow, { task: remoteTask(), job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(read).not.toHaveBeenCalled();
    const toggle = getByRole('button', { name: /Show remote job log/ });
    await fireEvent.click(toggle);
    expect(read).toHaveBeenCalledWith('thread', 'nexus', 'job-1', -1, 16384);
    await fireEvent.click(toggle);
    finish(chunk);
    expect(queryByText('epoch 10 complete')).toBeNull();
    setBindingMock('ReadThreadRemoteLog', async () => chunk);
    await fireEvent.click(toggle);
    await waitFor(() => expect(queryByText('epoch 10 complete')).not.toBeNull());
    expect(queryByText('Showing the latest output.')).not.toBeNull();
  });

  it('keeps log errors visible and supports retry without losing the row', async () => {
    setBindingMock('ReadThreadRemoteLog', async () => { throw new Error('Nexus is offline'); });
    const { getByRole, findByRole, findByText } = render(RemoteJobTrayRow, { task: remoteTask(), job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    await fireEvent.click(getByRole('button', { name: /Show remote job log/ }));
    expect(await findByRole('alert')).toHaveTextContent('Nexus is offline');
    setBindingMock('ReadThreadRemoteLog', async () => ({ ...chunk, text: '', expired: true }));
    await fireEvent.click(getByRole('button', { name: 'Refresh log' }));
    expect(await findByText('This log has expired.')).toBeInTheDocument();
  });

  it.each(['claude', 'codex'] as const)('stops a remote-only job without calling %s provider controls or scrolling', async (provider) => {
    const cancel = setBindingMock('CancelThreadRemoteCommand', async () => ({}));
    const claude = setBindingMock('StopClaudeTask', async () => {});
    const codex = setBindingMock('CleanCodexBackgroundTerminals', async () => {});
    const pane = { requestScrollToItem: vi.fn() };
    const task = remoteTask();
    expect(trayRemoteJob(task)).toEqual(job);
    const { getByRole } = render(ActivityRailBackgroundBody, { tasks: [task], provider, threadId: 'thread', runningCount: 1, pane: pane as never });
    await fireEvent.click(getByRole('button', { name: 'Stop Remote Job' }));
    await waitFor(() => expect(cancel).toHaveBeenCalledTimes(1));
    await fireEvent.click(getByRole('button', { name: 'Stop All Running Background Tasks' }));
    await waitFor(() => expect(cancel).toHaveBeenCalledTimes(2));
    expect(cancel).toHaveBeenCalledWith('thread', 'nexus', 'job-1');
    expect(claude).not.toHaveBeenCalled();
    expect(codex).not.toHaveBeenCalled();
    expect(pane.requestScrollToItem).not.toHaveBeenCalled();
  });

  it('keeps a remote row at Stopping… after the cancel returns, until the receipt leaves running', async () => {
    setBindingMock('CancelThreadRemoteCommand', async () => ({}));
    const task = remoteTask();
    const view = render(ActivityRailBackgroundBody, { tasks: [task], provider: 'claude', threadId: 'thread', runningCount: 1 });
    const stop = view.getByRole('button', { name: 'Stop Remote Job' });
    await fireEvent.click(stop);
    await waitFor(() => expect(stop).toHaveTextContent('Stopping…'));
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(stop).toBeDisabled();
    expect(stop).toHaveTextContent('Stopping…');
    // The watcher observes the canceled receipt: the row settles.
    const launch = remoteItem({});
    const done = remoteItem({}, job, { id: `${launch.id}:done`, kind: 'tool_completion', completionOf: launch.id, status: 'killed' });
    await view.rerender({ tasks: deriveTrayTasks([launch, done], Date.now(), 200), provider: 'claude', threadId: 'thread', runningCount: 0 });
    expect(view.queryByRole('button', { name: 'Stop Remote Job' })).toBeNull();
    // A later job under a fresh id starts unmarked.
    const next = { ...job, requestId: 'job-2' };
    await view.rerender({ tasks: [remoteTask({}, next)], provider: 'claude', threadId: 'thread', runningCount: 1 });
    expect(view.getByRole('button', { name: 'Stop Remote Job' })).toHaveTextContent('Stop');
  });

  it('Stop All combines provider tasks with remote jobs once each', async () => {
    const cancel = setBindingMock('CancelThreadRemoteCommand', async () => ({}));
    const cleanup = setBindingMock('CleanCodexBackgroundTerminals', async () => {});
    const tasks = [...deriveTrayTasks([makeItem({ id: 'pty', toolName: 'exec_command', isBackground: true,
      status: 'running', meta: JSON.stringify({ process_id: '12' }) })], Date.now(), 200), remoteTask()];
    const { getByRole } = render(ActivityRailBackgroundBody, { tasks, provider: 'codex', threadId: 'thread', runningCount: 2 });
    await fireEvent.click(getByRole('button', { name: 'Stop All Running Background Tasks' }));
    await waitFor(() => expect(cleanup).toHaveBeenCalledTimes(1));
    expect(cancel).toHaveBeenCalledTimes(1);
  });
});
