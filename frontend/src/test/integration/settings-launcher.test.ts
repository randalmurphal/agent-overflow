import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  resetAppState,
} from './_helpers';
import { OPEN_SETTINGS_EVENT } from '../../lib/stores/events';

beforeAll(installAnimateShim);

describe('App integration — settings launcher', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
  });

  it('opens and closes settings from the sidebar footer button', async () => {
    const rendered = render(App);
    await flush(10);

    await fireEvent.click(rendered.getByTestId('sidebar-settings-button'));

    // SettingsView is a lazy import; its first-ever load in a suite run
    // pays the on-demand transform, which can exceed waitFor's default
    // 1s under full-suite load.
    await waitFor(() => {
      expect(rendered.getByRole('tab', { name: 'General' })).toBeInTheDocument();
      expect(rendered.getByTestId('global-pane-surface')).toBeInTheDocument();
    }, { timeout: 5000 });

    await fireEvent.click(rendered.getByRole('button', { name: 'Close Settings' }));

    await waitFor(() => {
      expect(rendered.queryByTestId('global-pane-surface')).not.toBeInTheDocument();
    });
  });

  it('opens requested settings sections from the global event while mounted', async () => {
    const rendered = render(App);
    await flush(10);

    window.dispatchEvent(new CustomEvent(OPEN_SETTINGS_EVENT, {
      detail: { section: 'providers' },
    }));

    await waitFor(() => {
      expect(rendered.getByTestId('global-pane-surface')).toBeInTheDocument();
      expect(rendered.getByRole('tab', { name: 'Providers' })).toHaveAttribute('aria-selected', 'true');
    });

    window.dispatchEvent(new CustomEvent(OPEN_SETTINGS_EVENT, {
      detail: { section: 'observability' },
    }));

    await waitFor(() => {
      expect(rendered.getByRole('tab', { name: 'Observability' })).toHaveAttribute('aria-selected', 'true');
    });
  });
});
