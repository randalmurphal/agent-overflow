import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Composer from './Composer.svelte';
import { createComposerDraftStore } from '../../stores/composerDraft.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
import type { Attachment } from '../../types/attachment';
import { setBindingMock, getBindingMock } from '../../../test/mocks/bindings-app';

function seedThread(): Thread {
  return {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp',
    projectPath: '/tmp',
    interactionMode: 'default',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  };
}

async function makeReadyPane() {
  setBindingMock('SwitchThread', async () => {});
  setBindingMock('ListItems', async () => []);
  setBindingMock('ListPayloadMetas', async () => []);
  const pane = createThreadPane();
  await pane.switchThread(seedThread());
  return pane;
}

function mockDraftBindings() {
  setBindingMock('GetDraft', async (id: string) => ({
    threadId: id,
    content: '',
    attachmentIds: [],
    terminalChips: [],
    updatedAt: 0,
  }));
  setBindingMock('SaveDraft', async () => {});
  setBindingMock('ClearDraft', async () => {});
  setBindingMock('ListAttachments', async () => []);
  setBindingMock('UploadAttachment', async () => ({
    id: 'uploaded-1',
    threadId: 'thread-1',
    filename: 'pic.png',
    mimeType: 'image/png',
    size: 100,
    relativePath: 'thread-1/uploaded-1.png',
    createdAt: 1,
  }));
  setBindingMock('DeleteAttachment', async () => {});
  setBindingMock('SearchWorkspaceFiles', async () => ({
    files: [],
    truncated: false,
    root: '/tmp',
  }));
  // Default: no slash commands available. Individual tests override this to
  // surface a populated list.
  setBindingMock('GetThreadSlashCommands', async () => []);
}

async function makeDraftStore(threadId: string | null = 'thread-1') {
  // debounceMs: 0 so save flushes immediately in tests.
  const draft = createComposerDraftStore({ debounceMs: 0 });
  await draft.setThread(threadId);
  return draft;
}

function makeDataTransfer(files: File[]) {
  // happy-dom's DataTransfer exposes `files`/`items` as getters, and
  // @testing-library/dom replaces our mock properties onto a fresh DataTransfer
  // via defineProperty. Populating the real items list makes the getter
  // return the files we attached.
  const dt = new DataTransfer();
  for (const file of files) {
    dt.items.add(file);
  }
  return dt;
}

function makeClipboardData(files: File[]) {
  // ClipboardEvent.clipboardData is a DataTransfer in the real DOM too, so
  // the same construction works.
  return makeDataTransfer(files);
}

function flushMicrotasks(iterations = 5) {
  return (async () => {
    for (let i = 0; i < iterations; i++) {
      await Promise.resolve();
    }
  })();
}

// FileReader.readAsDataURL resolves on a macrotask in happy-dom; a microtask
// flush is not enough. Use `flushTimers` to yield long enough for the reader
// callbacks to fire and any subsequent async work (UploadAttachment, state
// updates) to settle.
function flushTimers(ms = 20) {
  return new Promise((r) => setTimeout(r, ms));
}

describe('<Composer>', () => {
  beforeEach(() => {
    setBindingMock('SendMessage', async () => {});
    setBindingMock('InterruptTurn', async () => {});
    mockDraftBindings();
  });

  it('disables the textarea when no thread is selected', async () => {
    const pane = createThreadPane();
    const draft = await makeDraftStore(null);
    const { getByLabelText, getByRole } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(true);
    expect(textarea.placeholder).toMatch(/Select or create a thread/);
    const send = getByRole('button', { name: /send/i }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
  });

  it('renders Send when session is idle', async () => {
    const pane = await makeReadyPane();
    pane.setSessionStatus('ready');
    const draft = await makeDraftStore();
    const { getByRole, queryByTestId } = render(Composer, { props: { pane, draft } });
    expect(getByRole('button', { name: /send/i })).toBeInTheDocument();
    // Interrupt affordance is hidden when the turn isn't active.
    expect(queryByTestId('composer-interrupt')).toBeNull();
    expect(queryByTestId('composer-turn-banner')).toBeNull();
  });

  it('renders Interrupt banner alongside disabled Send while a turn streams', async () => {
    const pane = await makeReadyPane();
    pane.setSessionStatus('running');
    pane.appendTextDelta('partial');
    const draft = await makeDraftStore();
    const { getByRole, getByTestId } = render(Composer, { props: { pane, draft } });
    const send = getByRole('button', { name: /send/i }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    expect(getByTestId('composer-interrupt')).toBeInTheDocument();
    expect(getByTestId('composer-turn-banner')).toBeInTheDocument();
  });

  it('send button stays disabled until there is content', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const { getByLabelText, getByRole } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    const send = getByRole('button', { name: /send/i }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    await fireEvent.input(textarea, { target: { value: '   ' } });
    expect(send.disabled).toBe(true);
    await fireEvent.input(textarea, { target: { value: 'hello' } });
    expect(send.disabled).toBe(false);
  });

  it('sends message via RPC and clears the textarea optimistically', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const sendMock = setBindingMock('SendMessage', async () => {});
    const { getByLabelText, getByRole } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'hello world' } });
    await fireEvent.click(getByRole('button', { name: /send/i }));

    expect(sendMock).toHaveBeenCalledWith('thread-1', 'hello world');
    expect(pane.pendingMessage).toBe('hello world');
    expect(draft.content).toBe('');
  });

  it('Enter without shift triggers send; Shift+Enter does not', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const sendMock = setBindingMock('SendMessage', async () => {});
    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'line 1' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true });
    expect(sendMock).not.toHaveBeenCalled();

    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    expect(sendMock).toHaveBeenCalledWith('thread-1', 'line 1');
  });

  it('rolls back optimistic state and surfaces error when SendMessage rejects', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('SendMessage', async () => {
      throw new Error('rpc down');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByLabelText, getByRole } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'fails' } });
    await fireEvent.click(getByRole('button', { name: /send/i }));
    // `send()` awaits clearAfterSend then SendMessage then restoreDraftFor —
    // give microtasks several ticks to settle.
    await new Promise((r) => setTimeout(r, 10));

    expect(pane.pendingMessage).toBeNull();
    expect(pane.error).toMatch(/Failed to send message/);
    expect(draft.content).toBe('fails');
    consoleErr.mockRestore();
  });

  // --- Bug D3 regression ---
  it('late SendMessage failure after thread switch does NOT clobber new thread draft', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore('thread-1');
    // Per-thread GetDraft so setThread('thread-B') doesn't bring back A's
    // stale snapshot.
    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: id === 'thread-B' ? 'B draft' : '',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 0,
    }));

    // SendMessage stays pending until we release it so we can switch threads
    // in between.
    let rejectSend!: (err: Error) => void;
    const sendP = new Promise<void>((_r, rej) => { rejectSend = rej; });
    sendP.catch(() => {});
    setBindingMock('SendMessage', () => sendP);

    // Record every SaveDraft invocation so we can verify which thread each
    // save targets. thread-1 is restored; thread-B must be left alone.
    const saveMock = setBindingMock('SaveDraft', async () => {});
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByLabelText, getByRole } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'A message' } });
    // Clear prior SaveDraft calls triggered by input debounce so we only
    // see the restore path below.
    saveMock.mockClear();

    // Kick off send; SendMessage is pending.
    await fireEvent.click(getByRole('button', { name: /send/i }));
    await Promise.resolve();
    // Switch the draft store to thread-B mid-flight.
    await draft.setThread('thread-B');
    expect(draft.threadId).toBe('thread-B');
    // B should have hydrated its own backend draft content.
    expect(draft.content).toBe('B draft');

    // Now the send fails; give the rejection chain time to run
    // restoreDraftFor (which awaits SaveDraft) and the toast/error branch.
    rejectSend(new Error('rpc down'));
    await new Promise((r) => setTimeout(r, 10));

    // B's draft must not be overwritten with A's failed message.
    expect(draft.threadId).toBe('thread-B');
    expect(draft.content).toBe('B draft');

    // thread-1's SaveDraft (the sender) should have been called with the
    // snapshot so returning to it shows the preserved message.
    const savesForSender = saveMock.mock.calls.filter((c) => c[0] === 'thread-1');
    const lastSaveForSender = savesForSender[savesForSender.length - 1];
    expect(lastSaveForSender).toBeDefined();
    expect(lastSaveForSender![1]).toBe('A message');

    // No SaveDraft for thread-B containing thread-1's message.
    const bWroteAContent = saveMock.mock.calls.some(
      (c) => c[0] === 'thread-B' && c[1] === 'A message',
    );
    expect(bWroteAContent).toBe(false);

    consoleErr.mockRestore();
  });

  it('send failure while still on the original thread restores local draft', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore('thread-1');
    setBindingMock('SendMessage', async () => {
      throw new Error('rpc down');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByLabelText, getByRole } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'retry me' } });
    await fireEvent.click(getByRole('button', { name: /send/i }));
    // Let restoreDraftFor settle.
    await new Promise((r) => setTimeout(r, 10));

    expect(draft.threadId).toBe('thread-1');
    expect(draft.content).toBe('retry me');
    expect(pane.error).toMatch(/Failed to send message/);
    consoleErr.mockRestore();
  });

  it('Interrupt button calls InterruptTurn with the current threadId', async () => {
    const pane = await makeReadyPane();
    pane.setSessionStatus('running');
    pane.appendTextDelta('mid-stream');
    const draft = await makeDraftStore();
    const interruptMock = setBindingMock('InterruptTurn', async () => {});

    const { getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.click(getByTestId('composer-interrupt'));

    expect(interruptMock).toHaveBeenCalledWith('thread-1');
  });

  it('does not call SendMessage for a whitespace-only message', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const sendMock = setBindingMock('SendMessage', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '   ' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    expect(sendMock).not.toHaveBeenCalled();
    expect(pane.pendingMessage).toBeNull();
    expect(getBindingMock('SendMessage')!.mock.calls.length).toBe(0);
  });

  it('drops on composer trigger UploadAttachment and adds chip', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const uploadMock = setBindingMock('UploadAttachment', async () => ({
      id: 'att-new',
      threadId: 'thread-1',
      filename: 'drop.png',
      mimeType: 'image/png',
      size: 3,
      relativePath: 'thread-1/att-new.png',
      createdAt: 2,
    }));
    const { getByTestId, findByText } = render(Composer, { props: { pane, draft } });
    const root = getByTestId('composer-root') as HTMLElement;

    const file = new File(['abc'], 'drop.png', { type: 'image/png' });
    const dataTransfer = makeDataTransfer([file]);
    await fireEvent.drop(root, { dataTransfer });

    await flushTimers();

    expect(uploadMock).toHaveBeenCalled();
    const args = uploadMock.mock.calls[0];
    expect(args[0]).toBe('thread-1');
    expect(args[1]).toBe('drop.png');
    expect(args[2]).toBe('image/png');
    await findByText('drop.png');
    expect(draft.attachments.some((a) => a.id === 'att-new')).toBe(true);
  });

  it('pasted image triggers UploadAttachment', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const uploadMock = setBindingMock('UploadAttachment', async () => ({
      id: 'att-pasted',
      threadId: 'thread-1',
      filename: 'image.png',
      mimeType: 'image/png',
      size: 5,
      relativePath: 'thread-1/att-pasted.png',
      createdAt: 3,
    }));
    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    const file = new File(['xyz'], 'image.png', { type: 'image/png' });
    const clipboardData = makeClipboardData([file]);
    await fireEvent.paste(textarea, { clipboardData });
    await flushTimers();

    expect(uploadMock).toHaveBeenCalled();
    expect(draft.attachments.some((a) => a.id === 'att-pasted')).toBe(true);
  });

  it('clicking the remove button removes the attachment and calls DeleteAttachment', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const record: Attachment = {
      id: 'att-remove',
      threadId: 'thread-1',
      filename: 'old.png',
      mimeType: 'image/png',
      size: 10,
      relativePath: 'thread-1/att-remove.png',
      createdAt: 1,
    };
    draft.addAttachment(record);

    const deleteMock = setBindingMock('DeleteAttachment', async () => {});
    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const remove = getByLabelText('Remove old.png');
    await fireEvent.click(remove);
    await Promise.resolve();

    expect(draft.attachments.find((a) => a.id === 'att-remove')).toBeUndefined();
    expect(deleteMock).toHaveBeenCalledWith('att-remove');
  });

  it('@mention opens popover and filters results', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    const searchMock = setBindingMock('SearchWorkspaceFiles', async (_id, query) => {
      if (query === 'hello') {
        return {
          files: [
            { path: 'hello.ts', kind: 'file', parentPath: '' },
            { path: 'src/hello.go', kind: 'file', parentPath: 'src' },
          ],
          truncated: false,
          root: '/tmp',
        };
      }
      return { files: [], truncated: false, root: '/tmp' };
    });
    const { getByLabelText, findByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '@hello' } });
    await findByTestId('mention-popover');
    await Promise.resolve();
    await Promise.resolve();

    expect(searchMock).toHaveBeenCalled();
  });

  it('selecting a mention inserts the file path and closes the popover', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('SearchWorkspaceFiles', async () => ({
      files: [{ path: 'src/main.ts', kind: 'file', parentPath: 'src' }],
      truncated: false,
      root: '/tmp',
    }));
    const { getByLabelText, findAllByTestId, queryByTestId } = render(Composer, {
      props: { pane, draft },
    });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '@main' } });
    const options = await findAllByTestId('mention-option');
    await fireEvent.click(options[0]);

    expect(draft.content).toContain('@src/main.ts');
    expect(queryByTestId('mention-popover')).toBeNull();
  });

  it('Escape closes the mention popover', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('SearchWorkspaceFiles', async () => ({
      files: [{ path: 'x.ts', kind: 'file', parentPath: '' }],
      truncated: false,
      root: '/tmp',
    }));
    const { getByLabelText, findByTestId, queryByTestId } = render(Composer, {
      props: { pane, draft },
    });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: '@x' } });
    await findByTestId('mention-popover');
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    expect(queryByTestId('mention-popover')).toBeNull();
  });

  it('terminal chips render and are inlined in the outgoing message', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    draft.addTerminalChip({
      id: 'chip-1',
      label: 'terminal',
      preview: '$ ls',
      content: '$ ls\nREADME.md',
      createdAt: 10,
    });

    const sendMock = setBindingMock('SendMessage', async () => {});
    const { getByLabelText, getByRole, getByTestId } = render(Composer, {
      props: { pane, draft },
    });
    expect(getByTestId('terminal-chip')).toBeInTheDocument();

    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'please look at:' } });
    await fireEvent.click(getByRole('button', { name: /send/i }));

    const args = sendMock.mock.calls[0];
    expect(args[0]).toBe('thread-1');
    expect(args[1]).toContain('please look at:');
    expect(args[1]).toContain('```terminal');
    expect(args[1]).toContain('README.md');
  });

  it('save draft is debounced and forwards content + attachment ids', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    render(Composer, { props: { pane, draft } });

    // Let any prior-test pending timers settle, then install a fresh mock so
    // we only observe the next save from this test.
    await new Promise((r) => setTimeout(r, 10));
    const saveMock = setBindingMock('SaveDraft', async () => {});

    draft.setContent('hi');
    await new Promise((r) => setTimeout(r, 10));

    expect(saveMock).toHaveBeenCalled();
    const lastCall = saveMock.mock.calls[saveMock.mock.calls.length - 1];
    expect(lastCall[0]).toBe('thread-1');
    expect(lastCall[1]).toBe('hi');
    expect(Array.isArray(lastCall[2])).toBe(true);
  });

  it('typing "/" at the start of the message opens the slash popover', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('GetThreadSlashCommands', async () => ['init', 'review', 'deploy-staging']);
    const { getByLabelText, findByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '/' } });
    const popover = await findByTestId('slash-popover');
    expect(popover).toBeInTheDocument();
  });

  it('slash popover does not open when "/" is not the first character', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('GetThreadSlashCommands', async () => ['init']);
    const { getByLabelText, queryByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'hello /init' } });
    expect(queryByTestId('slash-popover')).toBeNull();
  });

  it('filters slash-command options as the user types', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('GetThreadSlashCommands', async () => [
      'init',
      'review',
      'deploy-staging',
    ]);
    const { getByLabelText, findAllByTestId } = render(Composer, {
      props: { pane, draft },
    });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '/' } });
    // Wait for the binding + state update so all three commands render.
    let options = await findAllByTestId('slash-option');
    expect(options.length).toBe(3);

    await fireEvent.input(textarea, { target: { value: '/rev' } });
    options = await findAllByTestId('slash-option');
    expect(options.length).toBe(1);
    expect(options[0].textContent).toMatch(/\/review/);
  });

  it('ArrowDown + Enter inserts the highlighted command and closes the popover', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('GetThreadSlashCommands', async () => ['init', 'review']);
    const { getByLabelText, findAllByTestId, queryByTestId } = render(Composer, {
      props: { pane, draft },
    });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '/' } });
    await findAllByTestId('slash-option');

    await fireEvent.keyDown(textarea, { key: 'ArrowDown' });
    await fireEvent.keyDown(textarea, { key: 'Enter' });

    // Draft content gets the replacement with a trailing space; pendingMessage
    // is unset because Enter inside the popover is a selection, not a send.
    expect(draft.content).toBe('/review ');
    expect(pane.pendingMessage).toBeNull();
    expect(queryByTestId('slash-popover')).toBeNull();
  });

  it('Escape closes the slash popover without mutating the draft', async () => {
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    setBindingMock('GetThreadSlashCommands', async () => ['init']);
    const { getByLabelText, findByTestId, queryByTestId } = render(Composer, {
      props: { pane, draft },
    });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '/ini' } });
    await findByTestId('slash-popover');
    await fireEvent.keyDown(textarea, { key: 'Escape' });
    expect(queryByTestId('slash-popover')).toBeNull();
    expect(draft.content).toBe('/ini');
  });

  it('draft hydrates on thread switch and shows stored content', async () => {
    setBindingMock('GetDraft', async (id: string) => ({
      threadId: id,
      content: 'hydrated content',
      attachmentIds: [],
      terminalChips: [],
      updatedAt: 99,
    }));
    const pane = await makeReadyPane();
    const draft = await makeDraftStore();
    expect(draft.content).toBe('hydrated content');
    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.value).toBe('hydrated content');
  });
});

describe('<Composer> mid-turn guard', () => {
  beforeEach(() => {
    setBindingMock('SendMessage', async () => {});
    setBindingMock('InterruptTurn', async () => {});
    mockDraftBindings();
  });

  it('disables Send while streaming content is live even with a draft', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('partial response');
    const draft = await makeDraftStore();
    draft.setContent('next question');
    const { getByTestId } = render(Composer, { props: { pane, draft } });
    const send = getByTestId('composer-send') as HTMLButtonElement;
    expect(send.disabled).toBe(true);
    expect(send.getAttribute('title') ?? '').toMatch(/Agent is responding/);
  });

  it('disables Send while active tool calls are in flight', async () => {
    const pane = await makeReadyPane();
    pane.addToolCall('tool-1', { toolName: 'bash' });
    const draft = await makeDraftStore();
    draft.setContent('queued');
    const { getByTestId } = render(Composer, { props: { pane, draft } });
    expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(true);
  });

  it('disables Send while pendingMessage is set (optimistic in-flight turn)', async () => {
    const pane = await makeReadyPane();
    pane.setPendingMessage('just sent');
    const draft = await makeDraftStore();
    draft.setContent('immediate follow-up');
    const { getByTestId } = render(Composer, { props: { pane, draft } });
    expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(true);
  });

  it('Enter during active turn does not call SendMessage and announces the block', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('streaming');
    const draft = await makeDraftStore();
    draft.setContent('queued follow-up');
    const sendMock = setBindingMock('SendMessage', async () => {});
    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    expect(sendMock).not.toHaveBeenCalled();
    const alert = getByTestId('composer-midturn-error');
    expect(alert.textContent ?? '').toMatch(/Cannot send during an active turn/i);
    expect(alert.getAttribute('aria-live')).toBe('polite');
  });

  it('clicking Send during an active turn is a no-op', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('streaming');
    const draft = await makeDraftStore();
    draft.setContent('queued');
    const sendMock = setBindingMock('SendMessage', async () => {});
    const { getByTestId } = render(Composer, { props: { pane, draft } });
    await fireEvent.click(getByTestId('composer-send'));
    expect(sendMock).not.toHaveBeenCalled();
  });

  it('Interrupt button surfaces an error when InterruptTurn rejects', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('streaming');
    const draft = await makeDraftStore();
    setBindingMock('InterruptTurn', async () => {
      throw new Error('rpc down');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByTestId } = render(Composer, { props: { pane, draft } });

    await fireEvent.click(getByTestId('composer-interrupt'));
    await Promise.resolve();
    await Promise.resolve();

    expect(pane.error ?? '').toMatch(/Failed to interrupt/);
    consoleErr.mockRestore();
  });

  it('uploading attachments is still allowed during an active turn', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('streaming');
    const draft = await makeDraftStore();
    const uploadMock = setBindingMock('UploadAttachment', async () => ({
      id: 'att-mid',
      threadId: 'thread-1',
      filename: 'midturn.png',
      mimeType: 'image/png',
      size: 4,
      relativePath: 'thread-1/att-mid.png',
      createdAt: 4,
    }));
    const { getByTestId } = render(Composer, { props: { pane, draft } });
    const root = getByTestId('composer-root') as HTMLElement;

    const file = new File(['mid'], 'midturn.png', { type: 'image/png' });
    const dataTransfer = makeDataTransfer([file]);
    await fireEvent.drop(root, { dataTransfer });
    await flushTimers();

    expect(uploadMock).toHaveBeenCalled();
    expect(draft.attachments.some((a) => a.id === 'att-mid')).toBe(true);
  });

  it('editing the draft is still allowed during an active turn', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('streaming');
    const draft = await makeDraftStore();
    const { getByLabelText } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(false);
    await fireEvent.input(textarea, { target: { value: 'queued text' } });
    expect(draft.content).toBe('queued text');
  });

  it('typing a new character clears the mid-turn polite error message', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('streaming');
    const draft = await makeDraftStore();
    const { getByLabelText, getByTestId } = render(Composer, { props: { pane, draft } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    const alert = getByTestId('composer-midturn-error');
    expect(alert.textContent ?? '').toMatch(/Cannot send/);
    await fireEvent.input(textarea, { target: { value: 'a' } });
    expect((alert.textContent ?? '').trim()).toBe('');
  });

  it('Interrupt success leaves Send re-enabled once the pane clears', async () => {
    const pane = await makeReadyPane();
    pane.appendTextDelta('streaming');
    const draft = await makeDraftStore();
    draft.setContent('queued');
    const interruptMock = setBindingMock('InterruptTurn', async () => {});
    const { getByTestId } = render(Composer, { props: { pane, draft } });

    expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(true);
    await fireEvent.click(getByTestId('composer-interrupt'));
    expect(interruptMock).toHaveBeenCalledWith('thread-1');
    // The backend is responsible for delivering turn_complete, but once the
    // pane clears streamingContent the guard lifts. Simulate that here.
    pane.finalizeTurn();
    await Promise.resolve();
    await Promise.resolve();
    expect((getByTestId('composer-send') as HTMLButtonElement).disabled).toBe(false);
  });
});
