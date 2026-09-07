import { afterEach, describe, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, waitFor } from '@testing-library/svelte';
import NearbyComputers from './NearbyComputers.svelte';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend, REMOTE_BACKEND_UUID } from '../../../test/helpers/backends';
import { __resetSystemsForTest, addSystem } from '../../stores/systems.svelte';

const WINDOWS = { backendId: 'windows', name: 'Workstation', address: 'https://workstation.ts.net', network: 'tailnet' };

afterEach(() => { cleanup(); resetBindingMocks(); resetStagedBackends(); __resetSystemsForTest(); });

describe('available computers', () => {
  it('filters existing and pending connections while pairing a named Tailscale computer by its address', async () => {
    stageBackend();
    setBindingMock('DiscoverComputers', async () => [
      { ...WINDOWS, backendId: REMOTE_BACKEND_UUID, name: 'Already paired' },
      WINDOWS,
    ]);
    const add = setBindingMock('AddBackend', async () => ({ id: 'windows', name: WINDOWS.name, endpoint: WINDOWS.address, verificationNumber: '135 791' }));
    const connect = vi.fn(async (address: string) => { await addSystem(address); });
    const view = render(NearbyComputers, { connecting: false, onConnect: connect });
    await fireEvent.click(await view.findByRole('button', { name: 'Connect to Workstation' }));
    expect(connect).toHaveBeenCalledExactlyOnceWith(WINDOWS.address);
    expect(add).toHaveBeenCalledExactlyOnceWith(WINDOWS.address);
    await waitFor(() => expect(view.queryByRole('button', { name: 'Connect to Workstation' })).toBeNull());
    expect(view.queryByText('Already paired')).toBeNull();
  });

  it('shows discovery errors inline and only scans again on explicit refresh', async () => {
    const discover = setBindingMock('DiscoverComputers', async () => { throw new Error('network unavailable'); });
    const view = render(NearbyComputers, { connecting: false, onConnect: vi.fn() });
    expect(await view.findByRole('alert')).toHaveTextContent('network unavailable');
    expect(discover).toHaveBeenCalledTimes(1);
    discover.mockResolvedValue([WINDOWS]);
    await fireEvent.click(view.getByRole('button', { name: 'Refresh' }));
    await view.findByRole('button', { name: 'Connect to Workstation' });
    expect(view.queryByRole('alert')).toBeNull();
    expect(discover).toHaveBeenCalledTimes(2);
  });
});
