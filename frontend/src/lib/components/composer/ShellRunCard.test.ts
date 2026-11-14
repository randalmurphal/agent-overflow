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

  it('is a named group, and hands focus to the composer when its button goes away', async () => {
    const host = document.createElement('div');
    host.setAttribute('data-pane-id', 'pane-1');
    const composer = document.createElement('textarea');
    composer.setAttribute('aria-label', 'Message Input');
    host.appendChild(composer);
    document.body.appendChild(host);
    try {
      const pane = { threadId: 'thread-1', paneId: 'pane-1', setGeneralError: vi.fn() } as unknown as ThreadPane;
      setBindingMock('StopThreadShellCommand', async () => undefined);
      const { getByTestId } = render(ShellRunCard, { target: host, props: { pane } });
      start();
      await tick();
      // A live region would announce every output line; a group is read once.
      expect(getByTestId('shell-run-card').getAttribute('role')).toBe('group');
      expect(getByTestId('shell-run-card').getAttribute('aria-label')).toBe('Running command');

      getByTestId('shell-run-stop').focus();
      await fireEvent.click(getByTestId('shell-run-stop'));
      expect(document.activeElement).toBe(composer);

      applyShellCommandEvent({ phase: 'finished', threadId: 'thread-1', runId: 'run-1', state: 'failed', error: 'boom' });
      await tick();
      expect(getByTestId('shell-run-card').getAttribute('aria-label')).toBe('Command failed');
      getByTestId('shell-run-dismiss').focus();
      await fireEvent.click(getByTestId('shell-run-dismiss'));
      expect(document.activeElement).toBe(composer);
    } finally {
      host.remove();
    }
  });

  it('stays as an error when the output was not sent, until dismissed', async () => {
    const { pane } = makePane();
    const { getByTestId, queryByTestId } = render(ShellRunCard, { props: { pane } });
    start();
    applyShellCommandEvent({
      phase: 'finished', threadId: 'thread-1', runId: 'run-1', state: 'failed',
      error: 'The command finished, but its output was not sent: provider is gone',
    });
    await tick();
    expect(getByTestId('shell-run-card').getAttribute('data-state')).toBe('failed');
    expect(getByTestId('shell-run-error').textContent).toContain('provider is gone');
    expect(queryByTestId('shell-run-stop')).toBeNull();

    await fireEvent.click(getByTestId('shell-run-dismiss'));
    expect(queryByTestId('shell-run-card')).toBeNull();
  });

  describe('a prompt on the command\'s terminal', () => {
    function ask(text: string, terminalSeq = 1, threadId = 'thread-1'): void {
      applyShellCommandEvent({ phase: 'terminal', threadId, runId: 'run-1', terminalSeq, terminal: text });
    }

    it('shows the prompt and a masked answer field while the terminal waits on an open line', async () => {
      const { pane } = makePane();
      const { getByTestId, queryByTestId } = render(ShellRunCard, { props: { pane } });
      start('sudo apt update');
      await tick();
      expect(queryByTestId('shell-run-prompt')).toBeNull();
      expect(queryByTestId('shell-run-answer')).toBeNull();

      ask('[sudo] password for me: ');
      await tick();
      expect(getByTestId('shell-run-prompt').textContent).toContain('The command is asking:');
      expect(getByTestId('shell-run-prompt').textContent).toContain('[sudo] password for me:');
      const field = getByTestId('shell-run-answer') as HTMLInputElement;
      expect(field.type).toBe('password');
      expect(field.getAttribute('aria-label')).toBe("Answer the command's prompt");
      for (const [name, value] of [
        ['autocomplete', 'off'], ['autocapitalize', 'off'], ['autocorrect', 'off'], ['spellcheck', 'false'],
        ['data-1p-ignore', ''], ['data-lpignore', 'true'], ['data-bwignore', ''],
      ]) {
        expect(field.getAttribute(name), name).toBe(value);
      }

      // Answered: the terminal ended the line and the command runs on.
      ask('[sudo] password for me: \n', 2);
      await tick();
      expect(queryByTestId('shell-run-prompt')).toBeNull();
      expect(queryByTestId('shell-run-answer')).toBeNull();
      expect(getByTestId('shell-run-command').classList.contains('truncate')).toBe(true);

      // It asks again.
      ask('[sudo] password for me: \nSorry, try again.\n[sudo] password for me: ', 3);
      await tick();
      expect(getByTestId('shell-run-prompt').textContent).toContain('Sorry, try again.');
      expect((getByTestId('shell-run-answer') as HTMLInputElement).type).toBe('password');
    });

    it('shows only the last lines of the terminal', async () => {
      const { pane } = makePane();
      const { getByTestId } = render(ShellRunCard, { props: { pane } });
      start();
      ask('first\nsecond\nthird\nfourth: ');
      await tick();
      const shown = getByTestId('shell-run-prompt').textContent ?? '';
      expect(shown).not.toContain('first');
      expect(shown).toContain('second');
      expect(shown).toContain('fourth:');
    });

    it('wraps the command while it asks and truncates it otherwise', async () => {
      const { pane } = makePane();
      const { getByTestId } = render(ShellRunCard, { props: { pane } });
      start('ssh build-host.example.com make deploy');
      await tick();
      expect(getByTestId('shell-run-command').classList.contains('truncate')).toBe(true);
      ask('password: ');
      await tick();
      const command = getByTestId('shell-run-command');
      expect(command.classList.contains('truncate')).toBe(false);
      expect(command.classList.contains('whitespace-pre-wrap')).toBe(true);
    });

    it('shows no field for a failed run', async () => {
      const { pane } = makePane();
      const { getByTestId, queryByTestId } = render(ShellRunCard, { props: { pane } });
      start();
      ask('Password: ');
      applyShellCommandEvent({ phase: 'finished', threadId: 'thread-1', runId: 'run-1', state: 'failed', error: 'boom' });
      await tick();
      expect(getByTestId('shell-run-card').getAttribute('data-state')).toBe('failed');
      expect(queryByTestId('shell-run-prompt')).toBeNull();
      expect(queryByTestId('shell-run-answer')).toBeNull();
    });

    it('sends the answer on Enter and on Send, emptying the field before the call settles', async () => {
      const { pane } = makePane();
      let settle!: () => void;
      const answer = setBindingMock('AnswerThreadShellCommand', () => new Promise<void>((resolve) => { settle = resolve; }));
      const { getByTestId } = render(ShellRunCard, { props: { pane } });
      start();
      ask('Password: ');
      await tick();
      const field = getByTestId('shell-run-answer') as HTMLInputElement;

      await fireEvent.input(field, { target: { value: 'hunter2' } });
      await fireEvent.keyDown(field, { key: 'Enter' });
      expect(answer).toHaveBeenCalledTimes(1);
      expect(answer).toHaveBeenCalledWith('thread-1', { runId: 'run-1', text: 'hunter2' });
      expect(field.value).toBe('');
      settle();

      // A bare answer is "press Enter to continue".
      await fireEvent.click(getByTestId('shell-run-answer-send'));
      expect(answer).toHaveBeenCalledTimes(2);
      expect(answer).toHaveBeenLastCalledWith('thread-1', { runId: 'run-1', text: '' });
      settle();

      // Other keys type; they do not send.
      await fireEvent.keyDown(field, { key: 'a' });
      expect(answer).toHaveBeenCalledTimes(2);
    });

    it('shows the answer on request and masks it again after it is sent', async () => {
      const { pane } = makePane();
      setBindingMock('AnswerThreadShellCommand', async () => undefined);
      const { getByTestId } = render(ShellRunCard, { props: { pane } });
      start();
      ask('Password: ');
      await tick();
      const field = getByTestId('shell-run-answer') as HTMLInputElement;
      const reveal = getByTestId('shell-run-answer-reveal');
      expect(reveal.getAttribute('aria-pressed')).toBe('false');

      await fireEvent.click(reveal);
      expect(field.type).toBe('text');
      expect(reveal.getAttribute('aria-pressed')).toBe('true');
      expect(reveal.getAttribute('aria-label')).toBe('Hide the answer');

      await fireEvent.input(field, { target: { value: 'yes' } });
      await fireEvent.keyDown(field, { key: 'Enter' });
      await tick();
      expect(field.type).toBe('password');
      expect(reveal.getAttribute('aria-pressed')).toBe('false');
    });

    it('reports a failed answer without the answer, and does not put it back', async () => {
      const { pane, setGeneralError } = makePane();
      setBindingMock('AnswerThreadShellCommand', async () => { throw new Error('shell command is not running'); });
      const { getByTestId } = render(ShellRunCard, { props: { pane } });
      start();
      ask('Password: ');
      await tick();
      const field = getByTestId('shell-run-answer') as HTMLInputElement;
      await fireEvent.input(field, { target: { value: 'hunter2' } });
      await fireEvent.click(getByTestId('shell-run-answer-send'));
      await waitFor(() => expect(setGeneralError).toHaveBeenCalledTimes(1));
      const [message, err] = setGeneralError.mock.calls[0];
      expect(message).toBe('Failed to send the answer: shell command is not running');
      expect(message).not.toContain('hunter2');
      expect(err).toBeInstanceOf(Error);
      expect(field.value).toBe('');
    });
  });
});
