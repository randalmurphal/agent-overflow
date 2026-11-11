// The match rule of every find and row filter: the query trimmed, matched
// literally as a substring, with ASCII letters folded to lower case and
// every other character compared as is; matches are counted left to right
// without overlap. Folding keeps the text's length, so an offset in folded
// text is an offset in the original (findHighlights.ts paints from it), and
// text folds the same whole or node by node. Thread find's SQLite LOWER and
// the CI log search (cilog.FoldForFind) fold the same; the vectors in
// src/test/fixtures/findVectors.json, read by both tests, hold them to it.

const ASCII_UPPER = /[A-Z]/g;

/** `text` folded for matching; the same length as `text`. */
export function foldFindText(text: string): string {
  return text.replace(ASCII_UPPER, (char) => String.fromCharCode(char.charCodeAt(0) + 32));
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
