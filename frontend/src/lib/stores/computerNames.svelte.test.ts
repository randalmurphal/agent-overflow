import { beforeEach, afterEach, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { stageBackend, resetStagedBackends, REMOTE_BACKEND_UUID } from '../../test/helpers/backends';
import { attachedBackendEntry, backendDisplayName, backendNickname, setBackendNickname } from './attachedBackends.svelte';
import { setBackendIdentityFromBootstrap, __resetBackendIdentityForTest } from '../transport/backendIdentity';
import { storeBackendEndpoint, __resetHomeEndpointForTest } from '../transport/homeEndpoint';

function reloadNames(): void {
  window.dispatchEvent(new StorageEvent('storage', { key: 'agent-overflow:frontend:computer-nicknames', storageArea: localStorage }));
}
beforeEach(() => {
  resetStagedBackends(); __resetBackendIdentityForTest(); __resetHomeEndpointForTest();
  localStorage.clear(); reloadNames();
});
afterEach(() => { resetStagedBackends(); __resetBackendIdentityForTest(); __resetHomeEndpointForTest(); });

it('replaces a phone’s saved address label with its host name, including offline restores', () => {
  storeBackendEndpoint('mac', 'https://192.168.1.55:60522');
  stageBackend({ id: 'mac', name: '192.168.1.55:60522' });
  const entry = attachedBackendEntry('mac')!;
  expect(backendDisplayName(entry)).toBe('192.168.1.55:60522');
  setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'generation', 'Randy’s Mac', 'mac');
  expect(backendDisplayName(entry)).toBe('Randy’s Mac');
  // Forget only live identity, as a fresh page starts before bootstrap.
  __resetBackendIdentityForTest();
  expect(backendDisplayName(entry)).toBe('Randy’s Mac');
});

it('stores nicknames on this frontend by identity and clearing restores the hostname', () => {
  storeBackendEndpoint('mac', 'https://192.168.1.55:60522');
  stageBackend({ id: 'mac', name: '192.168.1.55:60522' });
  setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'generation', 'Mac', 'mac');
  const entry = attachedBackendEntry('mac')!;
  expect(setBackendNickname('mac', '  Workhorse  ')).toBe(true);
  expect(backendDisplayName(entry)).toBe('Workhorse');
  expect(backendNickname('mac')).toBe('Workhorse');
  // Another route/registry slot for the same host retains the local name.
  stageBackend({ id: 'tailnet-mac', backendId: REMOTE_BACKEND_UUID, name: 'Mac' });
  expect(backendNickname('tailnet-mac')).toBe('Workhorse');
  expect(setBackendNickname('mac', '')).toBe(true);
  expect(backendDisplayName(entry)).toBe('Mac');
});

it('updates reactive labels on identity arrival and cross-window nickname changes', () => {
  storeBackendEndpoint('mac', 'https://192.168.1.55:60522');
  stageBackend({ id: 'mac', name: '192.168.1.55:60522' });
  let label = '';
  const cleanup = $effect.root(() => { $effect(() => { label = backendDisplayName(attachedBackendEntry('mac')!); }); });
  try {
    flushSync();
    expect(label).toBe('192.168.1.55:60522');
    setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'generation', 'Mac', 'mac');
    flushSync(); expect(label).toBe('Mac');
    localStorage.setItem('agent-overflow:frontend:computer-nicknames', JSON.stringify([[REMOTE_BACKEND_UUID, 'Desk']]));
    reloadNames(); flushSync(); expect(label).toBe('Desk');
  } finally { cleanup(); }
});

it('preserves existing desktop profile nicknames and rejects overlong local names', () => {
  stageBackend({ id: 'mac', name: 'Legacy nickname' });
  setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'generation', 'Mac', 'mac');
  expect(backendDisplayName(attachedBackendEntry('mac')!)).toBe('Legacy nickname');
  expect(() => setBackendNickname('mac', 'x'.repeat(81))).toThrow('80 characters');
  expect(backendNickname('mac')).toBe('');
});

it('names HOME by its actual identity without carrying the nickname to a different host', () => {
  setBackendIdentityFromBootstrap('first-home', 'generation', 'First Mac');
  expect(setBackendNickname('', 'Desk')).toBe(true);
  expect(backendDisplayName(attachedBackendEntry('')!)).toBe('Desk');
  setBackendIdentityFromBootstrap('different-home', 'generation', 'Second Mac');
  expect(backendNickname('')).toBe('');
  expect(backendDisplayName(attachedBackendEntry('')!)).toBe('Second Mac');
});

it('resolves address/identity storage once for every rendered label on a computer', () => {
  storeBackendEndpoint('mac', 'https://192.168.1.55:60522');
  stageBackend({ id: 'mac', name: '192.168.1.55:60522' });
  setBackendIdentityFromBootstrap(REMOTE_BACKEND_UUID, 'generation', 'Mac', 'mac');
  const entry = attachedBackendEntry('mac')!;
  expect(backendDisplayName(entry)).toBe('Mac');
  const reads = vi.spyOn(localStorage, 'getItem');
  try {
    for (let i = 0; i < 100; i++) expect(backendDisplayName(entry)).toBe('Mac');
    expect(reads).not.toHaveBeenCalled();
  } finally { reads.mockRestore(); }
});

it('leaves a nickname unchanged when frontend persistence fails', () => {
  stageBackend({ id: 'mac' });
  setBackendNickname('mac', 'Desk');
  const writes = vi.spyOn(localStorage, 'setItem').mockImplementation(() => { throw new Error('full'); });
  try {
    expect(setBackendNickname('mac', 'Lost')).toBe(false);
    expect(backendNickname('mac')).toBe('Desk');
  } finally { writes.mockRestore(); }
});
