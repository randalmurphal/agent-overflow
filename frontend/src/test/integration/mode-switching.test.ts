// Integration tests for the interaction mode badge on threads. Tests mount
// App, select a thread, and verify the badge reflects + mutates the mode.

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
} from './_helpers';

beforeAll(installAnimateShim);

async function mountWithThread(thread: Thread) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
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

  it('renders the current mode in the thread header badge', async () => {
    // Design mode threads mount DesignView, which calls
    // ListDesignArtifacts on $effect; mock it so the side panel is happy.
    setBindingMock('ListDesignArtifacts', async () => []);
    const thread = makeThread({ title: 'Design Thread', interactionMode: 'design' });
    const { getByTestId } = await mountWithThread(thread);
    const badge = getByTestId('interaction-mode-badge');
    expect(badge.textContent).toMatch(/DESIGN/i);
  });

  it('switches mode via the dropdown and calls SetThreadInteractionMode', async () => {
    const thread = makeThread({ title: 'Default Thread', interactionMode: 'default' });
    const setMode = setBindingMock('SetThreadInteractionMode', async (_id, mode) => ({
      ...thread,
      interactionMode: mode as Thread['interactionMode'],
    }));
    const { getByTestId } = await mountWithThread(thread);
    const badge = getByTestId('interaction-mode-badge');
    expect(badge.textContent).toMatch(/DEFAULT/i);

    await fireEvent.click(badge);
    await flush();
    const planOption = getByTestId('interaction-mode-option-plan');
    await fireEvent.click(planOption);

    await waitFor(() => expect(setMode).toHaveBeenCalled());
    expect(setMode.mock.calls[0][0]).toBe(thread.id);
    expect(setMode.mock.calls[0][1]).toBe('plan');
    await waitFor(() => expect(badge.textContent).toMatch(/PLAN/i));
  });

  it('logs and surfaces an error when SetThreadInteractionMode rejects', async () => {
    const thread = makeThread({ title: 'Reject Thread', interactionMode: 'default' });
    setBindingMock('SetThreadInteractionMode', async () => {
      throw new Error('backend down');
    });
    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByTestId } = await mountWithThread(thread);
    const badge = getByTestId('interaction-mode-badge');
    await fireEvent.click(badge);
    await flush();
    await fireEvent.click(getByTestId('interaction-mode-option-plan'));
    await flush(10);

    // The badge stays on its previous value (mode didn't change).
    await waitFor(() =>
      expect(
        consoleErr.mock.calls.some((c) =>
          String(c[0] ?? '').includes('Failed to set interaction mode'),
        ),
      ).toBe(true),
    );
    // Current mode should still be DEFAULT since the backend rejected.
    expect(badge.textContent).toMatch(/DEFAULT/i);
    consoleErr.mockRestore();
  });

  it('shows the mode picker in the new-thread form and passes the chosen mode to CreateThread', async () => {
    const created = makeThread({
      id: 'newly-created',
      title: 'Plan Thread',
      interactionMode: 'plan',
    });
    const createMock = setBindingMock('CreateThread', async (
      _p: unknown,
      _ws: unknown,
      _m: unknown,
      mode: unknown,
    ) => ({
      ...created,
      interactionMode: mode as Thread['interactionMode'],
    }));
    setBindingMock('StartSession', async () => {});
    installAppDefaults();
    installThreadViewDefaults();
    installComposerDefaults(created.id);

    const { getByText, getByTestId } = render(App);
    await flush();
    await fireEvent.click(getByText('+ New Thread'));
    await flush();

    // Picker is visible.
    expect(getByTestId('new-thread-mode-picker')).toBeInTheDocument();
    // Select plan mode.
    await fireEvent.click(getByTestId('new-thread-mode-plan'));
    await flush();
    const wsInput = document.querySelector<HTMLInputElement>('input[aria-label="Workspace path"]');
    await fireEvent.input(wsInput!, { target: { value: '/tmp/plan' } });
    await flush();
    await fireEvent.click(getByText('Create'));
    await waitFor(() => expect(createMock).toHaveBeenCalled());
    expect(createMock.mock.calls[0][3]).toBe('plan');
  });
});
