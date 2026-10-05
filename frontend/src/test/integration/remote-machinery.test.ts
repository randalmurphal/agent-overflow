// App boot starts the machinery that serves only other computers in the
// standard build and none of it in a build without remote access. The
// installers are wrapped rather than replaced, so the standard case still
// runs them for real.

import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { render } from '@testing-library/svelte';
import App from '../../App.svelte';
import { flush, installAnimateShim, installAppDefaults, resetAppState } from './_helpers';
import { stageNoRemoteBuild } from '../helpers/buildVariant';

vi.mock('../../lib/stores/serviceUpdate.svelte', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../lib/stores/serviceUpdate.svelte')>();
  return { ...mod, initServiceUpdates: vi.fn(mod.initServiceUpdates) };
});
vi.mock('../../lib/stores/conversationTransfers.svelte', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../lib/stores/conversationTransfers.svelte')>();
  return { ...mod, initConversationTransfers: vi.fn(mod.initConversationTransfers) };
});
vi.mock('../../lib/stores/devServers.svelte', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../lib/stores/devServers.svelte')>();
  return { ...mod, initDevServers: vi.fn(mod.initDevServers) };
});
vi.mock('../../lib/stores/ownDevices.svelte', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../lib/stores/ownDevices.svelte')>();
  return { ...mod, installOwnDeviceSync: vi.fn(mod.installOwnDeviceSync) };
});
vi.mock('../../lib/stores/computerRouteUpdates', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../lib/stores/computerRouteUpdates')>();
  return { ...mod, installComputerRouteUpdates: vi.fn(mod.installComputerRouteUpdates) };
});
vi.mock('../../lib/stores/deviceNames', async (importOriginal) => {
  const mod = await importOriginal<typeof import('../../lib/stores/deviceNames')>();
  return { ...mod, installDeviceNameSync: vi.fn(mod.installDeviceNameSync) };
});

import { initServiceUpdates } from '../../lib/stores/serviceUpdate.svelte';
import { initConversationTransfers } from '../../lib/stores/conversationTransfers.svelte';
import { initDevServers } from '../../lib/stores/devServers.svelte';
import { installOwnDeviceSync } from '../../lib/stores/ownDevices.svelte';
import { installComputerRouteUpdates } from '../../lib/stores/computerRouteUpdates';
import { installDeviceNameSync } from '../../lib/stores/deviceNames';

const REMOTE_ONLY = [
  initServiceUpdates,
  initConversationTransfers,
  initDevServers,
  installOwnDeviceSync,
  installComputerRouteUpdates,
];

beforeAll(installAnimateShim);

describe('App boot machinery', () => {
  beforeEach(() => {
    resetAppState();
    installAppDefaults();
    for (const fn of [...REMOTE_ONLY, installDeviceNameSync]) vi.mocked(fn).mockClear();
  });

  it('starts the remote machinery in the standard build', async () => {
    const view = render(App);
    await flush(5);
    for (const fn of REMOTE_ONLY) expect(fn).toHaveBeenCalledTimes(1);
    expect(installDeviceNameSync).toHaveBeenCalledTimes(1);
    view.unmount();
  });

  it('starts none of it in a build without remote access', async () => {
    stageNoRemoteBuild();
    const view = render(App);
    await flush(5);
    for (const fn of REMOTE_ONLY) expect(fn).not.toHaveBeenCalled();
    // It also carries this computer's own name changes.
    expect(installDeviceNameSync).toHaveBeenCalledTimes(1);
    view.unmount();
  });
});
