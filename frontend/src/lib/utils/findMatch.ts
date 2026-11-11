// The match rule every pane find uses: the trimmed query as a literal
// substring, case-insensitive. Lower-casing applies only where it keeps the
// text's length, so a match offset in the folded text is an offset in the
// original (findHighlights.ts paints ranges from it).

export function lowerPreservingLength(value: string): string {
  const lower = value.toLowerCase();
  return lower.length === value.length ? lower : value;
}

/** The folded needle for `query`; '' when the query is blank. */
export function findNeedle(query: string): string {
  return lowerPreservingLength(query.trim());
}

/** True when any of `texts` contains `needle` (from `findNeedle`). An empty
 * needle matches everything. */
export function textsMatchNeedle(needle: string, texts: readonly (string | null | undefined)[]): boolean {
  if (!needle) return true;
  for (const text of texts) {
    if (text && lowerPreservingLength(text).includes(needle)) return true;
  }
  return false;
}
