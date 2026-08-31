import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DevicesSection from './DevicesSection.svelte';
import { setBindingMock, getBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { setRunMode, resetRunMode } from '../../../test/runMode';

interface MockDevice {
  id: string;
  label: string;
  class: string;
  platform?: string;
  channel?: string;
  createdAtMs: number;
  lastSeenAtMs?: number;
  revokedAtMs?: number;
  sessions?: Array<{
    id: string;
    binding: string;
    awaitingConfirmation?: boolean;
    createdAtMs: number;
    expiresAtMs: number;
    connections?: number;
    lastUsedAtMs?: number;
  }>;
}

function overview(overrides: {
  devices?: MockDevice[];
  pendingPairings?: unknown[];
  audit?: unknown[];
} = {}) {
  return {
    devices: overrides.devices ?? [],
    pendingPairings: overrides.pendingPairings ?? [],
    audit: overrides.audit ?? [],
  };
}

const LOCAL_DEVICE: MockDevice = {
  id: 'dev-local',
  label: 'Desktop app',
  class: 'desktop',
  channel: 'local',
  createdAtMs: 1000,
};

const PHONE: MockDevice = {
  id: 'dev-phone',
  label: "Randal's phone",
  class: 'phone',
  platform: 'ios',
  createdAtMs: 2000,
  lastSeenAtMs: Date.now() - 60_000,
  sessions: [
    {
      id: 'ses-1',
      binding: 'device-bound',
      createdAtMs: 2000,
      expiresAtMs: Date.now() + 3_600_000,
      connections: 1,
    },
  ],
};

describe('<DevicesSection>', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetRunMode();
    setBindingMock('GetNetworkSettings', async () => ({ bindAll: false, url: '', token: '' }));
  });

  afterEach(() => {
    resetBindingMocks();
    resetRunMode();
  });

  it('renders the local channel as this computer, with no revoke control', async () => {
    setBindingMock('GetAccessOverview', async () => overview({ devices: [LOCAL_DEVICE] }));
    const { findByText, queryByRole } = render(DevicesSection);
    await findByText('This computer');
    expect(queryByRole('button', { name: 'Revoke' })).toBeNull();
  });

  it('revokes a paired device only on the second, arming click', async () => {
    setBindingMock('GetAccessOverview', async () => overview({ devices: [LOCAL_DEVICE, PHONE] }));
    const revoke = setBindingMock('RevokeAccessDevice', async () => undefined);
    const { findByRole } = render(DevicesSection);

    const button = await findByRole('button', { name: 'Revoke' });
    await fireEvent.click(button);
    expect(revoke).not.toHaveBeenCalled();

    const armed = await findByRole('button', { name: 'Confirm revoke' });
    await fireEvent.click(armed);
    await waitFor(() => expect(revoke).toHaveBeenCalledWith('dev-phone'));
  });

  it('ends a single session through its own two-step control', async () => {
    setBindingMock('GetAccessOverview', async () => overview({ devices: [PHONE] }));
    const revoke = setBindingMock('RevokeAccessSession', async () => undefined);
    const { findByRole } = render(DevicesSection);

    await fireEvent.click(await findByRole('button', { name: 'End' }));
    await fireEvent.click(await findByRole('button', { name: 'Confirm end' }));
    await waitFor(() => expect(revoke).toHaveBeenCalledWith('ses-1'));
  });

  it('surfaces a redeemed pairing with its verification number and confirms it', async () => {
    setBindingMock('GetAccessOverview', async () =>
      overview({
        pendingPairings: [
          {
            linkId: 'link-1',
            createdAtMs: 1000,
            expiresAtMs: Date.now() + 300_000,
            redeemed: true,
            deviceLabel: 'iPhone',
            verificationNumber: '481 923',
          },
        ],
      }),
    );
    const confirm = setBindingMock('ConfirmDevicePairing', async () => undefined);
    const { findByText, findByRole } = render(DevicesSection);

    await findByText('481 923');
    await fireEvent.click(await findByRole('button', { name: 'It matches — allow' }));
    await waitFor(() => expect(confirm).toHaveBeenCalledWith('link-1'));
    // Settling an action reloads the overview.
    await waitFor(() =>
      expect(getBindingMock('GetAccessOverview')!.mock.calls.length).toBeGreaterThan(1),
    );
  });

  it('offers cancel on a link nothing has opened yet', async () => {
    setBindingMock('GetAccessOverview', async () =>
      overview({
        pendingPairings: [
          {
            linkId: 'link-2',
            createdAtMs: 1000,
            expiresAtMs: Date.now() + 240_000,
            redeemed: false,
          },
        ],
      }),
    );
    const cancel = setBindingMock('CancelDevicePairing', async () => undefined);
    const { findByRole } = render(DevicesSection);

    await fireEvent.click(await findByRole('button', { name: 'Cancel link' }));
    await waitFor(() => expect(cancel).toHaveBeenCalledWith('link-2'));
  });

  it('lists a revoked device apart, with restore instead of revoke', async () => {
    setBindingMock('GetAccessOverview', async () =>
      overview({
        devices: [
          LOCAL_DEVICE,
          { ...PHONE, revokedAtMs: Date.now() - 3_600_000, sessions: [] },
        ],
      }),
    );
    const restore = setBindingMock('RestoreAccessDevice', async () => undefined);
    const { findByRole, queryByRole, findByTestId } = render(DevicesSection);

    await findByTestId('revoked-device');
    // A revoked row offers restore, never a second revoke.
    expect(queryByRole('button', { name: 'Revoke' })).toBeNull();
    await fireEvent.click(await findByRole('button', { name: 'Restore' }));
    await waitFor(() => expect(restore).toHaveBeenCalledWith('dev-phone'));
  });

  it('renders a pointer instead of controls in client mode', async () => {
    setRunMode('client');
    const { findByText, queryByRole } = render(DevicesSection);
    await findByText(/managed from the backend machine/);
    expect(queryByRole('button', { name: 'Pair a device' })).toBeNull();
  });
});
