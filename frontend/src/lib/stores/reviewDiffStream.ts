import { ReadReviewDiff, ReadReviewDiffPacked, ReleaseReviewDiff } from './bindings';
import { getTransportHelloFor } from './transportStatus.svelte';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { PatchParser, type PatchChunkSpan, type PatchTextKey, type PatchTextLost, type PatchTextSource } from '../utils/patchStore';

// A review diff read from the backend in chunks. An Open*Diff call returns
// the first chunk and, when the patch continues, a handle that only the
// connection that opened it can read; every call on it is pinned to that
// computer. The handle is released when the read is abandoned, and at the
// end of the patch when all of its text fit the memory budget; otherwise
// the read's store keeps it to read evicted text again until the read is
// disposed.
//
// A backend that advertises REVIEW_DIFF_PACKED serves the chunks after
// the open's through ReadReviewDiffPacked, which packs a chunk
// (utils/packedText.ts) where characters past ASCII are sparse in it: the
// page then holds it at its UTF-8 size. An evicted chunk is read again in
// the form it arrived in.

/** Bytes asked of each ReadReviewDiff (the backend clamps to its bounds). */
export const REVIEW_DIFF_READ_BYTES = 2 * 1024 * 1024;

// The backend serves ReadReviewDiffPacked (internal/transport/frame.go).
const REVIEW_DIFF_PACKED = 'review-diff.packed.v1';

export interface ReviewDiffChunk {
  data: string;
  offset: number;
  nextOffset: number;
  eof: boolean;
  /** Set on a chunk ReadReviewDiffPacked packed, with its wide-line
   * marks (PatchChunkSpan). */
  packed?: boolean;
  wide?: number[];
  /** The patch text's source key through the chunk's end
   * (PatchChunkSpan). A backend older than the key answers none. */
  key?: PatchTextKey;
}

// The span a chunk is stored under: its byte range, its source key, and
// its marks when it is packed.
function chunkSpan(chunk: ReviewDiffChunk): PatchChunkSpan {
  const span: PatchChunkSpan = { offset: chunk.offset, nextOffset: chunk.nextOffset };
  if (chunk.key !== undefined) span.key = chunk.key;
  if (chunk.packed !== true) return span;
  // The backend packs only text holding a byte past ASCII, so a packed
  // chunk has a mark; without them its wide lines would read as bytes.
  if (!chunk.wide?.length) throw new Error('review diff: a packed chunk came without its wide marks');
  // The packed read and the key ship together, and a packed chunk is not
  // hashed here.
  if (chunk.key === undefined) throw new Error('review diff: a packed chunk came without its key');
  span.wide = chunk.wide;
  return span;
}

/** The answer of an Open*Diff call. */
export interface ReviewDiffOpened {
  id: string;
  chunk: ReviewDiffChunk;
  /** Edits diffs: the payloads joined, in patch order. */
  payloadIds?: string[];
  /** A git diff's handle for its file reads (every git-backed open
   * answers one; an edits diff none). */
  filesHandle?: string;
}

export interface ReviewDiffRead {
  /** The whole patch, ended. */
  parser: PatchParser;
  /** Runs once when evicted text of the patch can no longer be read. */
  onLost(listener: (err: PatchTextLost) => void): void;
  /** Releases the handle, if the read kept it, and the text's budget. */
  dispose(): void;
  /** The files handle the open this patch came from answered ('' for
   * an edits diff). */
  files: string;
}

export interface ReviewDiffReadOptions {
  /** Checked between chunks; a true answer stops the read. */
  cancelled: () => boolean;
  /** Runs after each chunk but the last, with the parser being filled. A
   * retry fills a new parser; the parser a failed or cancelled read
   * filled is disposed. */
  onProgress?: (parser: PatchParser) => void;
  /** Runs when an Open answers, before the rest is read. A retry opens
   * again. */
  onOpened?: (opened: ReviewDiffOpened) => void;
}

/** One diff the review surface can read: which computer serves it and the
 * Open*Diff call that opens it. */
export class ReviewDiffSource {
  private first: Promise<ReviewDiffOpened> | null = null;

  constructor(
    readonly backend: BackendKey,
    private readonly openDiff: () => Promise<ReviewDiffOpened>,
    /** The open's inputs are immutable revisions (a PR revision, or
     * commits by sha), so opening it again answers the same diff and its
     * file handle names the same bytes. False for an open that reads the
     * working tree or resolves a branch name. */
    readonly revisionPinned = false,
  ) {}

  /** Sends the Open call now, so it overlaps the caller's other requests. */
  start(): this {
    this.first ??= this.open();
    return this;
  }

  /** Gives up a source that will not be read, releasing the handle a
   * started Open returns. */
  abandon(): void {
    const first = this.first;
    this.first = null;
    first?.then((opened) => this.release(opened.id), () => {});
  }

  /**
   * Reads the whole patch. Returns null when `cancelled` stops it. A read
   * whose handle was lost (the connection that opened it ended) or whose
   * diff stopped verifying is retried once from a fresh Open.
   */
  async read(options: ReviewDiffReadOptions): Promise<ReviewDiffRead | null> {
    try {
      return await this.readOnce(options);
    } catch (err) {
      if (!retryableReadError(err) || options.cancelled()) throw err;
      return this.readOnce(options);
    }
  }

  private async readOnce({ cancelled, onProgress, onOpened }: ReviewDiffReadOptions): Promise<ReviewDiffRead | null> {
    const pending = this.first ?? this.open();
    this.first = null;
    const opened = await pending;
    const id = opened.id;
    // Decided once the open answered, over the connection the handle
    // belongs to.
    const packed = this.servesPacked();
    const parser = new PatchParser();
    const store = parser.store;
    // The store owns the handle from here: it releases it on dispose.
    if (id) store.attachSource(this.textSource(id));
    let read: ReviewDiffRead | null = null;
    try {
      onOpened?.(opened);
      let chunk = opened.chunk;
      for (;;) {
        if (cancelled()) return null;
        parser.append(chunk.data, chunkSpan(chunk));
        if (chunk.eof) break;
        if (!id) throw new Error('review diff: the backend returned no handle for an unfinished diff');
        onProgress?.(parser);
        chunk = await this.readChunk(id, chunk.nextOffset, REVIEW_DIFF_READ_BYTES, packed);
      }
      parser.end();
      // All of the text is resident: nothing needs the handle again.
      if (!store.evictedAny) store.detachSource()?.release();
      read = {
        parser,
        onLost: (listener) => store.whenLost(listener),
        dispose: () => store.dispose(),
        files: opened.filesHandle ?? '',
      };
      return read;
    } finally {
      if (!read) store.dispose();
    }
  }

  /** Opens the diff again for a new file handle, reading none of its
   * patch (the server forgot the last one). */
  async reopenFiles(): Promise<string> {
    if (!this.revisionPinned) throw new Error('This diff reads the working tree; reload it to read its files again.');
    const opened = await this.open();
    this.release(opened.id);
    return opened.filesHandle ?? '';
  }

  // Reads an evicted chunk again: the same offset and a window no larger
  // than the first read's cut the same chunk (gitdiff.ChunkCut).
  private textSource(id: string): PatchTextSource {
    return {
      // A chunk that arrived as text reads the same from either method.
      read: async (offset, nextOffset, packed) => {
        const chunk = await this.readChunk(id, offset, nextOffset - offset, packed);
        return { data: chunk.data, nextOffset: chunk.nextOffset, packed: chunk.packed === true };
      },
      release: () => this.release(id),
    };
  }

  private servesPacked(): boolean {
    return getTransportHelloFor(this.backend)?.capabilities.includes(REVIEW_DIFF_PACKED) ?? false;
  }

  // `packed` asks ReadReviewDiffPacked, which answers each chunk in the
  // form that is cheaper to hold.
  private readChunk(id: string, offset: number, maxBytes: number, packed: boolean): Promise<ReviewDiffChunk> {
    return withBackendTarget(this.backend, () => (packed ? ReadReviewDiffPacked(id, offset, maxBytes) : ReadReviewDiff(id, offset, maxBytes)));
  }

  private open(): Promise<ReviewDiffOpened> {
    return withBackendTarget(this.backend, this.openDiff);
  }

  private release(id: string): void {
    if (!id) return;
    withBackendTarget(this.backend, () => ReleaseReviewDiff(id)).catch((err: unknown) => {
      // The backend closes the diff with its connection, so a failed
      // release holds it at most until then.
      reportFrontendDiagnostic('review diff: release failed', err instanceof Error ? err.message : String(err));
    });
  }
}

function retryableReadError(err: unknown): boolean {
  const message = err instanceof Error ? err.message : String(err);
  return message.includes('review diff: not open')
    || message.includes('the diff changed since it was opened');
}
