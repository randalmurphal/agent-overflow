import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import { tick } from 'svelte';
import Composer from './Composer.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { buildPane } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

function installDraftMocks() {
  setBindingMock('GetDraft', async (threadId: string) => ({
    threadId,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('ListLiveBackgroundTasks', async () => []);
  setBindingMock('GetThreadSlashCommands', async () => []);
  setBindingMock('SearchWorkspaceFiles', async () => ({
    files: [],
    truncated: false,
    root: '/tmp/workspace',
  }));
}

async function buildDraft(threadId: string | null = 'thread-1') {
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread(threadId);
  return draft;
}

describe('<Composer>', () => {
  beforeEach(() => {
    resetBindingMocks();
    installDraftMocks();
    setBindingMock('SendMessage', async () => {});
    setBindingMock('InterruptTurn', async () => {});
  });

  it('disables input when no thread is selected', async () => {
    const pane = createThreadPane();
    const draft = await buildDraft(null);

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });

    expect((getByLabelText('Message input') as HTMLTextAreaElement).disabled).toBe(true);
    expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(true);
  });

  it('sends the draft and clears it on success', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    const send = setBindingMock('SendMessage', async () => {});

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'hello world' } });
    await fireEvent.click(getByTestId('composer-send'));

    expect(send).toHaveBeenCalledWith('thread-1', 'hello world');
    expect(draft.content).toBe('');
  });

  it('shows the interrupt affordance while a turn is active and interrupts on click', async () => {
    const pane = await buildPane();
    // Post-refactor, isTurnActive is wire-pushed — a streaming item no
    // longer flips it on. Driving setActiveTurn directly simulates the
    // `provider:turn_started` event the composer really depends on.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    const draft = await buildDraft();
    const interrupt = setBindingMock('InterruptTurn', async () => {});

    const { getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });

    expect(queryByTestId('composer-send')).toBeNull();
    await fireEvent.click(getByTestId('composer-interrupt'));

    expect(interrupt).toHaveBeenCalledWith('thread-1');
  });

  it('renders the background tray inside the composer card before the input', async () => {
    const pane = await buildPane();
    setBindingMock('ListLiveBackgroundTasks', async () => [{
      id: 'launch-a',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      status: 'running',
      summary: 'Bash',
      isBackground: true,
      createdAt: Date.now() - 1_000,
      updatedAt: Date.now() - 1_000,
    }]);
    const draft = await buildDraft();

    const { getByTestId, getByLabelText } = render(Composer, { props: { pane, draft } });
    await tick();
    await tick();

    const root = getByTestId('composer-root');
    const tray = getByTestId('background-task-tray');
    const input = getByLabelText('Message input');

    expect(root.contains(tray)).toBe(true);
    expect(tray.compareDocumentPosition(input) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('does not send while a turn is active', async () => {
    const pane = await buildPane();
    // See note above — drive the active-turn state via the wire-push API
    // rather than relying on the removed "streaming item = active turn"
    // derivation.
    pane.setActiveTurn({ turnId: 't1', turnIndex: 0, startedAt: 0 });
    const draft = await buildDraft();
    const send = setBindingMock('SendMessage', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'should not send' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    expect(send).not.toHaveBeenCalled();
  });

  it('autosizes multiline input and clamps at the maximum composer height', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    Object.defineProperty(textarea, 'scrollHeight', {
      configurable: true,
      get: () => 260,
    });

    await fireEvent.input(textarea, { target: { value: 'one\ntwo\nthree\nfour' } });

    expect(textarea.style.height).toBe('200px');
  });

  it('autosizes multiline input below the maximum composer height', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    Object.defineProperty(textarea, 'scrollHeight', {
      configurable: true,
      get: () => 96,
    });

    await fireEvent.input(textarea, { target: { value: 'one\ntwo' } });

    expect(textarea.style.height).toBe('96px');
  });

  it('restores the draft and surfaces an error when send fails', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    setBindingMock('SendMessage', async () => {
      throw new Error('rpc down');
    });
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'fails' } });
    await fireEvent.click(getByTestId('composer-send'));
    await new Promise((resolve) => setTimeout(resolve, 10));

    expect(draft.content).toBe('fails');
    expect(pane.generalError).toMatch(/Failed to send message/);
    consoleError.mockRestore();
  });
});
