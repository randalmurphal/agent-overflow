import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { ThreadPane } from '../../stores/thread.svelte';
import { applyShellCommandEvent, getShellRun, resetShellRunsForTest } from '../../stores/shellCommands.svelte';
import ShellRunCard from './ShellRunCard.svelte';

function makePane(threadId: string | null = 'thread-1') {
  const setGeneralError = vi.fn();
  return { pane: { threadId, setGeneralError } as unknown as ThreadPane, setGeneralError };
}

function start(command = 'make test', threadId = 'thread-1'): void {
  applyShellCommandEvent({ phase: 'started', threadId, runId: 'run-1', command, startedAt: Date.now() });
}

describe('<ShellRunCard>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetShellRunsForTest();
  });

  it('renders nothing while no command runs, and nothing for another thread', async () => {
    const { pane } = makePane();
    const { queryByTestId } = render(ShellRunCard, { props: { pane } });
    expect(queryByTestId('shell-run-card')).toBeNull();
    start('ls', 'thread-2');
    await tick();
    expect(queryByTestId('shell-run-card')).toBeNull();
  });

  it('shows the command and the last lines of its output, then leaves when it is sent', async () => {
    const { pane } = makePane();
    const { getByTestId, queryByTestId } = render(ShellRunCard, { props: { pane } });
    start();
    await tick();
    expect(getByTestId('shell-run-card').getAttribute('data-state')).toBe('running');
    expect(getByTestId('shell-run-command').textContent).toBe('make test');
    // No output yet: the card is its header alone.
    expect(queryByTestId('shell-run-output')).toBeNull();

    const lines = Array.from({ length: 9 }, (_, i) => `line ${i + 1}\n`).join('');
    applyShellCommandEvent({ phase: 'output', threadId: 'thread-1', runId: 'run-1', seq: 1, chunk: lines });
    await tick();
    const shown = getByTestId('shell-run-output').textContent ?? '';
    expect(shown).not.toContain('line 3');
    expect(shown.startsWith('line 4')).toBe(true);
    expect(shown.endsWith('line 9')).toBe(true);

    applyShellCommandEvent({ phase: 'finished', threadId: 'thread-1', runId: 'run-1', state: 'sent' });
    await tick();
    expect(queryByTestId('shell-run-card')).toBeNull();
  });

  it('stops the command once, and reports a stop that fails', async () => {
    const { pane, setGeneralError } = makePane();
    let fail = false;
    const stop = setBindingMock('StopThreadShellCommand', async () => {
      if (fail) throw new Error('not running');
    });
    const { getByTestId } = render(ShellRunCard, { props: { pane } });
    start();
    await tick();

    await fireEvent.click(getByTestId('shell-run-stop'));
    expect(stop).toHaveBeenCalledWith('thread-1', 'run-1');
    await waitFor(() => expect(getByTestId('shell-run-stop').textContent).toContain('Stopping'));
    expect((getByTestId('shell-run-stop') as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(getByTestId('shell-run-stop'));
    expect(stop).toHaveBeenCalledTimes(1);

    // A fresh run whose stop is refused says so and offers Stop again.
    applyShellCommandEvent({ phase: 'finished', threadId: 'thread-1', runId: 'run-1', state: 'stopped' });
    start();
    await tick();
    fail = true;
    await fireEvent.click(getByTestId('shell-run-stop'));
    await waitFor(() => expect(setGeneralError).toHaveBeenCalledWith(
      'Failed to stop the command: not running', expect.any(Error),
    ));
    expect(getByTestId('shell-run-stop').textContent).toContain('Stop');
    expect(getShellRun('thread-1')?.stopping).toBe(false);
  });

  it('stays as an error when the output could not be sent, until dismissed', async () => {
    const { pane } = makePane();
    const { getByTestId, queryByTestId } = render(ShellRunCard, { props: { pane } });
    start();
    applyShellCommandEvent({
      phase: 'finished', threadId: 'thread-1', runId: 'run-1', state: 'failed',
      error: 'The command finished, but its output could not be sent: provider is gone',
    });
    await tick();
    expect(getByTestId('shell-run-card').getAttribute('data-state')).toBe('failed');
    expect(getByTestId('shell-run-error').textContent).toContain('provider is gone');
    expect(queryByTestId('shell-run-stop')).toBeNull();

    await fireEvent.click(getByTestId('shell-run-dismiss'));
    expect(queryByTestId('shell-run-card')).toBeNull();
  });
});
