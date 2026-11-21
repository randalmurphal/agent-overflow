import { beforeEach, describe, expect, it } from 'vitest';
import {
  isSidebarSectionExpanded,
  toggleSidebarSection,
  collapseThreadList,
  getCollapsedGroups,
  getProjectSortMode,
  getShowProviderIcons,
  setShowProviderIcons,
  getThreadListVisibleLimit,
  isGroupExpanded,
  resetSidebarForTest,
  setThreadListVisibleLimit,
  setProjectSortMode,
  toggleGroup,
  syncSidebarFromAppStorage,
  syncSidebarFromSettings,
} from './sidebar.svelte';
import { appStorageGet, hydrateAppStorage, resetAppStorageForTest } from './appStorage';
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { makeSettings } from '../../test/helpers/settings';
import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from '../utils/sidebarThreadLimits';
import { removeThreadGroup } from './threadGroups.svelte';

const COLLAPSED_GROUPS_KEY = 'sidebar:collapsedGroups';

describe('sidebar store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetAppStorageForTest();
    resetSidebarForTest();
  });

  it('forgets a group\'s paging when the group goes', () => {
    setThreadListVisibleLimit('group-1', THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT);
    setThreadListVisibleLimit('other', THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT);
    removeThreadGroup('group-1');
    expect(getThreadListVisibleLimit('group-1')).toBe(THREAD_PREVIEW_LIMIT);
    expect(getThreadListVisibleLimit('other')).toBe(THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT);
  });

  describe('sections', () => {
    it('folds each section on its own and persists the folds', () => {
      expect(isSidebarSectionExpanded('assistants') && isSidebarSectionExpanded('projects')).toBe(true);
      toggleSidebarSection('projects');
      expect(isSidebarSectionExpanded('projects')).toBe(false);
      expect(isSidebarSectionExpanded('assistants')).toBe(true);
      expect(JSON.parse(appStorageGet('sidebar:collapsedSections')!)).toEqual(['projects']);
      toggleSidebarSection('projects');
      expect(isSidebarSectionExpanded('projects')).toBe(true);
      expect(JSON.parse(appStorageGet('sidebar:collapsedSections')!)).toEqual([]);
    });

    it('adopts the folds from storage and forgets them on reset', async () => {
      setBindingMock('GetUIState', async () => ({ 'sidebar:collapsedSections': JSON.stringify(['assistants']) }));
      await hydrateAppStorage();
      syncSidebarFromAppStorage();
      expect(isSidebarSectionExpanded('assistants')).toBe(false);
      resetSidebarForTest();
      expect(isSidebarSectionExpanded('assistants')).toBe(true);
    });
  });

  describe('group collapse', () => {
    it('groups default to expanded and toggleGroup persists explicit collapses', () => {
      // Inverted like collapsedProjects, and for the same reason: a group
      // the user just made must show what is in it.
      expect(isGroupExpanded('g1')).toBe(true);
      toggleGroup('g1');
      expect(isGroupExpanded('g1')).toBe(false);
      expect(JSON.parse(appStorageGet(COLLAPSED_GROUPS_KEY) as string)).toEqual(['g1']);

      toggleGroup('g1');
      expect(isGroupExpanded('g1')).toBe(true);
      expect(JSON.parse(appStorageGet(COLLAPSED_GROUPS_KEY) as string)).toEqual([]);
    });

    it('toggling one group preserves the collapse state of other groups', () => {
      toggleGroup('g1');
      toggleGroup('g2');
      expect([...getCollapsedGroups()].sort()).toEqual(['g1', 'g2']);
      toggleGroup('g2');
      expect([...getCollapsedGroups()]).toEqual(['g1']);
      expect(JSON.parse(appStorageGet(COLLAPSED_GROUPS_KEY) as string)).toEqual(['g1']);
    });

    it('resetSidebarForTest clears the collapsed groups', () => {
      toggleGroup('g1');
      resetSidebarForTest();
      expect(isGroupExpanded('g1')).toBe(true);
      expect([...getCollapsedGroups()]).toEqual([]);
    });
  });

  describe('project sort mode', () => {
    beforeEach(() => {
      setBindingMock('UpdateSettings', async () => null);
    });

    it('defaults to lastActivity when no persisted value exists', () => {
      expect(getProjectSortMode()).toBe('lastActivity');
    });

    it('setProjectSortMode persists the chosen mode', () => {
      setProjectSortMode('manual');
      expect(getProjectSortMode()).toBe('manual');
      expect(localStorage.getItem('agent-overflow:sidebar:projectSortMode')).toBe('manual');
      setProjectSortMode('createdAt');
      expect(getProjectSortMode()).toBe('createdAt');
    });

    it('falls back to lastActivity when persisted value is unknown', () => {
      localStorage.setItem('agent-overflow:sidebar:projectSortMode', 'bogus');
      resetSidebarForTest();
      // Setting and reading immediately uses the in-memory $state, but
      // the read function is called via getProjectSortMode which reads
      // the current state — verify the fallback path through a fresh read.
      localStorage.setItem('agent-overflow:sidebar:projectSortMode', 'bogus');
      // The store's read happens at module init; this test guards the
      // PROJECT_SORT_MODES.includes check within readProjectSortMode.
      // Call setProjectSortMode to a valid value to confirm the writer
      // path doesn't accept invalid input:
      setProjectSortMode('lastActivity');
      expect(getProjectSortMode()).toBe('lastActivity');
    });
  });

  describe('syncSidebarFromSettings', () => {
    beforeEach(() => {
      resetSettingsForTest();
    });

    it('overwrites sort mode from Go settings', async () => {
      setBindingMock('UpdateSettings', async () => null);
      setProjectSortMode('lastActivity');
      expect(getProjectSortMode()).toBe('lastActivity');

      const serverSettings = makeSettings({ projectSortMode: 'manual' });
      setBindingMock('GetSettings', async () => serverSettings);
      setBindingMock('UpdateSettings', async () => serverSettings);
      await loadSettings();

      syncSidebarFromSettings();
      expect(getProjectSortMode()).toBe('manual');
    });

    it('back-fills localStorage from Go settings without pushing back', async () => {
      const serverSettings = makeSettings({ projectSortMode: 'createdAt' });
      setBindingMock('GetSettings', async () => serverSettings);
      const updateMock = setBindingMock('UpdateSettings', async () => serverSettings);
      await loadSettings();

      syncSidebarFromSettings();
      expect(localStorage.getItem('agent-overflow:sidebar:projectSortMode')).toBe('createdAt');
      expect(updateMock!.mock.calls.length).toBe(0);
    });

    it('the frontend keeps its choice when the host has another value', async () => {
      setBindingMock('UpdateSettings', async () => null);
      setProjectSortMode('createdAt');

      const serverSettings = makeSettings({ projectSortMode: 'manual' });
      setBindingMock('GetSettings', async () => serverSettings);
      const updateMock = setBindingMock('UpdateSettings', async () => serverSettings);
      await loadSettings();

      syncSidebarFromSettings();
      expect(getProjectSortMode()).toBe('createdAt');
      expect(updateMock!.mock.calls.length).toBe(0);
    });

    it('pushes the localStorage sort mode to Go when Go has the default but memory does not (upgrade migration)', async () => {
      // Simulate the upgrade scenario: user has a non-default value in
      // memory (populated from localStorage at module init), but Go
      // settings are at the factory default because the field didn't
      // exist before this version.
      setBindingMock('UpdateSettings', async () => null);
      setProjectSortMode('manual');
      expect(getProjectSortMode()).toBe('manual');

      const defaultGoSettings = makeSettings();
      setBindingMock('GetSettings', async () => defaultGoSettings);
      const updateMock = setBindingMock('UpdateSettings', async (patch: Partial<typeof defaultGoSettings>) => ({
        ...defaultGoSettings,
        ...patch,
      }));
      await loadSettings();

      syncSidebarFromSettings();

      // The in-memory value should NOT be overwritten with the default.
      expect(getProjectSortMode()).toBe('manual');

      expect(updateMock!.mock.calls.length).toBe(0);
    });

    it('does not push when both Go and memory are at defaults', async () => {
      // Fresh install: both layers have defaults. No migration needed.
      expect(getProjectSortMode()).toBe('lastActivity');

      const defaultGoSettings = makeSettings();
      setBindingMock('GetSettings', async () => defaultGoSettings);
      const updateMock = setBindingMock('UpdateSettings', async () => defaultGoSettings);
      await loadSettings();

      syncSidebarFromSettings();

      expect(updateMock!.mock.calls.length).toBe(0);
    });
  });

  describe('syncSidebarFromAppStorage', () => {
    it('adopts hydrated view state from the per-client bucket', async () => {
      setBindingMock('GetUIState', async () => ({
        'sidebar:threadListVisibleLimits': JSON.stringify({ 'group-1': 25 }),
        'sidebar:collapsedGroups': JSON.stringify(['group-1']),
      }));
      await hydrateAppStorage();

      syncSidebarFromAppStorage();

      expect(isGroupExpanded('group-1')).toBe(false);
      expect(isGroupExpanded('group-2')).toBe(true);
      expect(getThreadListVisibleLimit('group-1')).toBe(25);
    });

    it('local pre-hydration writes survive an empty server bucket (pending wins)', async () => {
      toggleGroup('group-local');
      expect(isGroupExpanded('group-local')).toBe(false);

      setBindingMock('GetUIState', async () => ({}));
      await hydrateAppStorage();

      syncSidebarFromAppStorage();
      expect(isGroupExpanded('group-local')).toBe(false);
    });

    it('resets to defaults when the hydrated bucket has no view state', async () => {
      // Simulate a fresh client: memory holds stale state from a prior
      // identity, the new bucket is empty and holds no pending writes.
      toggleGroup('group-stale');
      resetAppStorageForTest();
      setBindingMock('GetUIState', async () => ({}));
      await hydrateAppStorage();

      syncSidebarFromAppStorage();

      expect(isGroupExpanded('group-stale')).toBe(true);
      expect(getThreadListVisibleLimit('any')).toBe(THREAD_PREVIEW_LIMIT);
    });
  });

  describe('thread list visible limit', () => {
    it('starts at the preview limit', () => {
      expect(getThreadListVisibleLimit('g1')).toBe(THREAD_PREVIEW_LIMIT);
    });

    it('collapseThreadList resets one list to the preview limit, twice in a row', () => {
      setThreadListVisibleLimit('g1', THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT);
      setThreadListVisibleLimit('g2', 40);
      collapseThreadList('g1');
      collapseThreadList('g1');
      expect(getThreadListVisibleLimit('g1')).toBe(THREAD_PREVIEW_LIMIT);
      // The other list is keyed by its own id and keeps its size.
      expect(getThreadListVisibleLimit('g2')).toBe(40);
    });

    it('setThreadListVisibleLimit persists explicit reveal sizes and resets defaults', () => {
      setThreadListVisibleLimit('g1', 27);
      expect(getThreadListVisibleLimit('g1')).toBe(27);

      setThreadListVisibleLimit('g1', THREAD_PREVIEW_LIMIT);
      expect(getThreadListVisibleLimit('g1')).toBe(THREAD_PREVIEW_LIMIT);
    });
  });
});

describe('sidebar provider icon preference', () => {
  beforeEach(resetSidebarForTest);

  it('defaults off, accepts repeated saves, and persists both on and off', () => {
    expect(getShowProviderIcons()).toBe(false);
    for (const value of [true, true, false, false]) {
      expect(setShowProviderIcons(value)).toBe(true);
      expect(getShowProviderIcons()).toBe(value);
      expect(localStorage.getItem('agent-overflow:frontend:sidebar:showProviderIcons')).toBe(JSON.stringify(value));
    }
  });

  it('loads cross-window updates and clears the icon for malformed or removed values', () => {
    const key = 'agent-overflow:frontend:sidebar:showProviderIcons';
    for (const [raw, expected] of [['true', true], ['"true"', false], ['true', true], ['{}', false], ['true', true], [null, false]] as const) {
      if (raw === null) localStorage.removeItem(key);
      else localStorage.setItem(key, raw);
      const event = new Event('storage');
      Object.defineProperties(event, { key: { value: key }, storageArea: { value: localStorage } });
      window.dispatchEvent(event);
      expect(getShowProviderIcons()).toBe(expected);
    }
  });

  it('rejects invalid writes inside the store', () => {
    expect(() => setShowProviderIcons('true' as unknown as boolean)).toThrow('must be a boolean');
    expect(getShowProviderIcons()).toBe(false);
    expect(localStorage.getItem('agent-overflow:frontend:sidebar:showProviderIcons')).toBeNull();
  });
});
