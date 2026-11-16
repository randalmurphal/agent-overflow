import { appendFNV1a32, FNV_OFFSET_BASIS_32, formatContentKey } from './fnv1a';
import { parseHunkHeader, type PatchLine } from './patchFiles';
import {
  LINE_ADD,
  LINE_CONTEXT,
  LINE_DEL,
  LINE_HUNK,
  LINE_MARKER,
  LINE_META,
  PatchStore,
  type LineKind,
  type StoredLine,
} from './patchStore';

// Each file of a review patch is a `PatchBody`, a view over line ranges
// of one or more stores (`patchStore.ts`).

/** What the highlighter receives in place of a marker or fold row: a
 * `\`-prefixed line is a non-content marker that keeps line alignment
 * (see `diffSpanCache.svelte.ts`). */
const MARKER_PATCH_TEXT = '\\ marker';

let nextBodyId = 1;

/** One run of a file's lines: lines [start, end) of a store. */
export interface BodySegment {
  store: PatchStore;
  start: number;
  end: number;
}

/**
 * One file's patch lines, as ranges of stores, with the per-file facts
 * the row model needs without reading any text. Lines are addressed by
 * their index in the file, which is also the index the highlighter's
 * spans are aligned with.
 *
 * The facts are computed as `buildPatchDisplayRows` would with gaps on;
 * `patchRows.ts` applies a file's `suppressGaps` and `newSideTotal`.
 */
export class PatchBody {
  readonly id = nextBodyId++;
  readonly segments: readonly BodySegment[];
  private readonly bases: number[];
  readonly lineCount: number;
  /** Display rows that are not gaps: content, marker and fold lines. */
  readonly contentRows: number;
  /** Hunk-header lines that open a gap, in line order. */
  readonly gapLines: Int32Array;
  /** Whether the file can continue past its last hunk. */
  readonly trailingGap: boolean;
  /** The new-side line after the last row. */
  readonly endNew: number;
  /** Largest line number on either side. */
  readonly maxLine: number;
  /** Conflict pseudo-files carry marker rows and never show gaps. */
  readonly hasMarkers: boolean;
  /** UTF-8 bytes of `patchText()`, known without its text. A size,
   * never a string length. */
  readonly textSize: number;
  private key: string | null = null;

  constructor(segments: readonly BodySegment[]) {
    this.segments = segments.filter((segment) => segment.end > segment.start);
    this.bases = [];
    let lineCount = 0;
    for (const segment of this.segments) {
      this.bases.push(lineCount);
      lineCount += segment.end - segment.start;
    }
    this.lineCount = lineCount;

    let contentRows = 0;
    const gaps: number[] = [];
    let sawHunk = false;
    let firstOldStart = 0;
    let lastNewStart = 0;
    let oldLine = 0;
    let newLine = 0;
    let maxLine = 0;
    let hasMarkers = false;
    let markerBytes = 0;
    for (let index = 0; index < lineCount; index += 1) {
      const kind = this.kind(index);
      if (kind === LINE_HUNK) {
        const oldStart = this.hunkOldStart(index);
        const newStart = this.hunkNewStart(index);
        if (!sawHunk) {
          firstOldStart = oldStart;
          if (oldStart > 1 && newStart > 1) gaps.push(index);
        } else if (newStart > 0 && newStart > newLine) {
          gaps.push(index);
        }
        sawHunk = true;
        lastNewStart = newStart;
        oldLine = oldStart;
        newLine = newStart;
      } else if (kind === LINE_DEL) {
        if (oldLine > maxLine) maxLine = oldLine;
        oldLine += 1;
        contentRows += 1;
      } else if (kind === LINE_ADD) {
        if (newLine > maxLine) maxLine = newLine;
        newLine += 1;
        contentRows += 1;
      } else if (kind === LINE_CONTEXT) {
        if (oldLine > maxLine) maxLine = oldLine;
        if (newLine > maxLine) maxLine = newLine;
        oldLine += 1;
        newLine += 1;
        contentRows += 1;
      } else if (kind === LINE_MARKER) {
        hasMarkers = true;
        contentRows += 1;
        const { store, line } = this.locate(index);
        markerBytes += MARKER_PATCH_TEXT.length - (store.storedLineEnd(line) - store.storedLineStart(line));
      }
    }
    this.contentRows = contentRows;
    this.gapLines = Int32Array.from(gaps);
    this.trailingGap = sawHunk && firstOldStart > 0 && lastNewStart > 0;
    this.endNew = newLine;
    this.maxLine = maxLine;
    this.hasMarkers = hasMarkers;

    let textSize = Math.max(0, this.segments.length - 1);
    for (const segment of this.segments) {
      textSize += segment.store.storedLineEnd(segment.end - 1) - segment.store.storedLineStart(segment.start);
    }
    this.textSize = lineCount === 0 ? 0 : textSize + markerBytes;
  }

  /** A body over a parsed PatchFile's lines (conflict pseudo-files,
   * merged edit sections). */
  static fromPatchLines(lines: readonly PatchLine[]): PatchBody {
    const store = PatchStore.fromLines(lines.map(storedLine));
    return new PatchBody([{ store, start: 0, end: store.lineCount }]);
  }

  kind(index: number): LineKind {
    const { store, line } = this.locate(index);
    return store.kind(line);
  }

  /** A line's text. Throws PatchTextNotResident when it is evicted. */
  text(index: number): string {
    const { store, line } = this.locate(index);
    return store.text(line);
  }

  /** UTF-8 bytes of a line's text, known without its text. */
  lineSize(index: number): number {
    const { store, line } = this.locate(index);
    return store.storedLineEnd(line) - store.storedLineStart(line);
  }

  /** Whether the text of lines [start, end) is resident, by default of
   * every line. */
  resident(start = 0, end = this.lineCount): boolean {
    if (end <= start) return true;
    for (let at = this.segmentAt(start); at < this.segments.length && this.bases[at] < end; at += 1) {
      const segment = this.segments[at];
      const base = this.bases[at];
      const from = segment.start + Math.max(0, start - base);
      const to = segment.start + Math.min(end - base, segment.end - segment.start);
      if (!segment.store.resident(from, to)) return false;
    }
    return true;
  }

  /** The end of the segment holding line `index`: lines [index, end) are
   * consecutive lines of one store. */
  segmentEnd(index: number): number {
    if (!(index >= 0 && index < this.lineCount)) throw new RangeError(`patch body: line ${index} is not in the body`);
    const segment = this.segmentAt(index);
    return this.bases[segment] + this.segments[segment].end - this.segments[segment].start;
  }

  /** Where a line starts in its store's UTF-8 bytes. Within one segment,
   * the distance between two lines' positions is the bytes of the text
   * between them. */
  storedLineStart(index: number): number {
    const { store, line } = this.locate(index);
    return store.storedLineStart(line);
  }

  /** Where a line's text ends in its store's UTF-8 bytes. */
  storedLineEnd(index: number): number {
    const { store, line } = this.locate(index);
    return store.storedLineEnd(line);
  }

  /** Lines [start, end) of one segment (`segmentEnd`) joined with
   * newlines, decoded at once (PatchStore.textRange). Throws
   * PatchTextNotResident when any is evicted. */
  segmentText(start: number, end: number): string {
    if (end <= start) return '';
    if (end > this.segmentEnd(start)) throw new RangeError(`patch body: lines ${start}..${end} are not in one segment`);
    const { store, line } = this.locate(start);
    return store.textRange(line, line + end - start);
  }

  /** Runs `read` with lines [start, end) resident (whenResident). */
  whenResident<T>(read: () => T, start = 0, end = this.lineCount): Promise<T> {
    return whenResident(this.slice(start, end), read);
  }

  /** Keeps the text of lines [start, end) from eviction until the
   * returned function runs; running it again does nothing. */
  pin(start: number, end: number): () => void {
    return pinRanges(this.slice(start, end));
  }

  /** The end of the lines, within the segment holding line `end - 1`,
   * that a pin of [start, end) holds anyway (PatchStore.pinnedEnd). */
  pinnedEnd(start: number, end: number): number {
    if (end <= start) return end;
    const at = this.segmentAt(end - 1);
    const segment = this.segments[at];
    const base = this.bases[at];
    const from = segment.start + Math.max(0, start - base);
    const held = segment.store.pinnedEnd(from, segment.start + end - base);
    return base + Math.min(held, segment.end) - segment.start;
  }

  /** Reads the evicted text of lines [start, end) again: null when none
   * is evicted. Text can be evicted again once it lands unless it is
   * pinned (`pin`). Rejects with PatchTextLost when evicted text can no
   * longer be read. */
  load(start: number, end: number): Promise<void> | null {
    return loadRanges(this.slice(start, end));
  }

  hunkOldStart(index: number): number {
    const { store, line } = this.locate(index);
    return store.hunkOldStart(line);
  }

  hunkNewStart(index: number): number {
    const { store, line } = this.locate(index);
    return store.hunkNewStart(line);
  }

  fold(index: number): { id: number; lines: number } | undefined {
    const { store, line } = this.locate(index);
    return store.fold(line);
  }

  /**
   * The text the highlighter aligns spans with: every line joined by
   * newlines, marker and fold rows replaced by a non-content marker (see
   * `diffSpanCache.svelte.ts` patchTextOf, which this must match).
   * Throws PatchTextNotResident when any line is evicted.
   */
  patchText(): string {
    if (!this.hasMarkers) {
      return this.segments.map((segment) => segment.store.textRange(segment.start, segment.end)).join('\n');
    }
    const parts: string[] = [];
    for (let index = 0; index < this.lineCount; index += 1) {
      parts.push(this.kind(index) === LINE_MARKER ? MARKER_PATCH_TEXT : this.text(index));
    }
    return parts.join('\n');
  }

  /** The highlighter cache key for this file's patch text
   * (`contentKey` of `patchText()`), computed once. Null while any line
   * is evicted and the key was not computed yet. */
  contentKey(): string | null {
    if (this.key !== null) return this.key;
    if (!this.resident()) return null;
    let hash = FNV_OFFSET_BASIS_32;
    // The length of the text hashed: the stored size is not it.
    let length = 0;
    if (!this.hasMarkers) {
      for (let index = 0; index < this.segments.length; index += 1) {
        if (index > 0) {
          hash = appendFNV1a32(hash, '\n');
          length += 1;
        }
        const segment = this.segments[index];
        const text = segment.store.textRange(segment.start, segment.end);
        hash = appendFNV1a32(hash, text);
        length += text.length;
      }
    } else {
      const text = this.patchText();
      hash = appendFNV1a32(hash, text);
      length = text.length;
    }
    this.key = formatContentKey(hash, length);
    return this.key;
  }

  /** Store ranges holding the file's lines [start, end). */
  slice(start: number, end: number): BodySegment[] {
    const out: BodySegment[] = [];
    for (let index = 0; index < this.segments.length; index += 1) {
      const segment = this.segments[index];
      const base = this.bases[index];
      const from = Math.max(start, base);
      const to = Math.min(end, base + segment.end - segment.start);
      if (from < to) out.push({ store: segment.store, start: segment.start + from - base, end: segment.start + to - base });
    }
    return out;
  }

  /** The file's lines as PatchLine objects. Allocates per line: only for
   * joining edit sections into one file (reviewPaneLoad). Throws
   * PatchTextNotResident when any line is evicted. */
  toPatchLines(): PatchLine[] {
    const lines: PatchLine[] = [];
    for (let index = 0; index < this.lineCount; index += 1) {
      const kind = this.kind(index);
      const line: PatchLine = { content: this.text(index), type: lineTypeOf(kind) };
      const fold = kind === LINE_MARKER ? this.fold(index) : undefined;
      if (fold) line.fold = fold;
      lines.push(line);
    }
    return lines;
  }

  private locate(index: number): { store: PatchStore; line: number } {
    const at = this.segmentAt(index);
    const segment = this.segments[at];
    return { store: segment.store, line: segment.start + index - this.bases[at] };
  }

  // The segment holding line `index`.
  private segmentAt(index: number): number {
    let low = 0;
    let high = this.bases.length - 1;
    while (low < high) {
      const mid = (low + high + 1) >> 1;
      if (this.bases[mid] <= index) low = mid;
      else high = mid - 1;
    }
    return low;
  }
}

/**
 * Runs `read` once the text of every range is resident, and keeps it
 * resident while `read` runs; synchronously when it already is. Rejects
 * with PatchTextLost when evicted text can no longer be read.
 */
export async function whenResident<T>(ranges: readonly BodySegment[], read: () => T): Promise<T> {
  const release = pinRanges(ranges);
  try {
    const loading = loadRanges(ranges);
    if (loading !== null) await loading;
    return read();
  } finally {
    release();
  }
}

function pinRanges(ranges: readonly BodySegment[]): () => void {
  const releases = ranges.map((range) => range.store.pin(range.start, range.end));
  return () => {
    for (const release of releases.splice(0)) release();
  };
}

function loadRanges(ranges: readonly BodySegment[]): Promise<void> | null {
  const loads = ranges
    .filter((range) => !range.store.resident(range.start, range.end))
    .map((range) => range.store.load(range.start, range.end));
  return loads.length > 0 ? Promise.all(loads).then(() => undefined) : null;
}

export function lineTypeOf(kind: LineKind): PatchLine['type'] {
  switch (kind) {
    case LINE_ADD:
      return 'add';
    case LINE_DEL:
      return 'del';
    case LINE_CONTEXT:
      return 'context';
    case LINE_MARKER:
      return 'marker';
    default:
      return 'meta';
  }
}

function storedLine(line: PatchLine): StoredLine {
  switch (line.type) {
    case 'add':
      return { content: line.content, kind: LINE_ADD };
    case 'del':
      return { content: line.content, kind: LINE_DEL };
    case 'context':
      return { content: line.content, kind: LINE_CONTEXT };
    case 'marker':
      return line.fold ? { content: line.content, kind: LINE_MARKER, fold: line.fold } : { content: line.content, kind: LINE_MARKER };
    default:
      return { content: line.content, kind: parseHunkHeader(line.content) ?? LINE_META };
  }
}
