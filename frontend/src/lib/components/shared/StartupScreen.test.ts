import { describe, expect, it } from 'vitest';
import { render } from '@testing-library/svelte';
import { tick } from 'svelte';
import StartupScreen from './StartupScreen.svelte';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';

const report = {
  phase: 'store.migrate',
  detail: 'Applying migration 3 of 7 add_index',
  step: 3,
  steps: 7,
  elapsedMs: 72_000,
  updatingTo: '',
};

describe('<StartupScreen>', () => {
  it('stays blank on an ordinary boot', async () => {
    const view = render(StartupScreen);
    await tick();
    expect(view.getByTestId('startup-screen')).toBeInTheDocument();
    expect(view.queryByTestId('startup-screen-status')).toBeNull();
  });

  it('shows the boot phase and progress while the computer is starting', async () => {
    __setTransportStatusForTest({ status: 'starting', nextAttemptAt: null, startup: report });
    const view = render(StartupScreen);
    await tick();
    expect(view.getByTestId('startup-screen-status')).toHaveTextContent('Starting Agent Overflow');
    expect(view.getByTestId('startup-screen-phase')).toHaveTextContent('Applying migration 3 of 7 add_index');
    expect(view.getByTestId('startup-screen-meta')).toHaveTextContent('Step 3 of 7 · 1:12 elapsed');

    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await tick();
    expect(view.queryByTestId('startup-screen-status')).toBeNull();
  });

  it('presents a boot right after an update as finishing that update', async () => {
    __setTransportStatusForTest({
      status: 'starting', nextAttemptAt: null, startup: { ...report, updatingTo: 'v1.4.0' },
    });
    const view = render(StartupScreen);
    await tick();
    expect(view.getByTestId('startup-screen-status')).toHaveTextContent('Updating Agent Overflow');
    expect(view.getByTestId('startup-screen-phase'))
      .toHaveTextContent('Finishing update to v1.4.0: applying migration 3 of 7 add_index');
  });
});
