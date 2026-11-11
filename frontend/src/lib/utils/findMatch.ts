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
