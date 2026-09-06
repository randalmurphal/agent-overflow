import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { pairViewOnly, resetToLocalPage } from '../../test/helpers/scopes';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { attachedBackends, backendById } from '../transport/backends';
import { __resetManifestBackendsForTest, manifestBackendDescriptors } from '../transport/manifestBackends';
import {
  __resetSystemsForTest,
  addSystem,
  applyBackendAttach,
  applyBackendSetChange,
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

  it('does not resurrect a removed computer from a stale list or attachment result', async () => {
    let reply!: (rows: typeof LAPTOP[]) => void;
    let calls = 0;
    setBindingMock('ListBackends', () => ++calls === 1
      ? new Promise<typeof LAPTOP[]>((resolve) => { reply = resolve; }) : Promise.resolve([]));
    setBindingMock('RemoveBackend', async () => {});
    const loading = loadSystems();
    await removeSystem('laptop');
    reply([LAPTOP]);
    await loading;
    expect(getSystems()).toEqual([]);
    expect(backendById('laptop')).toBeUndefined();
    applyBackendAttach({ id: 'laptop', attached: true });
    await loadSystems();
    expect(backendById('laptop')).toBeUndefined();
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

  // The event hub subscribes EVERY attached backend, so this handler is
  // reachable from a machine that is not the one whose profile directory
  // the four system RPCs act on, and the descriptor it would build names
  // this machine's own proxy path. A frame from anywhere but home would
  // register a door home does not serve.
  it('says nothing about a frame that arrived on another backend', async () => {
    setBindingMock('AddBackend', async () => ({
      id: 'laptop', name: 'Laptop', endpoint: LAPTOP.endpoint, verificationNumber: '42',
    }));
    await addSystem('link');

    expect(applyBackendAttach({ id: 'laptop', attached: true }, 'desktop')).toBeNull();
    // The pending row survives: the pairing home is waiting on has not
    // been answered, and retiring it would drop the confirmation UI.
    expect(getPendingAttachments().map((p) => p.id)).toEqual(['laptop']);
    expect(backendById('laptop')).toBeUndefined();
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
    expect(manifestBackendDescriptors().find((row) => row.id === 'laptop')?.nickname).toBe('');
    const rename = setBindingMock('RenameBackend', async () => {});
    await renameSystem('laptop', 'Work laptop');
    expect(manifestBackendDescriptors().find((row) => row.id === 'laptop')?.nickname).toBe('Work laptop');
    expect(rename).toHaveBeenCalledWith('laptop', 'Work laptop');
    expect(systemLabel(getSystems()[0])).toBe('Work laptop');
    expect(systemLabel({ id: 'x', name: 'Named', nickname: '' })).toBe('Named');
    expect(systemLabel({ id: 'x', name: '', nickname: '' })).toBe('x');
  });

  // `backend:attach` reported only how a pairing ENDED, so a removal or a
  // rename made in one window left every other page on this host showing a
  // machine that is gone, or the superseded nickname, until reload.
  it('drops a row another page removed', async () => {
    stageBackend();
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();

    applyBackendSetChange({ action: 'removed', id: 'laptop' });

    expect(getSystems()).toEqual([]);
    // The same purge a local removeSystem does: the door is closed too, not
    // just the row forgotten.
    expect(attachedBackends().some((b) => b.id === 'laptop')).toBe(false);
  });

  it('takes a rename another page made', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();

    applyBackendSetChange({ action: 'renamed', id: 'laptop', nickname: 'Work laptop' });

    expect(systemLabel(getSystems()[0])).toBe('Work laptop');
  });

  it('clears a nickname a rename emptied', async () => {
    setBindingMock('ListBackends', async () => [{ ...LAPTOP, nickname: 'Old' }]);
    await loadSystems();

    applyBackendSetChange({ action: 'renamed', id: 'laptop' });

    expect(systemLabel(getSystems()[0])).toBe('Laptop');
  });

  // The event hub subscribes EVERY attached backend, and these four RPCs act
  // on THIS machine's profile directory: another backend's frame names an id
  // in its own directory, which would drop the wrong row here.
  it('refuses a frame that did not come from home', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();

    applyBackendSetChange({ action: 'removed', id: 'laptop' }, 'desktop');

    expect(getSystems().map((s) => s.id)).toEqual(['laptop']);
  });

  it('ignores an unnamed row and an action it does not know', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();

    applyBackendSetChange({ action: 'removed', id: '' });
    applyBackendSetChange({ action: 'attached' as never, id: 'laptop' });

    expect(getSystems().map((s) => s.id)).toEqual(['laptop']);
  });
});
