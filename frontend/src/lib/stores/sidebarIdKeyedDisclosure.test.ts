// The disclosure records a project id was the key of are dropped at startup,
// in either order the durable bucket can land in.
//
// The sidebar mounts before `appStorageReady` resolves (App.svelte), so on a
// first migration this module initializes while the server's copy of the
// bucket is still in flight. Each case imports the store itself, which is
// when the drop runs, so the two orders are the two import orders.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { THREAD_PREVIEW_LIMIT } from '../utils/sidebarThreadLimits';

const COLLAPSED_PROJECTS_KEY = 'sidebar:collapsedProjects';
const THREAD_LIST_LIMITS_KEY = 'sidebar:threadListVisibleLimits';
const MIGRATED_KEY = 'agent-overflow:uistate:frontend';
const BUCKET_KEY = 'agent-overflow:uistate:bucket';

/** What a client that collapsed a project before the upgrade had stored. */
const ID_KEYED_STATE = {
  [COLLAPSED_PROJECTS_KEY]: JSON.stringify(['p1']),
  [THREAD_LIST_LIMITS_KEY]: JSON.stringify({ p1: 20 }),
  // Not one of them: a group's collapse was always keyed by the group id.
  'sidebar:collapsedGroups': JSON.stringify(['g1']),
};

async function freshModules() {
  vi.resetModules();
  const appStorage = await import('./appStorage');
  const bindings = await import('../../test/mocks/bindings-app');
  appStorage.resetAppStorageForTest();
  bindings.setBindingMock('GetUIState', async () => ({ ...ID_KEYED_STATE }));
  return { appStorage, importSidebar: () => import('./sidebar.svelte') };
}

beforeEach(() => {
  localStorage.clear();
});

afterEach(() => {
  vi.resetModules();
  localStorage.clear();
});

describe('the id-keyed disclosure record', () => {
  it('does not come back with the server copy when it was dropped before hydration', async () => {
    const { appStorage, importSidebar } = await freshModules();

    const sidebar = await importSidebar();
    expect(appStorage.isAppStorageHydrated()).toBe(false);
    expect(await appStorage.hydrateAppStorage()).toBe(true);
    sidebar.syncSidebarFromAppStorage();

    expect(appStorage.appStorageGet(COLLAPSED_PROJECTS_KEY)).toBeNull();
    expect(appStorage.appStorageGet(THREAD_LIST_LIMITS_KEY)).toBeNull();
    // Which is the price: the project opens, showing its preview.
    expect(sidebar.isProjectExpanded('p1')).toBe(true);
    expect(sidebar.getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT);
    // The rest of the bucket migrated, so the two above were refused and
    // not simply never offered.
    expect(sidebar.isGroupExpanded('g1')).toBe(false);
  });

  it('is dropped from a bucket that hydrated before the sidebar read it', async () => {
    const { appStorage, importSidebar } = await freshModules();

    expect(await appStorage.hydrateAppStorage()).toBe(true);
    expect(appStorage.appStorageGet(COLLAPSED_PROJECTS_KEY)).not.toBeNull();
    const sidebar = await importSidebar();

    expect(appStorage.appStorageGet(COLLAPSED_PROJECTS_KEY)).toBeNull();
    expect(appStorage.appStorageGet(THREAD_LIST_LIMITS_KEY)).toBeNull();
    expect(sidebar.isProjectExpanded('p1')).toBe(true);
    expect(sidebar.getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT);
    expect(sidebar.isGroupExpanded('g1')).toBe(false);
  });

  it('leaves a client with nothing to drop unmigrated, so its next boot still reads the bucket', async () => {
    const { appStorage, importSidebar } = await freshModules();

    await importSidebar();

    // The drop wrote nothing, so this client is not marked migrated: a boot
    // whose hydration failed must read the server bucket again, and the mark
    // is what would stop it.
    expect(localStorage.getItem(MIGRATED_KEY)).toBeNull();
    expect(localStorage.getItem(BUCKET_KEY)).toBeNull();
    expect(appStorage.isAppStorageHydrated()).toBe(false);
  });

  it('reads nothing under a project id, pre-appStorage keys included', async () => {
    localStorage.setItem('agent-overflow:sidebar:collapsedProjects', JSON.stringify(['p1']));
    localStorage.setItem('agent-overflow:sidebar:threadListVisibleLimits', JSON.stringify({ p1: 20 }));
    const { appStorage, importSidebar } = await freshModules();

    const sidebar = await importSidebar();

    expect(sidebar.isProjectExpanded('p1')).toBe(true);
    expect(sidebar.getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT);
    // Gone from both stores, so no later boot can adopt them either.
    expect(localStorage.getItem('agent-overflow:sidebar:collapsedProjects')).toBeNull();
    expect(localStorage.getItem('agent-overflow:sidebar:threadListVisibleLimits')).toBeNull();
    expect(appStorage.appStorageGet(COLLAPSED_PROJECTS_KEY)).toBeNull();
    expect(appStorage.appStorageGet('sidebar:collapsedEntries')).toBeNull();
  });
});
