import { appendFNV1a32 } from './fnv1a';
import { patchMemory, type EvictableText } from './patchMemory.svelte';
import {
  cleanPath,
  HUNK_ADDED,
  HUNK_CONTEXT,
  HUNK_HEADER,
  HUNK_OUTSIDE,
  HUNK_REMOVED,
  HunkBody,
  parseHunkHeader,
  PATCH_META_PREFIXES,
  type PatchFile,
  type PatchLine,
} from './patchFiles';

// Compact storage for the review pane's patches. A patch of any size
// arrives in chunks (`ReadReviewDiff`) and is held as those chunk
// strings plus typed-array indexes: one Uint32 line start per line while
// the chunk's text is held, and one kind byte per line for as long as
// the patch is open. Nothing allocates per line. Rows, line objects and
// line strings are materialized only for the rows on screen
// (`patchRows.ts`).
//
// `PatchParser` classifies lines as they complete and splits the patch
// into files with the same rules as `parsePatchFiles`; each file is a
// `PatchBody`, a view over line ranges of one or more stores.
//
// Text read from a diff counts against the memory budget
// (`patchMemory.svelte.ts`). A store whose diff is still open can give up
// chunk text under it and read the text again from the diff. Line
// indexes, kinds and hunks stay resident, so everything but a line's text
// is always answerable. A reader of evicted text either shows a
// placeholder and asks for the text (`load`), or reads once it is back
// (`whenResident`).

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

/** What the highlighter receives in place of a marker or fold row: a
 * `\`-prefixed line is a non-content marker that keeps line alignment
 * (see `diffSpanCache.svelte.ts`). */
const MARKER_PATCH_TEXT = '\\ marker';

let nextStoreId = 1;
let nextBodyId = 1;

// Characters of a line spanning chunks that its classification reads:
// enough for any header line's path.
const SPANNING_PREFIX_CHARS = 16 * 1024;

// Line starts of the chunk being appended, copied out once its size is known.
let scratchStarts = new Uint32Array(4096);

export interface StoredLine {
  content: string;
  kind: LineKind;
  fold?: { id: number; lines: number };
}

/** Where a store's chunks came from, to read an evicted one again. */
export interface PatchTextSource {
  /** Reads the chunk at backend bytes [offset, nextOffset) again. */
  read(offset: number, nextOffset: number): Promise<{ data: string; nextOffset: number }>;
  release(): void;
}

/** Evicted text that can no longer be read: the diff's handle ended, the
 * diff no longer serves the same bytes, or the store was disposed. */
export class PatchTextLost extends Error {
  constructor(message = 'patch text: the diff can no longer be read') {
    super(message);
    this.name = 'PatchTextLost';
  }
}

/** A read of text the store does not hold. */
export class PatchTextNotResident extends Error {
  constructor(line: number) {
    super(`patch store: line ${line} is not resident`);
    this.name = 'PatchTextNotResident';
  }
}

// A store the owner never disposed stops being counted when it is collected.
const heldText = new FinalizationRegistry<{ bytes: number; pinned: number }>((held) => {
  patchMemory.remove(held.bytes);
  patchMemory.pin(-held.pinned);
});

// What a chunk's text costs: V8 holds a string with any character past
// Latin-1 at two bytes per character.
function textBytes(text: string): number {
  return /[^\x00-\xff]/.test(text) ? text.length * 2 : text.length;
}

/**
 * A copy of text read from a store that shares no memory with its chunk.
 * V8 and JavaScriptCore slice strings by reference, so a line kept after
 * its chunk is evicted would keep the whole chunk; slicing a
 * concatenation flattens it into a new string first.
 */
export function ownText(text: string): string {
  return (' ' + text).slice(1);
}

/**
 * The lines of one patch, stored as the chunks they arrived in.
 *
 * A line normally lies inside one chunk. A line longer than a read spans
 * several: it starts in one chunk, every chunk in between holds only its
 * middle, and a later chunk holds its end. Lines are numbered from 0 in
 * arrival order.
 */
export class PatchStore implements EvictableText {
  readonly id = nextStoreId++;
  private readonly texts: (string | null)[] = [];
  private readonly starts: Uint32Array[] = [];
  private readonly firstLines: number[] = [];
  private readonly counts: number[] = [];
  private readonly charStarts: number[] = [];
  // Per chunk: whether its text holds a newline and ends with one, so a
  // line's chunks are known without its text.
  private readonly newlines: boolean[] = [];
  private readonly newlineEnds: boolean[] = [];
  // Per chunk: its backend byte range (-1 when it has none) and what its
  // text costs.
  private readonly offsets: number[] = [];
  private readonly nextOffsets: number[] = [];
  private readonly sizes: number[] = [];
  private readonly held = { bytes: 0, pinned: 0 };
  private source: PatchTextSource | null = null;
  private readonly reloads = new Map<number, Promise<void>>();
  // Reads of evicted chunks run one at a time, so reads in patch order
  // continue one backend stream.
  private reloadChain: Promise<unknown> = Promise.resolve();
  private readonly pins = new Map<number, number>();
  // Bytes of the chunks pinned by `pin` and by the line still arriving;
  // their sum is reported to the budget (`held.pinned`).
  private pinnedBytes = 0;
  private openPinnedBytes = 0;
  private lastRead = -1;
  private disposed = false;
  private lost: PatchTextLost | null = null;
  private lostListeners: ((err: PatchTextLost) => void)[] = [];
  /** Whether any chunk was evicted: its text lives only in the source. */
  evictedAny = false;
  private kinds = new Uint8Array(256);
  private hunkLines = new Int32Array(16);
  private hunkOld = new Int32Array(16);
  private hunkNew = new Int32Array(16);
  private hunkCount = 0;
  private folds: Map<number, { id: number; lines: number }> | null = null;
  /** Lines started so far. */
  lineCount = 0;
  /** UTF-16 length of everything appended. */
  totalChars = 0;
  private openLine = -1;
  private openChunk = 0;
  private openOffset = 0;
  private openLength = 0;

  constructor() {
    heldText.register(this, this.held, this);
  }

  /**
   * Appends one chunk of patch text. `onLine` runs once per line that the
   * chunk completes, in line order, with the text its first character is
   * in and that line's length; it may not read the store's line text,
   * which is indexed after the chunk is. `span` is the chunk's backend
   * byte range, which an evicted chunk is read again by.
   */
  append(
    data: string,
    onLine: (line: number, text: string, offset: number, length: number) => void,
    span?: { offset: number; nextOffset: number },
  ): void {
    const chunk = this.texts.length;
    this.texts.push(data);
    this.charStarts.push(this.totalChars);
    this.totalChars += data.length;
    this.newlines.push(data.includes('\n'));
    this.newlineEnds.push(data.endsWith('\n'));
    this.offsets.push(span?.offset ?? -1);
    this.nextOffsets.push(span?.nextOffset ?? -1);
    const size = textBytes(data);
    this.sizes.push(size);
    this.hold(size);
    this.index(data, onLine);
    this.pinOpenLine();
    if (this.source && span) {
      patchMemory.offer(this, chunk, true);
      patchMemory.enforce();
    }
  }

  // The chunks of a line still arriving are pinned until it ends.
  private pinOpenLine(): void {
    let bytes = 0;
    if (this.openLine >= 0) {
      for (let chunk = this.openChunk; chunk < this.sizes.length; chunk += 1) bytes += this.sizes[chunk];
    }
    this.reportPinned(this.pinnedBytes, bytes);
  }

  private reportPinned(explicit: number, open: number): void {
    if (this.disposed) return;
    patchMemory.pin(explicit + open - this.held.pinned);
    this.pinnedBytes = explicit;
    this.openPinnedBytes = open;
    this.held.pinned = explicit + open;
  }

  private index(data: string, onLine: (line: number, text: string, offset: number, length: number) => void): void {
    let pos = 0;
    if (this.openLine >= 0) {
      const end = data.indexOf('\n');
      if (end < 0) {
        // The middle of a line longer than the chunk.
        this.openLength += data.length;
        this.firstLines.push(this.lineCount);
        this.counts.push(0);
        this.starts.push(new Uint32Array(0));
        return;
      }
      this.completeOpenLine(this.openLength + end, onLine);
      pos = end + 1;
    }
    const firstLine = this.lineCount;
    let count = 0;
    while (pos < data.length) {
      const line = this.lineCount;
      this.lineCount += 1;
      if (count === scratchStarts.length) {
        const grown = new Uint32Array(scratchStarts.length * 2);
        grown.set(scratchStarts);
        scratchStarts = grown;
      }
      scratchStarts[count] = pos;
      count += 1;
      this.ensureKindCapacity(this.lineCount);
      const end = data.indexOf('\n', pos);
      if (end < 0) {
        this.openLine = line;
        this.openChunk = this.texts.length - 1;
        this.openOffset = pos;
        this.openLength = data.length - pos;
        break;
      }
      onLine(line, data, pos, end - pos);
      pos = end + 1;
    }
    this.firstLines.push(firstLine);
    this.counts.push(count);
    this.starts.push(scratchStarts.slice(0, count));
  }

  /** Completes the last line when the patch does not end with a newline. */
  end(onLine: (line: number, text: string, offset: number, length: number) => void): void {
    if (this.openLine < 0) return;
    this.completeOpenLine(this.openLength, onLine);
    this.pinOpenLine();
    patchMemory.enforce();
  }

  // A line that spans chunks is classified from its first characters,
  // joined from its pieces: a header line is short and arrives whole this
  // way, and a longer line only needs its prefix.
  private completeOpenLine(length: number, onLine: (line: number, text: string, offset: number, length: number) => void): void {
    const line = this.openLine;
    this.openLine = -1;
    let prefix = '';
    // The open line's chunks are pinned while it is open.
    for (let chunk = this.openChunk; chunk < this.texts.length && prefix.length < SPANNING_PREFIX_CHARS; chunk += 1) {
      const text = this.texts[chunk] ?? '';
      prefix += chunk === this.openChunk ? text.slice(this.openOffset) : text;
    }
    prefix = prefix.slice(0, Math.min(length, SPANNING_PREFIX_CHARS));
    onLine(line, prefix, 0, length);
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
      let kind = line.kind;
      if (kind === LINE_HUNK) {
        const header = parseHunkHeader(line.content);
        if (header) store.addHunk(index, header.oldStart, header.newStart);
        else kind = LINE_META;
      }
      store.kinds[index] = kind;
      if (line.fold) {
        store.folds ??= new Map();
        store.folds.set(index, line.fold);
      }
    }
    store.texts.push(text);
    store.charStarts.push(0);
    store.newlines.push(true);
    store.newlineEnds.push(true);
    store.offsets.push(-1);
    store.nextOffsets.push(-1);
    store.sizes.push(textBytes(text));
    store.firstLines.push(0);
    store.counts.push(lines.length);
    store.starts.push(starts);
    store.lineCount = lines.length;
    store.totalChars = text.length;
    return store;
  }

  kind(line: number): LineKind {
    return this.kinds[line] as LineKind;
  }

  setKind(line: number, kind: LineKind): void {
    this.kinds[line] = kind;
  }

  /** Records a hunk header's starts. Headers are added in line order. */
  addHunk(line: number, oldStart: number, newStart: number): void {
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
    const text = this.residentText(chunk, line);
    const local = line - this.firstLines[chunk];
    const start = this.starts[chunk][local];
    if (local + 1 < this.counts[chunk]) return text.slice(start, this.starts[chunk][local + 1] - 1);
    const newline = text.indexOf('\n', start);
    if (newline >= 0) return text.slice(start, newline);
    // A line longer than its chunk: the rest is in the chunks after it.
    let joined = text.slice(start);
    for (let next = chunk + 1; next < this.texts.length; next += 1) {
      const piece = this.residentText(next, line);
      const end = piece.indexOf('\n');
      if (end >= 0) return joined + piece.slice(0, end);
      joined += piece;
    }
    return joined;
  }

  /** Whether the store holds a line's text. */
  resident(line: number): boolean {
    const first = this.chunkOf(line);
    const last = this.endChunkOf(line, first);
    for (let chunk = first; chunk <= last; chunk += 1) {
      if (this.texts[chunk] === null) return false;
    }
    return true;
  }

  /** Whether the store holds the text of lines [start, end). */
  residentRange(start: number, end: number): boolean {
    if (end <= start) return true;
    const last = this.endChunkOf(end - 1, this.chunkOf(end - 1));
    for (let chunk = this.chunkOf(start); chunk <= last; chunk += 1) {
      if (this.texts[chunk] === null) return false;
    }
    return true;
  }

  /**
   * Reads evicted text of lines [start, end) again. The text can be
   * evicted again once it lands; a reader that must read all of it at
   * once pins it first (`whenResident`). Rejects with PatchTextLost when
   * the text can no longer be read.
   */
  load(start: number, end: number): Promise<void> {
    if (end <= start) return Promise.resolve();
    const pending: Promise<void>[] = [];
    const last = this.endChunkOf(end - 1, this.chunkOf(end - 1));
    for (let chunk = this.chunkOf(start); chunk <= last; chunk += 1) {
      if (this.texts[chunk] === null) pending.push(this.reload(chunk));
    }
    if (pending.length === 0) return Promise.resolve();
    return Promise.all(pending).then(() => undefined);
  }

  /** Keeps the chunks of lines [start, end) from eviction until the
   * returned function runs. */
  pin(start: number, end: number): () => void {
    if (end <= start) return () => {};
    const first = this.chunkOf(start);
    const last = this.endChunkOf(end - 1, this.chunkOf(end - 1));
    let bytes = this.pinnedBytes;
    for (let chunk = first; chunk <= last; chunk += 1) {
      const count = this.pins.get(chunk) ?? 0;
      if (count === 0) bytes += this.sizes[chunk];
      this.pins.set(chunk, count + 1);
    }
    this.reportPinned(bytes, this.openPinnedBytes);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      let bytes = this.pinnedBytes;
      for (let chunk = first; chunk <= last; chunk += 1) {
        const count = (this.pins.get(chunk) ?? 1) - 1;
        if (count > 0) {
          this.pins.set(chunk, count);
        } else {
          this.pins.delete(chunk);
          bytes -= this.sizes[chunk];
        }
      }
      this.reportPinned(bytes, this.openPinnedBytes);
      patchMemory.enforce();
    };
  }

  /** Makes chunks appended from now on, and every chunk held so far,
   * evictable: they can be read again from `source`. */
  attachSource(source: PatchTextSource): void {
    this.source = source;
    this.lastRead = -1;
    for (let chunk = 0; chunk < this.texts.length; chunk += 1) {
      if (this.texts[chunk] !== null && this.offsets[chunk] >= 0) patchMemory.offer(this, chunk, true);
    }
    patchMemory.enforce();
  }

  /** Stops evicting: every chunk is resident and stays. Returns the source
   * for its owner to release. */
  detachSource(): PatchTextSource | null {
    const source = this.source;
    this.source = null;
    for (let chunk = 0; chunk < this.texts.length; chunk += 1) patchMemory.withdraw(this, chunk);
    return source;
  }

  /**
   * Runs `listener` once evicted text can no longer be read, at once when
   * that already happened. Whoever shows the store reads the diff again.
   */
  whenLost(listener: (err: PatchTextLost) => void): void {
    if (this.lost) listener(this.lost);
    else this.lostListeners.push(listener);
  }

  /** Stops counting the store's text and releases its source. Text it
   * still holds stays readable; evicted text is lost. */
  dispose(): void {
    if (this.disposed) return;
    this.reportPinned(0, 0);
    this.disposed = true;
    this.lostListeners = [];
    for (let chunk = 0; chunk < this.texts.length; chunk += 1) patchMemory.withdraw(this, chunk);
    patchMemory.remove(this.held.bytes);
    this.held.bytes = 0;
    heldText.unregister(this);
    const source = this.source;
    this.source = null;
    source?.release();
  }

  pinned(chunk: number): boolean {
    return (this.openLine >= 0 && chunk >= this.openChunk) || this.reloads.has(chunk) || this.pins.has(chunk);
  }

  evict(chunk: number): number {
    if (this.texts[chunk] === null) return 0;
    this.texts[chunk] = null;
    this.evictedAny = true;
    if (chunk === this.lastRead) this.lastRead = -1;
    const size = this.sizes[chunk];
    this.held.bytes -= size;
    return size;
  }

  private hold(bytes: number): void {
    if (this.disposed) return;
    this.held.bytes += bytes;
    patchMemory.add(bytes);
  }

  private residentText(chunk: number, line: number): string {
    const text = this.texts[chunk];
    if (text === null) throw new PatchTextNotResident(line);
    // Consecutive reads of one chunk are one use.
    if (chunk !== this.lastRead) {
      this.lastRead = chunk;
      patchMemory.touch(this, chunk);
    }
    return text;
  }

  private reload(chunk: number): Promise<void> {
    const pending = this.reloads.get(chunk);
    if (pending) return pending;
    const source = this.source;
    if (this.lost) return Promise.reject(this.lost);
    if (!source || this.disposed) return Promise.reject(new PatchTextLost('patch text: the diff was closed'));
    const offset = this.offsets[chunk];
    const next = this.nextOffsets[chunk];
    const chars = this.charLength(chunk);
    const read = this.reloadChain
      .then(() => {
        if (this.disposed) throw new PatchTextLost('patch text: the diff was closed');
        return source.read(offset, next);
      })
      .then((answer) => {
        if (answer.nextOffset !== next || answer.data.length !== chars) {
          throw new PatchTextLost('patch text: the diff no longer serves the same chunk');
        }
        if (this.disposed || this.texts[chunk] !== null) return;
        this.texts[chunk] = answer.data;
        this.hold(this.sizes[chunk]);
        patchMemory.offer(this, chunk, false);
        patchMemory.restored();
      })
      .catch((err: unknown) => {
        // A chunk that cannot be read is text this store can no longer
        // show, whatever the cause; its owner reads the diff again.
        const lost = err instanceof PatchTextLost
          ? err
          : new PatchTextLost(`patch text: ${err instanceof Error ? err.message : String(err)}`);
        if (!this.lost && !this.disposed) {
          this.lost = lost;
          const listeners = this.lostListeners;
          this.lostListeners = [];
          for (const listener of listeners) listener(lost);
        }
        throw lost;
      })
      .finally(() => {
        this.reloads.delete(chunk);
        patchMemory.enforce();
      });
    this.reloads.set(chunk, read);
    this.reloadChain = read.catch(() => {});
    return read;
  }

  private charLength(chunk: number): number {
    return (chunk + 1 < this.charStarts.length ? this.charStarts[chunk + 1] : this.totalChars) - this.charStarts[chunk];
  }

  // The chunk a line's text ends in. A chunk's last line continues into
  // the chunks after it until one holds a newline.
  private endChunkOf(line: number, first: number): number {
    if (line - this.firstLines[first] + 1 < this.counts[first]) return first;
    let chunk = first;
    while (!this.newlineEnds[chunk] && chunk + 1 < this.texts.length) {
      chunk += 1;
      if (this.newlines[chunk]) break;
    }
    return chunk;
  }

  /** Absolute UTF-16 offset of a line's first character. */
  lineStart(line: number): number {
    const chunk = this.chunkOf(line);
    return this.charStarts[chunk] + this.starts[chunk][line - this.firstLines[chunk]];
  }

  /** Absolute UTF-16 offset just past a line's last character. */
  lineEnd(line: number): number {
    if (line + 1 < this.lineCount) return this.lineStart(line + 1) - 1;
    return this.totalChars - (this.newlineEnds[this.newlineEnds.length - 1] ? 1 : 0);
  }

  /** Lines [start, end) joined with newlines. Throws PatchTextNotResident
   * when the store does not hold all of them. */
  textRange(start: number, end: number): string {
    if (end <= start) return '';
    const from = this.lineStart(start);
    const to = this.lineEnd(end - 1);
    const first = this.chunkOf(start);
    const last = this.chunkOf(end - 1);
    const firstBase = this.charStarts[first];
    const firstText = this.residentText(first, start);
    if (first === last && to - firstBase <= firstText.length) {
      return firstText.slice(from - firstBase, to - firstBase);
    }
    let out = '';
    for (let chunk = first; chunk < this.texts.length; chunk += 1) {
      const base = this.charStarts[chunk];
      if (base >= to) break;
      const text = this.residentText(chunk, start);
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
  }

  /** Bytes the store holds, for tests and measurement. */
  heldBytes(): number {
    let bytes = this.kinds.byteLength + this.hunkLines.byteLength * 3;
    for (let chunk = 0; chunk < this.texts.length; chunk += 1) {
      bytes += (this.texts[chunk] === null ? 0 : this.sizes[chunk]) + this.starts[chunk].byteLength;
    }
    return bytes;
  }
}

function growInt32(array: Int32Array): Int32Array<ArrayBuffer> {
  const grown = new Int32Array(array.length * 2);
  grown.set(array);
  return grown;
}

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
  /** Length of `patchText()`. */
  readonly textLength: number;
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
    let markerChars = 0;
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
        markerChars += MARKER_PATCH_TEXT.length - (store.lineEnd(line) - store.lineStart(line));
      }
    }
    this.contentRows = contentRows;
    this.gapLines = Int32Array.from(gaps);
    this.trailingGap = sawHunk && firstOldStart > 0 && lastNewStart > 0;
    this.endNew = newLine;
    this.maxLine = maxLine;
    this.hasMarkers = hasMarkers;

    let textLength = Math.max(0, this.segments.length - 1);
    for (const segment of this.segments) {
      textLength += segment.store.lineEnd(segment.end - 1) - segment.store.lineStart(segment.start);
    }
    this.textLength = lineCount === 0 ? 0 : textLength + markerChars;
  }

  /** A body over explicitly typed lines. */
  static fromLines(lines: readonly StoredLine[]): PatchBody {
    const store = PatchStore.fromLines(lines);
    return new PatchBody([{ store, start: 0, end: store.lineCount }]);
  }

  /** A body over a parsed PatchFile's lines (conflict pseudo-files,
   * merged edit sections). */
  static fromPatchLines(lines: readonly PatchLine[]): PatchBody {
    return PatchBody.fromLines(lines.map(storedLine));
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

  /** Whether a line's text is resident. */
  residentLine(index: number): boolean {
    const { store, line } = this.locate(index);
    return store.resident(line);
  }

  /** Whether the text of every line is resident. */
  resident(): boolean {
    return this.segments.every((segment) => segment.store.residentRange(segment.start, segment.end));
  }

  /** Reads the evicted text of lines [start, end) again (PatchStore.load). */
  loadLines(start: number, end: number): Promise<void> {
    return Promise.all(
      this.slice(start, end).map((range) => range.store.load(range.start, range.end)),
    ).then(() => undefined);
  }

  /** Runs `read` with lines [start, end) resident (whenResident). */
  whenResident<T>(read: () => T, start = 0, end = this.lineCount): Promise<T> {
    return whenResident(this.slice(start, end), read);
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
    let hash = 0x811c9dc5;
    if (!this.hasMarkers) {
      for (let index = 0; index < this.segments.length; index += 1) {
        if (index > 0) hash = appendFNV1a32(hash, '\n');
        const segment = this.segments[index];
        hash = appendFNV1a32(hash, segment.store.textRange(segment.start, segment.end));
      }
    } else {
      hash = appendFNV1a32(hash, this.patchText());
    }
    this.key = `${this.textLength}:${hash.toString(36)}`;
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
    if (this.segments.length === 1) {
      const segment = this.segments[0];
      return { store: segment.store, line: segment.start + index };
    }
    let low = 0;
    let high = this.bases.length - 1;
    while (low < high) {
      const mid = (low + high + 1) >> 1;
      if (this.bases[mid] <= index) low = mid;
      else high = mid - 1;
    }
    const segment = this.segments[low];
    return { store: segment.store, line: segment.start + index - this.bases[low] };
  }
}

/**
 * Runs `read` once the text of every range is resident, and keeps it
 * resident while `read` runs; synchronously when it already is. Rejects
 * with PatchTextLost when evicted text can no longer be read.
 */
export async function whenResident<T>(ranges: readonly BodySegment[], read: () => T): Promise<T> {
  const releases = ranges.map((range) => range.store.pin(range.start, range.end));
  try {
    const loads = ranges
      .filter((range) => !range.store.residentRange(range.start, range.end))
      .map((range) => range.store.load(range.start, range.end));
    if (loads.length > 0) await Promise.all(loads);
    return read();
  } finally {
    for (const release of releases) release();
  }
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
      return { content: line.content, kind: parseHunkHeader(line.content) ? LINE_HUNK : LINE_META };
  }
}

/** The file-level facts every file list, header and tree reads. */
export interface DiffFileSummary {
  path: string;
  kind: string;
  additions: number;
  deletions: number;
  /** Conflict-region count for `kind === 'conflict'` pseudo-files. */
  conflicts?: number;
  /** Structural-conflict badge for conflict pseudo-files with no
   * textual regions, e.g. "modify/delete". */
  conflictLabel?: string;
}

/** One file of the review surface: its summary and its compact lines. */
export interface ReviewFile extends DiffFileSummary {
  readonly body: PatchBody;
  /** New-side file length, learned from a context expansion. Sizes (or
   * retires) the trailing gap. */
  newSideTotal?: number;
  /** Skip hunk-gap rows (see PatchFile.suppressGaps). */
  suppressGaps?: boolean;
}

/** A review file over a parsed PatchFile (conflict pseudo-files). */
export function reviewFileFromPatchFile(file: PatchFile): ReviewFile {
  const out: ReviewFile = {
    path: file.path,
    kind: file.kind,
    additions: file.additions,
    deletions: file.deletions,
    body: PatchBody.fromPatchLines(file.lines),
  };
  if (file.conflicts !== undefined) out.conflicts = file.conflicts;
  if (file.conflictLabel !== undefined) out.conflictLabel = file.conflictLabel;
  if (file.newSideTotal !== undefined) out.newSideTotal = file.newSideTotal;
  if (file.suppressGaps) out.suppressGaps = true;
  return out;
}

/** A PatchFile over a review file's lines, for surfaces that render one
 * whole file with the PatchFile renderers. */
export function patchFileFromReviewFile(file: ReviewFile): PatchFile {
  const out: PatchFile = {
    path: file.path,
    kind: file.kind,
    additions: file.additions,
    deletions: file.deletions,
    lines: file.body.toPatchLines(),
  };
  if (file.newSideTotal !== undefined) out.newSideTotal = file.newSideTotal;
  if (file.suppressGaps) out.suppressGaps = true;
  return out;
}

interface Section {
  path: string;
  kind: string;
  additions: number;
  deletions: number;
  start: number;
  end: number;
  /** First hunk header, or -1. */
  firstHunk: number;
}

/**
 * Splits patch text into review files as it arrives. The rules are
 * `parsePatchFiles`': a file starts at each `diff --git` line, its path
 * and kind come from its header lines, and a type change (the same path
 * deleted then added in adjacent sections) is one file.
 *
 * `files` grows as files complete. A deleted file is held back until the
 * next section shows whether it is half of a type change, so a published
 * file is final.
 */
export class PatchParser {
  readonly store = new PatchStore();
  readonly files: ReviewFile[] = [];
  private current: Section | null = null;
  private held: Section | null = null;
  private readonly body = new HunkBody();
  private readonly finished: ([Section] | [Section, Section])[] = [];
  private hash = 0x811c9dc5;
  private ended = false;
  private readonly onLine = (line: number, text: string, offset: number, length: number): void => {
    this.completeLine(line, text, offset, length);
  };

  /** Appends patch text; any chunk boundary is allowed. `span` is the
   * chunk's backend byte range, for a store that can evict it. */
  append(data: string, span?: { offset: number; nextOffset: number }): void {
    if (this.ended) throw new Error('patch parser: append after end');
    if (data === '') return;
    this.hash = appendFNV1a32(this.hash, data);
    this.store.append(data, this.onLine, span);
    this.publish();
  }

  /** Marks the end of the patch and publishes the last files. */
  end(): void {
    if (this.ended) return;
    this.ended = true;
    this.store.end(this.onLine);
    this.finishSection(this.store.lineCount);
    if (this.held) {
      this.finished.push([this.held]);
      this.held = null;
    }
    this.publish();
  }

  get complete(): boolean {
    return this.ended;
  }

  /** The review comment source key of the whole patch: `diffSourceKey`
   * of its text. Empty for an empty patch and before the end. */
  sourceKey(): string {
    if (!this.ended || this.store.totalChars === 0) return '';
    return `fnv1a:${this.hash.toString(16).padStart(8, '0')}:${this.store.totalChars}`;
  }

  private completeLine(line: number, text: string, offset: number, length: number): void {
    const store = this.store;
    const kind = this.body.next(text, offset, length);
    if (startsAt(text, offset, length, 'diff --git ')) {
      this.finishSection(line);
      const parts = text.slice(offset, offset + length).split(/\s+/);
      this.current = {
        path: cleanPath(parts[3] ?? ''),
        kind: 'modified',
        additions: 0,
        deletions: 0,
        start: line,
        end: line,
        firstHunk: -1,
      };
      store.setKind(line, LINE_META);
      return;
    }
    const current = this.current;
    // parsePatchFiles' rule: a hunk body's lines by its header's counts,
    // and a line outside every body by its prefix.
    const outside = kind === HUNK_OUTSIDE;
    const plus = kind === HUNK_ADDED
      || (outside && length > 0 && text.charCodeAt(offset) === 43 && !startsAt(text, offset, length, '+++'));
    const minus = kind === HUNK_REMOVED
      || (outside && length > 0 && text.charCodeAt(offset) === 45 && !startsAt(text, offset, length, '---'));
    if (current) {
      if (startsAt(text, offset, length, 'new file')) current.kind = 'added';
      if (startsAt(text, offset, length, 'deleted file')) current.kind = 'deleted';
      if (startsAt(text, offset, length, 'rename from ')) current.kind = 'renamed';
      if (startsAt(text, offset, length, 'rename to ')) {
        current.path = cleanPath(text.slice(offset + 'rename to '.length, offset + length));
      }
      if (outside && startsAt(text, offset, length, '+++ ')) {
        const next = cleanPath(text.slice(offset + 4, offset + length));
        if (next && next !== '/dev/null') current.path = next;
      }
      if (plus) current.additions += 1;
      if (minus) current.deletions += 1;
    }
    if (plus) {
      store.setKind(line, LINE_ADD);
    } else if (minus) {
      store.setKind(line, LINE_DEL);
    } else if (kind === HUNK_CONTEXT) {
      store.setKind(line, LINE_CONTEXT);
    } else if (kind === HUNK_HEADER && this.body.header) {
      store.addHunk(line, this.body.header.oldStart, this.body.header.newStart);
      store.setKind(line, LINE_HUNK);
      if (current && current.firstHunk < 0) current.firstHunk = line;
    } else if (kind !== HUNK_OUTSIDE || PATCH_META_PREFIXES.some((prefix) => startsAt(text, offset, length, prefix))) {
      // A malformed hunk header, the no-newline marker, or a file header.
      store.setKind(line, LINE_META);
    } else {
      store.setKind(line, LINE_CONTEXT);
    }
  }

  // Sections finish while a chunk is being indexed, so their files are
  // built by publish() once the store can read the chunk's lines.
  private finishSection(end: number): void {
    const section = this.current;
    if (!section) return;
    this.current = null;
    section.end = end;
    if (!section.path) return;
    const held = this.held;
    if (held) {
      this.held = null;
      if (held.path === section.path && section.kind === 'added') {
        this.finished.push([held, section]);
        return;
      }
      this.finished.push([held]);
    }
    if (section.kind === 'deleted') {
      this.held = section;
      return;
    }
    this.finished.push([section]);
  }

  private publish(): void {
    for (const sections of this.finished) {
      this.files.push(sections.length === 2 ? this.typeChangeFile(sections[0], sections[1]) : this.sectionFile(sections[0]));
    }
    this.finished.length = 0;
  }

  private sectionFile(section: Section): ReviewFile {
    return {
      path: section.path,
      kind: section.kind,
      additions: section.additions,
      deletions: section.deletions,
      body: new PatchBody([{ store: this.store, start: section.start, end: section.end }]),
    };
  }

  // The created section's header block is dropped: meta rows never render,
  // and one file carries one preamble. Both sides are present whole, so no
  // gap is shown between the deletion and creation hunks.
  private typeChangeFile(deleted: Section, added: Section): ReviewFile {
    const segments: BodySegment[] = [{ store: this.store, start: deleted.start, end: deleted.end }];
    if (added.firstHunk >= 0) segments.push({ store: this.store, start: added.firstHunk, end: added.end });
    return {
      path: deleted.path,
      kind: 'modified',
      additions: deleted.additions + added.additions,
      deletions: deleted.deletions + added.deletions,
      body: new PatchBody(segments),
      suppressGaps: true,
    };
  }
}

function startsAt(text: string, offset: number, length: number, prefix: string): boolean {
  return length >= prefix.length && text.startsWith(prefix, offset);
}

/** Parses a whole patch held in memory. */
export function parseReviewFiles(patch: string): ReviewFile[] {
  const parser = new PatchParser();
  parser.append(patch);
  parser.end();
  return parser.files;
}
