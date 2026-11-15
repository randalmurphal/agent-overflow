import { describe, expect, it, vi } from 'vitest';
import vectors from '../../test/fixtures/findVectors.json';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  countFindMatches, FIND_FILTER_CODE_POINTS, findFilterPattern, findMatchChunk, findNeedle, flagFilterLines, foldFindText, forEachFindMatch, LONG_TEXT,
  textsMatchNeedle,
} from './findMatch';
import { FIND_FOLD_RANGES, FIND_FOLD_UNICODE_VERSION } from './findFoldTable';
import { unpackText } from './packedText';
import { packText } from '../../test/helpers/streamedPatch';
import { seededRandom as random } from '../../test/helpers/seededRandom';

// The lines `flagFilterLines` flags in `lines` stored as one block.
function flaggedLines(pattern: RegExp, lines: readonly string[]): Set<number> {
  const starts: number[] = [];
  for (let at = 0, line = 0; line < lines.length; line += 1) {
    starts.push(at);
    at += lines[line].length + 1;
  }
  const out = new Int32Array(lines.length);
  const count = flagFilterLines(pattern, lines.join('\n'), 0, lines.length, (line) => starts[line], out);
  return new Set(out.subarray(0, count));
}

function ruleMatches(text: string, needle: string): boolean {
  let found = false;
  forEachFindMatch(foldFindText(text), needle, () => { found = true; });
  return found;
}

// Plain text with no character in U+0080..U+00FF: no packed spelling can
// match it, so the filter is exact there.
const PACKED_RANGE = /[\u0080-\u00ff]/;

// ASCII, capitals, letters past ASCII with and without a fold, astral
// letters that fold (Deseret), regex syntax, the Kelvin sign, dotted and
// dotless i, characters whose plain form is a packed spelling (Ã©), and
// U+FFFD in text.
const ALPHABET = [
  'a', 'b', 'k', 'i', 's', 'A', 'B', 'K', 'I', 'S', ' ', '+', '-', '0',
  '.', '*', '[', ']', '\\', '(', ')', '^', '$', '|', '?', '{', '}', '/',
  '\u212a', 'İ', 'ı', 'ſ', 'é', 'É', 'Ã', '©', 'ß', 'ẞ',
  'Σ', 'σ', 'ς', 'Ж', 'ж', '中', '\u1c89', '\u1c8a', '\u{10400}', '\u{10428}', '😀', '\ufffd',
];

describe('find filter pattern', () => {
  it('is null for what it cannot spell, and bounded for a long needle', () => {
    expect(findFilterPattern('')).toBeNull();
    expect(findFilterPattern('a\ud800b')).toBeNull();
    expect(findFilterPattern('\udc28')).toBeNull();
    expect(findFilterPattern('x\ufffd')).toBeNull();
    // Past the spelled prefix nothing is read, a lone surrogate included.
    expect(findFilterPattern('a'.repeat(FIND_FILTER_CODE_POINTS) + '\ud800')).not.toBeNull();
    // The largest spelling of one code point, spelled at every place: a
    // code point something else folds to, or one that only folds from
    // itself, whose spelling is set by its UTF-8 length.
    const candidates = new Set([0x80, 0x800, 0x10000, 0x10ffff]);
    for (let code = 0; code <= 0x10ffff; code += 1) {
      if (code >= 0xd800 && code <= 0xdfff) continue;
      const folded = foldFindText(String.fromCodePoint(code)).codePointAt(0)!;
      if (folded !== code) candidates.add(folded);
    }
    let widest = '';
    for (const code of candidates) {
      const char = String.fromCodePoint(code);
      if (findFilterPattern(char)!.source.length > (widest === '' ? 0 : findFilterPattern(widest)!.source.length)) widest = char;
    }
    const longest = findFilterPattern(widest.repeat(1000))!;
    expect(longest.source.length).toBe(findFilterPattern(widest)!.source.length * FIND_FILTER_CODE_POINTS);
    expect(longest.source.length).toBeLessThan(2 * 1024);
    // A line holding the spelled prefix and not the rest is flagged: the
    // rule decides.
    const prefix = [...findNeedle('Needle '.repeat(20))].slice(0, FIND_FILTER_CODE_POINTS).join('');
    expect(flaggedLines(findFilterPattern(findNeedle('Needle '.repeat(20)))!, [`a ${prefix.toUpperCase()} b`])).toEqual(new Set([0]));
    // A needle longer than the prefix still flags every line holding it.
    const needle = findNeedle('Needle '.repeat(20));
    expect(flaggedLines(findFilterPattern(needle)!, ['x', `say ${'NEEDLE '.repeat(20)}`, 'y'])).toEqual(new Set([1]));
  });

  it('flags every vector the rule matches, in plain and packed text', () => {
    const cases: [string, string][] = [
      ...vectors.count.map(({ text, query }) => [text, query] as [string, string]),
      ...vectors.offsets.map(({ text, query }) => [text, query] as [string, string]),
      ...(vectors.fold as [string, string][]).flatMap(([text, folded]) => [[text, folded], [folded, text]] as [string, string][]),
    ];
    for (const [text, query] of cases) {
      const needle = findNeedle(query);
      if (!needle) continue;
      const pattern = findFilterPattern(needle)!;
      const matches = ruleMatches(text, needle);
      if (matches) {
        expect(flaggedLines(pattern, ['', text, '']), `${query} in ${text}`).toEqual(new Set([1]));
        expect(flaggedLines(pattern, ['', packText(text), '']), `${query} in packed ${text}`).toEqual(new Set([1]));
      }
      if (!PACKED_RANGE.test(text + needle)) expect(flaggedLines(pattern, [text]).size, `${query} in ${text}`).toBe(matches ? 1 : 0);
    }
  });

  it('spells every code point that folds, plain and packed', () => {
    let spelled = 0;
    for (let code = 0; code <= 0x10ffff; code += 1) {
      if (code >= 0xd800 && code <= 0xdfff) continue;
      const char = String.fromCodePoint(code);
      const folded = foldFindText(char);
      if (folded === char) continue;
      // The Kelvin sign and U+0130 fold to ASCII needles.
      const pattern = findFilterPattern(folded)!;
      if (flaggedLines(pattern, [char]).size !== 1) throw new Error(`U+${code.toString(16)} is not spelled`);
      if (flaggedLines(pattern, [packText(char)]).size !== 1) throw new Error(`packed U+${code.toString(16)} is not spelled`);
      spelled += 1;
    }
    expect(spelled).toBeGreaterThan(1400);
  });

  it('flags every line the rule matches over random text, and in plain text exactly those', () => {
    const next = random(7);
    const word = (length: number) => Array.from({ length }, () => ALPHABET[next(ALPHABET.length)]).join('');
    for (let round = 0; round < 3000; round += 1) {
      const lines = Array.from({ length: 1 + next(6) }, () => word(next(12)));
      const from = lines[next(lines.length)];
      // A needle cut from the text (folded or not) or made up.
      const query = next(3) === 0 || from === '' ? word(1 + next(3)) : [...from].slice(next(2), 1 + next(4)).join('');
      const needle = findNeedle(next(2) === 0 ? query : query.toUpperCase());
      const pattern = needle === '' ? null : findFilterPattern(needle);
      if (pattern === null) {
        expect(needle === '' || needle.includes('\ufffd')).toBe(true);
        continue;
      }
      const want = new Set(lines.flatMap((line, index) => ruleMatches(line, needle) ? [index] : []));
      const plain = flaggedLines(pattern, lines);
      for (const line of want) expect(plain.has(line), `${needle} in ${lines[line]}`).toBe(true);
      if (!PACKED_RANGE.test(lines.join('') + needle)) expect(plain, `${needle} in ${lines.join(' | ')}`).toEqual(want);
      const packed = flaggedLines(pattern, lines.map(packText));
      for (const line of want) expect(packed.has(line), `${needle} in packed ${lines[line]}`).toBe(true);
    }
  });

  it('flags every line the rule matches in packed text holding invalid bytes', () => {
    const next = random(11);
    for (let round = 0; round < 2000; round += 1) {
      let packed = '';
      for (let piece = next(8); piece >= 0; piece -= 1) {
        packed += next(3) === 0 ? String.fromCharCode(0x80 + next(0x80)) : packText(ALPHABET[next(ALPHABET.length)]);
      }
      const text = unpackText(packed);
      const chars = [...text];
      const at = next(chars.length + 1);
      const needle = findNeedle(chars.slice(at, at + 1 + next(3)).join(''));
      if (needle === '' || needle.includes('\ufffd')) continue;
      if (ruleMatches(text, needle)) expect(flaggedLines(findFilterPattern(needle)!, [packed]).size, `${needle} in ${text}`).toBe(1);
    }
  });
});

// The Go side (internal/stringsx, internal/cilog, internal/store) reads the
// same vectors.
describe('find matching', () => {
  it('meets the vectors the Go side shares', () => {
    expect(vectors.fold.length).toBeGreaterThan(0);
    for (const [text, folded] of vectors.fold as [string, string][]) {
      expect(foldFindText(text), text).toBe(folded);
      expect(foldFindText(text)).toHaveLength(text.length);
    }
    for (const { text, query, count } of vectors.count) {
      expect(countFindMatches(text, findNeedle(query)), `${query} in ${text}`).toBe(count);
    }
    for (const { text, query, at } of vectors.offsets) {
      const found: number[] = [];
      forEachFindMatch(foldFindText(text), findNeedle(query), (offset) => found.push(Array.from(text.slice(0, offset)).length));
      expect(found, `${query} in ${text}`).toEqual(at);
    }
  });

  it('a chunk says what it folded: the chunk and the needle less one past it, never past the run or inside a pair', () => {
    const run = 'a'.repeat(LONG_TEXT + 10);
    expect(findMatchChunk(run, 0, 'abcd').folded).toBe(LONG_TEXT + 3);
    expect(findMatchChunk(run, 5, 'abcd').folded).toBe(LONG_TEXT + 3);
    expect(findMatchChunk(run, 0, 'a'.repeat(40)).folded).toBe(run.length);
    expect(findMatchChunk('needle', 0, 'needle')).toEqual({ count: 1, next: 6, last: true, folded: 6 });
    // Both edges fall inside a pair and move past it.
    const pairs = 'x'.repeat(LONG_TEXT - 1) + '\u{10400}'.repeat(4);
    expect(findMatchChunk(pairs, 0, 'xx').folded).toBe(LONG_TEXT + 3);
  });

  it('flags lines placed by the caller\'s line starts, from any line of the store', () => {
    const lines = ['zero', 'one needle', 'two', 'needle three needle', 'four'];
    // Lines 2..4 of a store whose positions start at 1000.
    const starts = [1000, 1005, 1016, 1020, 1040];
    const out = new Int32Array(3);
    const count = flagFilterLines(findFilterPattern('needle')!, lines.slice(2).join('\n'), 2, 5, (line) => starts[line], out);
    expect(Array.from(out.subarray(0, count))).toEqual([3]);
  });

  it('keeps the UTF-16 length of every code point', () => {
    for (let code = 0; code <= 0x10ffff; code += 1) {
      if (code >= 0xd800 && code <= 0xdfff) continue;
      const char = String.fromCodePoint(code);
      if (foldFindText(char).length !== char.length) throw new Error(`U+${code.toString(16)} changes length`);
    }
  });

  it('folds the same whole or in pieces', () => {
    for (const text of ['ΣΑΣ', 'İX', 'a😀B', 'AbC', 'ΣΊΣΥΦΟΣ', '\u{10400}\u{10401}']) {
      expect(foldFindText(text)).toBe([...text].map(foldFindText).join(''));
    }
  });

  it('a row filter matches any of its texts by the same rule', () => {
    expect(textsMatchNeedle(findNeedle(' RETRY '), [null, 'Builds', 'needs a retry'])).toBe(true);
    expect(textsMatchNeedle(findNeedle('école'), ['École'])).toBe(true);
    expect(textsMatchNeedle(findNeedle('ÉCOLE'), ['une école'])).toBe(true);
    expect(textsMatchNeedle(findNeedle('strasse'), ['Straße'])).toBe(false);
    expect(textsMatchNeedle('', [])).toBe(true);
  });

  it('reads the table the Go side reads', () => {
    // Tests run from frontend/.
    const go = readFileSync(resolve(process.cwd(), '../internal/stringsx/findfold_table.go'), 'utf8');
    expect(go).toContain(`const FindFoldUnicodeVersion = "${FIND_FOLD_UNICODE_VERSION}"`);
    const ranges = [...go.matchAll(/^\t\{(0x[0-9A-F]+), (0x[0-9A-F]+), (-?\d+), (\d+)\},$/gm)].flatMap((m) => m.slice(1).map(Number));
    expect(ranges.length).toBeGreaterThan(100);
    expect(Array.from(FIND_FOLD_RANGES)).toEqual(ranges);
  });

  it('folds by the table, not the engine: U+1C89 (Unicode 16) lowers to U+1C8A', () => {
    expect(foldFindText('\u1C89')).toBe('\u1C8A');
    // An engine's case tables follow its ICU (WebKitGTK's is Unicode 15.1).
    const lower = vi.spyOn(String.prototype, 'toLowerCase');
    const locale = vi.spyOn(String.prototype, 'toLocaleLowerCase');
    try {
      for (const [text, folded] of vectors.fold as [string, string][]) expect(foldFindText(text)).toBe(folded);
      expect(lower).not.toHaveBeenCalled();
      expect(locale).not.toHaveBeenCalled();
    } finally {
      lower.mockRestore();
      locale.mockRestore();
    }
  });
});
