// Integration tests for the interaction-mode control on threads. After
// Wave 3c, the mode badge in the chat header is gone — the composer
// toolbar's ModeCycleButton owns the switch. These tests mount App,
// select a thread, and drive the composer button through the same
// UpdateThreadMode binding the keyboard shortcut uses.

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

describe('App integration — interaction mode', () => {
  beforeEach(() => {
    resetAppState();
  });

  it('shows the current mode label on the composer cycle button', async () => {
    setBindingMock('ListDesignArtifacts', async () => []);
    const thread = makeThread({ title: 'Design Thread', mode: 'design' });
    const { getByTestId } = await mountWithThread(thread);
    const btn = getByTestId('composer-mode-cycle');
    expect(btn.textContent ?? '').toMatch(/design/i);
  });

  it('cycles chat → plan via the composer button and calls UpdateThreadMode', async () => {
    const thread = makeThread({ title: 'Default Thread', mode: 'chat' });
    const update = setBindingMock('UpdateThreadMode', async (_id, mode) => ({
      ...thread,
      mode: mode as Thread['mode'],
    }));
    const { getByTestId } = await mountWithThread(thread);
    const btn = getByTestId('composer-mode-cycle');
    expect(btn.textContent ?? '').toMatch(/chat/i);

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
    const btn = getByTestId('composer-mode-cycle');
    await fireEvent.click(btn);
    await flush(10);

    await waitFor(() =>
      expect(
        consoleErr.mock.calls.some((c) =>
          String(c[0] ?? '').includes('mode.cycle: UpdateThreadMode failed'),
        ),
      ).toBe(true),
    );
    // The button stays on chat since the backend rejected.
    expect(btn.textContent ?? '').toMatch(/chat/i);
    consoleErr.mockRestore();
  });
});
