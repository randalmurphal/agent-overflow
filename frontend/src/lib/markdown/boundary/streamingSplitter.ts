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
// work is O(n), including for one open block that has not committed yet.
// Append-only growth resumes at the prior trailing line. That is the only
// old line whose bytes can change, and scanning the first new line still
// catches every boundary that can make its predecessor stable.
//
// The line MATERIALIZATION is incremental too: re-running
// `text.split('\n')` over the full accumulated text allocated a fresh
// array of every line's substring per tick — O(total message) string
// work at reveal cadence, O(n²) cumulative, in the exact path the
// detector work above was made O(n) for. Instead the previous call's
// line array is cached and only the appended delta is split, merging
// the partial trailing line across the join. Append-only growth is
// supplied by the assistant reveal chokepoint. Callers without that suffix
// retain the `startsWith` fallback, so a non-append rewrite can never smuggle
// stale cached lines into detection. A rewrite that preserves the committed prefix keeps the
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
import {
  createProvenAppend,
  matchesProvenAppend,
  type ProvenAppend,
} from '../index';

export class StreamingBoundarySplitter {
  /** Proven append applied to the volatile tail by the latest split. */
  tailAppend: ProvenAppend | undefined = undefined;
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
  // Exact split strings from the previous call. On append, only the bounded
  // volatile tail is sliced when a boundary advances. Slicing `text` itself
  // would flatten the full growing V8 cons string on every reveal.
  private cachedPrefix = '';
  private cachedTail = '';

  /**
   * Split `text` at the last stable block boundary, resuming detection
   * from the previously committed boundary. Returns the same shape as
   * `splitAtBoundary` and is behaviourally identical to calling that pure
   * function with the running high-water prefix length on each successive
   * (growing) source — see `streamingSplitter.test.ts` for the
   * exhaustive equivalence corpus that guards this.
   */
  split(text: string, append?: ProvenAppend): BoundarySplit {
    this.tailAppend = undefined;
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
    let appendOnly = false;
    let appendedText = '';
    let tailAppend: ProvenAppend | undefined;
    let scanStartLine = this.committedLine + 1;
    const prev = this.cachedText;
    const exactAppend = matchesProvenAppend(append, prev, text);
    if (exactAppend || (text.length >= prev.length && text.startsWith(prev))) {
      appendOnly = true;
      // Every line before the old trailing line is closed and immutable. The
      // trailing line can grow, close a fence/container, or become stable when
      // the append adds its successor. Its preceding context remains cached.
      scanStartLine = Math.max(
        this.committedLine + 1,
        Math.max(0, this.cachedLines.length - 1),
      );
      // Append-only growth (the streaming contract): split only the
      // delta and merge the partial trailing line across the join.
      // `prev.split('\n')` concat-merged this way is byte-equivalent to
      // `text.split('\n')` for any junction position.
      lines = this.cachedLines;
      if (lines.length === 0) {
        lines = text.split('\n');
        this.cachedLines = lines;
        this.cachedPrefix = '';
        this.cachedTail = text;
      } else if (text.length > prev.length) {
        appendedText = exactAppend
          ? append.delta
          : text.slice(prev.length);
        const deltaLines = appendedText.split('\n');
        lines[lines.length - 1] += deltaLines[0];
        for (let i = 1; i < deltaLines.length; i++) {
          lines.push(deltaLines[i]);
        }
        tailAppend = createProvenAppend(this.cachedTail, appendedText);
        this.cachedTail = tailAppend.next;
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
      scanStartLine = this.committedLine + 1;
    }
    this.cachedText = text;
    const previousCommittedOffset = this.committedOffset;

    // On a rewrite, resume after the last committed boundary. On an append,
    // resume at the old trailing line as described above. The detector reads
    // contextCache[startLine - 1], which both paths preserve. The initial
    // context is only the fallback for startLine 0. `result.context` is not
    // used because a checker can commit line i - 1 after processing line i.
    const result = this.detector.findStableBoundary(
      lines,
      scanStartLine,
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
    // An append can resume at the current trailing line and needs only the
    // context immediately before it. A rewrite resumes at the committed
    // boundary. Retaining those two checkpoints prevents an open 100K-line
    // fence from leaving 100K context objects in the detector cache.
    this.detector.retainContextCheckpoints(
      this.committedLine,
      lines.length - 2,
    );

    if (appendOnly) {
      const committedAdvance = this.committedOffset - previousCommittedOffset;
      if (committedAdvance > 0) {
        if (committedAdvance > this.cachedTail.length) {
          throw new Error('streaming boundary cache advanced past its tail');
        }
        this.cachedPrefix += this.cachedTail.slice(0, committedAdvance);
        this.cachedTail = this.cachedTail.slice(committedAdvance);
      }
      if (committedAdvance === 0 && appendedText.length > 0) {
        this.tailAppend = tailAppend;
      }
    } else {
      this.cachedPrefix = text.slice(0, this.committedOffset);
      this.cachedTail = text.slice(this.committedOffset);
    }

    return { prefix: this.cachedPrefix, tail: this.cachedTail };
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
    this.cachedPrefix = '';
    this.cachedTail = '';
    this.tailAppend = undefined;
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
