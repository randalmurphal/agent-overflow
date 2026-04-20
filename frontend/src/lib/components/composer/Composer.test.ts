import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render } from '@testing-library/svelte';
import Composer from './Composer.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { buildPane, makeItem } from '../../../test/helpers/chat';
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
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'text:0:0',
        kind: 'assistant_text',
        status: 'streaming',
      }),
    ]);
    const draft = await buildDraft();
    const interrupt = setBindingMock('InterruptTurn', async () => {});

    const { getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });

    expect(queryByTestId('composer-send')).toBeNull();
    await fireEvent.click(getByTestId('composer-interrupt'));

    expect(interrupt).toHaveBeenCalledWith('thread-1');
  });

  it('does not send while a turn is active', async () => {
    const pane = await buildPane(undefined, [
      makeItem({
        id: 'tool-1',
        kind: 'tool_call',
        status: 'running',
        isBackground: false,
      }),
    ]);
    const draft = await buildDraft();
    const send = setBindingMock('SendMessage', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'should not send' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    expect(send).not.toHaveBeenCalled();
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
