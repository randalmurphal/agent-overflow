// Integration tests for the in-thread agent-mode toggle. After the
// design-mode rebuild, thread *type* (design / discussion) is immutable
// and the composer toolbar carries either AgentModeToggle (chat ↔ plan
// on chat threads) or DesignLockPill (display-only on design threads).
// These tests mount App, select a thread, and exercise the toggle path.

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

  it('shows the design lock pill on a design thread', async () => {
    setBindingMock('ListDesignSnapshots', async () => []);
    const thread = makeThread({ title: 'Design Thread', mode: 'design' });
    const { getByTestId, queryByTestId } = await mountWithThread(thread);
    expect(getByTestId('composer-design-lock-pill')).toBeTruthy();
    // The chat/plan toggle must NOT be present on design threads.
    expect(queryByTestId('composer-agent-mode-toggle')).toBeNull();
  });

  it('toggles chat → plan via the composer button and calls UpdateThreadMode', async () => {
    const thread = makeThread({ title: 'Default Thread', mode: 'chat' });
    const update = setBindingMock('UpdateThreadMode', async (_id, mode) => ({
      ...thread,
      mode: mode as Thread['mode'],
    }));
    const { getByTestId } = await mountWithThread(thread);
    const btn = getByTestId('composer-agent-mode-toggle');
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
    // The button stays on chat since the backend rejected.
    expect(btn.textContent ?? '').toMatch(/chat/i);
    consoleErr.mockRestore();
  });
});
