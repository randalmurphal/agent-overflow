import { checkWideMarks, unpackText } from './packedText';
import { patchMemory, type EvictableText } from './patchMemory.svelte';
import { parseHunkHeader } from './patchFiles';

// Compact storage for the review pane's patches. A patch of any size
// arrives in chunks (`ReadReviewDiff`) and is held as those chunk
// strings plus typed-array indexes: one Uint32 line start per line while
// the chunk's text is held, and one kind byte per line for as long as
// the patch is open. Nothing allocates per line. Rows, line objects and
// line strings are materialized only for the rows on screen
// (`patchRows.ts`).
//
// A chunk is plain (its text) or packed (its UTF-8 bytes, one character
// each: `packedText.ts`). A line of packed text holding a byte past ASCII
// is a wide line, one bit per line; reading one decodes it, and every
// other line reads as it is stored. Positions and lengths the store keeps
// count stored characters, which for packed text are bytes.
//
// Text read from a diff counts against the memory budget
// (`patchMemory.svelte.ts`). A store whose diff is still open can give up
// chunk text under it and read the text again from the diff. Line
// indexes, kinds and hunks stay resident, so everything but a line's text
// is always answerable. A reader of evicted text either shows a
// placeholder and asks for the text (`load`), or reads once it is back
// (`whenResident` in `patchBody.ts`).

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

let nextStoreId = 1;

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

/** The patch text from offset 0 to a chunk's end, as the backend keys it:
 * FNV-1a (32-bit) over the text's UTF-16 code units, and how many units
 * that is. */
export interface PatchTextKey {
  hash: number;
  units: number;
}

/** A chunk's backend byte range, which an evicted chunk is read again
 * by, and its form: `wide` present means its text is packed, and lists
 * the index of the first byte past ASCII of each line that has one.
 * `key` is the text's key through the chunk's end; a packed chunk always
 * has one. */
export interface PatchChunkSpan {
  offset: number;
  nextOffset: number;
  wide?: readonly number[];
  key?: PatchTextKey;
}

/** Runs for each line a chunk completes: the text its first character is
 * in, the line's offset there and stored length, and whether that range
 * is packed text holding a byte past ASCII (unpack what is kept of it). */
export type PatchLineCallback = (line: number, text: string, offset: number, length: number, packed: boolean) => void;

/** Where a store's chunks came from, to read an evicted one again. */
export interface PatchTextSource {
  /** Reads the chunk at backend bytes [offset, nextOffset) again, asking
   * the form it first arrived in, and says which form the answer has. */
  read(offset: number, nextOffset: number, packed: boolean): Promise<{ data: string; nextOffset: number; packed: boolean }>;
  release(): void;
}

const NO_MARKS: readonly number[] = [];

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

// What a plain chunk's text costs: V8 holds a string with any character
// past Latin-1 at two bytes per character. Packed text is one byte each.
function textBytes(text: string): number {
  return /[\u0100-\uffff]/.test(text) ? text.length * 2 : text.length;
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
export class PatchStore implements EvictableText {
  readonly id = nextStoreId++;
  private readonly texts: (string | null)[] = [];
  private readonly starts: Uint32Array[] = [];
  private readonly firstLines: number[] = [];
  private readonly counts: number[] = [];
  // Per chunk: where it starts, in stored characters, and whether it is
  // packed.
  private readonly storedStarts: number[] = [];
  private readonly packed: boolean[] = [];
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
  // One bit per line: whether it is a wide line. Null until the first.
  private wideBits: Uint8Array | null = null;
  /** Lines started so far. */
  lineCount = 0;
  /** Stored characters of everything appended: a size, not the text's
   * UTF-16 length (packed text stores bytes). */
  storedSize = 0;
  private openLine = -1;
  private openChunk = 0;
  private openOffset = 0;
  private openLength = 0;

  constructor() {
    heldText.register(this, this.held, this);
  }

  /**
   * Appends one chunk of patch text. `onLine` runs once per line that the
   * chunk completes, in line order; it may not read the store's line
   * text, which is indexed after the chunk is. `span` is the chunk's
   * backend byte range and form (PatchChunkSpan).
   */
  append(data: string, onLine: PatchLineCallback, span?: PatchChunkSpan): void {
    const chunk = this.texts.length;
    const wide = span?.wide;
    if (wide !== undefined) checkWideMarks(data, wide);
    this.texts.push(data);
    this.storedStarts.push(this.storedSize);
    this.packed.push(wide !== undefined);
    this.storedSize += data.length;
    this.newlines.push(data.includes('\n'));
    this.newlineEnds.push(data.endsWith('\n'));
    this.offsets.push(span?.offset ?? -1);
    this.nextOffsets.push(span?.nextOffset ?? -1);
    const size = wide !== undefined ? data.length : textBytes(data);
    this.sizes.push(size);
    this.hold(size);
    this.index(data, onLine, wide ?? NO_MARKS);
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

  // A cursor over the chunk's marks (`wide`) marks its wide lines.
  private index(data: string, onLine: PatchLineCallback, wide: readonly number[]): void {
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
      let packed = false;
      if (mark < wide.length && wide[mark] < (end < 0 ? data.length : end)) {
        this.markWide(line);
        mark += 1;
        packed = true;
      }
      if (end < 0) {
        this.openLine = line;
        this.openChunk = this.texts.length - 1;
        this.openOffset = pos;
        this.openLength = data.length - pos;
        break;
      }
      onLine(line, data, pos, end - pos, packed);
      pos = end + 1;
    }
    this.firstLines.push(firstLine);
    this.counts.push(count);
    this.starts.push(scratchStarts.slice(0, count));
  }

  /** Completes the last line when the patch does not end with a newline. */
  end(onLine: PatchLineCallback): void {
    if (this.openLine < 0) return;
    this.completeOpenLine(this.openLength, onLine);
    this.pinOpenLine();
    patchMemory.enforce();
  }

  // A line that spans chunks is classified from its first characters,
  // joined from its pieces: a header line is short and arrives whole this
  // way, and a longer line only needs its prefix.
  private completeOpenLine(length: number, onLine: PatchLineCallback): void {
    const line = this.openLine;
    this.openLine = -1;
    if (this.isWide(line)) {
      // The open line's chunks are pinned while it is open.
      onLine(line, this.wideText(line, this.openChunk, this.openOffset, Math.min(length, SPANNING_PREFIX_CHARS), false), 0, length, false);
      return;
    }
    let prefix = '';
    // The open line's chunks are pinned while it is open.
    for (let chunk = this.openChunk; chunk < this.texts.length && prefix.length < SPANNING_PREFIX_CHARS; chunk += 1) {
      const text = this.texts[chunk] ?? '';
      prefix += chunk === this.openChunk ? text.slice(this.openOffset) : text;
    }
    prefix = prefix.slice(0, Math.min(length, SPANNING_PREFIX_CHARS));
    onLine(line, prefix, 0, length, false);
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

  // A wide line's text from `start` in `chunk`: its pieces joined, each
  // run of packed pieces unpacked together so a sequence cut between
  // chunks decodes whole. Reads at most `limit` stored characters;
  // `touch` counts the read as a use of the chunks.
  private wideText(line: number, chunk: number, start: number, limit: number, touch: boolean): string {
    let out = '';
    let packed = '';
    let stored = 0;
    for (let at = chunk; at < this.texts.length && stored < limit; at += 1) {
      const text = touch ? this.residentText(at, line) : this.texts[at] ?? '';
      const from = at === chunk ? start : 0;
      const newline = text.indexOf('\n', from);
      const to = Math.min(newline < 0 ? text.length : newline, from + limit - stored);
      stored += to - from;
      if (this.packed[at]) {
        packed += text.slice(from, to);
      } else {
        if (packed !== '') out += unpackText(packed);
        packed = '';
        out += text.slice(from, to);
      }
      if (newline >= 0) break;
    }
    return packed === '' ? out : out + unpackText(packed);
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
    store.storedStarts.push(0);
    store.packed.push(false);
    store.newlines.push(true);
    store.newlineEnds.push(true);
    store.offsets.push(-1);
    store.nextOffsets.push(-1);
    store.sizes.push(textBytes(text));
    store.firstLines.push(0);
    store.counts.push(lines.length);
    store.starts.push(starts);
    store.lineCount = lines.length;
    store.storedSize = text.length;
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
    const local = line - this.firstLines[chunk];
    const start = this.starts[chunk][local];
    if (this.isWide(line)) return this.wideText(line, chunk, start, Infinity, true);
    const text = this.residentText(chunk, line);
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
    const packed = this.packed[chunk];
    const stored = this.storedLength(chunk);
    const read = this.reloadChain
      .then(() => {
        if (this.disposed) throw new PatchTextLost('patch text: the diff was closed');
        return source.read(offset, next, packed);
      })
      .then((answer) => {
        // The line index and wide bits stay from the first read, so the
        // answer must be that chunk in that form.
        if (answer.nextOffset !== next || answer.packed !== packed || answer.data.length !== stored) {
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

  private storedLength(chunk: number): number {
    return (chunk + 1 < this.storedStarts.length ? this.storedStarts[chunk + 1] : this.storedSize) - this.storedStarts[chunk];
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
    const firstText = this.residentText(first, start);
    if (first === last && to - firstBase <= firstText.length) {
      return firstText.slice(from - firstBase, to - firstBase);
    }
    let out = '';
    for (let chunk = first; chunk < this.texts.length; chunk += 1) {
      const base = this.storedStarts[chunk];
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
    if (this.wideBits !== null) {
      const bits = new Uint8Array(size >> 3);
      bits.set(this.wideBits);
      this.wideBits = bits;
    }
  }

  /** Bytes the store holds, for tests and measurement. */
  heldBytes(): number {
    let bytes = this.kinds.byteLength + this.hunkLines.byteLength * 3 + (this.wideBits?.byteLength ?? 0);
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
