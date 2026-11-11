import { describe, expect, it } from 'vitest';
import { ciLogFindHits } from './ciLogFind';
import type { CILogSection } from '../types/models';

function section(key: string, start: number, end: number): CILogSection {
  return { key, name: key, status: 'success', start, end, lines: end - start, bytes: 0 };
}

describe('ciLogFindHits', () => {
  it('maps per-line counts onto the view rows, numbering matches within each row', () => {
    const sections = [section('b', 150, 450), section('a', 0, 150), section('empty', 450, 450)];
    const search = {
      version: 3, totalLines: 600, matches: 7, capped: false,
      hits: [{ line: 10, count: 2 }, { line: 160, count: 1 }, { line: 199, count: 1 }, { line: 200, count: 1 }, { line: 500, count: 2 }],
    };
    expect(ciLogFindHits(search, sections, 100).map((hit) => [hit.identity, hit.rowKey, hit.occurrence, hit.sectionKey])).toEqual([
      ['10:0', 'cl:0', 0, 'a'],
      ['10:1', 'cl:0', 1, 'a'],
      ['160:0', 'cl:150', 0, 'b'],
      ['199:0', 'cl:150', 1, 'b'],
      ['200:0', 'cl:200', 0, 'b'],
    ]);
  });

  it('stops at its limit', () => {
    const search = { version: 1, totalLines: 10, matches: 5, capped: false, hits: [{ line: 1, count: 5 }] };
    expect(ciLogFindHits(search, [section('a', 0, 10)], 3)).toHaveLength(3);
  });
});
