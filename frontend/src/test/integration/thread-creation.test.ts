// Integration tests for thread creation through the Wave 3a projects-first
// sidebar. The "+ New Thread" form is gone — users create threads via a
// per-project pencil button or the command palette. These tests mount the
// full <App> against mocked Wails bindings and exercise the new flows.

import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import { setBindingMock } from '../mocks/bindings-app';
import { modelCatalog } from '../helpers/modelCatalog';
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

  it('opens an unpersisted draft placeholder from the per-project pencil with the normal composer controls', async () => {
    const existing = makeThread({ id: 'existing', title: 'Existing Thread' });
    setBindingMock('ListThreads', async () => [existing]);
    seedSidebarProject([existing]);
    // No CreateThread should be called until the user actually types.
    const createMock = setBindingMock('CreateThread', async () => {
      throw new Error('CreateThread should not be called for placeholder open');
    });
    setBindingMock('StartSession', async () => {});
    installThreadViewDefaults();

    const { findByTestId } = render(App);
    await flush(10);

    const pencil = await findByTestId('project-item-new-thread');
    await fireEvent.click(pencil);
    await flush(10);

    // Placeholder open is a local UI-only operation — no DB row yet.
    expect(createMock).not.toHaveBeenCalled();
    // The composer toolbar mounts against the placeholder so the user
    // can configure model/effort/etc. before their first keystroke.
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
      return modelCatalog([{ slug: `${providerName}-model`, name: `${providerName} model`, provider: providerName }]);
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
    const getModels = setBindingMock('GetModelsForProvider', async () => modelCatalog([], 'shipped'));

    render(App);
    await flush(10);

    expect(getModels).not.toHaveBeenCalled();
  });

  it('surfaces backend error when draft materialization fails on first input', async () => {
    const existing = makeThread({ id: 'existing', title: 'Existing Thread' });
    setBindingMock('ListThreads', async () => [existing]);
    seedSidebarProject([existing]);
    setBindingMock('CreateThread', async () => {
      throw new Error('db locked');
    });
    installThreadViewDefaults();

    const { findByTestId, findByLabelText } = render(App);
    await flush(10);

    const pencil = await findByTestId('project-item-new-thread');
    await fireEvent.click(pencil);
    await flush(10);
    // Typing into the placeholder kicks off CreateThread; the failure
    // is rendered to the user as a pane-level error string.
    const textarea = await findByLabelText('Message Input') as HTMLTextAreaElement;
    await fireEvent.input(textarea, { target: { value: 'first char' } });
    await waitFor(() => {
      expect(document.body.textContent).toMatch(/db locked/i);
    });
  });

  it('Cmd+K opens the palette and a discussion command surfaces the Start Discussion flow', async () => {
    const existing = makeThread({ id: 'origin', title: 'Origin Thread' });
    setBindingMock('ListThreads', async () => [existing]);
    seedSidebarProject([existing]);
    setBindingMock('GetKeybindings', async () => ({
      bindings: [{ key: 'mod+k', command: 'palette.open' }],
    }));
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
