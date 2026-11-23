import { describe, expect, it, beforeEach } from 'vitest';
import {
  clearThreadSelection,
  getSelectedThreadIds,
  getThreadFilterQuery,
  isThreadSelected,
  setThreadFilterQuery,
  setThreadSelection,
  toggleThreadSelection,
} from './threadFilter.svelte';

describe('threadFilter store', () => {
  beforeEach(() => {
    setThreadFilterQuery('');
    clearThreadSelection();
  });

  it('stores and returns query', () => {
    setThreadFilterQuery('AUTH');
    expect(getThreadFilterQuery()).toBe('AUTH');
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

  it('filter query changes do NOT clear the selection (query is not a visibility gate for operations)', () => {
    toggleThreadSelection('keep-me');
    setThreadFilterQuery('abc');
    expect(getSelectedThreadIds().has('keep-me')).toBe(true);
    setThreadFilterQuery('');
    expect(getSelectedThreadIds().has('keep-me')).toBe(true);
  });
});
