// Integration tests for the in-thread agent-mode toggle. Thread *type*
// (design / discussion) is immutable post-creation; on chat threads the
// composer toolbar carries AgentModeToggle (chat ↔ plan), and on design
// / discussion threads the slot is empty (the in-pane ThreadModePicker
// in the workspace strip already surfaces the mode). These tests mount
// App, select a thread, and exercise the toggle path.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
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

async function mountWithThread(thread: Thread) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  installComposerDefaults(thread.id);

  const rendered = render(App);
  await flush();
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return { ...rendered, thread };
}

describe('App integration — agent-mode toggle', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('toggles chat → plan via the composer button and calls UpdateThreadMode', async () => {
    const thread = makeThread({ title: 'Default Thread', mode: 'chat' });
    const update = setBindingMock('UpdateThreadMode', async (_id, mode) => ({
      ...thread,
      mode: mode as Thread['mode'],
    }));
    const { getByTestId } = await mountWithThread(thread);
    const btn = getByTestId('composer-agent-mode-toggle');
    expect(btn.textContent ?? '').toMatch(/build/i);

    await fireEvent.click(btn);
    await waitFor(() => expect(update).toHaveBeenCalled());
    expect(update.mock.calls[0][0]).toBe(thread.id);
    expect(update.mock.calls[0][1]).toBe('plan');
    await waitFor(() => expect(btn.textContent ?? '').toMatch(/plan/i));
  });

  it('surfaces an error toast when UpdateThreadMode rejects', async () => {
    const thread = makeThread({ title: 'Reject Thread', mode: 'chat' });
    setBindingMock('UpdateThreadMode', async () => {
      throw new Error('backend down');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByTestId } = await mountWithThread(thread);
    const btn = getByTestId('composer-agent-mode-toggle');
    await fireEvent.click(btn);
    await flush(10);

    await waitFor(() =>
      expect(
        consoleErr.mock.calls.some((c) =>
          String(c[0] ?? '').includes('agent mode toggle: UpdateThreadMode failed'),
        ),
      ).toBe(true),
    );
    // The button stays on build since the backend rejected.
    expect(btn.textContent ?? '').toMatch(/build/i);
    consoleErr.mockRestore();
  });
});
