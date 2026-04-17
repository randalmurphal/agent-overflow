// Pure helper that splits a text string into alternating "match" and
// "text" segments for case-insensitive substring highlighting. Used by
// MessageSearch to mark every occurrence of the user's query in thread
// titles and item summaries without HTML injection risk.
//
// Empty / whitespace-only queries short-circuit to a single text segment
// holding the original string — callers can render that uniformly without
// a special-case.

export type HighlightSegment =
  | { type: 'text'; value: string }
  | { type: 'match'; value: string };

export function computeHighlightSegments(text: string, query: string): HighlightSegment[] {
  if (text.length === 0) return [];
  const q = query.trim();
  if (q.length === 0) return [{ type: 'text', value: text }];

  const lowerText = text.toLowerCase();
  const lowerQuery = q.toLowerCase();
  const segments: HighlightSegment[] = [];
  let cursor = 0;

  while (cursor < text.length) {
    const at = lowerText.indexOf(lowerQuery, cursor);
    if (at === -1) {
      segments.push({ type: 'text', value: text.slice(cursor) });
      break;
    }
    if (at > cursor) {
      segments.push({ type: 'text', value: text.slice(cursor, at) });
    }
    segments.push({ type: 'match', value: text.slice(at, at + q.length) });
    cursor = at + q.length;
  }
  return segments;
}
