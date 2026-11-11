import { describe, expect, it } from 'vitest';
import { ciLogAnswerHits, ciLogFindHits, findCILogKey, placeCILogLine, resolveCILogHit, type CILogFindHit } from './ciLogFind';
import { findNeedle } from './findMatch';
import { countFindMatches } from './reviewFindScan';
import { CI_LOG_BLOCK_LINES } from '../stores/ciLogBlocks.svelte';
import type { CILogSection } from '../types/models';

function section(key: string, start: number, end: number): CILogSection {
  return { key, name: key, status: 'success', start, end, lines: end - start, bytes: 0 };
}

// A deterministic generator, so a failure names its seed.
function random(seed: number): () => number {
  let state = seed >>> 0;
  return () => {
    state = (state + 0x6d2b79f5) >>> 0;
    let t = state;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

describe('ciLogFindHits', () => {
  it('keeps the matches of lines a section lists, by line and index on it, and places their lines in the rows shown now', () => {
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
      const at = placeCILogLine(shown, hit.line);
      return at && [at.rowFrom, at.sectionKey];
    });
    expect(placed(sections)).toEqual([[0, 'a'], [0, 'a'], [150, 'b'], [150, 'b'], [200, 'b']]);
    // The log grew and its sections moved: the same lines, placed in the
    // rows shown now; a line no section lists is not present.
    expect(placed([section('c', 100, 180), section('d', 180, 450)])).toEqual([null, null, [100, 'c'], [180, 'd'], [200, 'd']]);
  });

  it('stops at its limit', () => {
    const search = { version: 1, totalLines: 10, matches: 5, capped: false, hits: [{ line: 1, count: 5 }] };
    expect(ciLogFindHits(search, [section('a', 0, 10)], 3)).toHaveLength(3);
  });

  it('applies an answer only to the job and log version it was read from', () => {
    const search = { version: 4, totalLines: 10, matches: 1, capped: false, hits: [{ line: 2, count: 1 }] };
    const a = { jobId: 'A', version: 4, sections: [section('a', 0, 10)] };
    expect(ciLogAnswerHits(search, 'A', a, 10)?.map((hit) => [hit.line, placeCILogLine(a.sections, hit.line)?.sectionKey])).toEqual([[2, 'a']]);
    // Job B opened while A's read was out: A's lines are not B's.
    expect(ciLogAnswerHits(search, 'A', { ...a, jobId: 'B', sections: [section('b', 0, 10)] }, 10)).toBeNull();
    // A rewritten or grown log: its sections are not the answer's.
    expect(ciLogAnswerHits(search, 'A', { ...a, version: 5 }, 10)).toBeNull();
    expect(ciLogAnswerHits(search, 'A', null, 10)).toBeNull();
  });
});

describe('resolveCILogHit', () => {
  const needle = findNeedle('ab');
  const hit = (line: number, occurrence: number): CILogFindHit => ({ identity: `${line}:${occurrence}`, line, occurrence });

  it('counts the row index from the text shown now, and answers unknown for a line not loaded', () => {
    const sections = [section('a', 0, 300)];
    const text = new Map<number, string>([[200, 'ab ab'], [201, 'x'], [202, 'ab']]);
    const shown = (line: number) => text.get(line) ?? null;
    expect(resolveCILogHit(sections, shown, needle, hit(202, 0))).toEqual({
      kind: 'present', at: { sectionKey: 'a', rowFrom: 200, rowKey: findCILogKey(200), occurrence: 2 },
    });
    // The match is gone from its own line.
    expect(resolveCILogHit(sections, shown, needle, hit(201, 0))).toEqual({ kind: 'absent' });
    // An earlier line of the row is not loaded: its count is not known.
    text.delete(200);
    expect(resolveCILogHit(sections, shown, needle, hit(202, 0))).toEqual({ kind: 'unknown', sectionKey: 'a' });
    // A line no section lists.
    expect(resolveCILogHit(sections, shown, needle, hit(400, 0))).toEqual({ kind: 'absent' });
  });

  // Invariant: resolving an identity against the log shown now answers what
  // a fresh count of that text says: its row and the index the painter
  // gives it there, or absent. Lines are rewritten, the log grows and
  // sections move between draws; the hits are the first draw's.
  it('agrees with a fresh count of the shown text after random rewrites and growth', () => {
    const words = ['ab', 'x', 'abab', 'AB', 'xab', ''];
    for (let seed = 1; seed <= 200; seed += 1) {
      const next = random(seed);
      const pick = <T>(items: readonly T[]) => items[Math.floor(next() * items.length)];
      let total = 50 + Math.floor(next() * 400);
      const text: string[] = [];
      for (let line = 0; line < total; line += 1) text.push(Array.from({ length: Math.floor(next() * 4) }, () => pick(words)).join(' '));
      const sectionsOf = () => {
        const cut = Math.floor(next() * total);
        return [section('s1', 0, cut), section('s2', cut, total)];
      };
      const published: CILogFindHit[] = [];
      text.forEach((line, index) => {
        for (let occurrence = 0; occurrence < countFindMatches(line, needle); occurrence += 1) published.push(hit(index, occurrence));
      });
      for (let change = 0; change < 6; change += 1) {
        const rewrites = Math.floor(next() * 20);
        for (let r = 0; r < rewrites; r += 1) text[Math.floor(next() * total)] = Array.from({ length: Math.floor(next() * 4) }, () => pick(words)).join(' ');
        if (next() < 0.3) {
          const grown = Math.floor(next() * 200);
          for (let g = 0; g < grown; g += 1) text.push(pick(words));
          total += grown;
        }
        const sections = sectionsOf();
        const loaded = new Set<number>();
        for (let block = 0; block * CI_LOG_BLOCK_LINES < total; block += 1) if (next() < 0.8) loaded.add(block);
        const shown = (line: number) => (line < total && loaded.has(Math.floor(line / CI_LOG_BLOCK_LINES)) ? text[line] : null);
        for (const each of published) {
          const got = resolveCILogHit(sections, shown, needle, each);
          const placed = placeCILogLine(sections, each.line);
          const rowLines = placed ? Array.from({ length: each.line - placed.rowFrom + 1 }, (_, i) => placed.rowFrom + i) : [];
          const label = `seed ${seed} change ${change} ${each.identity}`;
          if (!placed) {
            expect(got, label).toEqual({ kind: 'absent' });
          } else if (rowLines.some((line) => shown(line) === null)) {
            // Not loaded: only its own line, when shown, may already say absent.
            const own = shown(each.line);
            expect(got, label).toEqual(own !== null && each.occurrence >= countFindMatches(own, needle) ? { kind: 'absent' } : { kind: 'unknown', sectionKey: placed.sectionKey });
          } else {
            // The fresh count: this row's matches in order, line by line.
            const row: string[] = [];
            for (const line of rowLines) for (let o = 0; o < countFindMatches(text[line], needle); o += 1) row.push(`${line}:${o}`);
            const ordinal = row.indexOf(each.identity);
            expect(got, label).toEqual(ordinal < 0 ? { kind: 'absent' } : {
              kind: 'present', at: { sectionKey: placed.sectionKey, rowFrom: placed.rowFrom, rowKey: findCILogKey(placed.rowFrom), occurrence: ordinal },
            });
          }
        }
      }
    }
  });
});
