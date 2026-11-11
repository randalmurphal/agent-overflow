// The matching rule of every in-pane find: the query trimmed, matched
// literally and case-insensitively, matches counted left to right without
// overlap. Folding is per character, so text folds the same whether it is
// read whole or node by node: a character takes its lowercase form when
// that is one character of the same UTF-16 length, else it is kept. The
// CI log search folds the same way on the backend (cilog.FoldForFind).

const NON_ASCII = /[^\p{ASCII}]/u;

/** `text` folded for matching; the same length as `text`. */
export function foldFindText(text: string): string {
  if (!NON_ASCII.test(text)) return text.toLowerCase();
  let out = '';
  for (const char of text) {
    const lower = char.toLowerCase();
    out += lower.length === char.length && [...lower].length === 1 ? lower : char;
  }
  return out;
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
