import { ReadReviewDiff, ReleaseReviewDiff } from './bindings';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { PatchParser } from '../utils/patchStore';

// A review diff read from the backend in chunks. An Open*Diff call returns
// the first chunk and, when the patch continues, a handle that only the
// connection that opened it can read; every call on it is pinned to that
// computer. The handle is released at the end of the patch or when the
// read is abandoned.

/** Bytes asked of each ReadReviewDiff (the backend clamps to its bounds). */
export const REVIEW_DIFF_READ_BYTES = 2 * 1024 * 1024;

export interface ReviewDiffChunk {
  data: string;
  offset: number;
  nextOffset: number;
  eof: boolean;
}

/** The answer of an Open*Diff call. */
export interface ReviewDiffOpened {
  id: string;
  chunk: ReviewDiffChunk;
  headSha?: string;
  /** Edits diffs: the payloads joined, in patch order. */
  payloadIds?: string[];
}

export interface ReviewDiffRead {
  /** The whole patch, ended. */
  parser: PatchParser;
  /** Pull request diffs: the head commit the diff was computed at. */
  headSha: string;
}

export interface ReviewDiffReadOptions {
  /** Checked between chunks; a true answer stops the read. */
  cancelled: () => boolean;
  /** Runs after each chunk but the last, with the parser being filled. A
   * retry fills a new parser. */
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
    try {
      onOpened?.(opened);
      const parser = new PatchParser();
      let chunk = opened.chunk;
      for (;;) {
        if (cancelled()) return null;
        parser.append(chunk.data);
        if (chunk.eof) break;
        if (!id) throw new Error('review diff: the backend returned no handle for an unfinished diff');
        onProgress?.(parser);
        const offset = chunk.nextOffset;
        chunk = await withBackendTarget(this.backend, () => ReadReviewDiff(id, offset, REVIEW_DIFF_READ_BYTES));
      }
      parser.end();
      return { parser, headSha: opened.headSha ?? '' };
    } finally {
      if (id) this.release(id);
    }
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
