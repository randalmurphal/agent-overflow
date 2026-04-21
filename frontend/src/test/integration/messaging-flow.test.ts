// Integration tests covering the composer + message timeline working
// together. These tests mount the full App and drive user input through
// the composer, then observe the side effects the unified item stream
// drives in the message timeline and composer.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import { emitWailsEvent } from '../mocks/wailsio-runtime';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

// Mount App with a single existing thread already selected. Returns the
// rendered result for assertions.
//
// NOTE: callers should install SendMessage / InterruptTurn mocks themselves
// before calling this function. The helper intentionally does not overwrite
// them so per-test mocks survive.
async function mountWithActiveThread(thread: Thread = makeThread({ title: 'Messaging Spec Thread' })) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  installComposerDefaults(thread.id);

  const rendered = render(App);
  await flush();
  // Click the thread row to activate it.
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return { ...rendered, thread };
}

describe('App integration — messaging flow', () => {
  beforeEach(() => {
    resetAppState();
    // Default SendMessage + InterruptTurn mocks — tests that need to spy
    // or reject reassign these with fresh `setBindingMock` calls.
    setBindingMock('SendMessage', async () => {});
    setBindingMock('InterruptTurn', async () => {});
  });

  it('sends a message and clears the composer draft', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    // Re-assign the mock AFTER mount so the call count starts at 0.
    const sendMock = setBindingMock('SendMessage', async () => {});

    // Composer textarea is keyed with aria-label "Message input".
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(false);
    await fireEvent.input(textarea, { target: { value: 'hello agent' } });
    await flush();

    const sendBtn = getByTestId('composer-send') as HTMLButtonElement;
    await waitFor(() => expect(sendBtn.disabled).toBe(false));
    await fireEvent.click(sendBtn);
    await waitFor(() => expect(sendMock).toHaveBeenCalled());

    expect(sendMock.mock.calls[0][0]).toBe('thread-1');
    expect(sendMock.mock.calls[0][1]).toBe('hello agent');
    expect(textarea.value).toBe('');
  });

  it('blocks Enter during an active turn and surfaces the mid-turn banner', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    const sendMock = setBindingMock('SendMessage', async () => {});
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'queued message' } });
    await flush();

    // Post-refactor isTurnActive is wire-pushed (invariant 22). Simulate
    // the real Go → frontend path by emitting provider:turn_started. A
    // streaming item no longer flips the composer's active-turn guard
    // on its own.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });
    emitWailsEvent('provider:item_upsert', {
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'response...',
      createdAt: 1,
      updatedAt: 1,
    });
    await flush();

    // Banner + Interrupt button are visible; Enter must not fire SendMessage.
    expect(getByTestId('composer-turn-banner')).toBeInTheDocument();
    await fireEvent.keyDown(textarea, { key: 'Enter', shiftKey: false });
    await flush();
    expect(sendMock).not.toHaveBeenCalled();
    // A polite error is announced.
    const err = getByTestId('composer-midturn-error');
    expect(err.textContent).toMatch(/Cannot send/);
  });

  it('interrupts an active turn via the Interrupt button', async () => {
    const { getByTestId } = await mountWithActiveThread();
    const interruptMock = setBindingMock('InterruptTurn', async () => {});

    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });
    emitWailsEvent('provider:item_upsert', {
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'streaming...',
      createdAt: 1,
      updatedAt: 1,
    });
    await flush();
    await fireEvent.click(getByTestId('composer-interrupt'));
    await waitFor(() => expect(interruptMock).toHaveBeenCalled());
    expect(interruptMock.mock.calls[0][0]).toBe('thread-1');
  });

  it('renders streaming assistant item updates as they arrive', async () => {
    await mountWithActiveThread();
    emitWailsEvent('provider:item_upsert', {
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'first ',
      createdAt: 1,
      updatedAt: 1,
    });
    await flush();
    emitWailsEvent('provider:item_upsert', {
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'first second',
      createdAt: 1,
      updatedAt: 2,
    });
    await flush();

    await waitFor(() => {
      expect(document.body.textContent).toContain('first second');
    });
  });

  it('renders tool_call rows inline as provider:item_upsert events arrive', async () => {
    const { queryByText, findByText } = await mountWithActiveThread();

    // Backend persisted a tool_call item and pushed the upsert; the
    // timeline should reflect it without any transient grouping.
    emitWailsEvent('provider:item_upsert', {
      id: 'tool-1',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'tool_call',
      role: 'assistant',
      summary: 'Bash: ls -la',
      status: 'running',
      isBackground: false,
      createdAt: 1,
    });
    expect(await findByText(/Bash: ls -la/)).toBeInTheDocument();

    // A second concurrent tool_call shows up as its own row — no
    // grouping chip, no relocation.
    emitWailsEvent('provider:item_upsert', {
      id: 'tool-2',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'tool_call',
      role: 'assistant',
      summary: 'Read: README.md',
      status: 'running',
      isBackground: false,
      createdAt: 2,
    });
    expect(await findByText(/Read: README.md/)).toBeInTheDocument();
    expect(queryByText(/Running 2 tools/i)).toBeNull();
  });

  it('handles SendMessage rejection by restoring draft and logging the error', async () => {
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    // Override SendMessage to reject AFTER mount so earlier calls don't trip.
    setBindingMock('SendMessage', async () => {
      throw new Error('rpc down');
    });
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'will fail' } });
    await flush();
    await fireEvent.click(getByTestId('composer-send'));
    await waitFor(() => {
      const call = consoleErr.mock.calls.find((c) =>
        String(c[0] ?? '').includes('Failed to send message'),
      );
      expect(call).toBeDefined();
    });
    // The draft was restored (textarea content matches).
    await waitFor(() => {
      expect(textarea.value).toBe('will fail');
    });
    consoleErr.mockRestore();
  });

  it('marks the pane idle once the streaming item completes', async () => {
    await mountWithActiveThread();
    // Post-refactor pane.isTurnActive only clears on provider:turn_completed
    // (invariant 22). Drive the full turn lifecycle so the assertion is
    // exercising the real wire path.
    emitWailsEvent('provider:turn_started', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
    });
    emitWailsEvent('provider:item_upsert', {
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: 'persist me',
      createdAt: 1,
      updatedAt: 1,
    });
    emitWailsEvent('provider:item_upsert', {
      id: 'text:0:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'completed',
      summary: 'persist me',
      createdAt: 1,
      updatedAt: 2,
    });
    emitWailsEvent('provider:turn_completed', {
      threadId: 'thread-1',
      turnId: 'turn-1',
      turnIndex: 0,
      startedAt: 1,
      completedAt: 2,
      stopReason: 'end_turn',
    });
    await flush(10);

    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    await waitFor(() => expect(pane.isTurnActive).toBe(false));
  });
});
