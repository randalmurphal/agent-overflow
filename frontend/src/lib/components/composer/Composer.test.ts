import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, fireEvent } from '@testing-library/svelte';
import Composer from './Composer.svelte';
import { createThreadPane } from '../../stores/thread.svelte';
import type { Thread } from '../../types/models';
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

describe('<Composer>', () => {
  beforeEach(() => {
    setBindingMock('SendMessage', async () => {});
    setBindingMock('InterruptTurn', async () => {});
  });

  it('disables the textarea when no thread is selected', () => {
    const pane = createThreadPane();
    const { getByLabelText, getByRole } = render(Composer, { props: { pane } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(true);
    expect(textarea.placeholder).toMatch(/Select or create a thread/);
    // Send button is disabled too.
    const send = getByRole('button', { name: /send/i }) as HTMLButtonElement;
    expect(send.disabled).toBe(true);
  });

  it('renders Send when session is idle', async () => {
    const pane = await makeReadyPane();
    pane.setSessionStatus('ready');
    const { getByRole, queryByRole } = render(Composer, { props: { pane } });
    expect(getByRole('button', { name: /send/i })).toBeInTheDocument();
    expect(queryByRole('button', { name: /stop/i })).toBeNull();
  });

  it('renders Stop when the turn is running', async () => {
    const pane = await makeReadyPane();
    pane.setSessionStatus('running');
    const { getByRole, queryByRole } = render(Composer, { props: { pane } });
    expect(getByRole('button', { name: /stop/i })).toBeInTheDocument();
    expect(queryByRole('button', { name: /send/i })).toBeNull();
  });

  it('send button stays disabled until the textarea has non-whitespace content', async () => {
    const pane = await makeReadyPane();
    const { getByLabelText, getByRole } = render(Composer, { props: { pane } });
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
    const sendMock = setBindingMock('SendMessage', async () => {});
    const { getByLabelText, getByRole } = render(Composer, { props: { pane } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'hello world' } });
    await fireEvent.click(getByRole('button', { name: /send/i }));

    expect(sendMock).toHaveBeenCalledWith('thread-1', 'hello world');
    expect(pane.pendingMessage).toBe('hello world');
    // Textarea cleared after submit.
    expect(textarea.value).toBe('');
  });

  it('Enter without shift triggers send; Shift+Enter does not', async () => {
    const pane = await makeReadyPane();
    const sendMock = setBindingMock('SendMessage', async () => {});
    const { getByLabelText } = render(Composer, { props: { pane } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'line 1' } });
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: true });
    expect(sendMock).not.toHaveBeenCalled();

    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    expect(sendMock).toHaveBeenCalledWith('thread-1', 'line 1');
  });

  it('rolls back optimistic state and surfaces error when SendMessage rejects', async () => {
    const pane = await makeReadyPane();
    setBindingMock('SendMessage', async () => { throw new Error('rpc down'); });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});

    const { getByLabelText, getByRole } = render(Composer, { props: { pane } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'fails' } });
    await fireEvent.click(getByRole('button', { name: /send/i }));
    // Let the rejection settle.
    await Promise.resolve();
    await Promise.resolve();

    expect(pane.pendingMessage).toBeNull();
    expect(pane.error).toMatch(/Failed to send message/);
    expect(textarea.value).toBe('fails');
    consoleErr.mockRestore();
  });

  it('Stop button calls InterruptTurn with the current threadId', async () => {
    const pane = await makeReadyPane();
    pane.setSessionStatus('running');
    const interruptMock = setBindingMock('InterruptTurn', async () => {});

    const { getByRole } = render(Composer, { props: { pane } });
    await fireEvent.click(getByRole('button', { name: /stop/i }));

    expect(interruptMock).toHaveBeenCalledWith('thread-1');
  });

  it('does not call SendMessage for a whitespace-only message', async () => {
    const pane = await makeReadyPane();
    const sendMock = setBindingMock('SendMessage', async () => {});

    const { getByLabelText } = render(Composer, { props: { pane } });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: '   ' } });
    // Force keyboard submit path.
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });

    expect(sendMock).not.toHaveBeenCalled();
    // Ensure no pending message side-effect leaked.
    expect(pane.pendingMessage).toBeNull();
    expect(getBindingMock('SendMessage')!.mock.calls.length).toBe(0);
  });
});
