import { describe, expect, it, beforeEach } from 'vitest';
import {
  clearThreadSelection,
  filterThreads,
  getIncludeArchived,
  getSelectedThreadIds,
  getThreadFilterQuery,
  getWorkspaceFilter,
  isThreadSelected,
  setIncludeArchived,
  setThreadFilterQuery,
  setThreadSelection,
  setWorkspaceFilter,
  toggleThreadSelection,
} from './threadFilter.svelte';
import type { Thread } from '../types/models';

function makeThread(id: string, extra: Partial<Thread> = {}): Thread {
  return {
    id,
    title: `Thread ${id}`,
    provider: 'claude',
    workspacePath: '/tmp/ws',
    projectPath: '/tmp/ws',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...extra,
  };
}

describe('threadFilter store', () => {
  beforeEach(() => {
    setThreadFilterQuery('');
    setIncludeArchived(false);
    setWorkspaceFilter(null);
    clearThreadSelection();
  });

  it('hides archived threads by default; shows them when toggled on', () => {
    const items = [
      makeThread('a'),
      makeThread('b', { archived: true }),
      makeThread('c'),
    ];
    expect(filterThreads(items).map((t) => t.id)).toEqual(['a', 'c']);

    setIncludeArchived(true);
    expect(getIncludeArchived()).toBe(true);
    expect(filterThreads(items).map((t) => t.id)).toEqual(['a', 'b', 'c']);
  });

  it('filters by case-insensitive title substring', () => {
    const items = [
      makeThread('1', { title: 'Refactor auth' }),
      makeThread('2', { title: 'Build frontend' }),
      makeThread('3', { title: 'auth tokens' }),
    ];
    setThreadFilterQuery('AUTH');
    expect(getThreadFilterQuery()).toBe('AUTH');
    expect(filterThreads(items).map((t) => t.id).sort()).toEqual(['1', '3']);
  });

  it('filters by workspace path', () => {
    const items = [
      makeThread('1', { workspacePath: '/a' }),
      makeThread('2', { workspacePath: '/b' }),
      makeThread('3', { workspacePath: '/a' }),
    ];
    setWorkspaceFilter('/a');
    expect(getWorkspaceFilter()).toBe('/a');
    expect(filterThreads(items).map((t) => t.id).sort()).toEqual(['1', '3']);
  });

  it('combines archived toggle with title filter', () => {
    const items = [
      makeThread('1', { title: 'auth', archived: true }),
      makeThread('2', { title: 'auth' }),
    ];
    setThreadFilterQuery('auth');
    expect(filterThreads(items).map((t) => t.id)).toEqual(['2']);
    setIncludeArchived(true);
    expect(filterThreads(items).map((t) => t.id).sort()).toEqual(['1', '2']);
  });

  it('multi-select toggle + clear round-trip', () => {
    expect(getSelectedThreadIds().size).toBe(0);
    toggleThreadSelection('a');
    toggleThreadSelection('b');
    expect(isThreadSelected('a')).toBe(true);
    expect(isThreadSelected('b')).toBe(true);

    toggleThreadSelection('a');
    expect(isThreadSelected('a')).toBe(false);

    clearThreadSelection();
    expect(getSelectedThreadIds().size).toBe(0);
  });

  it('setThreadSelection replaces the selection set', () => {
    toggleThreadSelection('a');
    setThreadSelection(['x', 'y', 'z']);
    const ids = Array.from(getSelectedThreadIds()).sort();
    expect(ids).toEqual(['x', 'y', 'z']);
  });

  // --- Bug D8 regression ---
  describe('toggling filters clears the multi-select set', () => {
    it('flipping includeArchived clears the selection', () => {
      setIncludeArchived(true);
      toggleThreadSelection('archived-1');
      toggleThreadSelection('archived-2');
      toggleThreadSelection('archived-3');
      expect(getSelectedThreadIds().size).toBe(3);

      setIncludeArchived(false);
      expect(getSelectedThreadIds().size).toBe(0);
    });

    it('setting includeArchived to the same value does NOT clear selection', () => {
      toggleThreadSelection('a');
      toggleThreadSelection('b');
      // Already false.
      setIncludeArchived(false);
      expect(Array.from(getSelectedThreadIds()).sort()).toEqual(['a', 'b']);
    });

    it('changing workspaceFilter clears the selection (hidden ids can no longer be acted on)', () => {
      setWorkspaceFilter('/a');
      toggleThreadSelection('a-1');
      toggleThreadSelection('a-2');
      expect(getSelectedThreadIds().size).toBe(2);

      setWorkspaceFilter('/b');
      expect(getSelectedThreadIds().size).toBe(0);
    });

    it('setting workspaceFilter to the same value keeps selection intact', () => {
      setWorkspaceFilter('/a');
      toggleThreadSelection('a-1');
      setWorkspaceFilter('/a');
      expect(Array.from(getSelectedThreadIds()).sort()).toEqual(['a-1']);
    });

    it('clearing workspace filter (null -> set -> null) clears selection each transition', () => {
      toggleThreadSelection('a-1');
      setWorkspaceFilter('/a');
      // First transition clears.
      expect(getSelectedThreadIds().size).toBe(0);

      // Re-select under workspace /a, then clear filter -> selection drops.
      toggleThreadSelection('a-1');
      setWorkspaceFilter(null);
      expect(getSelectedThreadIds().size).toBe(0);
    });

    it('filter query changes do NOT clear the selection (query is not a visibility gate for operations)', () => {
      toggleThreadSelection('keep-me');
      setThreadFilterQuery('abc');
      expect(getSelectedThreadIds().has('keep-me')).toBe(true);
      setThreadFilterQuery('');
      expect(getSelectedThreadIds().has('keep-me')).toBe(true);
    });
  });
});
