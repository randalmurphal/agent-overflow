import { buildPRLineIndex, resolvePRNewLine, prIndexHasAnchor, type PRLine, type PRLineIndex } from '../utils/prLineIndex';
import type { ReviewFile } from '../utils/patchStore';
import type { RowAnchor } from '../utils/patchRows';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';

// The whole PR's diff, as a hunk index, for one review pane while it shows
// a commit run ending at the PR's head (utils/prLineIndex.ts). It belongs
// to the revision and whitespace mode it was read at (its key): opening
// another key drops it, a read answering for a key no longer open is
// dropped, and closing the view drops it. Built from the whole diff the
// pane already holds when it has it, otherwise read once.

export type PRLineIndexState = 'closed' | 'loading' | 'ready' | 'failed';

export interface ReviewPRLineIndex {
  readonly state: PRLineIndexState;
  /** The read's failure sentence while `failed`. */
  readonly error: string | null;
  /** The key of the index open (or being read), or null. */
  readonly key: string | null;
  /** Opens the index for `key`, reading it with `read` unless it is open
   * or seeded for that key already. */
  open(key: string, read: () => Promise<PRLineIndex>): void;
  /** Builds the index for `key` from the whole diff's files on hand. */
  seed(key: string, files: readonly ReviewFile[]): void;
  /** Reads `key` again after a failure. */
  retry(): void;
  close(): void;
  /** The whole diff's line for new line `line` of `path`; null when it is
   * not in a hunk, or the index is not ready. */
  resolveNewLine(path: string, line: number): PRLine | null;
  /** Whether a PR draft's anchor is a row of the whole diff; false when
   * the index is not ready. */
  hasAnchor(path: string, anchor: RowAnchor): boolean;
}

export function createReviewPRLineIndex(): ReviewPRLineIndex {
  let key = $state<string | null>(null);
  let index = $state.raw<PRLineIndex | null>(null);
  let state = $state<PRLineIndexState>('closed');
  let error = $state<string | null>(null);
  let reader: (() => Promise<PRLineIndex>) | null = null;
  // Each read's generation; an answer for an older one is dropped.
  let generation = 0;

  function start(): void {
    const read = reader;
    const asked = key;
    if (!read || asked === null) return;
    const mine = ++generation;
    state = 'loading';
    error = null;
    index = null;
    read().then(
      (built) => {
        if (mine !== generation || key !== asked) return;
        index = built;
        state = 'ready';
      },
      (err: unknown) => {
        if (mine !== generation || key !== asked) return;
        const message = err instanceof Error ? err.message : String(err);
        reportFrontendDiagnostic('review diff: the whole pull request diff could not be read for a commit run', message);
        error = message;
        state = 'failed';
      },
    );
  }

  return {
    get state() { return state; },
    get error() { return error; },
    get key() { return key; },
    open(next, read) {
      if (key === next && state !== 'closed') return;
      key = next;
      reader = read;
      start();
    },
    seed(next, files) {
      generation += 1;
      key = next;
      reader = null;
      index = buildPRLineIndex(files);
      state = 'ready';
      error = null;
    },
    retry() {
      if (state === 'failed') start();
    },
    close() {
      generation += 1;
      key = null;
      reader = null;
      index = null;
      state = 'closed';
      error = null;
    },
    resolveNewLine(path, line) {
      return index ? resolvePRNewLine(index, path, line) : null;
    },
    hasAnchor(path, anchor) {
      return index ? prIndexHasAnchor(index, path, anchor) : false;
    },
  };
}
