import { beforeEach, describe, expect, it } from 'vitest';
import {
  BRANCH_MRU_MAX_ENTRIES,
  recentBranchSelections,
  recordBranchSelection,
} from './branchMru';
import { appStorageSet, resetAppStorageForTest } from './appStorage';

describe('branchMru', () => {
  beforeEach(() => {
    resetAppStorageForTest();
  });

  it('returns empty for unknown or empty project', () => {
    expect(recentBranchSelections('proj-1')).toEqual([]);
    expect(recentBranchSelections('')).toEqual([]);
  });

  it('records selections most-recent first and dedupes to the front', () => {
    recordBranchSelection('proj-1', 'feat/a');
    recordBranchSelection('proj-1', 'feat/b');
    recordBranchSelection('proj-1', 'feat/a');
    expect(recentBranchSelections('proj-1')).toEqual(['feat/a', 'feat/b']);
  });

  it('keeps projects isolated', () => {
    recordBranchSelection('proj-1', 'feat/a');
    recordBranchSelection('proj-2', 'feat/b');
    expect(recentBranchSelections('proj-1')).toEqual(['feat/a']);
    expect(recentBranchSelections('proj-2')).toEqual(['feat/b']);
  });

  it('ignores empty branch names and trims whitespace', () => {
    recordBranchSelection('proj-1', '   ');
    recordBranchSelection('proj-1', ' feat/a ');
    expect(recentBranchSelections('proj-1')).toEqual(['feat/a']);
  });

  it('caps the list', () => {
    for (let i = 0; i < BRANCH_MRU_MAX_ENTRIES + 5; i++) {
      recordBranchSelection('proj-1', `branch-${i}`);
    }
    const list = recentBranchSelections('proj-1');
    expect(list).toHaveLength(BRANCH_MRU_MAX_ENTRIES);
    expect(list[0]).toBe(`branch-${BRANCH_MRU_MAX_ENTRIES + 4}`);
  });

  it('reads corrupt storage as empty', () => {
    appStorageSet('branch-mru:proj-1', '{not json');
    expect(recentBranchSelections('proj-1')).toEqual([]);
    appStorageSet('branch-mru:proj-1', JSON.stringify({ nope: true }));
    expect(recentBranchSelections('proj-1')).toEqual([]);
    appStorageSet('branch-mru:proj-1', JSON.stringify(['ok', 42, '', 'also-ok']));
    expect(recentBranchSelections('proj-1')).toEqual(['ok', 'also-ok']);
  });
});
