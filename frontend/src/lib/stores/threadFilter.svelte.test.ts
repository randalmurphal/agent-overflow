import { describe, expect, it, beforeEach } from 'vitest';
import {
  clearThreadSelection,
  getSelectedThreadIds,
  getThreadFilterQuery,
  getWorkspaceFilter,
  isThreadSelected,
  setThreadFilterQuery,
  setThreadSelection,
  setWorkspaceFilter,
  toggleThreadSelection,
} from './threadFilter.svelte';

describe('threadFilter store', () => {
  beforeEach(() => {
    setThreadFilterQuery('');
    setWorkspaceFilter(null);
    clearThreadSelection();
  });

  it('stores and returns query', () => {
    setThreadFilterQuery('AUTH');
    expect(getThreadFilterQuery()).toBe('AUTH');
  });

  it('stores and returns workspace filter', () => {
    setWorkspaceFilter('/a');
    expect(getWorkspaceFilter()).toBe('/a');
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

  describe('toggling filters clears the multi-select set', () => {
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
