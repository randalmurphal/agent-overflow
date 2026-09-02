// Settings → Updates, the supervised-machine cards. The section is a
// projection of the serviceUpdate store, so the cases drive that store
// through its own channels and assert what the card says at each phase.

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import MachineUpdates from './MachineUpdates.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { emitWailsEvent, resetWailsMocks } from '../../../test/mocks/wailsio-runtime';
import { REMOTE_BACKEND_UUID, resetStagedBackends, stageBackend } from '../../../test/helpers/backends';
import {
  initServiceUpdates,
  resetServiceUpdatesForTest,
  type ServiceUpdateStatus,
} from '../../stores/serviceUpdate.svelte';

function status(overrides: Partial<ServiceUpdateStatus> = {}): ServiceUpdateStatus {
  return {
    supervised: true,
    available: true,
    currentVersion: '1.2.0',
    phase: 'idle',
    ...overrides,
  } as ServiceUpdateStatus;
}

describe('<MachineUpdates>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetWailsMocks();
    resetServiceUpdatesForTest();
    resetStagedBackends();
    initServiceUpdates();
  });

  afterEach(() => {
    cleanup();
    resetServiceUpdatesForTest();
    resetStagedBackends();
  });

  it('renders nothing until a machine reports a supervisor', async () => {
    const { container } = render(MachineUpdates);
    expect(container.textContent).toBe('');
    emitWailsEvent('service:update-status', status({ supervised: false }));
    await waitFor(() => expect(container.textContent).toBe(''));
  });

  it('names each supervised machine and what it runs', async () => {
    stageBackend({ name: 'Laptop' });
    const { findAllByTestId } = render(MachineUpdates);
    emitWailsEvent('service:update-status', status({ currentVersion: '1.2.0' }));
    emitWailsEvent('service:update-status', status({ currentVersion: '1.1.0' }), REMOTE_BACKEND_UUID);
    const cards = await findAllByTestId('machine-update');
    expect(cards).toHaveLength(2);
    expect(cards[1].textContent).toContain('Laptop');
    expect(cards[1].textContent).toContain('Running 1.1.0');
    expect(cards[0].textContent).toContain('Running 1.2.0');
  });

  it('says why a supervised host cannot update, with no button', async () => {
    const { findByTestId } = render(MachineUpdates);
    emitWailsEvent(
      'service:update-status',
      status({
        available: false,
        latestVersion: '1.3.0',
        unavailable: 'This build has no release artifact a supervisor can stage.',
      }),
    );
    const card = await findByTestId('machine-update');
    expect(card.textContent).toContain('no release artifact');
    expect(card.querySelector('button')).toBeNull();
  });

  it('offers the newer release and sends its tag to that machine', async () => {
    stageBackend();
    const request = setBindingMock('RequestServiceUpdate', async () => undefined);
    const { findByText } = render(MachineUpdates);
    emitWailsEvent(
      'service:update-status',
      status({ latestVersion: '1.3.0', latestTag: 'v1.3.0' }),
      REMOTE_BACKEND_UUID,
    );
    await fireEvent.click(await findByText('Update to 1.3.0'));
    await waitFor(() => expect(request).toHaveBeenCalledWith('v1.3.0'));
  });

  it('follows the flow live and hides the picker while it runs', async () => {
    const { findByTestId, queryByText } = render(MachineUpdates);
    emitWailsEvent('service:update-status', status({ latestVersion: '1.3.0', latestTag: 'v1.3.0' }));
    const card = await findByTestId('machine-update');
    expect(queryByText('Install a specific version')).not.toBeNull();

    emitWailsEvent(
      'service:update-status',
      status({ phase: 'downloading', targetTag: 'v1.3.0', written: 5 * 1024 * 1024, total: 10 * 1024 * 1024 }),
    );
    await waitFor(() => expect(card.textContent).toContain('Downloading…'));
    expect(card.textContent).toContain('5.0 / 10.0 MB');
    expect(queryByText('Install a specific version')).toBeNull();
    expect(queryByText('Update to 1.3.0')).toBeNull();

    emitWailsEvent(
      'service:update-status',
      status({ phase: 'requested', targetTag: 'v1.3.0', targetVersion: '1.3.0', updateId: 'u-1' }),
    );
    await waitFor(() => expect(card.textContent).toContain('Restarting into version 1.3.0…'));
  });

  it('reports how the last update ended, and a flow that failed', async () => {
    const { findByTestId } = render(MachineUpdates);
    emitWailsEvent('service:update-status', status({ currentVersion: '1.3.0' }));
    emitWailsEvent('service:update-outcome', { updateId: 'u-1', outcome: 'committed', version: '1.3.0' });
    const card = await findByTestId('machine-update');
    await waitFor(() => expect(card.textContent).toContain('Updated to version 1.3.0.'));

    emitWailsEvent('service:update-outcome', {
      updateId: 'u-2',
      outcome: 'rolled-back',
      version: '1.4.0',
      reason: 'The new version did not come up.',
    });
    await waitFor(() =>
      expect(card.textContent).toContain(
        'The update to version 1.4.0 was rolled back. The new version did not come up.',
      ),
    );

    emitWailsEvent(
      'service:update-status',
      status({ phase: 'error', error: 'verify: not an Agent Overflow binary' }),
    );
    await waitFor(() => expect(card.textContent).toContain('verify: not an Agent Overflow binary'));
  });

  it('loads that machine’s releases when its picker opens', async () => {
    stageBackend();
    const list = setBindingMock('ListServiceReleases', async () => []);
    const { findAllByText } = render(MachineUpdates);
    emitWailsEvent('service:update-status', status(), REMOTE_BACKEND_UUID);
    const [summary] = await findAllByText('Install a specific version');
    const details = summary.closest('details') as HTMLDetailsElement;
    details.open = true;
    await fireEvent(details, new Event('toggle'));
    await waitFor(() => expect(list).toHaveBeenCalledTimes(1));
  });
});
