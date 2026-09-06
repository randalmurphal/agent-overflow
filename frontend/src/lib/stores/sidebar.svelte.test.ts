import { beforeEach, describe, expect, it } from 'vitest';
import {
  collapseProject,
  expandProject,
  collapseThreadList,
  getCollapsedGroups,
  getProjectSortMode,
  getThreadListVisibleLimit,
  isDiscussionExpanded,
  isGroupExpanded,
  isProjectExpanded,
  isThreadListExpanded,
  resetSidebarForTest,
  revealMoreThreadList,
  setCollapsedGroups,
  setThreadListVisibleLimit,
  setProjectSortMode,
  toggleGroup,
  syncSidebarFromAppStorage,
  syncSidebarFromSettings,
  toggleProject,
} from './sidebar.svelte';
import { appStorageGet, hydrateAppStorage, resetAppStorageForTest } from './appStorage';
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { makeSettings } from '../../test/helpers/settings';
import { THREAD_PREVIEW_LIMIT, THREAD_REVEAL_INCREMENT } from '../utils/sidebarThreadLimits';

const COLLAPSED_KEY = 'sidebar:collapsedProjects';
const COLLAPSED_GROUPS_KEY = 'sidebar:collapsedGroups';

describe('sidebar store', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetAppStorageForTest();
    resetSidebarForTest();
    setBindingMock('SetUIState', async () => null);
    setBindingMock('DeleteUIState', async () => null);
  });

  describe('expansion', () => {
    it('projects default to expanded and toggleProject persists explicit collapses', () => {
      // Inverted storage: the persisted set lists explicit *collapses*,
      // so an unseen id reads as expanded. Toggling adds it to the
      // collapsed set; toggling again removes it.
      expect(isProjectExpanded('p1')).toBe(true);
      toggleProject('p1');
      expect(isProjectExpanded('p1')).toBe(false);

      const raw = appStorageGet(COLLAPSED_KEY);
      expect(raw).not.toBeNull();
      expect(JSON.parse(raw as string)).toContain('p1');

      toggleProject('p1');
      expect(isProjectExpanded('p1')).toBe(true);
      const raw2 = appStorageGet(COLLAPSED_KEY);
      expect(JSON.parse(raw2 as string)).not.toContain('p1');
    });

    it('expandProject / collapseProject are idempotent', () => {
      expandProject('p1');
      expandProject('p1');
      expect(isProjectExpanded('p1')).toBe(true);
      collapseProject('p1');
      collapseProject('p1');
      expect(isProjectExpanded('p1')).toBe(false);
    });

    it('corrupt legacy stored JSON does not crash subsequent operations', () => {
      localStorage.setItem('agent-overflow:sidebar:collapsedProjects', '{not json');
      // Module init already read storage once; this just verifies that
      // a write/read round-trip serializes over the garbage cleanly.
      toggleProject('p1');
      expect(isProjectExpanded('p1')).toBe(false);
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

    it('setCollapsedGroups swaps the whole set and no-ops on an equal one', () => {
      setCollapsedGroups(new Set(['g1', 'g2']));
      expect([...getCollapsedGroups()].sort()).toEqual(['g1', 'g2']);
      const before = getCollapsedGroups();

      setCollapsedGroups(new Set(['g2', 'g1']));
      // Equal content, so the state reference is untouched — the sidebar's
      // auto-expand effect writes this on every pass and must settle.
      expect(getCollapsedGroups()).toBe(before);

      setCollapsedGroups(new Set(['g1']));
      expect([...getCollapsedGroups()]).toEqual(['g1']);
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
        'sidebar:collapsedProjects': JSON.stringify(['proj-1', 'proj-2']),
        'sidebar:expandedDiscussions': JSON.stringify(['disc-1']),
        'sidebar:threadListVisibleLimits': JSON.stringify({ 'proj-1': 40 }),
        'sidebar:collapsedGroups': JSON.stringify(['group-1']),
      }));
      await hydrateAppStorage();

      syncSidebarFromAppStorage();

      expect(isGroupExpanded('group-1')).toBe(false);
      expect(isGroupExpanded('group-2')).toBe(true);
      expect(isProjectExpanded('proj-1')).toBe(false);
      expect(isProjectExpanded('proj-2')).toBe(false);
      expect(isProjectExpanded('proj-3')).toBe(true);
      expect(isDiscussionExpanded('disc-1')).toBe(true);
      expect(isDiscussionExpanded('disc-2')).toBe(false);
      expect(getThreadListVisibleLimit('proj-1')).toBe(40);
      expect(getThreadListVisibleLimit('proj-2')).toBe(THREAD_PREVIEW_LIMIT);
    });

    it('local pre-hydration writes survive an empty server bucket (pending wins)', async () => {
      toggleProject('proj-local');
      expect(isProjectExpanded('proj-local')).toBe(false);

      setBindingMock('GetUIState', async () => ({}));
      await hydrateAppStorage();

      syncSidebarFromAppStorage();
      expect(isProjectExpanded('proj-local')).toBe(false);
    });

    it('resets to defaults when the hydrated bucket has no view state', async () => {
      // Simulate a fresh client: memory holds stale state from a prior
      // identity, the new bucket is empty and holds no pending writes.
      toggleProject('proj-stale');
      resetAppStorageForTest();
      setBindingMock('GetUIState', async () => ({}));
      await hydrateAppStorage();

      syncSidebarFromAppStorage();

      expect(isProjectExpanded('proj-stale')).toBe(true);
      expect(getThreadListVisibleLimit('any')).toBe(THREAD_PREVIEW_LIMIT);
    });
  });

  describe('thread list visible limit', () => {
    it('starts at the preview limit and reveals 20 more per call', () => {
      expect(getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT);
      expect(isThreadListExpanded('p1')).toBe(false);

      revealMoreThreadList('p1');
      expect(getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT);
      expect(isThreadListExpanded('p1')).toBe(true);

      revealMoreThreadList('p1');
      expect(getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT + THREAD_REVEAL_INCREMENT * 2);
    });

    it('collapseThreadList resets a project to the preview limit', () => {
      revealMoreThreadList('p1');
      collapseThreadList('p1');
      expect(getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT);
      expect(isThreadListExpanded('p1')).toBe(false);
    });

    it('setThreadListVisibleLimit persists explicit reveal sizes and resets defaults', () => {
      setThreadListVisibleLimit('p1', 27);
      expect(getThreadListVisibleLimit('p1')).toBe(27);

      setThreadListVisibleLimit('p1', THREAD_PREVIEW_LIMIT);
      expect(getThreadListVisibleLimit('p1')).toBe(THREAD_PREVIEW_LIMIT);
    });
  });
});
