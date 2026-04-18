import { beforeEach, describe, expect, it } from 'vitest';
import {
  collapseProject,
  expandProject,
  getSortDirection,
  isProjectExpanded,
  resetSidebarForTest,
  toggleProject,
  toggleSortDirection,
} from './sidebar.svelte';

describe('sidebar store', () => {
  beforeEach(() => {
    resetSidebarForTest();
  });

  describe('expansion', () => {
    it('toggleProject flips the flag and persists', () => {
      expect(isProjectExpanded('p1')).toBe(false);
      toggleProject('p1');
      expect(isProjectExpanded('p1')).toBe(true);

      const raw = localStorage.getItem('agent-overflow:sidebar:expandedProjects');
      expect(raw).not.toBeNull();
      expect(JSON.parse(raw as string)).toContain('p1');

      toggleProject('p1');
      expect(isProjectExpanded('p1')).toBe(false);
      const raw2 = localStorage.getItem('agent-overflow:sidebar:expandedProjects');
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

    it('corrupt stored JSON resets to empty without crashing', () => {
      localStorage.setItem('agent-overflow:sidebar:expandedProjects', '{not json');
      // Import triggered the initial read already; round-trip through a
      // toggle which serializes over the garbage.
      toggleProject('p1');
      expect(isProjectExpanded('p1')).toBe(true);
    });
  });

  describe('sort direction', () => {
    it('defaults to desc when no persisted value exists', () => {
      expect(getSortDirection()).toBe('desc');
    });

    it('toggleSortDirection flips + persists', () => {
      toggleSortDirection();
      expect(getSortDirection()).toBe('asc');
      expect(localStorage.getItem('agent-overflow:sidebar:sortDirection')).toBe('asc');
      toggleSortDirection();
      expect(getSortDirection()).toBe('desc');
      expect(localStorage.getItem('agent-overflow:sidebar:sortDirection')).toBe('desc');
    });
  });
});
