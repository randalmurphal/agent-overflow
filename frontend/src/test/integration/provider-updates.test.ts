// Provider CLI updates at the app level: App subscribes the store, so a
// frame announces a new version, and the toast's action opens the one notes
// modal App mounts.

import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import { flush, installAnimateShim, installAppDefaults, resetAppState } from './_helpers';
import { setBindingMock } from '../mocks/bindings-app';
import { emitWailsEvent } from '../mocks/wailsio-runtime';

beforeAll(installAnimateShim);

describe('App integration: provider CLI updates', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
  });

  it('announces a new version and opens its notes from the toast', async () => {
    setBindingMock('GetProviderUpdateNotes', async () => ({
      provider: 'codex',
      fromVersion: '0.163.0',
      toVersion: '0.169.0',
      releases: [{ version: '0.169.0', name: '0.169.0', publishedAt: '', body: 'the newest notes', url: '' }],
      omitted: 0,
      url: 'https://github.com/openai/codex/releases',
    }));
    const view = render(App);
    await flush();
    emitWailsEvent('provider:updates', [{
      provider: 'codex', installedVersion: '0.163.0', latestVersion: '0.169.0', available: true, checkedAt: 1, run: { phase: 'idle' },
    }]);
    await flush();
    expect(view.getByText('Codex 0.169.0 is available')).toBeTruthy();
    await fireEvent.click(view.getByRole('button', { name: "What's new" }));
    await waitFor(() => expect(view.getByTestId('provider-update-notes-modal').textContent).toContain('the newest notes'));
    expect(view.getByRole('dialog', { name: "What's new in Codex" })).toBeTruthy();
  });
});
