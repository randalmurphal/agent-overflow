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

/** Characters of one text `findMatchChunk` folds and searches at a time. */
export const LONG_TEXT = 16_384;

// `at`, moved past a low surrogate so a slice there keeps its pair.
function codePointBoundary(text: string, at: number): number {
  if (at <= 0 || at >= text.length) return at;
  const low = text.charCodeAt(at);
  const high = text.charCodeAt(at - 1);
  return low >= 0xdc00 && low <= 0xdfff && high >= 0xd800 && high <= 0xdbff ? at + 1 : at;
}

/**
 * The rule over unfolded text a chunk at a time, so no call folds more
 * than a chunk: the matches of `needle` (folded, `findNeedle`) in `run`
 * from `from`, left to right without overlap, `LONG_TEXT` characters at a
 * time. Answers how many start in this chunk, where the next chunk
 * starts, whether this one ended the run, and how many characters from
 * `from` it folded: the chunk and up to the needle's length less one past
 * it, never past the run.
 */
export function findMatchChunk(run: string, from: number, needle: string): { count: number; next: number; last: boolean; folded: number } {
  const end = codePointBoundary(run, Math.min(run.length, from + LONG_TEXT));
  const foldEnd = codePointBoundary(run, Math.min(run.length, end + needle.length - 1));
  const folded = foldFindText(run.slice(from, foldEnd));
  let count = 0;
  let next = end;
  for (let at = folded.indexOf(needle); at >= 0 && from + at < end; at = folded.indexOf(needle, at + needle.length)) {
    count += 1;
    next = Math.max(next, from + at + needle.length);
  }
  return { count, next, last: end >= run.length, folded: foldEnd - from };
}

/** The matches of `needle` (folded, `findNeedle`) in unfolded `run`, by
 * `findMatchChunk`. */
export function countFindMatches(run: string, needle: string): number {
  let count = 0;
  for (let from = 0; needle !== '' && from + needle.length <= run.length;) {
    const chunk = findMatchChunk(run, from, needle);
    count += chunk.count;
    if (chunk.last) break;
    from = chunk.next;
  }
  return count;
}

/** Code points of a needle a filter pattern spells. A text holding a
 * match of the needle holds a match of its first code points, so a
 * longer needle is filtered by its prefix, and lines holding the prefix
 * without the rest are flagged in vain. The bound caps the alternatives a
 * start position can try, and so the work per position. Each code point
 * spells as at most 3 preimages (the fold table's largest), each at most
 * 2 UTF-16 units, so the pattern source stays under 1 KB. */
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

/**
 * A filter for the find rule over unfolded text: a global pattern that
 * matches, in any text that holds a match of `needle` by the rule
 * (`forEachFindMatch(foldFindText(text), needle)`), at or before that
 * match's start. Each of the needle's first FIND_FILTER_CODE_POINTS code
 * points becomes the alternatives of every code point that folds to it.
 * It can match where the rule does not (a prefix of a longer needle); the
 * rule decides. Folding keeps length, so a match's position in the text
 * is its position in the folded text.
 *
 * Null when every text must be taken as matching: `needle` is empty or
 * holds a code point the pattern cannot spell, a lone surrogate (which
 * can match half of a pair).
 */
export function findFilterPattern(needle: string): RegExp | null {
  if (needle === '') return null;
  let source = '';
  let spelled = 0;
  for (const char of needle) {
    if (spelled === FIND_FILTER_CODE_POINTS) break;
    spelled += 1;
    const code = char.codePointAt(0)!;
    if (code >= 0xd800 && code <= 0xdfff) return null;
    let units = '';
    const sequences: string[] = [];
    for (const x of foldPreimage(code)) {
      if (x < 0x10000) units += unitEscape(x);
      else sequences.push(unitEscape(0xd800 + ((x - 0x10000) >> 10)) + unitEscape(0xdc00 + ((x - 0x10000) & 0x3ff)));
    }
    source += sequences.length === 0 ? `[${units}]` : `(?:${units === '' ? '' : `[${units}]|`}${sequences.join('|')})`;
  }
  return new RegExp(source, 'g');
}

/**
 * Writes to `out`, ascending, each line of `text` whose text holds a match
 * of `filter` (`findFilterPattern`), and to `starts` where in `text` each
 * starts, and returns how many. `text` is lines `start` on joined by
 * newlines (PatchBody.segmentText). A match is placed by counting the
 * newlines before it and the search goes on from the next line, so the
 * work is one search per flagged line and one newline search per line
 * passed. `out` and `starts` must hold the text's lines.
 */
export function flagFilterLines(filter: RegExp, text: string, start: number, out: Int32Array, starts: Int32Array): number {
  let count = 0;
  filter.lastIndex = 0;
  // The line the search is in, and where it starts in `text`.
  let line = start;
  let lineStart = 0;
  for (let match = filter.exec(text); match !== null; match = filter.exec(text)) {
    for (let newline = text.indexOf('\n', lineStart); newline >= 0 && newline < match.index; newline = text.indexOf('\n', lineStart)) {
      line += 1;
      lineStart = newline + 1;
    }
    out[count] = line;
    starts[count] = lineStart;
    count += 1;
    const next = text.indexOf('\n', match.index);
    if (next < 0) break;
    line += 1;
    lineStart = next + 1;
    filter.lastIndex = lineStart;
  }
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
