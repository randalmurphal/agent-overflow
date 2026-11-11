import { describe, expect, it, vi } from 'vitest';
import vectors from '../../test/fixtures/findVectors.json';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { countFindMatches, findNeedle, foldFindText, forEachFindMatch, textsMatchNeedle } from './findMatch';
import { FIND_FOLD_RANGES, FIND_FOLD_UNICODE_VERSION } from './findFoldTable';

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
      expect(countFindMatches(foldFindText(text), findNeedle(query)), `${query} in ${text}`).toBe(count);
    }
    for (const { text, query, at } of vectors.offsets) {
      const found: number[] = [];
      forEachFindMatch(foldFindText(text), findNeedle(query), (offset) => found.push(Array.from(text.slice(0, offset)).length));
      expect(found, `${query} in ${text}`).toEqual(at);
    }
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
