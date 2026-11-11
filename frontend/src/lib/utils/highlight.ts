// Splits text into alternating "match" and "text" segments, for marking a
// query's occurrences without HTML injection. Each function is one match
// rule, named for the results it paints, so a highlight covers exactly
// what produced the row: every non-overlapping occurrence, left to right,
// at its offsets in the original text. A blank query marks nothing.

import { foldFindText } from './findMatch';

export type HighlightSegment =
  | { type: 'text'; value: string }
  | { type: 'match'; value: string };

// Go's strings.TrimSpace (unicode.IsSpace). JS trim differs: it keeps
// U+0085 and removes U+FEFF.
const GO_SPACE = /^[\t\n\v\f\r \x85\xa0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \x85\xa0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g;
const ASCII_CAPITAL = /[A-Z]/g;

/**
 * The cross-thread search's rule (store SearchThreadMessages): the query
 * trimmed by Go's strings.TrimSpace and lower cased by Go's
 * strings.ToLower, which is the find fold table's per-code-point simple
 * lower case (stringsx TestFindFoldTableIsUnicodeToLower), matched as a
 * literal substring of the text with ASCII letters folded only (SQLite
 * LOWER and LIKE). Folding keeps lengths, so offsets are the text's.
 * src/test/fixtures/searchVectors.json holds this to the SQL.
 */
export function searchHighlightSegments(text: string, query: string): HighlightSegment[] {
  const needle = foldFindText(query.replace(GO_SPACE, ''));
  const folded = text.replace(ASCII_CAPITAL, (c) => String.fromCharCode(c.charCodeAt(0) + 32));
  const ranges: Array<[number, number]> = [];
  if (needle !== '') {
    for (let at = folded.indexOf(needle); at >= 0; at = folded.indexOf(needle, at + needle.length)) {
      ranges.push([at, at + needle.length]);
    }
  }
  return segmentsOf(text, ranges);
}

/**
 * The thread picker's rule: the query trimmed and lower cased by JS,
 * a substring of the text lower cased by JS. A lower case that changes a
 * character's length (`İ`) is mapped back, so a match covers the whole
 * original characters it touches.
 */
export function lowerCaseHighlightSegments(text: string, query: string): HighlightSegment[] {
  const needle = query.trim().toLowerCase();
  const lower = text.toLowerCase();
  // starts[i] is where the original character holding lower[i] begins;
  // starts[lower.length] is text.length.
  const starts: number[] = [];
  for (let at = 0; at < text.length; ) {
    const code = text.codePointAt(at)!;
    const size = code > 0xffff ? 2 : 1;
    const length = String.fromCodePoint(code).toLowerCase().length;
    for (let i = 0; i < length; i++) starts.push(at);
    at += size;
  }
  // A context-dependent lower case that changed the length would leave no
  // map; mark nothing rather than the wrong text.
  if (starts.length !== lower.length) return segmentsOf(text, []);
  starts.push(text.length);
  const ranges: Array<[number, number]> = [];
  if (needle !== '') {
    for (let at = lower.indexOf(needle); at >= 0; at = lower.indexOf(needle, at + needle.length)) {
      const end = at + needle.length;
      // The end rounds up to the end of the character holding lower[end - 1].
      let to = end;
      while (to < lower.length && starts[to] === starts[end - 1]) to++;
      ranges.push([starts[at], starts[to]]);
    }
  }
  return segmentsOf(text, ranges);
}

function segmentsOf(text: string, ranges: ReadonlyArray<readonly [number, number]>): HighlightSegment[] {
  if (text.length === 0) return [];
  const segments: HighlightSegment[] = [];
  let cursor = 0;
  for (const [from, to] of ranges) {
    if (from < cursor || to <= from) continue;
    if (from > cursor) segments.push({ type: 'text', value: text.slice(cursor, from) });
    segments.push({ type: 'match', value: text.slice(from, to) });
    cursor = to;
  }
  if (cursor < text.length) segments.push({ type: 'text', value: text.slice(cursor) });
  return segments;
}
