import { describe, expect, it, beforeEach, afterEach } from 'vitest';
import { render, fireEvent, waitFor } from '@testing-library/svelte';
import DevicesSection from './DevicesSection.svelte';
import { setBindingMock, getBindingMock, resetBindingMocks } from '../../../test/mocks/bindings-app';
import { setRunMode, resetRunMode } from '../../../test/runMode';
import { getToasts } from '../../stores/toast.svelte';

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
    scopes?: string[];
    survivedRevocation?: boolean;
  }>;
}

// The observe tier, as the backend grants it for a `view-only` link
// (internal/transport/scopes.go). Spelled out rather than imported so the
// fixture states what the wire carries.
const OBSERVE: string[] = ['threads:read', 'files:read', 'settings:read'];

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
    // The passkeys block mounts inside the granted branch and loads its
    // own list, so every case here reaches it. Empty: what it renders is
    // PasskeysBlock.test.ts's subject, not this file's.
    setBindingMock('ListPasskeys', async () => []);
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
    const revoke = setBindingMock('RevokeAccessDevice', async () => ({
      deviceMoved: true,
      sessionsEnded: 1,
      connectionsClosed: 2,
    }));
    const { findByRole } = render(DevicesSection);

    const button = await findByRole('button', { name: 'Revoke' });
    await fireEvent.click(button);
    expect(revoke).not.toHaveBeenCalled();

    const armed = await findByRole('button', { name: 'Confirm revoke' });
    await fireEvent.click(armed);
    await waitFor(() => expect(revoke).toHaveBeenCalledWith('dev-phone'));
  });

  // A revoke that swept nothing has to say so. Reporting success uniformly
  // is how a device that kept access went unnoticed
  // (docs/specs/remote-access.md §2).
  it('reports what a revoke actually did, including when it did nothing', async () => {
    setBindingMock('GetAccessOverview', async () => overview({ devices: [PHONE] }));
    setBindingMock('RevokeAccessDevice', async () => ({
      deviceMoved: true,
      sessionsEnded: 2,
      connectionsClosed: 1,
    }));
    const { findByRole } = render(DevicesSection);
    await fireEvent.click(await findByRole('button', { name: 'Revoke' }));
    await fireEvent.click(await findByRole('button', { name: 'Confirm revoke' }));
    await waitFor(() =>
      expect(getToasts().at(-1)?.message).toBe(
        "Revoked Randal's phone. 2 sessions ended, 1 connection closed.",
      ),
    );

    setBindingMock('RevokeAccessDevice', async () => ({
      deviceMoved: false,
      sessionsEnded: 0,
      connectionsClosed: 0,
    }));
    await fireEvent.click(await findByRole('button', { name: 'Revoke' }));
    await fireEvent.click(await findByRole('button', { name: 'Confirm revoke' }));
    await waitFor(() =>
      expect(getToasts().at(-1)?.message).toBe(
        "Randal's phone was already revoked. Nothing was live.",
      ),
    );
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

  it('labels a device by what its session was actually granted', async () => {
    setBindingMock('GetAccessOverview', async () =>
      overview({
        devices: [
          { ...PHONE, sessions: [{ ...PHONE.sessions![0]!, scopes: OBSERVE }] },
          {
            ...PHONE,
            id: 'dev-laptop',
            label: 'A laptop',
            platform: 'macos',
            sessions: [
              { ...PHONE.sessions![0]!, id: 'ses-2', scopes: [...OBSERVE, 'threads:operate'] },
            ],
          },
        ],
      }),
    );
    const { findByText, queryByText } = render(DevicesSection);

    // The label rides the GRANT SET, never the device class: both rows
    // here are the same class and only one is read-only.
    await findByText(/ios · View only · connected now/);
    expect(queryByText(/macos · View only/)).toBeNull();
    await findByText(/macos · connected now/);
  });

  it('says so when a paired device holds nothing', async () => {
    setBindingMock('GetAccessOverview', async () =>
      overview({ devices: [{ ...PHONE, sessions: [] }] }),
    );
    const { findByText } = render(DevicesSection);
    // Used to read exactly like a device that was connected.
    await findByText(/ios · signed out/);
  });

  it('renders a credential that outlived its revocation as the anomaly it is', async () => {
    const survivor = {
      id: 'ses-standing',
      binding: 'device-bound',
      createdAtMs: 2000,
      expiresAtMs: Date.now() + 3_600_000,
      connections: 2,
      survivedRevocation: true,
    };
    setBindingMock('GetAccessOverview', async () =>
      overview({
        devices: [{ ...PHONE, revokedAtMs: Date.now() - 3_600_000, sessions: [survivor] }],
      }),
    );
    const end = setBindingMock('RevokeAccessSession', async () => undefined);
    const { findByRole, findByTestId, findByText } = render(DevicesSection);

    const note = await findByTestId('revoked-device-standing');
    expect(note.textContent).toMatch(/still standing/);
    await findByText(/2 connections/);

    // And the way out is on the row: end the credential that survived.
    await fireEvent.click(await findByRole('button', { name: 'End' }));
    await fireEvent.click(await findByRole('button', { name: 'Confirm end' }));
    await waitFor(() => expect(end).toHaveBeenCalledWith('ses-standing'));
  });

  it('forgets a revoked device on the second click, never the first', async () => {
    setBindingMock('GetAccessOverview', async () =>
      overview({
        devices: [{ ...PHONE, revokedAtMs: Date.now() - 3_600_000, sessions: [] }],
      }),
    );
    const forget = setBindingMock('ForgetAccessDevice', async () => undefined);
    const { findByRole } = render(DevicesSection);

    // Arming only. Deleting a row is destructive, so it takes the same
    // two steps a revoke does.
    await fireEvent.click(await findByRole('button', { name: 'Remove' }));
    expect(forget).not.toHaveBeenCalled();
    await fireEvent.click(await findByRole('button', { name: 'Confirm remove' }));
    await waitFor(() => expect(forget).toHaveBeenCalledWith('dev-phone'));
  });

  it('renders a pointer instead of controls in client mode', async () => {
    setRunMode('client');
    const { findByText, queryByRole } = render(DevicesSection);
    await findByText(/managed from the backend machine/);
    expect(queryByRole('button', { name: 'Pair a device' })).toBeNull();
  });

  it('refreshes reachability when a tailnet joins after the section mounted', async () => {
    setBindingMock('GetAccessOverview', async () => overview());
    let running = false;
    const network = setBindingMock('GetNetworkSettings', async () => ({
      bindAll: false, tailnet: { running, dnsName: 'ao.test.ts.net', https: true },
    }));
    const { findByRole, queryByText } = render(DevicesSection);
    await waitFor(() => expect(network).toHaveBeenCalled());
    running = true;
    await fireEvent.click(await findByRole('button', { name: /Pair a device/i }));
    await findByRole('button', { name: /Phone or tablet/ });
    await waitFor(() => expect(network).toHaveBeenCalledTimes(2));
    expect(queryByText(/currently reaches this computer only/)).toBeNull();
  });
});
