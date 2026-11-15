import { patchMemory, type EvictableText } from './patchMemory.svelte';

// The chunk texts of one patch and their residency under the memory
// budget (`patchMemory.svelte.ts`), addressed by chunk number. A store
// whose diff is still open can give up chunk text under the budget and
// read it again from the diff (`PatchTextSource`). The line index over
// the chunks is `PatchStore`'s.

let nextChunksId = 1;

/** Where a store's chunks came from, to read an evicted one again. */
export interface PatchTextSource {
  /** Reads the chunk at backend bytes [offset, nextOffset) again, asking
   * the form it first arrived in, and says which form the answer has. */
  read(offset: number, nextOffset: number, packed: boolean): Promise<{ data: string; nextOffset: number; packed: boolean }>;
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

// What the budget counts for a set of chunks: the bytes of its resident
// chunks, and of those the bytes of the pinned ones.
interface Held {
  bytes: number;
  pinned: number;
}

// Chunks the owner never disposed stop being counted when collected.
const heldText = new FinalizationRegistry<Held>((held) => {
  patchMemory.remove(held.bytes);
  patchMemory.pin(-held.pinned);
});

// What a plain chunk's text costs: V8 holds a string with any character
// past Latin-1 at two bytes per character. Packed text is one byte each.
function textBytes(text: string): number {
  return /[\u0100-\uffff]/.test(text) ? text.length * 2 : text.length;
}

/**
 * One patch's chunk strings, each plain (its text) or packed (its UTF-8
 * bytes, one character each). A chunk with a backend byte range can be
 * evicted once a source is attached, and is read again from it in the
 * form it first arrived in.
 *
 * A pinned chunk is not evicted: a reader holds it (`pin`), the line
 * still arriving is joined across it (`indexed`), or it is being read
 * again. The bytes of chunks both resident and pinned are one sum
 * (`held.pinned`), which the budget leaves out.
 */
export class PatchChunks implements EvictableText {
  readonly id = nextChunksId++;
  private readonly texts: (string | null)[] = [];
  private readonly packed: boolean[] = [];
  // Per chunk: its stored length, its backend byte range (-1 when it has
  // none), what the budget counts for its text (0 for text it does not
  // count, which so never raises the pinned sum) and how many pins hold it.
  private readonly lengths: number[] = [];
  private readonly offsets: number[] = [];
  private readonly nextOffsets: number[] = [];
  private readonly sizes: number[] = [];
  private readonly pins: number[] = [];
  private readonly held: Held = { bytes: 0, pinned: 0 };
  // The first chunk of the line still arriving, or -1. It and every
  // chunk after it hold one pin for that line.
  private openFirst = -1;
  private source: PatchTextSource | null = null;
  private readonly reloads = new Map<number, Promise<void>>();
  // Reads of evicted chunks run one at a time, so reads in patch order
  // continue one backend stream.
  private reloadChain: Promise<unknown> = Promise.resolve();
  private lastRead = -1;
  private evicted = false;
  private disposed = false;
  private lost: PatchTextLost | null = null;
  private lostListeners: ((err: PatchTextLost) => void)[] = [];

  constructor() {
    heldText.register(this, this.held, this);
  }

  get count(): number {
    return this.texts.length;
  }

  /** Adds a chunk and returns its number. A chunk with a `range` can be
   * evicted once indexed; one with none is never. `counted` false keeps
   * text built in memory off the budget, pinned or not. */
  push(data: string, packed: boolean, range: { offset: number; nextOffset: number } | undefined, counted: boolean): number {
    this.texts.push(data);
    this.packed.push(packed);
    this.lengths.push(data.length);
    this.offsets.push(range?.offset ?? -1);
    this.nextOffsets.push(range?.nextOffset ?? -1);
    const size = !counted ? 0 : packed ? data.length : textBytes(data);
    this.sizes.push(size);
    this.pins.push(0);
    this.hold(size);
    return this.texts.length - 1;
  }

  /** The last chunk is indexed: the line still arriving starts in chunk
   * `openFirst`, or none is (-1). Its chunks stay until it ends. */
  indexed(openFirst: number): void {
    const last = this.texts.length - 1;
    // A line still arriving either continues into the last chunk or
    // starts in it.
    if (this.openFirst >= 0 && this.openFirst !== openFirst) {
      for (let chunk = this.openFirst; chunk < last; chunk += 1) this.addPin(chunk, -1);
    }
    if (openFirst >= 0) this.addPin(last, 1);
    this.openFirst = openFirst;
    if (this.source && this.offsets[last] >= 0) {
      patchMemory.offer(this, last, true);
      patchMemory.enforce();
    }
  }

  /** The patch ended, its last line complete. When no chunk was evicted,
   * every chunk stays resident and the source is released. */
  ended(): void {
    if (this.openFirst >= 0) {
      for (let chunk = this.openFirst; chunk < this.texts.length; chunk += 1) this.addPin(chunk, -1);
      this.openFirst = -1;
    }
    const source = this.source;
    if (source && !this.evicted) {
      this.source = null;
      for (let chunk = 0; chunk < this.texts.length; chunk += 1) patchMemory.withdraw(this, chunk);
      source.release();
    }
    patchMemory.enforce();
  }

  isPacked(chunk: number): boolean {
    return this.packed[chunk];
  }

  /** A chunk's text, read by a reader: counts as a use of the chunk.
   * Throws PatchTextNotResident (naming `line`) when it is evicted. */
  read(chunk: number, line: number): string {
    const text = this.texts[chunk];
    if (text === null) throw new PatchTextNotResident(line);
    // Consecutive reads of one chunk are one use.
    if (chunk !== this.lastRead) {
      this.lastRead = chunk;
      patchMemory.touch(this, chunk);
    }
    return text;
  }

  /** A pinned chunk's text, read while indexing: not a use. */
  peek(chunk: number, line: number): string {
    const text = this.texts[chunk];
    if (text === null) throw new PatchTextNotResident(line);
    return text;
  }

  /** Whether chunks [first, last] are resident. */
  resident(first: number, last: number): boolean {
    for (let chunk = first; chunk <= last; chunk += 1) {
      if (this.texts[chunk] === null) return false;
    }
    return true;
  }

  /**
   * Reads the evicted chunks of [first, last] again. They can be evicted
   * again once they land unless pinned. Rejects with PatchTextLost when
   * the text can no longer be read.
   */
  load(first: number, last: number): Promise<void> {
    const pending: Promise<void>[] = [];
    for (let chunk = first; chunk <= last; chunk += 1) {
      if (this.texts[chunk] === null) pending.push(this.reload(chunk));
    }
    if (pending.length === 0) return Promise.resolve();
    return Promise.all(pending).then(() => undefined);
  }

  /** Keeps chunks [first, last] from eviction until the returned function
   * runs. */
  pin(first: number, last: number): () => void {
    for (let chunk = first; chunk <= last; chunk += 1) this.addPin(chunk, 1);
    let released = false;
    return () => {
      if (released) return;
      released = true;
      for (let chunk = first; chunk <= last; chunk += 1) this.addPin(chunk, -1);
      patchMemory.enforce();
    };
  }

  /** Makes every chunk with a range, held now or appended later,
   * evictable: it can be read again from `source`. */
  attachSource(source: PatchTextSource): void {
    this.source = source;
    this.lastRead = -1;
    for (let chunk = 0; chunk < this.texts.length; chunk += 1) {
      if (this.texts[chunk] !== null && this.offsets[chunk] >= 0) patchMemory.offer(this, chunk, true);
    }
    patchMemory.enforce();
  }

  /** Runs `listener` once evicted text can no longer be read, at once when
   * that already happened. */
  whenLost(listener: (err: PatchTextLost) => void): void {
    if (this.lost) listener(this.lost);
    else this.lostListeners.push(listener);
  }

  /** Stops counting the text and releases the source. Text still held
   * stays readable; evicted text is lost. */
  dispose(): void {
    if (this.disposed) return;
    patchMemory.pin(-this.held.pinned);
    this.held.pinned = 0;
    patchMemory.remove(this.held.bytes);
    this.held.bytes = 0;
    this.disposed = true;
    this.lostListeners = [];
    for (let chunk = 0; chunk < this.texts.length; chunk += 1) patchMemory.withdraw(this, chunk);
    heldText.unregister(this);
    const source = this.source;
    this.source = null;
    source?.release();
  }

  pinned(chunk: number): boolean {
    return this.pins[chunk] > 0 || this.reloads.has(chunk);
  }

  evict(chunk: number): number {
    if (this.texts[chunk] === null) return 0;
    this.texts[chunk] = null;
    this.evicted = true;
    if (chunk === this.lastRead) this.lastRead = -1;
    const size = this.sizes[chunk];
    this.held.bytes -= size;
    return size;
  }

  // The one writer of pin counts and of the pinned sum: a chunk counts
  // while it is resident and holds a pin.
  private addPin(chunk: number, delta: 1 | -1): void {
    const before = this.pins[chunk];
    const after = before + delta;
    this.pins[chunk] = after;
    if ((before === 0) !== (after === 0) && this.texts[chunk] !== null) this.countPinned(after === 0 ? -this.sizes[chunk] : this.sizes[chunk]);
  }

  private countPinned(delta: number): void {
    if (this.disposed) return;
    this.held.pinned += delta;
    patchMemory.pin(delta);
  }

  private hold(bytes: number): void {
    if (this.disposed) return;
    this.held.bytes += bytes;
    patchMemory.add(bytes);
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
    const length = this.lengths[chunk];
    const read = this.reloadChain
      .then(() => {
        if (this.disposed) throw new PatchTextLost('patch text: the diff was closed');
        return source.read(offset, next, packed);
      })
      .then((answer) => {
        // The line index and wide bits stay from the first read, so the
        // answer must be that chunk in that form.
        if (answer.nextOffset !== next || answer.packed !== packed || answer.data.length !== length) {
          throw new PatchTextLost('patch text: the diff no longer serves the same chunk');
        }
        if (this.disposed || this.texts[chunk] !== null) return;
        this.texts[chunk] = answer.data;
        this.hold(this.sizes[chunk]);
        if (this.pins[chunk] > 0) this.countPinned(this.sizes[chunk]);
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
}
