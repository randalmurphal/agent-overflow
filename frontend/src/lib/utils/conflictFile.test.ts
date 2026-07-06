import { describe, expect, it } from 'vitest';
import { conflictPatchFile } from './conflictFile';

describe('conflictPatchFile', () => {
  it('classifies conflict markers as metadata and preserves non-marker lines as context', () => {
    const file = conflictPatchFile('src/app.ts', [
      'before',
      '<<<<<<< ours',
      'current',
      '||||||| base',
      'base',
      '=======',
      'incoming',
      '>>>>>>> theirs',
      'after',
      '',
    ].join('\n'));

    expect(file).toMatchObject({
      path: 'src/app.ts',
      kind: 'conflict',
      additions: 0,
      deletions: 0,
    });
    expect(file.lines.map((line) => line.content)).toEqual([
      'before',
      '<<<<<<< ours',
      'current',
      '||||||| base',
      'base',
      '=======',
      'incoming',
      '>>>>>>> theirs',
      'after',
    ]);
    expect(file.lines.map((line) => line.type)).toEqual([
      'context',
      'meta',
      'context',
      'meta',
      'context',
      'meta',
      'context',
      'meta',
      'context',
    ]);
  });
});
