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
    interactionMode: 'default',
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
});
