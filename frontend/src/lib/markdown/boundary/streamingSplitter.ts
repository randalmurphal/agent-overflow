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
// PRECONDITION — append-only source. Correctness depends on committed
// lines never changing: the cached block context for a committed line
// must stay valid as the source grows. This holds for the only streaming
// caller (ChatMarkdown over an assistant_text row, whose summary the
// per-item smoother writes UNTRIMMED, i.e. append-only). A sliding-window
// source (e.g. the trimmed thinking tail) would violate it — those do
// NOT route through ChatMarkdown's streaming path (ThinkingBlock renders
// a plain <span>). The only non-append case handled is a wholesale SHRINK
// (source shorter than the committed prefix), which resets the splitter.
// A same-length, different-content replacement is out of contract; we
// deliberately do NOT pay an O(prefix) `startsWith` guard to detect it —
// that would reintroduce the O(n²) cost this class removes.

import { BoundaryDetector } from './BoundaryDetector';
import { createInitialContext } from './detector';
import { offsetAfterLine, type BoundarySplit } from './split';

export class StreamingBoundarySplitter {
  private detector = new BoundaryDetector();
  // Line index of the last committed (stable) boundary; -1 before any
  // boundary commits. Lines 0..committedLine form the committed prefix.
  private committedLine = -1;
  // Char offset of the committed prefix's end (== prefix.length). Tracked
  // directly so the shrink check and the slice stay O(1).
  private committedOffset = 0;

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

    const lines = text.split('\n');
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
      this.committedLine = result.line;
      this.committedOffset = offsetAfterLine(text, lines, result.line);
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
  }
}
