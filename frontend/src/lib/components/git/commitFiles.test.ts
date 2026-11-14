import { describe, expect, it } from 'vitest';
import type { CommitFile } from '../../types/git';
import {
  carryActive,
  carrySelection,
  changeKey,
  commitFileStatus,
  commitSelection,
  rowPaths,
  splitPath,
  toggleAll,
  toggleOne,
} from './commitFiles';

function file(path: string, overrides: Partial<CommitFile> = {}): CommitFile {
  return { path, status: 'M', additions: 1, deletions: 0, version: '100644:aaaa', ...overrides };
}

const FILES = [file('a.ts'), file('dir/new.ts', { status: 'R', oldPath: 'dir/old.ts' }), file('z.ts')];

describe('commit file selection', () => {
  it('names both paths of a rename for its diff', () => {
    expect(rowPaths(FILES[1])).toEqual(['dir/old.ts', 'dir/new.ts']);
    expect(rowPaths(FILES[0])).toEqual(['a.ts']);
  });

  it('covers everything while git commits whole, the selected rows as listed otherwise, and nothing when none is selected', () => {
    expect(commitSelection(FILES, new Set(), true)).toEqual({ all: true, files: [] });
    expect(commitSelection(FILES, new Set(['a.ts']), true)).toEqual({ all: true, files: [] });
    expect(commitSelection(FILES, new Set(['z.ts', 'dir/new.ts']), false)).toEqual({ all: false, files: [FILES[1], FILES[2]] });
    expect(commitSelection(FILES, new Set(), false)).toBeNull();
    // A selected path that is no longer listed covers nothing.
    expect(commitSelection(FILES, new Set(['gone.ts']), false)).toBeNull();
  });

  it('drops selected rows that left the list and never selects new ones', () => {
    const selected = new Set(['a.ts', 'gone.ts']);
    const carried = carrySelection(selected, [file('a.ts'), file('gone.ts')], [file('a.ts'), file('appeared.ts')]);
    expect([...carried]).toEqual(['a.ts']);
    const kept = new Set(['a.ts']);
    expect(carrySelection(kept, FILES, FILES)).toBe(kept);
  });

  it('keeps both halves of a selected rename that the new list shows apart', () => {
    const apart = [file('a.ts'), file('dir/new.ts', { status: 'A' }), file('dir/old.ts', { status: 'D' })];
    expect([...carrySelection(new Set(['dir/new.ts']), FILES, apart)].sort()).toEqual(['dir/new.ts', 'dir/old.ts']);
    // An unselected rename selects nothing, and one still listed as a rename adds nothing.
    expect([...carrySelection(new Set(['a.ts']), FILES, apart)]).toEqual(['a.ts']);
    const renamed = new Set(['dir/new.ts']);
    expect(carrySelection(renamed, FILES, FILES)).toBe(renamed);
  });

  it('toggles one row and all rows', () => {
    expect([...toggleOne(new Set(['a.ts']), 'z.ts')]).toEqual(['a.ts', 'z.ts']);
    expect([...toggleOne(new Set(['a.ts']), 'a.ts')]).toEqual([]);
    expect([...toggleAll(new Set(['a.ts']), FILES)]).toEqual(['a.ts', 'dir/new.ts', 'z.ts']);
    expect([...toggleAll(new Set(['a.ts', 'dir/new.ts', 'z.ts']), FILES)]).toEqual([]);
    expect([...toggleAll(new Set(), [])]).toEqual([]);
  });

  it('keeps the active row, else the row at its old position', () => {
    expect(carryActive(null, [], FILES)).toBe('a.ts');
    expect(carryActive('z.ts', FILES, FILES)).toBe('z.ts');
    expect(carryActive('dir/new.ts', FILES, [FILES[0], FILES[2]])).toBe('z.ts');
    expect(carryActive('z.ts', FILES, [FILES[0]])).toBe('a.ts');
    expect(carryActive('a.ts', FILES, [])).toBeNull();
  });
});

describe('commit file display', () => {
  it('splits a path into its name and directory', () => {
    expect(splitPath('src/lib/a.ts')).toEqual({ name: 'a.ts', dir: 'src/lib' });
    expect(splitPath('a.ts')).toEqual({ name: 'a.ts', dir: '' });
  });

  it('labels every status git reports and an unknown one', () => {
    expect(['A', 'M', 'D', 'R', 'T'].map((s) => commitFileStatus(s).label))
      .toEqual(['Added', 'Modified', 'Deleted', 'Renamed', 'Type changed']);
    expect(commitFileStatus('X').label).toBe('Changed');
  });

  it('changes a row key when its content moves, not when only its counts do', () => {
    expect(changeKey(file('a.ts'))).toBe(changeKey(file('a.ts', { additions: 2 })));
    expect(changeKey(file('a.ts'))).not.toBe(changeKey(file('a.ts', { version: '100644:bbbb' })));
  });
});
