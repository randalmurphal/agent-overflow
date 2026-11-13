import { GetEditDiffImage, GetReviewDiffFileSizes, GetReviewDiffImage } from './bindings';
import { withBackendTarget } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import type { BackendKey } from '../transport/backendKey';
import type { WorkspaceRef } from '../types/git';
import type { ReviewFile } from '../utils/patchStore';

// A review diff's files read by the diff they belong to: their two sides'
// byte sizes and, for an image, the bytes behind a single-use URL
// (docs/architecture/review-diff-streaming.md, "Reading a diff's files").
// The one place that names a file to the backend: every call to
// GetReviewDiffFileSizes, GetReviewDiffImage and GetEditDiffImage, the
// arguments they take, and what a refusal means for the row, are decided
// here. A git diff's reads name the handle its open answered and the
// file's paths; the client holds no object id.

/** A git diff's file handle: the server's record of what its open
 * pinned. The server forgets it when it is evicted or the connection that
 * opened it ends. An open of immutable revisions (`reopen` given) is
 * opened once more for a new handle, which names the same bytes; any other
 * open read the working tree, which may have moved since, so a forgotten
 * handle stays gone and its rows offer reloading the diff. It belongs to
 * one open of one diff: once that diff leaves the screen (`close`),
 * nothing reopens it. */
export class DiffFilesHandle {
  private reopening: Promise<string> | null = null;
  private closed = false;
  /** The refusal that ended this handle for good, answered to every
   * later read without asking the server. */
  private ended: unknown = null;
  /** A handle whose connection ended, reopened before its next read. */
  private lapsed: string | null = null;
  /** Runs with the handle a reopen replaced. */
  onRenewed: ((stale: string) => void) | null = null;

  constructor(
    private opened: string,
    /** Opens the same immutable revisions again for a new handle (the
     * patch is not read); null for an open that read the working tree. */
    private readonly reopen: (() => Promise<string>) | null,
  ) {}

  get current(): string {
    return this.opened;
  }

  /** Whether a forgotten handle is opened again. */
  get reopens(): boolean {
    return this.reopen !== null;
  }

  /** The refusal every read of this handle answers now, if it ended. */
  get endedBy(): unknown {
    return this.ended;
  }

  /** `refusal` (gone) answered a read on `stale`: answers the handle to
   * read again on, the one another read already reopened for or one
   * reopen shared by every read waiting on it, or rejects with `refusal`
   * when this handle is not reopened (or was already, and that one went
   * too). */
  renew(stale: string, refusal: unknown): Promise<string> {
    if (this.closed) return Promise.reject(new Error('The diff is no longer shown.'));
    if (this.ended !== null) return Promise.reject(this.ended);
    if (this.opened !== stale) return Promise.resolve(this.opened);
    const reopen = this.reopen;
    if (!reopen) {
      this.ended = refusal;
      return Promise.reject(refusal);
    }
    this.reopening ??= reopen().then(
      (next) => {
        this.opened = next;
        this.reopening = null;
        this.onRenewed?.(stale);
        return next;
      },
      (err: unknown) => {
        this.reopening = null;
        throw err;
      },
    );
    return this.reopening;
  }

  /** The reopened handle went too: this diff's files are not read again. */
  end(refusal: unknown): void {
    this.ended ??= refusal;
  }

  /** The connection that opened this handle ended, and the server drops
   * every handle with its connection: the next read reopens first, or,
   * for a handle that does not reopen, no read is made. */
  connectionEnded(): void {
    if (this.closed || this.ended !== null) return;
    if (!this.reopen) this.ended = new TransportError(DIFF_FILES_GONE, FILES_GONE_SENTENCE);
    else this.lapsed = this.opened;
  }

  /** The handle a read names: a lapsed one is reopened first, and a
   * read that starts while a reopen is in flight waits for its handle. */
  async readable(): Promise<string> {
    if (this.lapsed === this.opened) {
      const lapsed = this.lapsed;
      this.lapsed = null;
      return this.renew(lapsed, new TransportError(DIFF_FILES_GONE, FILES_GONE_SENTENCE));
    }
    return this.reopening ?? this.opened;
  }

  /** The diff left the screen. */
  close(): void {
    this.closed = true;
  }
}

/** The diff a pane has on screen, as it was opened. */
export type DiffFilesTarget =
  | {
    kind: 'git';
    backend: BackendKey;
    workspace: WorkspaceRef;
    files: DiffFilesHandle;
  }
  | {
    kind: 'edits';
    backend: BackendKey;
    threadId: string;
    /** The edit read, or '' for a turn's edits. */
    editPayloadId: string;
    /** The turn read, or -1 for one edit. */
    editTurnIndex: number;
  };

export interface DiffFileSide {
  exists: boolean;
  size: number;
}

export interface DiffFileSizes {
  old: DiffFileSide;
  new: DiffFileSide;
  /** This file's read failed (the others' stand). */
  error?: string;
}

export interface DiffImageSide extends DiffFileSide {
  /** The single-use address of the bytes; absent when they are not served. */
  url?: string;
  mimeType?: string;
  overCap?: boolean;
  notImage?: boolean;
  /** The bytes cannot be served, with the backend's one sentence (Git
   * LFS, a git filter, an edit that recorded no content). */
  unavailable?: string;
}

export interface DiffImageAnswer {
  old: DiffImageSide;
  new: DiffImageSide;
  /** The per-side cap the backend applied. */
  maxBytes: number;
}

/** Size batches the backend takes per call. */
export const DIFF_FILE_SIZES_BATCH = 100;

/** The file reads' refusals, by code: a handle this connection does not
 * hold (evicted, or opened on a connection that has ended), and a diff
 * whose files the server could not pin, which reopening would not change. */
export const DIFF_FILES_GONE = 'review_diff_files_gone';
export const DIFF_FILES_UNREADABLE = 'review_diff_files_unreadable';
// The server's sentence for the gone refusal, said here when the client
// knows the handle went with its connection without asking.
const FILES_GONE_SENTENCE = 'This diff\'s files are no longer held; reload the diff.';

function refusalCode(err: unknown): string | undefined {
  return err instanceof TransportError ? err.code : undefined;
}

/** Whether the target can read sizes: every scope but edits. */
export function diffFilesReadSizes(target: DiffFilesTarget): boolean {
  return target.kind === 'git';
}

/** What a refused read offers on its row: a retry of the same read (the
 * backend was busy), or a reload of the diff (the file changed since the
 * diff was read, or the server no longer holds the diff's files even
 * after one reopen, so the same read would be refused again). */
export type DiffReadAction = 'retry' | 'reload';

// The backend gives a working-tree file that changed since the open no
// code of its own; its sentence is all there is (a non-loopback caller
// gets it redacted, and Retry then).
const CHANGED_SINCE_READ = /changed since the diff was read/i;

/** A refused read's sentence and what its row offers. Unreadable files
 * offer nothing: every read of the diff answers the same. */
export function diffReadFailure(err: unknown): { reason: string; action: DiffReadAction | null } {
  const reason = err instanceof Error ? err.message : String(err);
  const code = refusalCode(err);
  if (code === DIFF_FILES_UNREADABLE) return { reason, action: null };
  if (code === DIFF_FILES_GONE || CHANGED_SINCE_READ.test(reason)) return { reason, action: 'reload' };
  return { reason, action: 'retry' };
}

function fileRef(file: ReviewFile) {
  const oldPath = file.header?.oldPath ?? undefined;
  return oldPath ? { path: file.path, oldPath } : { path: file.path };
}

// One read on the diff's handle. A handle the server no longer holds is
// reopened once and the read made once more; a second refusal is the
// caller's. Never more.
async function onHandle<T>(target: Extract<DiffFilesTarget, { kind: 'git' }>, read: (handle: string) => Promise<T>): Promise<T> {
  const files = target.files;
  if (files.endedBy !== null) throw files.endedBy;
  const first = await files.readable();
  try {
    return await withBackendTarget(target.backend, () => read(first));
  } catch (err) {
    if (refusalCode(err) !== DIFF_FILES_GONE) throw err;
    const next = await files.renew(first, err);
    try {
      return await withBackendTarget(target.backend, () => read(next));
    } catch (again) {
      if (refusalCode(again) === DIFF_FILES_GONE) files.end(again);
      throw again;
    }
  }
}

/** The sizes of up to DIFF_FILE_SIZES_BATCH files, by path. A failure to
 * read the diff rejects; a file's own failure is its `error`. */
export async function readDiffFileSizes(target: DiffFilesTarget, files: readonly ReviewFile[]): Promise<Map<string, DiffFileSizes>> {
  if (target.kind !== 'git') throw new Error('An edit\'s file sizes are not recorded.');
  if (files.length > DIFF_FILE_SIZES_BATCH) throw new Error(`At most ${DIFF_FILE_SIZES_BATCH} files per size read.`);
  const answers = (await onHandle(target, (handle) => GetReviewDiffFileSizes(target.workspace, handle, files.map(fileRef)))) ?? [];
  const out = new Map<string, DiffFileSizes>();
  for (const answer of answers) {
    out.set(answer.path, {
      old: { exists: answer.old.exists, size: answer.old.size },
      new: { exists: answer.new.exists, size: answer.new.size },
      ...(answer.error ? { error: answer.error } : {}),
    });
  }
  return out;
}

/** Both sides of an image file (an edit's old side is never recorded). */
export async function readDiffImage(target: DiffFilesTarget, file: ReviewFile): Promise<DiffImageAnswer> {
  const answer = target.kind === 'git'
    ? await onHandle(target, (handle) => GetReviewDiffImage(target.workspace, handle, fileRef(file)))
    : await withBackendTarget(target.backend, () => GetEditDiffImage(target.threadId, {
      path: file.path,
      ...(target.editPayloadId ? { editPayloadId: target.editPayloadId } : {}),
      editTurnIndex: target.editTurnIndex,
    }));
  return { old: { ...answer.old }, new: { ...answer.new }, maxBytes: answer.maxBytes };
}

// The transfer client loads with the first image read, which keeps it out
// of the review state's module graph (every thread pane's).
let transfer: Promise<typeof import('../transport/attachmentTransfer')> | null = null;

/** One side's bytes behind the single-use URL an image answer gave. */
export async function fetchDiffImageBytes(target: DiffFilesTarget, url: string): Promise<Blob> {
  transfer ??= import('../transport/attachmentTransfer');
  return (await transfer).fetchTicketedBytes(target.backend, url, 'Could not load image');
}

/** The identity of the open whose files `target` reads, as the media
 * cache keys its bytes: the files handle (one per open, never reused) or
 * the edit read. Two diffs, or two panes, never share one; a reopen mints
 * a new one. */
export function diffFilesIdentity(target: DiffFilesTarget): string {
  return target.kind === 'git'
    ? gitFilesIdentity(target.backend, target.files.current)
    : JSON.stringify(['review-image', target.backend, 'edit', target.threadId, target.editPayloadId, target.editTurnIndex]);
}

/** The identity of a git diff's open by its files handle. */
export function gitFilesIdentity(backend: BackendKey, handle: string): string {
  return JSON.stringify(['review-image', backend, 'git', handle]);
}

/** The prefix of every cache key of the open `identity` names. */
export function diffImageBlobPrefix(identity: string): string {
  return identity.slice(0, -1) + ',';
}

/** The media cache key of one side's bytes in the open `target` is. */
export function diffImageBlobKey(target: DiffFilesTarget, file: ReviewFile, side: 'old' | 'new'): string {
  return diffImageBlobPrefix(diffFilesIdentity(target)) + JSON.stringify([file.path, side]).slice(1);
}
