import { beforeAll, beforeEach, describe, expect, it } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  resetAppState,
} from './_helpers';

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

    await waitFor(() => {
      expect(rendered.getByRole('tab', { name: 'General' })).toBeInTheDocument();
      expect(rendered.getByTestId('global-pane-surface')).toBeInTheDocument();
    });

    await fireEvent.click(rendered.getByRole('button', { name: 'Close Settings' }));

    await waitFor(() => {
      expect(rendered.queryByTestId('global-pane-surface')).not.toBeInTheDocument();
    });
  });

  it('opens requested settings sections from the global event while mounted', async () => {
    const rendered = render(App);
    await flush(10);

    window.dispatchEvent(new CustomEvent('agent-overflow:open-settings', {
      detail: { section: 'providers' },
    }));

    await waitFor(() => {
      expect(rendered.getByTestId('global-pane-surface')).toBeInTheDocument();
      expect(rendered.getByRole('tab', { name: 'Providers' })).toHaveAttribute('aria-selected', 'true');
    });

    window.dispatchEvent(new CustomEvent('agent-overflow:open-settings', {
      detail: { section: 'observability' },
    }));

    await waitFor(() => {
      expect(rendered.getByRole('tab', { name: 'Observability' })).toHaveAttribute('aria-selected', 'true');
    });
  });
});
