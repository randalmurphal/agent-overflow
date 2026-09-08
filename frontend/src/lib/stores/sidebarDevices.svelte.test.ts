import { beforeEach, expect, it, vi } from 'vitest';

const KEY = 'agent-overflow:frontend:sidebar-hidden-computers';
beforeEach(() => { vi.resetModules(); localStorage.clear(); });

it('loads frontend exclusions before the first render and follows identity across routes, rename and offline', async () => {
  localStorage.setItem(KEY, JSON.stringify(['computer-id']));
  const store = await import('./sidebarDevices.svelte');
  const { stageBackend, resetStagedBackends } = await import('../../test/helpers/backends');
  const { setBackendIdentityFromBootstrap } = await import('../transport/backendIdentity');
  const host = stageBackend({ id: 'lan', backendId: 'computer-id', nickname: '', name: 'Workhorse' });
  expect(store.sidebarBackendVisible('lan')).toBe(false);
  host.setStatus('disconnected');
  expect(store.sidebarBackendVisible('lan')).toBe(false);
  setBackendIdentityFromBootstrap('computer-id', 'gen', 'Renamed', 'lan');
  expect(store.sidebarDevices().find((device) => device.id === 'computer-id')?.name).toBe('Renamed');
  stageBackend({ id: 'tailnet', backendId: 'computer-id' });
  expect(store.sidebarBackendVisible('tailnet')).toBe(false);
  expect(store.sidebarDevices().filter((device) => device.id === 'computer-id')).toHaveLength(1);
  stageBackend({ id: 'new', backendId: 'new-computer' });
  expect(store.sidebarBackendVisible('new')).toBe(true);
  resetStagedBackends();
  expect(store.sidebarDeviceFilterActive()).toBe(false);
  expect(store.sidebarDevices().some((device) => device.id === 'computer-id')).toBe(false);
});

it('persists UUIDs locally and reacts to this frontend’s other windows without modifying transport', async () => {
  const store = await import('./sidebarDevices.svelte');
  const { stageBackend, resetStagedBackends } = await import('../../test/helpers/backends');
  const host = stageBackend({ id: 'mutable-route', backendId: 'stable-id' });
  store.setSidebarDeviceVisible('stable-id', false);
  expect(JSON.parse(localStorage.getItem(KEY)!)).toEqual(['stable-id']);
  expect(store.sidebarBackendVisible('mutable-route')).toBe(false);
  localStorage.setItem(KEY, '[]');
  window.dispatchEvent(new StorageEvent('storage', { key: KEY, storageArea: localStorage }));
  expect(store.sidebarBackendVisible('mutable-route')).toBe(true);
  expect(host.reconnect).not.toHaveBeenCalled();
  resetStagedBackends();
});

it('keeps the previous filter if saving fails and All computers also reveals future hosts', async () => {
  const store = await import('./sidebarDevices.svelte');
  const { stageBackend, resetStagedBackends } = await import('../../test/helpers/backends');
  stageBackend({ id: 'remote', backendId: 'stable-id' });
  const writes = vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('full'); });
  store.setSidebarDeviceVisible('stable-id', false);
  expect(store.sidebarBackendVisible('remote')).toBe(true);
  writes.mockRestore();
  store.setSidebarDeviceVisible('stable-id', false);
  store.showAllSidebarDevices();
  expect(store.sidebarBackendVisible('remote')).toBe(true);
  stageBackend({ id: 'future', backendId: 'future-id' });
  expect(store.sidebarBackendVisible('future')).toBe(true);
  resetStagedBackends();
});
