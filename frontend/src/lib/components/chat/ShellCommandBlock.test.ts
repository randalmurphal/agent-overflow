import { beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { ThreadPane } from '../../stores/thread.svelte';
import { createThreadMessageUiState } from '../../stores/threadMessageUiState.svelte';
import type { ShellOutputMeta } from '../../utils/shellInvocation';
import ShellCommandBlock from './ShellCommandBlock.svelte';

function props(shell: Partial<ShellOutputMeta> = {}, size = 12) {
  return {
    itemId: 'item-1',
    threadId: 'thread-1',
    attachmentId: 'att-1',
    size,
    shell: { command: 'git status', exitCode: 0, durationMs: 1_240, preview: 'On branch main\n', ...shell },
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (err: unknown) => void;
  const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
  return { promise, resolve, reject };
}

describe('<ShellCommandBlock>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  it('shows the command, how it ended and the preview without reading anything', () => {
    const read = setBindingMock('ReadShellAttachmentOutput', async () => ({}));
    const { getByTestId, queryByTestId } = render(ShellCommandBlock, { props: props() });
    expect(getByTestId('shell-command-line').textContent).toBe('git status');
    expect(getByTestId('shell-command-outcome').textContent?.replace(/\s+/g, ' ').trim()).toBe('ok · 1.2s');
    expect(getByTestId('shell-command-outcome').className).not.toContain('text-error');
    expect(getByTestId('shell-command-output').querySelector('pre')?.textContent).toBe('On branch main\n');
    // The preview is the whole output, so there is nothing to open.
    expect(queryByTestId('shell-command-toggle')).toBeNull();
    expect(read).not.toHaveBeenCalled();
  });

  it('marks a failure, a timeout and a cut capture', () => {
    const failed = render(ShellCommandBlock, { props: props({ exitCode: 2 }) });
    expect(failed.getByTestId('shell-command-outcome').textContent).toContain('exit 2');
    expect(failed.getByTestId('shell-command-outcome').className).toContain('text-error');
    expect(failed.getByTestId('shell-command-block').getAttribute('data-exit-code')).toBe('2');
    failed.unmount();

    const timedOut = render(ShellCommandBlock, { props: props({ exitCode: 124, timedOut: true, truncated: true }) });
    expect(timedOut.getByTestId('shell-command-outcome').textContent).toContain('timed out');
    expect(timedOut.getByText('output cut in the middle')).toBeInTheDocument();
  });

  it('says so when the command printed nothing', () => {
    const { getByTestId, queryByTestId } = render(ShellCommandBlock, {
      props: props({ preview: undefined }, 0),
    });
    expect(getByTestId('shell-command-output').textContent?.trim()).toBe('No output');
    expect(queryByTestId('shell-command-toggle')).toBeNull();
  });

  it('reads the output in pages when opened, and returns to the preview when closed', async () => {
    const read = setBindingMock('ReadShellAttachmentOutput', async (_thread: string, _id: string, offset: number) =>
      offset === 0
        ? { command: 'git status', data: 'On branch main\nline 2\n', offset: 0, size: 30, next: 22 }
        : { command: 'git status', data: 'line 3\n', offset: 22, size: 30, next: 30 });
    const { getByTestId, queryByTestId } = render(ShellCommandBlock, {
      props: props({ more: true }, 30),
    });
    const toggle = getByTestId('shell-command-toggle');
    expect(toggle.textContent).toContain('Show all output (30 B)');
    expect(toggle.getAttribute('aria-expanded')).toBe('false');

    await fireEvent.click(toggle);
    await waitFor(() => expect(getByTestId('shell-command-output').textContent).toContain('line 2'));
    expect(read).toHaveBeenCalledWith('thread-1', 'att-1', 0, 0);
    expect(toggle.getAttribute('aria-expanded')).toBe('true');

    await fireEvent.click(getByTestId('shell-command-load-more'));
    await waitFor(() => expect(getByTestId('shell-command-output').textContent).toContain('line 3'));
    expect(read).toHaveBeenLastCalledWith('thread-1', 'att-1', 22, 0);
    expect(queryByTestId('shell-command-load-more')).toBeNull();

    await fireEvent.click(toggle);
    expect(getByTestId('shell-command-output').querySelector('pre')?.textContent).toBe('On branch main\n');
    // Reopening shows what was read without asking again.
    await fireEvent.click(toggle);
    expect(getByTestId('shell-command-output').textContent).toContain('line 3');
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('keeps the preview on screen while the first page loads', async () => {
    const pending = deferred<Record<string, unknown>>();
    setBindingMock('ReadShellAttachmentOutput', () => pending.promise);
    const { getByTestId } = render(ShellCommandBlock, { props: props({ more: true }, 30) });
    await fireEvent.click(getByTestId('shell-command-toggle'));
    expect(getByTestId('shell-command-output').textContent).toContain('On branch main');
    pending.resolve({ command: 'git status', data: 'On branch main\nrest\n', offset: 0, size: 20, next: 20 });
    await waitFor(() => expect(getByTestId('shell-command-output').textContent).toContain('rest'));
  });

  it('reports a failed read and retries it', async () => {
    let fail = true;
    const read = setBindingMock('ReadShellAttachmentOutput', async () => {
      if (fail) throw new Error('attachment is gone');
      return { command: 'git status', data: 'whole\n', offset: 0, size: 6, next: 6 };
    });
    const { getByTestId, getByRole, getByText } = render(ShellCommandBlock, {
      props: props({ more: true }, 30),
    });
    await fireEvent.click(getByTestId('shell-command-toggle'));
    await waitFor(() => expect(getByRole('alert').textContent).toContain('attachment is gone'));
    fail = false;
    await fireEvent.click(getByText('Retry'));
    await waitFor(() => expect(getByTestId('shell-command-output').textContent).toContain('whole'));
    expect(read).toHaveBeenCalledTimes(2);
  });

  it('shows a bounded command as cut until the whole one is read', async () => {
    setBindingMock('ReadShellAttachmentOutput', async () => ({
      command: 'echo the whole command', data: 'x\n', offset: 0, size: 2, next: 2,
    }));
    const { getByTestId } = render(ShellCommandBlock, {
      props: props({ command: 'echo the wh', commandTruncated: true, preview: 'x\n' }, 2),
    });
    expect(getByTestId('shell-command-line').textContent).toBe('echo the wh…');
    await fireEvent.click(getByTestId('shell-command-toggle'));
    await waitFor(() => expect(getByTestId('shell-command-line').textContent).toBe('echo the whole command'));
  });

  it('shows a whole short command when there is nothing to open', () => {
    const command = 'for f in a b c\ndo\n  echo $f\ndone\necho finished';
    const whole = render(ShellCommandBlock, { props: props({ command }) });
    // No toggle exists, so a clamp would hide lines for good.
    expect(whole.queryByTestId('shell-command-toggle')).toBeNull();
    expect(whole.getByTestId('shell-command-line').className).not.toContain('line-clamp-3');
    whole.unmount();

    const openable = render(ShellCommandBlock, { props: props({ command, more: true }, 30) });
    expect(openable.getByTestId('shell-command-line').className).toContain('line-clamp-3');
  });

  it('shows output as a terminal would leave it', () => {
    const preview = 'Building\rBuilding 50%\rBuilding 100%\x1b[K\n\x1b[2Kdone\x1b[0m\n';
    const { getByTestId } = render(ShellCommandBlock, { props: props({ preview }) });
    expect(getByTestId('shell-command-output').querySelector('pre')?.textContent).toBe('Building 100%\ndone\n');
  });

  it('is still open, and reads its output again, when it returns to the timeline', async () => {
    const read = setBindingMock('ReadShellAttachmentOutput', async () => ({
      command: 'git status', data: 'On branch main\nline 2\n', offset: 0, size: 22, next: 22,
    }));
    const ui = createThreadMessageUiState({ getItemById: () => undefined, loadedItems: () => [] });
    const pane = {
      isUserMessageExpanded: ui.isExpanded,
      setUserMessageExpanded: ui.setExpanded,
    } as unknown as ThreadPane;
    const first = render(ShellCommandBlock, { props: { ...props({ more: true }, 22), pane } });
    await fireEvent.click(first.getByTestId('shell-command-toggle'));
    await waitFor(() => expect(first.getByTestId('shell-command-output').textContent).toContain('line 2'));
    expect(ui.isExpanded('item-1', 'att-1')).toBe(true);
    // The message's own text is separate state.
    expect(ui.isExpanded('item-1')).toBe(false);
    first.unmount();

    // The virtual window dropped the row and mounted it again.
    const again = render(ShellCommandBlock, { props: { ...props({ more: true }, 22), pane } });
    expect(again.getByTestId('shell-command-toggle').getAttribute('aria-expanded')).toBe('true');
    await waitFor(() => expect(again.getByTestId('shell-command-output').textContent).toContain('line 2'));
    expect(read).toHaveBeenCalledTimes(2);

    await fireEvent.click(again.getByTestId('shell-command-toggle'));
    expect(ui.isExpanded('item-1', 'att-1')).toBe(false);
    expect(again.getByTestId('shell-command-output').querySelector('pre')?.textContent).toBe('On branch main\n');
  });
});
