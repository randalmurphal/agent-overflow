import { unpackText } from './packedText';
import { PatchChunks, type PatchTextLost, type PatchTextSource } from './patchChunks';

// Compact storage for the review pane's patches. A patch of any size
// arrives in chunks (`reviewDiffStream.ts`) and is held as those chunk
// strings (`patchChunks.ts`) plus typed-array indexes: one Uint32 line
// start per line, and one kind byte per line. Nothing allocates per line.
// Rows, line objects and line strings are materialized only for the rows
// on screen (`patchRows.ts`).
//
// A chunk is plain (its text) or packed (its UTF-8 bytes, one character
// each: `packedText.ts`). A line of packed text holding a byte past ASCII
// is a wide line, one bit per line; reading one decodes it, and every
// other line reads as it is stored. Positions and lengths the store keeps
// count stored characters, which for packed text are bytes.
//
// Chunk text can be evicted under the memory budget; line indexes, kinds
// and hunks stay, so everything but a line's text is always answerable.
// A reader of evicted text either shows a placeholder and asks for the
// text (`load`), or reads once it is back (`whenResident` in
// `patchBody.ts`).

export const LINE_META = 0;
export const LINE_HUNK = 1;
export const LINE_CONTEXT = 2;
export const LINE_ADD = 3;
export const LINE_DEL = 4;
/** Conflict-view marker and fold rows (`conflictFile.ts`). */
export const LINE_MARKER = 5;

export type LineKind =
  | typeof LINE_META
  | typeof LINE_HUNK
  | typeof LINE_CONTEXT
  | typeof LINE_ADD
  | typeof LINE_DEL
  | typeof LINE_MARKER;

/** A hunk header's starts. */
export interface HunkStarts {
  oldStart: number;
  newStart: number;
}

/** What a line is: a kind, or a hunk header with its starts. */
export type LineClass = Exclude<LineKind, typeof LINE_HUNK> | HunkStarts;

// Characters of a line spanning chunks that its classification reads:
// enough for any header line's path.
const SPANNING_PREFIX_CHARS = 16 * 1024;

// Line starts of the chunk being indexed, copied out once its size is
// known. Shared by every store, since indexing is synchronous; dropped back
// to its first size when a store stops taking chunks (`stopIndexing`).
const SCRATCH_LINES = 4096;
let scratchStarts = new Uint32Array(SCRATCH_LINES);

// A store stops taking chunks when its patch ends or it is disposed, the
// way a read that failed or was cancelled stops.
function stopIndexing(): void {
  if (scratchStarts.length > SCRATCH_LINES) scratchStarts = new Uint32Array(SCRATCH_LINES);
}

export interface StoredLine {
  content: string;
  kind: LineClass;
  fold?: { id: number; lines: number };
}

/** The patch text from offset 0 to a chunk's end, as the backend keys it:
 * FNV-1a (32-bit) over the text's UTF-16 code units, and how many units
 * that is. */
export interface PatchTextKey {
  hash: number;
  units: number;
}

/** A chunk's backend byte range, which an evicted chunk is read again
 * by, its form and its key. A plain chunk may have a key (the key of the
 * text through its end); a packed chunk always has one, and `wide`, the
 * index of the first byte past ASCII of each line that has one, which is
 * never empty. */
export type PatchChunkSpan =
  | { offset: number; nextOffset: number; wide?: undefined; key?: PatchTextKey }
  | { offset: number; nextOffset: number; wide: readonly number[]; key: PatchTextKey };

/** Runs for each line a chunk completes: the text its first character is
 * in, the line's offset there and stored length, and whether that range
 * is a wide line (unpack what is kept of it). Answers what the line is. */
export type PatchLineCallback = (line: number, text: string, offset: number, length: number, wide: boolean) => LineClass;

const NO_MARKS: readonly number[] = [];

/**
 * A copy of text read from a store that shares no memory with its chunk.
 * V8 and JavaScriptCore slice strings by reference, so a line kept after
 * its chunk is evicted would keep the whole chunk; slicing a
 * concatenation flattens it into a new string first.
 */
export function ownText(text: string): string {
  return (' ' + text).slice(1);
}

/** Copies (ownText) the text a parsed file or an open editor keeps from a
 * store's chunks. An object so a test can see each kept string pass
 * through it. */
export const keptText = { own: ownText };

/**
 * The lines of one patch, stored as the chunks they arrived in.
 *
 * A line normally lies inside one chunk. A line longer than a read spans
 * several: it starts in one chunk, every chunk in between holds only its
 * middle, and a later chunk holds its end. Lines are numbered from 0 in
 * arrival order.
 */
export class PatchStore {
  private readonly chunks = new PatchChunks();
  // Per chunk: its line starts, first line and line count; where it
  // starts, in stored characters; and whether its text holds a newline
  // and ends with one, so a line's chunks are known without its text.
  private readonly starts: Uint32Array[] = [];
  private readonly firstLines: number[] = [];
  private readonly counts: number[] = [];
  private readonly storedStarts: number[] = [];
  private readonly newlines: boolean[] = [];
  private readonly newlineEnds: boolean[] = [];
  private kinds = new Uint8Array(256);
  private hunkLines = new Int32Array(16);
  private hunkOld = new Int32Array(16);
  private hunkNew = new Int32Array(16);
  private hunkCount = 0;
  private folds: Map<number, { id: number; lines: number }> | null = null;
  // One bit per line: whether it is a wide line. Null until the first.
  private wideBits: Uint8Array | null = null;
  private lines = 0;
  private stored = 0;
  private openLine = -1;
  private openChunk = 0;
  private openOffset = 0;
  private openLength = 0;

  /** Lines started so far. */
  get lineCount(): number {
    return this.lines;
  }

  /** Stored characters of everything appended: a size, not the text's
   * UTF-16 length (packed text stores bytes). */
  get storedSize(): number {
    return this.stored;
  }

  /**
   * Appends one chunk of patch text. `onLine` runs once per line that the
   * chunk completes, in line order; it may not read the store's line
   * text, which is indexed after the chunk is. `span` is the chunk's
   * backend byte range and form (PatchChunkSpan); PatchParser, the one
   * appender, has validated it.
   */
  append(data: string, onLine: PatchLineCallback, span?: PatchChunkSpan): void {
    const wide = span?.wide;
    const chunk = this.chunks.push(data, wide !== undefined, span, true);
    this.pushChunk(data);
    this.index(chunk, data, onLine, wide ?? NO_MARKS);
    this.chunks.indexed(this.openLine >= 0 ? this.openChunk : -1);
  }

  // The store's half of a new chunk; its line index is set by `index`.
  private pushChunk(data: string): void {
    this.storedStarts.push(this.stored);
    this.stored += data.length;
    this.newlines.push(data.includes('\n'));
    this.newlineEnds.push(data.endsWith('\n'));
  }

  // A cursor over the chunk's marks (`wide`) marks its wide lines.
  private index(chunk: number, data: string, onLine: PatchLineCallback, wide: readonly number[]): void {
    let pos = 0;
    let mark = 0;
    if (this.openLine >= 0) {
      const end = data.indexOf('\n');
      if (mark < wide.length && wide[mark] < (end < 0 ? data.length : end)) {
        this.markWide(this.openLine);
        mark += 1;
      }
      if (end < 0) {
        // The middle of a line longer than the chunk.
        this.openLength += data.length;
        this.firstLines.push(this.lines);
        this.counts.push(0);
        this.starts.push(new Uint32Array(0));
        return;
      }
      this.completeOpenLine(this.openLength + end, onLine);
      pos = end + 1;
    }
    const firstLine = this.lines;
    let count = 0;
    while (pos < data.length) {
      const line = this.lines;
      this.lines += 1;
      if (count === scratchStarts.length) {
        const grown = new Uint32Array(scratchStarts.length * 2);
        grown.set(scratchStarts);
        scratchStarts = grown;
      }
      scratchStarts[count] = pos;
      count += 1;
      this.ensureKindCapacity(this.lines);
      const end = data.indexOf('\n', pos);
      let wideLine = false;
      if (mark < wide.length && wide[mark] < (end < 0 ? data.length : end)) {
        this.markWide(line);
        mark += 1;
        wideLine = true;
      }
      if (end < 0) {
        this.openLine = line;
        this.openChunk = chunk;
        this.openOffset = pos;
        this.openLength = data.length - pos;
        break;
      }
      this.record(line, onLine(line, data, pos, end - pos, wideLine));
      pos = end + 1;
    }
    this.firstLines.push(firstLine);
    this.counts.push(count);
    this.starts.push(scratchStarts.slice(0, count));
  }

  /** Ends the patch: completes the last line when the patch does not end
   * with a newline. */
  end(onLine: PatchLineCallback): void {
    if (this.openLine >= 0) this.completeOpenLine(this.openLength, onLine);
    stopIndexing();
    this.chunks.ended();
  }

  // A line that spans chunks is classified from its first characters,
  // joined from its pieces: a header line is short and arrives whole this
  // way, and a longer line only needs its prefix. The open line's chunks
  // are pinned while it is open.
  private completeOpenLine(length: number, onLine: PatchLineCallback): void {
    const line = this.openLine;
    this.openLine = -1;
    const prefix = this.joined(line, this.openChunk, this.openOffset, Math.min(length, SPANNING_PREFIX_CHARS), false);
    this.record(line, onLine(line, prefix, 0, length, false));
  }

  private record(line: number, kind: LineClass): void {
    if (typeof kind === 'number') {
      this.kinds[line] = kind;
      return;
    }
    this.addHunk(line, kind.oldStart, kind.newStart);
    this.kinds[line] = LINE_HUNK;
  }

  private markWide(line: number): void {
    this.wideBits ??= new Uint8Array(this.kinds.length >> 3);
    this.wideBits[line >> 3] |= 1 << (line & 7);
  }

  private isWide(line: number): boolean {
    const bits = this.wideBits;
    return bits !== null && (bits[line >> 3] & (1 << (line & 7))) !== 0;
  }

  // The first wide line in [line, end), or end.
  private nextWide(line: number, end: number): number {
    const bits = this.wideBits;
    if (bits === null) return end;
    while (line < end) {
      const rest = bits[line >> 3] >> (line & 7);
      if (rest === 0) line = (line | 7) + 1;
      else if ((rest & 1) !== 0) return line;
      else line += 1;
    }
    return end;
  }

  // A line's text from `start` in `chunk`, its pieces joined, at most
  // `limit` stored characters of it. A wide line's packed pieces are
  // decoded one by one: a chunk never ends inside a character. `read`
  // counts the read as a use of the chunks.
  private joined(line: number, chunk: number, start: number, limit: number, read: boolean): string {
    const wide = this.isWide(line);
    let out = '';
    let stored = 0;
    for (let at = chunk; at < this.chunks.count && stored < limit; at += 1) {
      const text = read ? this.chunks.read(at, line) : this.chunks.peek(at, line);
      const from = at === chunk ? start : 0;
      const newline = text.indexOf('\n', from);
      const to = Math.min(newline < 0 ? text.length : newline, from + limit - stored);
      stored += to - from;
      const piece = text.slice(from, to);
      out += wide && this.chunks.isPacked(at) ? unpackText(piece) : piece;
      if (newline >= 0) break;
    }
    return out;
  }

  /** A store of explicitly typed lines, for patches built in memory. Its
   * text is not read from a diff and is not counted. */
  static fromLines(lines: readonly StoredLine[]): PatchStore {
    const store = new PatchStore();
    if (lines.length === 0) return store;
    const text = lines.map((line) => line.content).join('\n') + '\n';
    const starts = new Uint32Array(lines.length);
    let pos = 0;
    store.ensureKindCapacity(lines.length);
    for (let index = 0; index < lines.length; index += 1) {
      const line = lines[index];
      starts[index] = pos;
      pos += line.content.length + 1;
      store.record(index, line.kind);
      if (line.fold) {
        store.folds ??= new Map();
        store.folds.set(index, line.fold);
      }
    }
    store.chunks.push(text, false, undefined, false);
    store.pushChunk(text);
    store.firstLines.push(0);
    store.counts.push(lines.length);
    store.starts.push(starts);
    store.lines = lines.length;
    return store;
  }

  kind(line: number): LineKind {
    return this.kinds[line] as LineKind;
  }

  // Hunk headers are recorded in line order.
  private addHunk(line: number, oldStart: number, newStart: number): void {
    if (this.hunkCount === this.hunkLines.length) {
      this.hunkLines = growInt32(this.hunkLines);
      this.hunkOld = growInt32(this.hunkOld);
      this.hunkNew = growInt32(this.hunkNew);
    }
    this.hunkLines[this.hunkCount] = line;
    this.hunkOld[this.hunkCount] = oldStart;
    this.hunkNew[this.hunkCount] = newStart;
    this.hunkCount += 1;
  }

  /** Old-side start of the hunk whose header is `line`. */
  hunkOldStart(line: number): number {
    return this.hunkOld[this.hunkIndex(line)];
  }

  /** New-side start of the hunk whose header is `line`. */
  hunkNewStart(line: number): number {
    return this.hunkNew[this.hunkIndex(line)];
  }

  fold(line: number): { id: number; lines: number } | undefined {
    return this.folds?.get(line);
  }

  /** A line's text, without its newline. Throws PatchTextNotResident
   * when the store does not hold it. */
  text(line: number): string {
    const chunk = this.chunkOf(line);
    const local = line - this.firstLines[chunk];
    const start = this.starts[chunk][local];
    if (this.isWide(line)) return this.joined(line, chunk, start, Infinity, true);
    const text = this.chunks.read(chunk, line);
    if (local + 1 < this.counts[chunk]) return text.slice(start, this.starts[chunk][local + 1] - 1);
    const newline = text.indexOf('\n', start);
    if (newline >= 0) return text.slice(start, newline);
    // A line longer than its chunk: the rest is in the chunks after it.
    return this.joined(line, chunk, start, Infinity, true);
  }

  /** Whether the store holds the text of lines [start, end). */
  resident(start: number, end: number): boolean {
    if (end <= start) return true;
    const first = this.chunkOf(start);
    return this.chunks.resident(first, this.lastChunk(first, start, end));
  }

  /**
   * Reads evicted text of lines [start, end) again. The text can be
   * evicted again once it lands; a reader that must read all of it at
   * once pins it first (`whenResident`). Rejects with PatchTextLost when
   * the text can no longer be read.
   */
  load(start: number, end: number): Promise<void> {
    if (end <= start) return Promise.resolve();
    const first = this.chunkOf(start);
    return this.chunks.load(first, this.lastChunk(first, start, end));
  }

  /** Keeps the text of lines [start, end) from eviction until the
   * returned function runs. */
  pin(start: number, end: number): () => void {
    if (end <= start) return () => {};
    const first = this.chunkOf(start);
    return this.chunks.pin(first, this.lastChunk(first, start, end));
  }

  /** The end of the lines whose text lies in the chunks that hold lines
   * [start, end), at least `end`: what a pin of [start, end) holds
   * anyway, since a pin holds whole chunks. */
  pinnedEnd(start: number, end: number): number {
    if (end <= start) return end;
    const last = this.lastChunk(this.chunkOf(start), start, end);
    if (last === this.firstLines.length - 1) return Math.max(end, this.lines);
    // The last line starting in the chunk continues past it unless the
    // chunk ends with a newline.
    const after = this.firstLines[last] + this.counts[last];
    return Math.max(end, this.newlineEnds[last] ? after : after - 1);
  }

  /** Makes the text held so far and appended from now on evictable: it
   * can be read again from `source`. The store releases the source when
   * it is disposed, or when the patch ends with nothing evicted. */
  attachSource(source: PatchTextSource): void {
    this.chunks.attachSource(source);
  }

  /**
   * Runs `listener` once evicted text can no longer be read, at once when
   * that already happened. Whoever shows the store reads the diff again.
   */
  whenLost(listener: (err: PatchTextLost) => void): void {
    this.chunks.whenLost(listener);
  }

  /** Stops counting the store's text and releases its source. Text it
   * still holds stays readable; evicted text is lost. */
  dispose(): void {
    stopIndexing();
    this.chunks.dispose();
  }

  // The last chunk holding text of lines [start, end), end > start, the
  // first being `first`.
  private lastChunk(first: number, start: number, end: number): number {
    return this.endChunkOf(end - 1, end - 1 === start ? first : this.chunkOf(end - 1));
  }

  // The chunk a line's text ends in. A chunk's last line continues into
  // the chunks after it until one holds a newline.
  private endChunkOf(line: number, first: number): number {
    if (line - this.firstLines[first] + 1 < this.counts[first]) return first;
    let chunk = first;
    while (!this.newlineEnds[chunk] && chunk + 1 < this.newlineEnds.length) {
      chunk += 1;
      if (this.newlines[chunk]) break;
    }
    return chunk;
  }

  /** Stored position of a line's first character: a size bound, not a
   * UTF-16 offset (packed text stores bytes). */
  storedLineStart(line: number): number {
    const chunk = this.chunkOf(line);
    return this.storedStarts[chunk] + this.starts[chunk][line - this.firstLines[chunk]];
  }

  /** Stored position just past a line's last character. */
  storedLineEnd(line: number): number {
    if (line + 1 < this.lineCount) return this.storedLineStart(line + 1) - 1;
    return this.storedSize - (this.newlineEnds[this.newlineEnds.length - 1] ? 1 : 0);
  }

  /** Lines [start, end) joined with newlines. Throws PatchTextNotResident
   * when the store does not hold all of them. */
  textRange(start: number, end: number): string {
    if (end <= start) return '';
    let wide = this.nextWide(start, end);
    if (wide === end) return this.storedRange(start, end);
    // Stored slices between wide lines, each wide line decoded alone.
    let out = '';
    let run = start;
    while (wide < end) {
      if (run < wide) out += this.storedRange(run, wide) + '\n';
      out += this.text(wide);
      run = wide + 1;
      if (run < end) out += '\n';
      wide = this.nextWide(run, end);
    }
    return run < end ? out + this.storedRange(run, end) : out;
  }

  /** Lines [start, end) as stored, joined with newlines: not their text.
   * A packed line is its UTF-8 bytes, one character each, never decoded.
   * Throws PatchTextNotResident when the store does not hold all of them. */
  storedRange(start: number, end: number): string {
    if (end <= start) return '';
    const from = this.storedLineStart(start);
    const to = this.storedLineEnd(end - 1);
    const first = this.chunkOf(start);
    const last = this.chunkOf(end - 1);
    const firstBase = this.storedStarts[first];
    const firstText = this.chunks.read(first, start);
    if (first === last && to - firstBase <= firstText.length) {
      return firstText.slice(from - firstBase, to - firstBase);
    }
    let out = '';
    for (let chunk = first; chunk < this.storedStarts.length; chunk += 1) {
      const base = this.storedStarts[chunk];
      if (base >= to) break;
      const text = this.chunks.read(chunk, start);
      out += text.slice(Math.max(0, from - base), Math.min(text.length, to - base));
    }
    return out;
  }

  private chunkOf(line: number): number {
    let low = 0;
    let high = this.firstLines.length - 1;
    while (low < high) {
      const mid = (low + high + 1) >> 1;
      if (this.firstLines[mid] <= line) low = mid;
      else high = mid - 1;
    }
    // Chunks holding only a long line's middle share the next line's number.
    while (low > 0 && (this.counts[low] === 0 || line >= this.firstLines[low] + this.counts[low])) low -= 1;
    return low;
  }

  private hunkIndex(line: number): number {
    let low = 0;
    let high = this.hunkCount - 1;
    while (low <= high) {
      const mid = (low + high) >> 1;
      const at = this.hunkLines[mid];
      if (at === line) return mid;
      if (at < line) low = mid + 1;
      else high = mid - 1;
    }
    throw new Error(`patch store: line ${line} is not a hunk header`);
  }

  private ensureKindCapacity(lines: number): void {
    if (lines <= this.kinds.length) return;
    let size = this.kinds.length * 2;
    while (size < lines) size *= 2;
    const grown = new Uint8Array(size);
    grown.set(this.kinds);
    this.kinds = grown;
    if (this.wideBits !== null) {
      const bits = new Uint8Array(size >> 3);
      bits.set(this.wideBits);
      this.wideBits = bits;
    }
  }
}

function growInt32(array: Int32Array): Int32Array<ArrayBuffer> {
  const grown = new Int32Array(array.length * 2);
  grown.set(array);
  return grown;
}
