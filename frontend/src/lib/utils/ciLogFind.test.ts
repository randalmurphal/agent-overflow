import { describe, expect, it } from 'vitest';
import { ciLogAnswerHits, ciLogFindHits, locateCILogHit } from './ciLogFind';
import type { CILogSection } from '../types/models';

function section(key: string, start: number, end: number): CILogSection {
  return { key, name: key, status: 'success', start, end, lines: end - start, bytes: 0 };
}

describe('ciLogFindHits', () => {
  it('keeps the matches of lines a section lists, by line and index on it, and places them in the rows shown now', () => {
    const sections = [section('b', 150, 450), section('a', 0, 150), section('empty', 450, 450)];
    const search = {
      version: 3, totalLines: 600, matches: 7, capped: false,
      hits: [{ line: 10, count: 2 }, { line: 160, count: 1 }, { line: 199, count: 1 }, { line: 200, count: 1 }, { line: 500, count: 2 }],
    };
    const hits = ciLogFindHits(search, sections, 100);
    expect(hits.map((hit) => [hit.identity, hit.line, hit.occurrence])).toEqual([
      ['10:0', 10, 0], ['10:1', 10, 1], ['160:0', 160, 0], ['199:0', 199, 0], ['200:0', 200, 0],
    ]);
    const placed = (shown: typeof sections) => hits.map((hit) => {
      const at = locateCILogHit(shown, hits, hit);
      return at && [at.rowKey, at.occurrence, at.sectionKey];
    });
    expect(placed(sections)).toEqual([
      ['cl:0', 0, 'a'],
      ['cl:0', 1, 'a'],
      ['cl:150', 0, 'b'],
      ['cl:150', 1, 'b'],
      ['cl:200', 0, 'b'],
    ]);
    // The log grew and its sections moved: the same matches, placed in
    // the rows shown now; a line no section lists is not present.
    expect(placed([section('c', 100, 180), section('d', 180, 450)])).toEqual([
      null,
      null,
      ['cl:100', 0, 'c'],
      ['cl:180', 0, 'd'],
      ['cl:200', 0, 'd'],
    ]);
  });

  it('stops at its limit', () => {
    const search = { version: 1, totalLines: 10, matches: 5, capped: false, hits: [{ line: 1, count: 5 }] };
    expect(ciLogFindHits(search, [section('a', 0, 10)], 3)).toHaveLength(3);
  });

  it('applies an answer only to the job and log version it was read from', () => {
    const search = { version: 4, totalLines: 10, matches: 1, capped: false, hits: [{ line: 2, count: 1 }] };
    const a = { jobId: 'A', version: 4, sections: [section('a', 0, 10)] };
    expect(ciLogAnswerHits(search, 'A', a, 10)?.map((hit) => [hit.line, locateCILogHit(a.sections, [hit], hit)?.sectionKey])).toEqual([[2, 'a']]);
    // Job B opened while A's read was out: A's lines are not B's.
    expect(ciLogAnswerHits(search, 'A', { ...a, jobId: 'B', sections: [section('b', 0, 10)] }, 10)).toBeNull();
    // A rewritten or grown log: its sections are not the answer's.
    expect(ciLogAnswerHits(search, 'A', { ...a, version: 5 }, 10)).toBeNull();
    expect(ciLogAnswerHits(search, 'A', null, 10)).toBeNull();
  });
});
