import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { pairViewOnly, resetToLocalPage } from '../../test/helpers/scopes';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { attachedBackends, backendById } from '../transport/backends';
import { __resetManifestBackendsForTest } from '../transport/manifestBackends';
import {
  __resetSystemsForTest,
  addSystem,
  applyBackendAttach,
  getPendingAttachments,
  getSystems,
  loadSystems,
  removeSystem,
  renameSystem,
  systemLabel,
  systemsLoaded,
} from './systems.svelte';

const LAPTOP = {
  id: 'laptop',
  backendId: '99999999-8888-4777-8666-555555555555',
  name: 'Laptop',
  nickname: '',
  endpoint: 'https://laptop.example:8123',
  lastReachedMs: 0,
};

describe('systems store', () => {
  beforeEach(() => {
    resetBindingMocks();
    __resetSystemsForTest();
    resetStagedBackends();
    __resetManifestBackendsForTest();
  });

  afterEach(() => {
    resetToLocalPage();
    resetStagedBackends();
    __resetManifestBackendsForTest();
  });

  it('loads the list once, and not at all for a session without host', async () => {
    const list = setBindingMock('ListBackends', async () => [LAPTOP]);
    await Promise.all([loadSystems(), loadSystems()]);
    expect(list).toHaveBeenCalledTimes(1);
    expect(getSystems()).toEqual([LAPTOP]);
    expect(systemsLoaded()).toBe(true);

    __resetSystemsForTest();
    await pairViewOnly();
    await loadSystems();
    expect(list).toHaveBeenCalledTimes(1);
    expect(systemsLoaded()).toBe(false);
  });

  it('holds a pairing as pending until backend:attach retires it, then opens the door', async () => {
    setBindingMock('AddBackend', async () => ({
      id: 'laptop', name: 'Laptop', endpoint: LAPTOP.endpoint, verificationNumber: '42',
    }));
    const list = setBindingMock('ListBackends', async () => [LAPTOP]);
    const row = await addSystem('https://laptop.example:8123/pair#tok');
    expect(row.verificationNumber).toBe('42');
    expect(getPendingAttachments()).toEqual([row]);

    const outcome = applyBackendAttach({ id: 'laptop', attached: true });
    expect(outcome).toEqual({ name: 'Laptop', error: '' });
    expect(getPendingAttachments()).toEqual([]);
    // The transport registry learned the door from the event, not from a
    // manifest re-fetch.
    expect(backendById('laptop')?.name).toBe('Laptop');
    await Promise.resolve();
    expect(list).toHaveBeenCalled();
  });

  it('reports a refused pairing by name and attaches nothing', async () => {
    setBindingMock('AddBackend', async () => ({
      id: 'laptop', name: 'Laptop', endpoint: LAPTOP.endpoint, verificationNumber: '42',
    }));
    await addSystem('link');
    const outcome = applyBackendAttach({ id: 'laptop', attached: false, error: 'declined' });
    expect(outcome).toEqual({ name: 'Laptop', error: 'declined' });
    expect(getPendingAttachments()).toEqual([]);
    expect(backendById('laptop')).toBeUndefined();
  });

  it('detaches the registry entry when a system is removed', async () => {
    stageBackend();
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();
    const remove = setBindingMock('RemoveBackend', async () => {});
    await removeSystem('laptop');
    expect(remove).toHaveBeenCalledWith('laptop');
    expect(getSystems()).toEqual([]);
    expect(attachedBackends().some((b) => b.id === 'laptop')).toBe(false);
  });

  it('renames in place and labels by nickname first', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();
    const rename = setBindingMock('RenameBackend', async () => {});
    await renameSystem('laptop', 'Work laptop');
    expect(rename).toHaveBeenCalledWith('laptop', 'Work laptop');
    expect(systemLabel(getSystems()[0])).toBe('Work laptop');
    expect(systemLabel({ id: 'x', name: 'Named', nickname: '' })).toBe('Named');
    expect(systemLabel({ id: 'x', name: '', nickname: '' })).toBe('x');
  });
});
