import { describe, expect, it } from 'vitest';
import { countFindMatches, findNeedle, foldFindText } from './findMatch';

describe('find matching', () => {
  it('folds per character and keeps the length', () => {
    expect(foldFindText('AbC')).toBe('abc');
    expect(foldFindText('ÀÉÎ')).toBe('àéî');
    // İ lowercases to two characters: kept, so offsets stay aligned.
    expect(foldFindText('İX')).toBe('İx');
    // Folding whole or in pieces agrees (no final-sigma context).
    expect(foldFindText('ΣΑΣ')).toBe(foldFindText('ΣΑ') + foldFindText('Σ'));
    for (const text of ['ΣΑΣ', 'İX', 'a😀B']) expect(foldFindText(text)).toHaveLength(text.length);
  });

  it('trims the query and counts literal matches without overlap', () => {
    expect(findNeedle('  A.B  ')).toBe('a.b');
    expect(findNeedle('   ')).toBe('');
    expect(countFindMatches(foldFindText('a.b axb A.B'), findNeedle('a.b'))).toBe(2);
    expect(countFindMatches('aaaa', 'aa')).toBe(2);
    expect(countFindMatches('abc', '')).toBe(0);
  });
});
