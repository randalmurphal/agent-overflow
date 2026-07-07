import { describe, expect, it } from 'vitest';
import { conflictPatchFile } from './conflictFile';
import { buildPatchDisplayRows } from './patchFiles';

function contents(file: ReturnType<typeof conflictPatchFile>): string[] {
  return file.lines.map((line) => line.content);
}

function types(file: ReturnType<typeof conflictPatchFile>): string[] {
  return file.lines.map((line) => line.type);
}

describe('conflictPatchFile', () => {
  it('renders a conflict as a pseudo-diff: ours del, theirs add, visible markers', () => {
    const file = conflictPatchFile('src/app.ts', [
      'before',
      '<<<<<<< ours',
      'current',
      '=======',
      'incoming',
      '>>>>>>> theirs',
      'after',
      '',
    ].join('\n'));

    expect(file).toMatchObject({
      path: 'src/app.ts',
      kind: 'conflict',
      additions: 1,
      deletions: 1,
      conflicts: 1,
    });
    expect(contents(file)).toEqual([
      '@@ -1 +1 @@',
      ' before',
      '<<<<<<< ours',
      '-current',
      '=======',
      '+incoming',
      '>>>>>>> theirs',
      ' after',
    ]);
    expect(types(file)).toEqual([
      'meta',
      'context',
      'marker',
      'del',
      'marker',
      'add',
      'marker',
      'context',
    ]);
  });

  it('relabels the outer markers when base/head labels are provided', () => {
    const file = conflictPatchFile(
      'a.txt',
      ['<<<<<<< abc123', 'x', '=======', 'y', '>>>>>>> def456', ''].join('\n'),
      { baseLabel: 'origin/main', headLabel: 'feat/thing' },
    );
    expect(contents(file)[0]).toBe('<<<<<<< origin/main');
    expect(contents(file).at(-1)).toBe('>>>>>>> feat/thing');
  });

  it('folds long non-conflict runs and keeps context around the conflict', () => {
    const before = Array.from({ length: 10 }, (_, i) => `c${i + 1}`);
    const after = Array.from({ length: 10 }, (_, i) => `d${i + 1}`);
    const file = conflictPatchFile('a.txt', [
      ...before,
      '<<<<<<< ours',
      'o1',
      '=======',
      't1',
      '>>>>>>> theirs',
      ...after,
      '',
    ].join('\n'));

    expect(contents(file)).toEqual([
      '⋯ 7 unchanged lines',
      '@@ -8 +8 @@',
      ' c8',
      ' c9',
      ' c10',
      '<<<<<<< ours',
      '-o1',
      '=======',
      '+t1',
      '>>>>>>> theirs',
      ' d1',
      ' d2',
      ' d3',
      '⋯ 7 unchanged lines',
    ]);
    expect(file.lines[0]?.fold).toEqual({ id: 0, lines: 7 });
    expect(file.lines.at(-1)?.fold).toEqual({ id: 1, lines: 7 });

    // Display-row numbering skips the folded spans on both sides.
    const rows = buildPatchDisplayRows(file.lines);
    const c8 = rows.find((row) => row.line.content === ' c8');
    expect(c8).toMatchObject({ oldLine: 8, newLine: 8 });
    const ours = rows.find((row) => row.line.content === '-o1');
    expect(ours).toMatchObject({ oldLine: 11, newLine: 0, side: 'old' });
    const theirs = rows.find((row) => row.line.content === '+t1');
    expect(theirs).toMatchObject({ oldLine: 0, newLine: 11, side: 'new' });
    const d1 = rows.find((row) => row.line.content === ' d1');
    expect(d1).toMatchObject({ oldLine: 12, newLine: 12 });
    // Marker/fold rows display but are unnumbered.
    const marker = rows.find((row) => row.line.content === '<<<<<<< ours');
    expect(marker).toMatchObject({ oldLine: 0, newLine: 0, side: 'context' });
  });

  it('expands a fold by id, leaving other folds and ids stable', () => {
    const before = Array.from({ length: 10 }, (_, i) => `c${i + 1}`);
    const after = Array.from({ length: 10 }, (_, i) => `d${i + 1}`);
    const content = [
      ...before,
      '<<<<<<< ours',
      'o1',
      '=======',
      't1',
      '>>>>>>> theirs',
      ...after,
      '',
    ].join('\n');

    const file = conflictPatchFile('a.txt', content, { expandedFolds: new Set([0]) });
    expect(contents(file).slice(0, 8)).toEqual([
      '@@ -1 +1 @@',
      ' c1',
      ' c2',
      ' c3',
      ' c4',
      ' c5',
      ' c6',
      ' c7',
    ]);
    // The trailing fold keeps its id even with the leading one expanded.
    expect(file.lines.at(-1)?.fold).toEqual({ id: 1, lines: 7 });
  });

  it('shows short hidden runs in full instead of a fold row', () => {
    const file = conflictPatchFile('a.txt', [
      'c1',
      'c2',
      'c3',
      'c4',
      '<<<<<<< ours',
      'o1',
      '=======',
      't1',
      '>>>>>>> theirs',
      '',
    ].join('\n'));
    // 4 lines, 3 kept as context, 1 hidden — under the fold minimum.
    expect(file.lines.some((line) => line.fold)).toBe(false);
    expect(contents(file).slice(0, 5)).toEqual(['@@ -1 +1 @@', ' c1', ' c2', ' c3', ' c4']);
  });

  it('renders diff3 base sections as unnumbered marker rows', () => {
    const file = conflictPatchFile('a.txt', [
      '<<<<<<< ours',
      'current',
      '||||||| base',
      'original',
      '=======',
      'incoming',
      '>>>>>>> theirs',
      '',
    ].join('\n'));
    expect(contents(file)).toEqual([
      '<<<<<<< ours',
      '@@ -1 +1 @@',
      '-current',
      '||||||| base',
      'original',
      '=======',
      '+incoming',
      '>>>>>>> theirs',
    ]);
    expect(types(file)).toEqual(['marker', 'meta', 'del', 'marker', 'marker', 'marker', 'add', 'marker']);
  });

  it('treats an unterminated conflict region as plain content', () => {
    const file = conflictPatchFile('a.txt', [
      '<<<<<<< ours',
      'dangling',
      '=======',
      'more',
      '',
    ].join('\n'));
    // No closing >>>>>>> — falls through to the no-markers presentation
    // rather than mislabeling half the file as a conflict side.
    expect(file.conflicts).toBe(0);
    expect(file.lines[0]?.type).toBe('marker');
    expect(file.lines[0]?.content).toContain('No conflict markers');
  });

  it('presents marker-free content behind one fold with an explanatory row', () => {
    const lines = Array.from({ length: 8 }, (_, i) => `l${i + 1}`);
    const file = conflictPatchFile('a.txt', [...lines, ''].join('\n'));
    expect(file.conflicts).toBe(0);
    expect(types(file)).toEqual(['marker', 'marker']);
    expect(file.lines[1]?.fold).toEqual({ id: 0, lines: 8 });

    const expanded = conflictPatchFile('a.txt', [...lines, ''].join('\n'), {
      expandedFolds: new Set([0]),
    });
    expect(contents(expanded).slice(1, 4)).toEqual(['@@ -1 +1 @@', ' l1', ' l2']);
  });

  it('handles empty content', () => {
    const file = conflictPatchFile('a.txt', '');
    expect(file.conflicts).toBe(0);
    expect(file.lines).toHaveLength(1);
    expect(file.lines[0]?.type).toBe('marker');
  });

  it('renders notes as top marker rows and derives the structural badge', () => {
    const note = 'CONFLICT (modify/delete): a.txt deleted in main and modified in feature.';
    const lines = Array.from({ length: 8 }, (_, i) => `l${i + 1}`);
    const file = conflictPatchFile('a.txt', [...lines, ''].join('\n'), { notes: [note] });
    // The note replaces the generic explanatory row; content folds below.
    expect(types(file)).toEqual(['marker', 'marker']);
    expect(file.lines[0]?.content).toBe(note);
    expect(file.lines[1]?.fold).toEqual({ id: 0, lines: 8 });
    expect(file.conflictLabel).toBe('modify/delete');
  });

  it('renders a note-only file (unfetchable content) as just its note rows', () => {
    const file = conflictPatchFile('a.txt', '', {
      notes: ['CONFLICT (rename/delete): a.txt renamed in main and deleted in feature.'],
    });
    expect(types(file)).toEqual(['marker']);
    expect(file.conflictLabel).toBe('rename/delete');
  });

  it('keeps the conflict count and puts notes first when textual regions exist too', () => {
    const file = conflictPatchFile(
      'a.txt',
      ['<<<<<<< ours', 'left', '=======', 'right', '>>>>>>> theirs', ''].join('\n'),
      { notes: ['CONFLICT (distinct types): a.txt had additional trouble'] },
    );
    expect(file.conflicts).toBe(1);
    expect(file.conflictLabel).toBeUndefined();
    expect(file.lines[0]?.content).toContain('additional trouble');
  });
});
