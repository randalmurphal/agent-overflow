import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import RemoteJobTrayRow from './RemoteJobTrayRow.svelte';
import ActivityRailBackgroundBody from './ActivityRailBackgroundBody.svelte';
import { makeItem } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { deriveTrayTasks, trayRemoteJob } from '../../utils/backgroundTray';
import { resetStagedBackends, stageBackend } from '../../../test/helpers/backends';

const job = { computerId: 'nexus', requestId: 'job-1', workspace: '/workspace', error: '', notification: '' };
function remoteTask(summary = 'Nexus · python train.py') {
  return deriveTrayTasks([makeItem({ id: 'remote-job:nexus:job-1', toolName: 'remote_command', isBackground: true,
    status: 'running', summary, meta: JSON.stringify({ remoteJob: job }) })], Date.now(), 200)[0];
}
const chunk = { text: 'epoch 10 complete', error: '', offset: 100, nextOffset: 117, expired: false };

describe('remote jobs in the background tray', () => {
  beforeEach(resetBindingMocks);
  afterEach(resetStagedBackends);

  it('names the attached computer and holds Stop while it is offline', async () => {
    const staged = stageBackend({ id: 'nexus', name: 'Nexus', status: 'reconnecting' });
    const view = render(RemoteJobTrayRow, { task: remoteTask('python train.py'), job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(view.getByTitle('python train.py · Nexus')).toBeInTheDocument();
    const stop = view.getByRole('button', { name: 'Stop Remote Job' });
    expect(stop).toBeDisabled();
    expect(stop).toHaveAttribute('title', 'Offline');
    staged.setStatus('connected');
    await waitFor(() => expect(stop).toBeEnabled());
    expect(stop).not.toHaveAttribute('title');
  });

  it('does not repeat a computer the receipt already names', () => {
    stageBackend({ id: 'nexus', name: 'Nexus' });
    const view = render(RemoteJobTrayRow, { task: remoteTask(), job, threadId: 'thread', isStopping: false, onStop: vi.fn() });
    expect(view.getByTitle('Nexus · python train.py')).toBeInTheDocument();
    expect(view.getByRole('button', { name: 'Stop Remote Job' })).toBeEnabled();
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
