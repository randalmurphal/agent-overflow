import { beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { tick } from 'svelte';
import Composer from './Composer.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import { buildPane, makeThread as makeTestThread } from '../../../test/helpers/chat';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import type { Attachment } from '../../types/attachment';
import {
  hasRuntimeModeDraft,
  resetRuntimeModeDraftsForTest,
  setRuntimeModeDraft,
} from '../../stores/runtimeModeDraft.svelte';
import {
  resetForTest as resetWorktreeIntent,
  setThreadEnvMode,
  setWorktreeBaseBranch,
  setWorktreeBranchName,
} from '../../stores/worktreeIntent.svelte';

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
  setBindingMock('GetAttachmentData', async () => 'iVBORw0KGgo=');
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

function makeAttachment(id: string, filename = `${id}.png`): Attachment {
  return {
    id,
    threadId: 'thread-1',
    filename,
    mimeType: 'image/png',
    size: 128,
    relativePath: `thread-1/${id}.png`,
    createdAt: 1,
  };
}

function makeClipboardPaste(files: File[]): ClipboardEvent {
  const event = new Event('paste', { bubbles: true, cancelable: true }) as ClipboardEvent;
  Object.defineProperty(event, 'clipboardData', {
    value: {
      items: files.map((file) => ({
        kind: 'file',
        type: file.type,
        getAsFile: () => file,
      })),
    },
  });
  return event;
}

describe('<Composer>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRuntimeModeDraftsForTest();
    resetWorktreeIntent();
    installDraftMocks();
    setBindingMock('SendMessageWithOptions', async () => makeTestThread({ runtimeMode: 'full-access' }));
    setBindingMock('InterruptTurn', async () => {});
    setBindingMock('DeleteAttachment', async () => {});
    setBindingMock('UploadAttachment', async (
      _threadId: string,
      filename: string,
      _mimeType: string,
    ) => makeAttachment(`att-${filename}`, filename));
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
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'hello world' } });
    await fireEvent.click(getByTestId('composer-send'));

    expect(send).toHaveBeenCalledWith('thread-1', 'hello world', {
      attachmentIds: [],
    });
    expect(draft.content).toBe('');
  });

  it('sends a staged runtime mode and clears the staged value on success', async () => {
    const pane = await buildPane(makeTestThread({ runtimeMode: 'approval-required' }));
    const draft = await buildDraft();
    setRuntimeModeDraft('thread-1', 'auto-accept-edits');
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'auto-accept-edits' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message input'), { target: { value: 'use this access' } });
    await fireEvent.click(getByTestId('composer-send'));

    expect(send).toHaveBeenCalledWith('thread-1', 'use this access', {
      attachmentIds: [],
      runtimeMode: 'auto-accept-edits',
    });
    expect(hasRuntimeModeDraft(pane.thread)).toBe(false);
  });

  it('does not synthesize a runtime override from a missing thread value', async () => {
    const pane = await buildPane(makeTestThread({ runtimeMode: undefined }));
    const draft = await buildDraft();
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'approval-required' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message input'), { target: { value: 'use persisted mode' } });
    await fireEvent.click(getByTestId('composer-send'));

    expect(send).toHaveBeenCalledWith('thread-1', 'use persisted mode', {
      attachmentIds: [],
    });
  });

  it('prepares a pending worktree before sending', async () => {
    const initialThread = makeTestThread({ branch: 'main' });
    const worktreeThread = makeTestThread({
      branch: 'feature/custom',
      workspacePath: '/tmp/wt-feature',
      worktreePath: '/tmp/wt-feature',
    });
    const pane = await buildPane(initialThread);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');
    setWorktreeBaseBranch(pane.thread, 'release');
    setWorktreeBranchName(pane.thread, 'feature/custom');

    const prepare = setBindingMock('PrepareThreadWorktree', async () => worktreeThread);
    const send = setBindingMock('SendMessageWithOptions', async () => worktreeThread);

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message input'), { target: { value: 'work there' } });
    await fireEvent.click(getByTestId('composer-send'));

    expect(prepare).toHaveBeenCalledWith('thread-1', 'release', 'feature/custom');
    expect(send).toHaveBeenCalledWith('thread-1', 'work there', {
      attachmentIds: [],
    });
    expect(prepare.mock.invocationCallOrder[0]).toBeLessThan(send.mock.invocationCallOrder[0]);
    expect(pane.thread?.worktreePath).toBe('/tmp/wt-feature');
  });

  it('shows worktree preparation status while creating the worktree', async () => {
    const initialThread = makeTestThread({ branch: 'main' });
    const worktreeThread = makeTestThread({
      branch: 'feature/custom',
      workspacePath: '/tmp/wt-feature',
      worktreePath: '/tmp/wt-feature',
    });
    const pane = await buildPane(initialThread);
    const draft = await buildDraft();
    if (!pane.thread) throw new Error('missing test thread');
    setThreadEnvMode(pane.thread, 'new-worktree');

    let finishPrepare!: () => void;
    setBindingMock('PrepareThreadWorktree', async () => {
      await new Promise<void>((resolve) => {
        finishPrepare = resolve;
      });
      return worktreeThread;
    });
    setBindingMock('SendMessageWithOptions', async () => worktreeThread);

    const { getByLabelText, getByTestId, queryByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message input'), { target: { value: 'work there' } });
    void fireEvent.click(getByTestId('composer-send'));

    await waitFor(() => {
      expect(getByTestId('composer-worktree-preparing').textContent).toContain('Preparing worktree...');
    });

    finishPrepare();

    await waitFor(() => {
      expect(queryByTestId('composer-worktree-preparing')).toBeNull();
    });
  });

  it('keeps the send bound to the original thread if the pane switches while clearing the draft', async () => {
    const threadOne = makeTestThread({ id: 'thread-1', runtimeMode: 'approval-required' });
    const threadTwo = makeTestThread({ id: 'thread-2', runtimeMode: 'full-access' });
    const pane = await buildPane(threadOne);
    const draft = await buildDraft('thread-1');
    setRuntimeModeDraft('thread-1', 'auto-accept-edits');

    let releaseClear!: () => void;
    const clearStarted = vi.fn();
    setBindingMock('ClearDraft', async () => {
      clearStarted();
      await new Promise<void>((resolve) => {
        releaseClear = resolve;
      });
    });
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ id: 'thread-1', runtimeMode: 'auto-accept-edits' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.input(getByLabelText('Message input'), { target: { value: 'race send' } });
    void fireEvent.click(getByTestId('composer-send'));
    await waitFor(() => expect(clearStarted).toHaveBeenCalled());

    await pane.switchThread(threadTwo);
    releaseClear();

    await waitFor(() => {
      expect(send).toHaveBeenCalledWith('thread-1', 'race send', {
        attachmentIds: [],
        runtimeMode: 'auto-accept-edits',
      });
    });
    expect(pane.thread?.id).toBe('thread-2');
  });

  it('sends image-only drafts with a visible image placeholder and attachment ids', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments('[Image #1]', [makeAttachment('att-1', 'hero.png')]);
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.click(getByTestId('composer-send'));

    expect(send).toHaveBeenCalledWith('thread-1', '[Image #1]', {
      attachmentIds: ['att-1'],
    });
  });

  it('pasting images inserts image placeholders at the cursor and sends ordered attachment ids', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    let nextId = 1;
    const upload = setBindingMock('UploadAttachment', async (
      threadId: string,
      filename: string,
      mimeType: string,
    ) => ({
      ...makeAttachment(`att-${nextId++}`, filename),
      threadId,
      mimeType,
    }));
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'please inspect' } });
    textarea.setSelectionRange(textarea.value.length, textarea.value.length);
    await fireEvent(textarea, makeClipboardPaste([
      new File(['png-one'], 'one.png', { type: 'image/png' }),
      new File(['png-two'], 'two.png', { type: 'image/png' }),
    ]));

    await waitFor(() => expect(upload).toHaveBeenCalledTimes(2));
    expect(draft.content).toBe('please inspect [Image #1] [Image #2]');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['att-1', 'att-2']);

    await fireEvent.click(getByTestId('composer-send'));
    expect(send).toHaveBeenCalledWith('thread-1', 'please inspect [Image #1] [Image #2]', {
      attachmentIds: ['att-1', 'att-2'],
    });
  });

  it('backspace after an image placeholder removes the whole placeholder and attachment', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments('before [Image #1] after', [makeAttachment('att-1', 'hero.png')]);
    const remove = setBindingMock('DeleteAttachment', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    const cursor = 'before [Image #1]'.length;
    textarea.setSelectionRange(cursor, cursor);

    await fireEvent.keyDown(textarea, { key: 'Backspace' });

    expect(draft.content).toBe('before after');
    expect(draft.attachments).toHaveLength(0);
    await waitFor(() => expect(remove).toHaveBeenCalledWith('att-1'));
  });

  it('delete before or inside an image placeholder removes the whole placeholder and attachment', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments('before [Image #1] after', [makeAttachment('att-1', 'hero.png')]);
    const remove = setBindingMock('DeleteAttachment', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    const cursor = 'before [Ima'.length;
    textarea.setSelectionRange(cursor, cursor);

    await fireEvent.keyDown(textarea, { key: 'Delete' });

    expect(draft.content).toBe('before after');
    expect(draft.attachments).toHaveLength(0);
    await waitFor(() => expect(remove).toHaveBeenCalledWith('att-1'));
  });

  it('removing an attachment with the thumbnail X removes its placeholder and renumbers later images', async () => {
    const pane = await buildPane();
    const draft = await buildDraft();
    draft.setContentAndAttachments(
      '[Image #1] [Image #2] [Image #3]',
      [
        makeAttachment('att-1', 'first.png'),
        makeAttachment('att-2', 'second.png'),
        makeAttachment('att-3', 'third.png'),
      ],
    );
    const remove = setBindingMock('DeleteAttachment', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    await fireEvent.click(getByLabelText('Remove second.png'));

    expect(draft.content).toBe('[Image #1] [Image #2]');
    expect(draft.attachments.map((attachment) => attachment.id)).toEqual(['att-1', 'att-3']);
    await waitFor(() => expect(remove).toHaveBeenCalledWith('att-2'));
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
    const send = setBindingMock('SendMessageWithOptions', async () =>
      makeTestThread({ runtimeMode: 'full-access' }));

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
    setBindingMock('SendMessageWithOptions', async () => {
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

  it('keeps a pending user-input answer separate from the normal draft', async () => {
    const pane = await buildPane();
    pane.addUserInput({
      requestId: 'req-input',
      threadId: 'thread-1',
      toolName: 'AskUserQuestion',
      title: 'Input requested',
      questions: [{ id: 'name', header: 'Name', question: 'Name?' }],
    });
    const draft = await buildDraft();
    draft.setContent('normal prompt stays here');
    const respond = setBindingMock('RespondToUserInput', async () => {});

    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    expect(textarea.value).toBe('');
    await fireEvent.input(textarea, { target: { value: 'Randy' } });
    await tick();
    await fireEvent.click(getByTestId('user-input-submit'));

    expect(draft.content).toBe('normal prompt stays here');
    expect(respond).toHaveBeenCalledTimes(1);
    expect(respond.mock.calls[0]).toMatchObject([
      'thread-1',
      {
        requestId: 'req-input',
        decision: 'accept',
        answers: { name: 'Randy' },
      },
    ]);
  });
});
