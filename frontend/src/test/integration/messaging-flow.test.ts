// Integration tests covering the composer + message timeline working
// together. These tests mount the full App and drive user input through
// the composer, then observe the side effects the message timeline
// renders (pending messages, streaming tokens, tool-call chips, etc).

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

  it('sends a message and shows it in the timeline as pending', async () => {
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
    // The optimistic pending message renders in the timeline.
    await waitFor(() => {
      expect(document.body.textContent).toContain('hello agent');
    });
  });

  it('blocks Enter during an active turn and surfaces the mid-turn banner', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    const sendMock = setBindingMock('SendMessage', async () => {});
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'queued message' } });
    await flush();

    // Simulate a streaming turn starting: the provider:event router calls
    // appendTextDelta. We emit through the Wails mock bus.
    emitWailsEvent('provider:event', {
      kind: 'text_delta',
      threadId: 'thread-1',
      content: 'response...',
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

    emitWailsEvent('provider:event', {
      kind: 'text_delta',
      threadId: 'thread-1',
      content: 'streaming...',
    });
    await flush();
    await fireEvent.click(getByTestId('composer-interrupt'));
    await waitFor(() => expect(interruptMock).toHaveBeenCalled());
    expect(interruptMock.mock.calls[0][0]).toBe('thread-1');
  });

  it('renders streaming text deltas as they arrive', async () => {
    await mountWithActiveThread();
    // Multiple deltas accumulate.
    emitWailsEvent('provider:event', {
      kind: 'text_delta',
      threadId: 'thread-1',
      content: 'first ',
    });
    await flush();
    emitWailsEvent('provider:event', {
      kind: 'text_delta',
      threadId: 'thread-1',
      content: 'second',
    });
    await flush();

    await waitFor(() => {
      expect(document.body.textContent).toContain('first second');
    });
  });

  it('replaces individual tool cards with a group chip when 2+ tools are active', async () => {
    const { queryByTestId, getByTestId } = await mountWithActiveThread();

    // One tool => individual card, no group chip.
    emitWailsEvent('provider:event', {
      kind: 'tool_start',
      threadId: 'thread-1',
      itemId: 'tool-1',
      meta: { toolName: 'bash' },
    });
    await flush();
    expect(queryByTestId('active-tools-chip')).toBeNull();

    // Second tool -> group chip appears, cards collapse into children.
    emitWailsEvent('provider:event', {
      kind: 'tool_start',
      threadId: 'thread-1',
      itemId: 'tool-2',
      meta: { toolName: 'read' },
    });
    await flush();
    expect(getByTestId('active-tools-chip')).toBeInTheDocument();
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

  it('clears pending message when turn_complete arrives', async () => {
    // finalizeTurn re-reads items via ListItems; stub that so it returns the
    // freshly-persisted user message as a solid item.
    const items: Array<Record<string, unknown>> = [];
    setBindingMock('ListItems', async () => items);
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    const textarea = getByLabelText('Message input') as HTMLTextAreaElement;

    await fireEvent.input(textarea, { target: { value: 'persist me' } });
    await flush();
    await fireEvent.click(getByTestId('composer-send'));
    await flush(10);

    // Pending message appears immediately.
    await waitFor(() => {
      expect(document.body.textContent).toContain('persist me');
    });

    // A turn_complete event triggers finalizeTurn which clears pending.
    items.push({
      id: 'user-1',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'message',
      role: 'user',
      summary: 'persist me',
      createdAt: 0,
    });
    emitWailsEvent('provider:event', {
      kind: 'turn_complete',
      threadId: 'thread-1',
    });
    await flush(10);

    // Pending has cleared; the persisted item took its place.
    const paneMod = await import('../../lib/stores/panes.svelte');
    const pane = paneMod.getMainPane();
    await waitFor(() => expect(pane.pendingMessage).toBeNull());
  });
});
