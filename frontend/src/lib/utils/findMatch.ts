// The match rule of every find and row filter: the query trimmed, matched
// literally as a substring, with each code point folded on its own to its
// simple lower case and matches counted left to right without overlap.
// The lower case is the checked-in table findFoldTable.ts (generated from
// Go's package unicode, the table the Go side reads), never an engine's
// own case tables, which differ by browser and ICU version.
// Folding keeps every code point and its UTF-16 length, so an offset in
// folded text is an offset in the original (findHighlights.ts paints from
// it), and text folds the same whole or node by node. Thread find's SQL
// (store find_fold) and the CI log search fold with the Go side of the
// rule (stringsx.FoldForFind); the vectors in
// src/test/fixtures/findVectors.json, read by both sides' tests, hold them
// to it.

import { FIND_FOLD_RANGES } from './findFoldTable';

// What may fold: ASCII capitals and every non-ASCII code point.
const FOLDABLE = /[A-Z]|[^\0-\x7f]/gu;
const RUNS = FIND_FOLD_RANGES.length / 4;

/** A code point's simple lower case by the find fold table. No mapping in
 * it changes a code point's UTF-16 length (the generator checks). */
export function foldCodePointValue(code: number): number {
  let lo = 0;
  let hi = RUNS - 1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    const at = mid * 4;
    if (FIND_FOLD_RANGES[at + 1] < code) lo = mid + 1;
    else if (FIND_FOLD_RANGES[at] > code) hi = mid - 1;
    else return (code - FIND_FOLD_RANGES[at]) % FIND_FOLD_RANGES[at + 3] === 0 ? code + FIND_FOLD_RANGES[at + 2] : code;
  }
  return code;
}

function foldCodePoint(char: string): string {
  if (char.length === 1 && char <= 'Z') return String.fromCharCode(char.charCodeAt(0) + 32);
  const code = char.codePointAt(0)!;
  const folded = foldCodePointValue(code);
  return folded === code ? char : String.fromCodePoint(folded);
}

/** `text` folded for matching; the same length as `text`. */
export function foldFindText(text: string): string {
  return text.replace(FOLDABLE, foldCodePoint);
}

/** The folded needle for a query; '' when the query is blank. */
export function findNeedle(query: string): string {
  return foldFindText(query.trim());
}

/** Calls `each` with the offset of every match of `needle` in folded text. */
export function forEachFindMatch(folded: string, needle: string, each: (at: number) => void): void {
  if (!needle) return;
  for (let at = folded.indexOf(needle); at >= 0; at = folded.indexOf(needle, at + needle.length)) each(at);
}

/** Code points of a needle a filter pattern spells. A text holding a
 * match of the needle holds a match of its first code points, so a
 * longer needle is filtered by its prefix. Each spelled code point is one
 * more alternative a start position can walk, so the bound trades lines
 * flagged in vain against the worst-case search. Measured over 200,000
 * lines of this repository's Go and TypeScript with 200 needles of 9 to
 * 40 characters copied from it, lines flagged that the needle does not
 * match, per 100,000: 85 at 8 code points, 8 at 12, 3 at 16. On text that
 * matches all but the last spelled code point at every start (512 lines
 * of 100 "é"), a block searches in 0.5 ms at 8, 0.9 ms at 12, 1.4 ms at 16
 * and 8.1 ms at 64, against 5.6 ms for the rule over the same lines. Each
 * code point spells as at most 3 preimages (the fold table's largest),
 * each at most 2 UTF-16 units plain and 4 bytes packed, so the pattern
 * source stays under 2 KB. */
export const FIND_FILTER_CODE_POINTS = 12;

// Every code point x with foldCodePointValue(x) === code, read from the
// fold table backwards; code itself when it folds to itself.
function foldPreimage(code: number): number[] {
  const out: number[] = [];
  if (foldCodePointValue(code) === code) out.push(code);
  for (let run = 0; run < RUNS; run += 1) {
    const at = run * 4;
    const x = code - FIND_FOLD_RANGES[at + 2];
    if (x >= FIND_FOLD_RANGES[at] && x <= FIND_FOLD_RANGES[at + 1] && (x - FIND_FOLD_RANGES[at]) % FIND_FOLD_RANGES[at + 3] === 0) out.push(x);
  }
  return out;
}

function unitEscape(unit: number): string {
  return `\\u${unit.toString(16).padStart(4, '0')}`;
}

// A code point's UTF-8 bytes, each written as the character of that value:
// its spelling in packed text (packedText.ts).
function packedEscape(code: number): string {
  const bytes = code < 0x800
    ? [0xc0 | (code >> 6), 0x80 | (code & 0x3f)]
    : code < 0x10000
      ? [0xe0 | (code >> 12), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f)]
      : [0xf0 | (code >> 18), 0x80 | ((code >> 12) & 0x3f), 0x80 | ((code >> 6) & 0x3f), 0x80 | (code & 0x3f)];
  return bytes.map((byte) => `\\x${byte.toString(16)}`).join('');
}

/**
 * A filter for the find rule over text as a patch store holds it, plain
 * or packed (packedText.ts), without folding or decoding it: a global
 * pattern that matches, in any text that holds a match of `needle` by the
 * rule (`forEachFindMatch(foldFindText(text), needle)`), at or before
 * that match's start. Each of the needle's first FIND_FILTER_CODE_POINTS
 * code points becomes the alternatives of every code point that folds to
 * it, spelled as UTF-16 and, past ASCII, as packed UTF-8 bytes. It can
 * match where the rule does not (a packed spelling read as plain text, a
 * prefix of a longer needle); the rule decides. Folding keeps length, so
 * a match's position in the text is its position in the folded text.
 *
 * Null when every text must be taken as matching: `needle` is empty or
 * holds a code point the pattern cannot spell, a lone surrogate (which
 * can match half of a pair) or U+FFFD (which packed text decodes from any
 * invalid byte sequence).
 */
export function findFilterPattern(needle: string): RegExp | null {
  if (needle === '') return null;
  let source = '';
  let spelled = 0;
  for (const char of needle) {
    if (spelled === FIND_FILTER_CODE_POINTS) break;
    spelled += 1;
    const code = char.codePointAt(0)!;
    if ((code >= 0xd800 && code <= 0xdfff) || code === 0xfffd) return null;
    let units = '';
    const sequences: string[] = [];
    for (const x of foldPreimage(code)) {
      if (x < 0x10000) units += unitEscape(x);
      else sequences.push(unitEscape(0xd800 + ((x - 0x10000) >> 10)) + unitEscape(0xdc00 + ((x - 0x10000) & 0x3ff)));
      if (x >= 0x80) sequences.push(packedEscape(x));
    }
    source += sequences.length === 0 ? `[${units}]` : `(?:${units === '' ? '' : `[${units}]|`}${sequences.join('|')})`;
  }
  return new RegExp(source, 'g');
}

/** The number of matches of `needle` in folded text. */
export function countFindMatches(folded: string, needle: string): number {
  let count = 0;
  forEachFindMatch(folded, needle, () => { count += 1; });
  return count;
}

/** True when any of `texts` contains `needle` (from `findNeedle`). An empty
 * needle matches everything. */
export function textsMatchNeedle(needle: string, texts: readonly (string | null | undefined)[]): boolean {
  if (!needle) return true;
  for (const text of texts) {
    if (text && foldFindText(text).includes(needle)) return true;
  }
  return false;
}
