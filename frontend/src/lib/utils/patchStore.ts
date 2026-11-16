import { PatchChunks, type PatchTextLost, type PatchTextSource } from './patchChunks';

// Compact storage for the review pane's patches. A patch of any size
// arrives in chunks (`reviewDiffStream.ts`) and is held as those chunks'
// UTF-8 bytes (`patchChunks.ts`) plus typed-array indexes: one Uint32 line
// start per line, and one kind byte per line. Nothing allocates per line.
// Line strings are decoded only for the rows on screen (`patchRows.ts`)
// and the ranges a reader asks for.
//
// Positions and sizes the store keeps count those bytes: a line's start,
// its size, a range's size. Only a line callback sees positions in the
// arrived chunk's text, which is not kept.
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

// Bytes of a line spanning chunks that its classification reads: enough
// for any header line's path.
const SPANNING_PREFIX_BYTES = 16 * 1024;

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

// Every decode of held bytes. A leading U+FEFF is text, not a mark to
// drop.
const decoder = new TextDecoder('utf-8', { ignoreBOM: true });

// The text of byte runs that are consecutive in the patch: a run may end
// inside a character the next one finishes.
function decodeRuns(runs: readonly Uint8Array[]): string {
  if (runs.length === 1) return decoder.decode(runs[0]);
  let out = '';
  for (let index = 0; index < runs.length; index += 1) out += decoder.decode(runs[index], { stream: index + 1 < runs.length });
  return out;
}

// `at`, or the start of the character `at` is inside, no earlier than
// `floor`: a cut there keeps whole characters.
function characterStart(bytes: Uint8Array, at: number, floor: number): number {
  while (at > floor && at < bytes.length && (bytes[at] & 0xc0) === 0x80) at -= 1;
  return at;
}

/** How many bytes TextEncoder writes for `text`, which writes a lone
 * surrogate as U+FFFD. */
export function utf8Length(text: string): number {
  let bytes = text.length;
  for (let index = 0; index < text.length; index += 1) {
    const unit = text.charCodeAt(index);
    if (unit < 0x80) continue;
    if (unit < 0x800) {
      bytes += 1;
    } else if (unit >= 0xd800 && unit <= 0xdbff && (text.charCodeAt(index + 1) & 0xfc00) === 0xdc00) {
      // A pair: two units, four bytes.
      bytes += 2;
      index += 1;
    } else {
      bytes += 2;
    }
  }
  return bytes;
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
 * by, and the key of the text through its end when the backend sent
 * one. */
export interface PatchChunkSpan {
  offset: number;
  nextOffset: number;
  key?: PatchTextKey;
}

/** Runs for each line a chunk completes, with text that holds the line
 * at [offset, offset + length) in UTF-16 units of `text`; a line spanning
 * chunks is given its first SPANNING_PREFIX_BYTES bytes. Answers what the
 * line is. */
export type PatchLineCallback = (line: number, text: string, offset: number, length: number) => LineClass;

/**
 * A copy of a slice that shares no memory with the string it was sliced
 * from. V8 and JavaScriptCore slice strings by reference, so a path kept
 * from a chunk's arrived text would keep all of that text; slicing a
 * concatenation flattens it into a new string first. Text a store decodes
 * is a string of its own.
 */
export function ownText(text: string): string {
  return (' ' + text).slice(1);
}

/** Copies (ownText) the text a parsed file or an open editor keeps from a
 * slice of a longer string. An object so a test can see each kept string
 * pass through it. */
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
  // Per chunk: its line starts (bytes into the chunk), first line and
  // line count; where it starts in the patch's bytes; and whether it holds
  // a newline and ends with one, so a line's chunks are known without its
  // text.
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
  private lines = 0;
  private stored = 0;
  // The line still arriving: its chunk, its start there and its bytes
  // so far.
  private openLine = -1;
  private openChunk = 0;
  private openOffset = 0;
  private openLength = 0;

  /** Lines started so far. */
  get lineCount(): number {
    return this.lines;
  }

  /** UTF-8 bytes of everything appended. */
  get storedSize(): number {
    return this.stored;
  }

  /**
   * Appends one chunk of patch text. `onLine` runs once per line that the
   * chunk completes, in line order; it may not read the store's line
   * text, which is indexed after the chunk is. The store keeps the
   * chunk's bytes, not `data`, so `data` must start and end between
   * characters: a surrogate pair cut in two would be held as two U+FFFD.
   * The backend never cuts inside a character (gitdiff.ChunkCut). `span`
   * is the chunk's backend byte range and key (PatchChunkSpan);
   * PatchParser, the one appender, has validated it.
   */
  append(data: string, onLine: PatchLineCallback, span?: PatchChunkSpan): void {
    if ((data.charCodeAt(0) & 0xfc00) === 0xdc00 || (data.charCodeAt(data.length - 1) & 0xfc00) === 0xd800) {
      throw new Error('patch store: a chunk is cut inside a character');
    }
    const chunk = this.chunks.push(data, span, true);
    const bytes = this.chunks.peek(chunk, this.lines);
    this.pushChunk(bytes);
    this.index(chunk, data, bytes, onLine);
    this.chunks.indexed(this.openLine >= 0 ? this.openChunk : -1);
  }

  // The store's half of a new chunk; its line index is set by `index`.
  private pushChunk(bytes: Uint8Array): void {
    this.storedStarts.push(this.stored);
    this.stored += bytes.length;
    this.newlines.push(bytes.includes(10));
    this.newlineEnds.push(bytes.length > 0 && bytes[bytes.length - 1] === 10);
  }

  // Walks the chunk's text, which the callback reads, and its bytes,
  // which the index records, a newline at a time: the nth newline of one
  // is the nth of the other. Text with nothing past ASCII is its bytes.
  private index(chunk: number, data: string, bytes: Uint8Array, onLine: PatchLineCallback): void {
    const ascii = bytes.length === data.length;
    let pos = 0;
    let at = 0;
    if (this.openLine >= 0) {
      const end = data.indexOf('\n');
      if (end < 0) {
        // The middle of a line longer than the chunk.
        this.openLength += bytes.length;
        this.firstLines.push(this.lines);
        this.counts.push(0);
        this.starts.push(new Uint32Array(0));
        return;
      }
      const byteEnd = ascii ? end : bytes.indexOf(10);
      this.completeOpenLine(this.openLength + byteEnd, onLine);
      pos = end + 1;
      at = byteEnd + 1;
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
      scratchStarts[count] = at;
      count += 1;
      this.ensureKindCapacity(this.lines);
      const end = data.indexOf('\n', pos);
      if (end < 0) {
        this.openLine = line;
        this.openChunk = chunk;
        this.openOffset = at;
        this.openLength = bytes.length - at;
        break;
      }
      this.record(line, onLine(line, data, pos, end - pos));
      pos = end + 1;
      at = ascii ? pos : bytes.indexOf(10, at) + 1;
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

  // A line that spans chunks is classified from its first bytes, joined
  // from its pieces: a header line is short and arrives whole this way,
  // and a longer line only needs its prefix. The open line's chunks are
  // pinned while it is open. `length` is the line's, in bytes.
  private completeOpenLine(length: number, onLine: PatchLineCallback): void {
    const line = this.openLine;
    this.openLine = -1;
    const prefix = this.joined(line, this.openChunk, this.openOffset, Math.min(length, SPANNING_PREFIX_BYTES), false);
    this.record(line, onLine(line, prefix, 0, prefix.length));
  }

  private record(line: number, kind: LineClass): void {
    if (typeof kind === 'number') {
      this.kinds[line] = kind;
      return;
    }
    this.addHunk(line, kind.oldStart, kind.newStart);
    this.kinds[line] = LINE_HUNK;
  }

  // A line's text from byte `start` in `chunk`, its pieces joined, at
  // most `limit` bytes of it, cut between characters. `read` counts the
  // read as a use of the chunks.
  private joined(line: number, chunk: number, start: number, limit: number, read: boolean): string {
    const runs: Uint8Array[] = [];
    let taken = 0;
    for (let at = chunk; at < this.chunks.count; at += 1) {
      const bytes = read ? this.chunks.read(at, line) : this.chunks.peek(at, line);
      const from = at === chunk ? start : 0;
      const newline = bytes.indexOf(10, from);
      const to = newline < 0 ? bytes.length : newline;
      if (to - from > limit - taken) {
        runs.push(bytes.subarray(from, characterStart(bytes, from + limit - taken, from)));
        break;
      }
      runs.push(bytes.subarray(from, to));
      taken += to - from;
      if (newline >= 0) break;
    }
    return decodeRuns(runs);
  }

  /** A store of explicitly typed lines, for patches built in memory. Its
   * text is not read from a diff and is not counted. A lone surrogate in
   * a line is held, and read back, as U+FFFD. */
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
      pos += utf8Length(line.content) + 1;
      store.record(index, line.kind);
      if (line.fold) {
        store.folds ??= new Map();
        store.folds.set(index, line.fold);
      }
    }
    store.chunks.push(text, undefined, false);
    store.pushChunk(store.chunks.peek(0, 0));
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
    const bytes = this.chunks.read(chunk, line);
    if (local + 1 < this.counts[chunk]) return decoder.decode(bytes.subarray(start, this.starts[chunk][local + 1] - 1));
    const newline = bytes.indexOf(10, start);
    if (newline >= 0) return decoder.decode(bytes.subarray(start, newline));
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

  /** Where a line starts in the patch's UTF-8 bytes. */
  storedLineStart(line: number): number {
    const chunk = this.chunkOf(line);
    return this.storedStarts[chunk] + this.starts[chunk][line - this.firstLines[chunk]];
  }

  /** Where a line's text ends in the patch's UTF-8 bytes, before its
   * newline. */
  storedLineEnd(line: number): number {
    if (line + 1 < this.lineCount) return this.storedLineStart(line + 1) - 1;
    return this.storedSize - (this.newlineEnds[this.newlineEnds.length - 1] ? 1 : 0);
  }

  /** Lines [start, end) joined with newlines, decoded at once. Throws
   * PatchTextNotResident when the store does not hold all of them. */
  textRange(start: number, end: number): string {
    if (end <= start) return '';
    const from = this.storedLineStart(start);
    const to = this.storedLineEnd(end - 1);
    const first = this.chunkOf(start);
    const base = this.storedStarts[first];
    const bytes = this.chunks.read(first, start);
    if (to - base <= bytes.length) return decoder.decode(bytes.subarray(from - base, to - base));
    const runs = [bytes.subarray(from - base)];
    for (let chunk = first + 1; chunk < this.storedStarts.length && this.storedStarts[chunk] < to; chunk += 1) {
      const held = this.chunks.read(chunk, start);
      runs.push(held.subarray(0, Math.min(held.length, to - this.storedStarts[chunk])));
    }
    return decodeRuns(runs);
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
  }
}

function growInt32(array: Int32Array): Int32Array<ArrayBuffer> {
  const grown = new Int32Array(array.length * 2);
  grown.set(array);
  return grown;
}
