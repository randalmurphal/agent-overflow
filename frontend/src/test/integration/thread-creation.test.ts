// Integration tests that mount the full <App> against mocked Wails bindings
// and drive the user flow for creating new threads.
//
// Every binding the app will call during these flows must be mocked via
// setBindingMock — the shared mock harness throws loudly when a binding is
// invoked without an installed mock, which catches regressions where a new
// call sneaks into a code path the tests were meant to cover.

import { describe, expect, it, beforeAll, beforeEach, vi } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
} from './_helpers';

beforeAll(installAnimateShim);

describe('App integration — thread creation', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
  });

  it('creates a thread from the sidebar and opens it', async () => {
    const created = makeThread({ id: 'sidebar-created', title: 'Fresh Thread' });
    const createMock = setBindingMock('CreateThread', async () => created);
    setBindingMock('StartSession', async () => {});
    installThreadViewDefaults();

    const { getByText, findAllByText } = render(App);
    await flush();

    await fireEvent.click(getByText('+ New Thread'));
    await flush();

    const wsInput = document.querySelector<HTMLInputElement>('input[aria-label="Workspace path"]');
    expect(wsInput).not.toBeNull();
    await fireEvent.input(wsInput!, { target: { value: '/tmp/newthread' } });
    await flush();

    await fireEvent.click(getByText('Create'));
    await waitFor(() => expect(createMock).toHaveBeenCalled());
    expect(createMock.mock.calls[0][0]).toBe('claude');
    expect(createMock.mock.calls[0][1]).toBe('/tmp/newthread');
    // Title shows in BOTH the sidebar row and the chat header once the pane
    // switched. `findAllByText` tolerates both.
    const matches = await findAllByText('Fresh Thread');
    expect(matches.length).toBeGreaterThan(0);
  });

  it('creates a thread from a PR URL', async () => {
    const created = makeThread({ id: 'pr-thread', title: 'PR #42 demo title' });
    const binding = setBindingMock('CreateThreadFromPR', async () => created);
    installThreadViewDefaults();

    const { getByTestId, findAllByText } = render(App);
    await flush();

    // Click "From PR…" to open the dialog.
    await fireEvent.click(getByTestId('sidebar-new-thread-from-pr'));
    await flush();

    const urlInput = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(urlInput, {
      target: { value: 'https://github.com/owner/repo/pull/42' },
    });
    await flush();

    await fireEvent.click(getByTestId('thread-from-pr-submit'));

    await waitFor(() => expect(binding).toHaveBeenCalled());
    expect(binding.mock.calls[0][0]).toBe('owner/repo');
    expect(binding.mock.calls[0][1]).toBe(42);
    const matches = await findAllByText('PR #42 demo title');
    expect(matches.length).toBeGreaterThan(0);
  });

  it('rejects invalid PR URL in dialog', async () => {
    const { getByTestId, findByTestId } = render(App);
    await flush();

    await fireEvent.click(getByTestId('sidebar-new-thread-from-pr'));
    await flush();

    const urlInput = getByTestId('thread-from-pr-url') as HTMLInputElement;
    await fireEvent.input(urlInput, { target: { value: 'definitely not a url' } });
    await flush();

    const parseErr = await findByTestId('thread-from-pr-parse-error');
    expect(parseErr).toBeInTheDocument();
    const submit = getByTestId('thread-from-pr-submit') as HTMLButtonElement;
    expect(submit.disabled).toBe(true);
  });

  it('shows loading state during CreateThread', async () => {
    // Delay the resolution so we can observe the intermediate state.
    let resolveCreate: ((value: Thread) => void) | null = null;
    setBindingMock('CreateThread', () =>
      new Promise<Thread>((resolve) => {
        resolveCreate = resolve;
      }),
    );
    setBindingMock('StartSession', async () => {});
    installThreadViewDefaults();

    const { getByText } = render(App);
    await flush();
    await fireEvent.click(getByText('+ New Thread'));
    await flush();
    const wsInput = document.querySelector<HTMLInputElement>('input[aria-label="Workspace path"]');
    await fireEvent.input(wsInput!, { target: { value: '/tmp/xx' } });
    await flush();

    const createBtn = getByText('Create') as HTMLButtonElement;
    await fireEvent.click(createBtn);
    await flush();

    // The button flips to "Creating..." while the promise is in-flight.
    await waitFor(() => {
      expect(document.body.textContent).toMatch(/Creating\.{3}/);
    });

    // Resolve so the test's afterEach doesn't see an unsettled promise.
    (resolveCreate as ((value: Thread) => void) | null)?.(makeThread({ id: 'finally' }));
    await flush();
  });

  it('surfaces backend error when CreateThread fails', async () => {
    setBindingMock('CreateThread', async () => {
      throw new Error('db locked');
    });
    setBindingMock('StartSession', async () => {});

    const consoleErr = vi.spyOn(console, 'error').mockImplementation(() => {});
    const { getByText } = render(App);
    await flush();

    await fireEvent.click(getByText('+ New Thread'));
    await flush();
    const wsInput = document.querySelector<HTMLInputElement>('input[aria-label="Workspace path"]');
    await fireEvent.input(wsInput!, { target: { value: '/tmp/willfail' } });
    await flush();

    await fireEvent.click(getByText('Create'));
    await flush(10);

    // Sidebar routes the error through pane.setError; App doesn't render a
    // sidebar-level banner, but the console.error path fires. Assert that.
    const call = consoleErr.mock.calls.find((c) =>
      String(c[0] ?? '').includes('Failed to create thread'),
    );
    expect(call).toBeDefined();
    consoleErr.mockRestore();
  });

  it('mod+shift+o dispatches the thread.new command event', async () => {
    // Bind the keychord to the built-in thread.new command.
    setBindingMock('GetKeybindings', async () => [
      { key: 'mod+shift+o', command: 'thread.new' },
    ]);
    const events: Event[] = [];
    const listener = (ev: Event) => events.push(ev);
    window.addEventListener('agent-overflow:open-thread-form', listener);
    try {
      render(App);
      // `loadKeybindings` is async; wait until the store is loaded before
      // firing the chord so dispatchKey has a resolved rule list.
      await waitFor(async () => {
        const mod = await import('../../lib/stores/keybindings.svelte');
        expect(mod.isKeybindingsLoaded()).toBe(true);
      });
      expect(events).toHaveLength(0);

      // Fire the chord at window level. On darwin/mac, mod => meta; we
      // dispatch both mac and non-mac variants to stay platform-agnostic.
      await fireEvent.keyDown(window, {
        key: 'o',
        metaKey: true,
        shiftKey: true,
      });
      await fireEvent.keyDown(window, {
        key: 'o',
        ctrlKey: true,
        shiftKey: true,
      });

      // The built-in thread.new command dispatches a CustomEvent on window.
      // No production component currently listens for this event (see report
      // for details); verify the chord still reaches the command hook so a
      // future subscriber will receive it.
      await waitFor(() => expect(events.length).toBeGreaterThan(0));
    } finally {
      window.removeEventListener('agent-overflow:open-thread-form', listener);
    }
  });

  it('Cmd+K opens the palette and a discussion command surfaces the Start Discussion flow', async () => {
    // Seed a non-discussion thread so the Start Discussion command is
    // eligible when we open the palette while it's active.
    const existing = makeThread({ id: 'origin', title: 'Origin Thread' });
    setBindingMock('ListThreads', async () => [existing]);
    setBindingMock('GetKeybindings', async () => [
      { key: 'mod+k', command: 'palette.open' },
    ]);
    // DiscussionStartFlow mounts ListDiscussions on open.
    setBindingMock('ListDiscussions', async () => []);
    installThreadViewDefaults();

    const { getByText, getByTestId, findByTestId } = render(App);
    await waitFor(async () => {
      const mod = await import('../../lib/stores/keybindings.svelte');
      expect(mod.isKeybindingsLoaded()).toBe(true);
    });

    // Click the thread in the sidebar so it becomes active.
    await fireEvent.click(getByText('Origin Thread'));
    await flush(10);

    // Open palette via Cmd+K (keybinding wired above). Cmd+K is always
    // allowed through the editable-target gate so we don't need to worry
    // about focus being on an input element.
    await fireEvent.keyDown(window, { key: 'k', metaKey: true });
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    await flush();

    const input = (await findByTestId('command-palette-input')) as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'start discussion' } });
    await flush();
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush(10);

    // DiscussionStartFlow mounts with aria-labelled title "Start discussion".
    await waitFor(() => {
      expect(document.body.textContent).toMatch(/Start discussion/i);
    });
    // Ensure the palette closed after Enter.
    expect(() => getByTestId('command-palette-backdrop')).toThrow();
  });
});
