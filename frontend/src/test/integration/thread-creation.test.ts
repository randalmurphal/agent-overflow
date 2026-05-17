// Integration tests for thread creation through the Wave 3a projects-first
// sidebar. The "+ New Thread" form is gone — users create threads via a
// per-project pencil button or the command palette. These tests mount the
// full <App> against mocked Wails bindings and exercise the new flows.

import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import { getProviderModels } from '../../lib/stores/providerModels.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

describe('App integration — thread creation', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
  });

  it('creates a hidden draft from the per-project pencil with the normal composer controls', async () => {
    const existing = makeThread({ id: 'existing', title: 'Existing Thread' });
    const created = makeThread({
      id: 'sidebar-created',
      title: 'Fresh Thread',
      projectId: 'proj-int',
    });
    setBindingMock('ListThreads', async () => [existing]);
    seedSidebarProject([existing]);
    const createMock = setBindingMock('CreateThread', async () => created);
    setBindingMock('StartSession', async () => {});
    installThreadViewDefaults();

    const { findByTestId, findAllByText } = render(App);
    await flush(10);

    const pencil = await findByTestId('project-item-new-thread');
    await fireEvent.click(pencil);
    await waitFor(() => expect(createMock).toHaveBeenCalled());
    // CreateThread takes a CreateThreadOptions struct as its sole arg.
    // + New is mode-contextual; the default Chat tab forwards mode='chat'.
    expect(createMock.mock.calls[0][0]).toEqual({ projectId: 'proj-int', mode: 'chat' });
    const matches = await findAllByText('Fresh Thread');
    expect(matches.length).toBeGreaterThan(0);
    expect(await findByTestId('composer-model-menu-trigger')).toBeInTheDocument();
    expect(await findByTestId('composer-effort-trigger')).toBeInTheDocument();
    expect(await findByTestId('composer-agent-mode-toggle')).toBeInTheDocument();
    expect(await findByTestId('composer-access-toggle')).toBeInTheDocument();
    expect(await findByTestId('env-picker-trigger')).toBeInTheDocument();
    expect(await findByTestId('branch-picker-trigger')).toBeInTheDocument();
  });

  it('preloads enabled provider model catalogs on app load', async () => {
    setBindingMock('GetSettings', async () => ({
      claudeEnabled: false,
      codexEnabled: true,
    }));
    const getModels = setBindingMock('GetModelsForProvider', async (provider) => {
      const providerName = String(provider);
      return [{ slug: `${providerName}-model`, name: `${providerName} model`, provider: providerName }];
    });

    render(App);

    await waitFor(() => {
      expect(getModels).toHaveBeenCalledWith('codex');
      expect(getProviderModels('codex')).toEqual([
        { slug: 'codex-model', name: 'codex model', provider: 'codex' },
      ]);
    });
    expect(getModels).not.toHaveBeenCalledWith('claude');
  });

  it('does not preload provider model catalogs when settings fail to load', async () => {
    setBindingMock('GetSettings', async () => {
      throw new Error('settings unavailable');
    });
    const getModels = setBindingMock('GetModelsForProvider', async () => []);

    render(App);
    await flush(10);

    expect(getModels).not.toHaveBeenCalled();
  });

  it('surfaces backend error when draft creation fails', async () => {
    const existing = makeThread({ id: 'existing', title: 'Existing Thread' });
    setBindingMock('ListThreads', async () => [existing]);
    seedSidebarProject([existing]);
    setBindingMock('CreateThread', async () => {
      throw new Error('db locked');
    });
    installThreadViewDefaults();

    const { findByTestId } = render(App);
    await flush(10);

    const pencil = await findByTestId('project-item-new-thread');
    await fireEvent.click(pencil);
    await flush(10);
    // A toast surface event is emitted; the test asserts no thread was
    // navigated to, which is the user-visible outcome.
    expect(document.body.textContent).toMatch(/db locked/i);
  });

  it('creates a thread from a PR URL via the command palette', async () => {
    const created = makeThread({
      id: 'pr-thread',
      title: 'PR #42 demo title',
      projectId: 'proj-int',
    });
    const binding = setBindingMock('CreateThreadFromPR', async () => created);
    setBindingMock('GetKeybindings', async () => [
      { key: 'mod+k', command: 'palette.open' },
    ]);
    installThreadViewDefaults();

    const { getByTestId, findByTestId, findAllByText } = render(App);
    // Wait for keybindings to load so Cmd+K actually opens the palette.
    await waitFor(async () => {
      const mod = await import('../../lib/stores/keybindings.svelte');
      expect(mod.isKeybindingsLoaded()).toBe(true);
    });
    await fireEvent.keyDown(window, { key: 'k', metaKey: true });
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    await flush();

    const input = (await findByTestId('command-palette-input')) as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'new from pull/merge' } });
    await flush();
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush(10);

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

  it('Cmd+K opens the palette and a discussion command surfaces the Start Discussion flow', async () => {
    const existing = makeThread({ id: 'origin', title: 'Origin Thread' });
    setBindingMock('ListThreads', async () => [existing]);
    seedSidebarProject([existing]);
    setBindingMock('GetKeybindings', async () => [
      { key: 'mod+k', command: 'palette.open' },
    ]);
    setBindingMock('ListDiscussions', async () => []);
    installThreadViewDefaults();

    const { findByText, getByTestId, findByTestId } = render(App);
    await waitFor(async () => {
      const mod = await import('../../lib/stores/keybindings.svelte');
      expect(mod.isKeybindingsLoaded()).toBe(true);
    });

    // Click the thread in the expanded project to activate it.
    const row = await findByText('Origin Thread');
    await fireEvent.click(row);
    await flush(10);

    await fireEvent.keyDown(window, { key: 'k', metaKey: true });
    await fireEvent.keyDown(window, { key: 'k', ctrlKey: true });
    await flush();

    const input = (await findByTestId('command-palette-input')) as HTMLInputElement;
    await fireEvent.input(input, { target: { value: 'start discussion' } });
    await flush();
    await fireEvent.keyDown(input, { key: 'Enter' });
    await flush(10);

    await waitFor(() => {
      expect(document.body.textContent).toMatch(/Start discussion/i);
    });
    expect(() => getByTestId('command-palette-backdrop')).toThrow();
  });
});
