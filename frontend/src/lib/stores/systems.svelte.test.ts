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
  loadSystems,
  removeSystem,
  systemLabel,
  systemsLoaded,
  systemStatus,
} from './systems.svelte';
import { getToasts, removeToast } from './toast.svelte';

const LAPTOP = {
  id: 'laptop',
  backendId: '99999999-8888-4777-8666-555555555555',
  name: 'Laptop',
  nickname: '',
  endpoint: 'https://laptop.example:8123',
  lastReachedMs: 0,
};

/** The published rows, as [id, folded name, nickname]. */
function publishedRows(): [string, string, string | undefined][] {
  return manifestBackendDescriptors().map((row) => [row.id, row.name, row.nickname]);
}

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
    for (const toast of getToasts()) removeToast(toast.id);
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
    expect(manifestBackendDescriptors()).toEqual([]);
    expect(backendById('laptop')).toBeUndefined();
    applyBackendAttach({ id: 'laptop', attached: true });
    await loadSystems();
    expect(backendById('laptop')).toBeUndefined();
  });

  it('replaces the catalog for membership changes only from its own controller', async () => {
    const list = setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();
    expect(backendById('laptop')).toBeDefined();
    list.mockResolvedValue([]);
    applyBackendSetChange({ action: 'membership', id: '' }, 'laptop');
    expect(list).toHaveBeenCalledOnce();
    applyBackendSetChange({ action: 'membership', id: '' });
    await loadSystems();
    expect(backendById('laptop')).toBeUndefined();
    expect(manifestBackendDescriptors()).toEqual([]);
  });

  it('loads the list once, and not at all for a session without host', async () => {
    const list = setBindingMock('ListBackends', async () => [LAPTOP]);
    await Promise.all([loadSystems(), loadSystems()]);
    expect(list).toHaveBeenCalledTimes(1);
    expect(publishedRows()).toEqual([['laptop', 'Laptop', '']]);
    expect(systemsLoaded()).toBe(true);

    __resetSystemsForTest();
    await pairViewOnly();
    await loadSystems();
    expect(list).toHaveBeenCalledTimes(1);
    expect(systemsLoaded()).toBe(false);
  });

  // Bug 1: the descriptor used to publish an empty backendId, so the entry
  // answered to its registry id alone and every UUID-keyed consumer (event
  // origins, nicknames, the nearby-computer filter) drew a blank.
  it('publishes the profile UUID so the registry entry answers to it', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();
    expect(backendById('laptop')?.backendId).toBe(LAPTOP.backendId);
    expect(backendById(LAPTOP.backendId)?.id).toBe('laptop');
  });

  // The descriptor has no field for reachability or sync errors; the side
  // map is where the section reads them, and a removal drops its entry.
  it('keeps the status fields the descriptor has no room for', async () => {
    setBindingMock('ListBackends', async () => [
      { ...LAPTOP, lastReachedMs: 123, deviceNameSyncError: 'x', ownDeviceSyncError: 'y' },
    ]);
    await loadSystems();
    expect(systemStatus('laptop')).toEqual({
      lastReachedMs: 123, deviceNameSyncError: 'x', ownDeviceSyncError: 'y',
    });
    setBindingMock('RemoveBackend', async () => {});
    await removeSystem('laptop');
    expect(systemStatus('laptop')).toBeUndefined();
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
  // the three system RPCs act on, and the descriptor it would build names
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

  // A cancelled pairing ends the host's wait, which arrives here as a
  // refusal for a row this page already dropped. The person who cancelled
  // it is not owed a "could not attach" toast about it.
  it('says nothing about a refusal for a pairing this page no longer holds', async () => {
    setBindingMock('AddBackend', async () => ({
      id: 'laptop', name: 'Laptop', endpoint: LAPTOP.endpoint, verificationNumber: '42',
    }));
    await addSystem('link');
    setBindingMock('RemoveBackend', async () => {});
    await removeSystem('laptop');
    expect(applyBackendAttach({ id: 'laptop', attached: false, error: 'forgotten' })).toBeNull();
    expect(getPendingAttachments()).toEqual([]);
  });

  it('detaches the registry entry when a system is removed', async () => {
    stageBackend();
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();
    const remove = setBindingMock('RemoveBackend', async () => {});
    await removeSystem('laptop');
    expect(remove).toHaveBeenCalledWith('laptop');
    expect(manifestBackendDescriptors()).toEqual([]);
    expect(attachedBackends().some((b) => b.id === 'laptop')).toBe(false);
  });

  it('folds the nickname into the published name, nickname first', async () => {
    setBindingMock('ListBackends', async () => [{ ...LAPTOP, nickname: 'Work laptop' }]);
    await loadSystems();
    expect(publishedRows()).toEqual([['laptop', 'Work laptop', 'Work laptop']]);
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

    expect(manifestBackendDescriptors()).toEqual([]);
    // The same purge a local removeSystem does: the door is closed too, not
    // just the row forgotten.
    expect(attachedBackends().some((b) => b.id === 'laptop')).toBe(false);
  });

  // A removal nobody here asked for — the far owner revoked this device —
  // used to look exactly like one this page made: the row vanished. The
  // reason on the frame is what tells the two apart, and the label is read
  // before the row goes, since afterwards nothing on this side knows the
  // machine's name.
  it('says which computer ended access, before forgetting its row', async () => {
    stageBackend();
    setBindingMock('ListBackends', async () => [{ ...LAPTOP, nickname: 'Work laptop' }]);
    await loadSystems();

    applyBackendSetChange({ action: 'removed', id: 'laptop', reason: 'ended-by-computer' });

    expect(getToasts().map((t) => [t.type, t.message])).toEqual([[
      'warning', "Work laptop ended this computer's access. Pair again from Connect to a computer.",
    ]]);
    expect(manifestBackendDescriptors()).toEqual([]);
    expect(attachedBackends().some((b) => b.id === 'laptop')).toBe(false);
  });

  // A page that never opened Settings holds no list row, but its transport
  // registry carries every attached door, which is enough for a name.
  it('names the computer from the registry when the list was never loaded', () => {
    stageBackend();

    applyBackendSetChange({ action: 'removed', id: 'laptop', reason: 'ended-by-computer' });

    expect(getToasts().map((t) => t.message)).toEqual([
      "Laptop ended this computer's access. Pair again from Connect to a computer.",
    ]);
    expect(attachedBackends().some((b) => b.id === 'laptop')).toBe(false);
  });

  it('says nothing about a removal this installation made', async () => {
    stageBackend();
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();

    applyBackendSetChange({ action: 'removed', id: 'laptop' });

    expect(getToasts()).toEqual([]);
    expect(manifestBackendDescriptors()).toEqual([]);
  });

  // A rename lands in fields only the authoritative list holds — the folded
  // label, and on a device-name-sync change the error text — so both re-read
  // it rather than patching a mirror that no longer exists.
  it('re-reads and republishes on a rename another page made', async () => {
    const list = setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();
    expect(publishedRows()).toEqual([['laptop', 'Laptop', '']]);

    list.mockResolvedValue([{ ...LAPTOP, nickname: 'Work laptop' }]);
    applyBackendSetChange({ action: 'renamed', id: 'laptop', nickname: 'Work laptop' });
    await loadSystems();

    expect(publishedRows()).toEqual([['laptop', 'Work laptop', 'Work laptop']]);
    expect(backendById('laptop')?.name).toBe('Work laptop');
  });

  it('clears a nickname a rename emptied', async () => {
    const list = setBindingMock('ListBackends', async () => [{ ...LAPTOP, nickname: 'Old' }]);
    await loadSystems();

    list.mockResolvedValue([LAPTOP]);
    applyBackendSetChange({ action: 'renamed', id: 'laptop' });
    await loadSystems();

    expect(publishedRows()).toEqual([['laptop', 'Laptop', '']]);
  });

  it('re-reads the list when device name sync state changes', async () => {
    const list = setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();
    expect(systemStatus('laptop')?.deviceNameSyncError).toBe('');

    list.mockResolvedValue([{ ...LAPTOP, deviceNameSyncError: 'pending' }]);
    applyBackendSetChange({ action: 'device-name-sync', id: 'laptop' });
    await loadSystems();

    expect(systemStatus('laptop')?.deviceNameSyncError).toBe('pending');
  });

  // The event hub subscribes EVERY attached backend, and these three RPCs act
  // on THIS machine's profile directory: another backend's frame names an id
  // in its own directory, which would drop the wrong row here.
  it('refuses a frame that did not come from home', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();

    applyBackendSetChange({ action: 'removed', id: 'laptop' }, 'desktop');

    expect(publishedRows().map((row) => row[0])).toEqual(['laptop']);
  });

  it('ignores an unnamed row and an action it does not know', async () => {
    setBindingMock('ListBackends', async () => [LAPTOP]);
    await loadSystems();

    applyBackendSetChange({ action: 'removed', id: '' });
    applyBackendSetChange({ action: 'attached' as never, id: 'laptop' });

    expect(publishedRows().map((row) => row[0])).toEqual(['laptop']);
  });
});
