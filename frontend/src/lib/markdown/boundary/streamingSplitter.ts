// Stateful streaming-markdown boundary splitter.
//
// `splitAtBoundary` (split.ts) is a pure one-shot: it constructs a fresh
// BoundaryDetector and scans from line 0 on every call. Correct, but
// O(lines) per call — and ChatMarkdown re-runs it on every reveal tick
// while a row streams, so a single long message costs O(n²) of detector
// work over its lifetime (measured ≈ 1.3 ms/tick and ≈ 9.5 s cumulative
// for a 100 KB message; the dominant streaming cost at that size).
//
// This class keeps ONE detector across calls and resumes detection from
// the line after the last committed boundary, using the detector's
// per-line `contextCache` — the incremental mode incremark designed the
// detector for. Only newly-revealed lines are scanned, so total detector
// work is O(n) for a message that commits boundaries regularly. (A single
// unbroken block that never commits — e.g. a giant fenced code dump — is
// still re-scanned each tick, but per-line work inside a fence is minimal
// because the detector skips its checker chain there; see split.bench.)
//
// The line MATERIALIZATION is incremental too: re-running
// `text.split('\n')` over the full accumulated text allocated a fresh
// array of every line's substring per tick — O(total message) string
// work at reveal cadence, O(n²) cumulative, in the exact path the
// detector work above was made O(n) for. Instead the previous call's
// line array is cached and only the appended delta is split, merging
// the partial trailing line across the join. Append-only growth is
// verified with a `startsWith` scan over the previous source — a
// no-allocation memcmp that costs microseconds where the full split
// cost milliseconds plus thousands of line-substring allocations — so
// a non-append rewrite can never smuggle stale cached lines into
// detection. A rewrite that preserves the committed prefix keeps the
// valid detector checkpoint and fully re-splits the source. Any rewrite
// that changes committed bytes resets the checkpoint before re-splitting.
//
// PRECONDITION — append-only source. Correctness depends on committed
// lines never changing: the cached block context for a committed line
// must stay valid as the source grows. This holds for the only streaming
// caller (ChatMarkdown over an assistant_text row, whose summary the
// per-item smoother writes UNTRIMMED, i.e. append-only). A sliding-window
// source (e.g. the trimmed thinking tail) would violate it — those do
// NOT route through ChatMarkdown's streaming path (ThinkingBlock renders
// a plain <span>). A wholesale SHRINK (source shorter than the committed
// prefix) resets the splitter. Other replacements keep the checkpoint only
// when every committed byte is unchanged. This makes the class safe for all
// source transitions while append-only growth remains its fast path.

import { BoundaryDetector } from './BoundaryDetector';
import { createInitialContext } from './detector';
import type { BoundarySplit } from './split';

export class StreamingBoundarySplitter {
  private detector = new BoundaryDetector();
  // Line index of the last committed (stable) boundary; -1 before any
  // boundary commits. Lines 0..committedLine form the committed prefix.
  private committedLine = -1;
  // Char offset of the committed prefix's end (== prefix.length). Tracked
  // directly so the shrink check and the slice stay O(1).
  private committedOffset = 0;
  // Char offset where line `committedLine` STARTS. A line's start
  // depends only on the closed lines before it, so it is stable under
  // append-only growth even when `committedOffset` was capped at a
  // then-missing trailing newline — this is the safe base the O(delta)
  // boundary-offset walk resumes from.
  private committedLineStart = 0;
  // The previous call's source and its line array. `cachedLines` is
  // exactly `cachedText.split('\n')`, maintained by splitting only the
  // appended delta per call.
  private cachedText = '';
  private cachedLines: string[] = [];

  /**
   * Split `text` at the last stable block boundary, resuming detection
   * from the previously committed boundary. Returns the same shape as
   * `splitAtBoundary` and is behaviourally identical to calling that pure
   * function with the running high-water prefix length on each successive
   * (growing) source — see `streamingSplitter.test.ts` for the
   * exhaustive equivalence corpus that guards this.
   */
  split(text: string): BoundarySplit {
    if (text.length === 0) {
      this.reset();
      return { prefix: '', tail: '' };
    }
    // Wholesale shrink (source trimmed below the committed prefix): the
    // committed lines are gone, so the cached contexts no longer apply.
    // Reset and render the new source whole; the next stable boundary on
    // this row re-commits. Mirrors splitAtBoundary's caller-side guard.
    if (text.length < this.committedOffset) {
      this.reset();
      return { prefix: '', tail: text };
    }

    let lines: string[];
    const prev = this.cachedText;
    if (text.length >= prev.length && text.startsWith(prev)) {
      // Append-only growth (the streaming contract): split only the
      // delta and merge the partial trailing line across the join.
      // `prev.split('\n')` concat-merged this way is byte-equivalent to
      // `text.split('\n')` for any junction position.
      lines = this.cachedLines;
      if (lines.length === 0) {
        lines = text.split('\n');
        this.cachedLines = lines;
      } else if (text.length > prev.length) {
        const deltaLines = text.slice(prev.length).split('\n');
        lines[lines.length - 1] += deltaLines[0];
        for (let i = 1; i < deltaLines.length; i++) {
          lines.push(deltaLines[i]);
        }
      }
    } else {
      // A detector checkpoint describes the committed bytes, not merely
      // their old numeric offset. Preserve it only when that entire prefix
      // is still byte-identical. A rewrite inside the prefix can also have
      // fewer lines at the same total length, so reusing committedLine would
      // index past the new line array before detection even begins.
      const committedPrefixUnchanged =
        this.committedOffset > 0 &&
        text.startsWith(prev.slice(0, this.committedOffset));
      if (!committedPrefixUnchanged) this.reset();

      // Non-append rewrites are rare. Materialize the real current lines so
      // neither the line cache nor the detector can observe mixed sources.
      lines = text.split('\n');
      this.cachedLines = lines;
      this.committedLineStart = this.committedLine >= 0
        ? startOfLine(lines, this.committedLine)
        : 0;
    }
    this.cachedText = text;

    // Resume from the line after the last committed boundary. The detector
    // reads its own contextCache[committedLine] for the resume context
    // (written on the scan that committed that line; clearContextCache
    // keeps exactly that entry). createInitialContext() is only the
    // fallback for committedLine === -1 (startLine 0), where it is the
    // correct context anyway. `result.context` is intentionally NOT used —
    // it is the context AFTER the scan's current line, which is wrong when
    // the boundary lands on line i-1 (the checker path).
    const result = this.detector.findStableBoundary(
      lines,
      this.committedLine + 1,
      createInitialContext(),
    );
    if (result.line > this.committedLine) {
      // Walk only the newly committed region: start from the stable
      // start-of-line base rather than re-walking from line 0 like
      // `offsetAfterLine` (O(n) per commit adds up to O(n²) over a
      // message that commits often). The trailing +1 assumes a
      // terminating newline; capping at text.length reproduces
      // offsetAfterLine's last-line (no trailing newline) case.
      let start = this.committedLineStart;
      for (let i = Math.max(this.committedLine, 0); i < result.line; i++) {
        start += lines[i].length + 1;
      }
      this.committedLine = result.line;
      this.committedLineStart = start;
      this.committedOffset = Math.min(start + lines[result.line].length + 1, text.length);
      // Drop cached contexts for lines now permanently inside the prefix;
      // keep committedLine itself for the next resume's context read.
      this.detector.clearContextCache(this.committedLine);
    }

    return {
      prefix: text.slice(0, this.committedOffset),
      tail: text.slice(this.committedOffset),
    };
  }

  private reset(): void {
    // Fresh detector → fresh contextCache. Reusing the old one would
    // leave stale per-line contexts keyed by indices that no longer line
    // up with the new (shorter) source.
    this.detector = new BoundaryDetector();
    this.committedLine = -1;
    this.committedOffset = 0;
    this.committedLineStart = 0;
    this.cachedText = '';
    this.cachedLines = [];
  }
}

// Offset where line `lineIndex` starts. Only used when a rare non-append
// rewrite preserves the committed prefix, which guarantees that this line
// still exists at the same index.
function startOfLine(lines: string[], lineIndex: number): number {
  let offset = 0;
  for (let i = 0; i < lineIndex; i++) {
    offset += lines[i].length + 1;
  }
  return offset;
}
