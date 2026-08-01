// Split a markdown string into a committed-prefix region and a
// volatile-tail region using incremark's BoundaryDetector.
//
// The committed prefix is everything up to and including the last stable
// block boundary; the volatile tail is whatever comes after. The
// detector naturally holds back the unfinished trailing block as
// lookahead (a setext underline arriving late, a GFM table alignment
// row, a paragraph that hasn't reached its terminating blank line yet),
// so callers get one block of lookahead for free.
//
// Monotonic guard: if `previousPrefixLength` is provided and the
// detector returns a prefix shorter than that, the caller's previous
// committed prefix is preserved — boundaries can never shrink. This
// defends against any detector quirk and keeps the tail-only update
// path stable.

import { BoundaryDetector } from './BoundaryDetector';
import { createInitialContext } from './detector';

export interface BoundarySplit {
  /** Committed prefix — everything before the last stable boundary. */
  prefix: string;
  /** Volatile tail — the unfinished trailing block plus lookahead. */
  tail: string;
}

/**
 * Splits `text` at the last stable markdown block boundary.
 *
 * @param text - The full revealed markdown so far.
 * @param previousPrefixLength - Optional. Length of the most recently
 *   returned committed prefix for this stream. If the detector tries
 *   to return a shorter prefix, the caller's previous length wins
 *   (monotonic commit). Pass 0 (or omit) on the first call.
 */
export function splitAtBoundary(
  text: string,
  previousPrefixLength = 0,
): BoundarySplit {
  if (text.length === 0) {
    return { prefix: '', tail: '' };
  }

  // Fresh detector per call. `findStableBoundary` is always invoked
  // with `startLine = 0`, so the detector's internal `contextCache` is
  // never read across calls (the read guard is `startLine > 0`) and
  // sharing a singleton would just accumulate per-line context entries
  // forever. Construction cost is a small constant.
  const detector = new BoundaryDetector();
  const lines = text.split('\n');
  const result = detector.findStableBoundary(lines, 0, createInitialContext());

  let prefixEnd = 0;
  if (result.line >= 0) {
    prefixEnd = offsetAfterLine(text, lines, result.line);
  }

  // Monotonic guard — never let the prefix shrink. Equal-length is
  // identity-preserving, so we allow it.
  if (prefixEnd < previousPrefixLength) {
    prefixEnd = previousPrefixLength;
  }

  return {
    prefix: text.slice(0, prefixEnd),
    tail: text.slice(prefixEnd),
  };
}

// Returns the character offset immediately after line `lineIndex`'s
// terminating newline. For the last line (no trailing newline), returns
// text.length. Walks via `lines[].length` so we don't re-scan the
// string for newlines. (`StreamingBoundarySplitter` computes the same
// offset incrementally from its committed-line base instead of walking
// from line 0 on every commit.)
function offsetAfterLine(
  text: string,
  lines: string[],
  lineIndex: number,
): number {
  let offset = 0;
  for (let i = 0; i <= lineIndex; i++) {
    offset += lines[i].length;
    if (i < lines.length - 1) {
      offset += 1; // the '\n' that split() consumed
    }
  }
  return Math.min(offset, text.length);
}
