// Intraline (word-level) change ranges for paired del/add lines.
//
// Within a deletion run followed by an addition run, the i-th deleted
// line pairs with the i-th added line (the same pairing split view
// uses). For each pair we trim the common prefix/suffix, snap the
// changed middle out to word boundaries, and — when the change is a
// small enough slice of the line to be signal rather than noise —
// emit a highlight range per side. Offsets are relative to the
// prefix-STRIPPED source text, matching what the tokenizer sees.

import type { LineToken } from './tokenCache';

export interface IntralineRange {
  start: number;
  end: number;
}

/** One render segment of a diff line: token styling plus whether it
 * falls inside the intraline changed range. */
export interface IntralineSegment {
  text: string;
  color?: string;
  fontStyle?: number;
  emph: boolean;
}

/**
 * Split a line's Shiki tokens (or its plain text when untokenized)
 * at the changed-range boundaries so the renderer can wrap the
 * changed slice in a stronger background without disturbing token
 * colors. `text` is the prefix-stripped source the offsets refer to.
 */
export function segmentLine(
  tokens: readonly LineToken[] | null,
  text: string,
  range: IntralineRange,
): IntralineSegment[] {
  const source: readonly LineToken[] =
    tokens && tokens.length > 0 ? tokens : [{ content: text }];
  const out: IntralineSegment[] = [];
  let offset = 0;
  for (const token of source) {
    const tokenStart = offset;
    const tokenEnd = offset + token.content.length;
    const emphStart = Math.min(Math.max(range.start, tokenStart), tokenEnd);
    const emphEnd = Math.max(Math.min(range.end, tokenEnd), tokenStart);
    pushSegment(out, token, tokenStart, tokenStart, emphStart, false);
    pushSegment(out, token, tokenStart, emphStart, emphEnd, true);
    pushSegment(out, token, tokenStart, emphEnd, tokenEnd, false);
    offset = tokenEnd;
  }
  return out;
}

function pushSegment(
  out: IntralineSegment[],
  token: LineToken,
  tokenStart: number,
  from: number,
  to: number,
  emph: boolean,
): void {
  if (to <= from) return;
  out.push({
    text: token.content.slice(from - tokenStart, to - tokenStart),
    color: token.color,
    fontStyle: token.fontStyle,
    emph,
  });
}

/** Lines longer than this skip intraline work entirely — same class of
 * guard as TOKENIZE_MAX_LINE_LENGTH (minified content). */
export const INTRALINE_MAX_LINE_LENGTH = 1000;

/** A changed middle wider than this share of the longer line means the
 * lines are mostly different — a highlight spanning nearly everything
 * is noise, so we emit none. */
const INTRALINE_MAX_CHANGED_RATIO = 0.7;

const WORD_CHAR = /[A-Za-z0-9_]/;

export interface IntralinePair {
  del: IntralineRange;
  add: IntralineRange;
}

/**
 * Changed-range pair for one del/add line pair, or null when the pair
 * shouldn't highlight (identical, too long, or mostly different).
 */
export function intralineRanges(delText: string, addText: string): IntralinePair | null {
  if (delText === addText) return null;
  const maxLen = Math.max(delText.length, addText.length);
  if (maxLen === 0 || maxLen > INTRALINE_MAX_LINE_LENGTH) return null;

  let prefix = 0;
  const prefixMax = Math.min(delText.length, addText.length);
  while (prefix < prefixMax && delText[prefix] === addText[prefix]) prefix += 1;

  let suffix = 0;
  const suffixMax = prefixMax - prefix;
  while (
    suffix < suffixMax &&
    delText[delText.length - 1 - suffix] === addText[addText.length - 1 - suffix]
  ) {
    suffix += 1;
  }

  let del: IntralineRange = { start: prefix, end: delText.length - suffix };
  let add: IntralineRange = { start: prefix, end: addText.length - suffix };
  del = snapToWordBounds(delText, del);
  add = snapToWordBounds(addText, add);

  const changed = Math.max(del.end - del.start, add.end - add.start);
  if (changed / maxLen > INTRALINE_MAX_CHANGED_RATIO) return null;

  return { del, add };
}

/** Widen a range so it never starts or ends mid-word — highlighting
 * the `az` of `bar`→`baz` reads worse than highlighting `baz`. */
function snapToWordBounds(text: string, range: IntralineRange): IntralineRange {
  let { start, end } = range;
  // An empty range (pure insertion relative to the other side) has
  // nothing to snap.
  if (start >= end) return range;
  while (start > 0 && WORD_CHAR.test(text[start - 1]) && WORD_CHAR.test(text[start])) start -= 1;
  while (end < text.length && WORD_CHAR.test(text[end]) && WORD_CHAR.test(text[end - 1])) end += 1;
  return { start, end };
}
