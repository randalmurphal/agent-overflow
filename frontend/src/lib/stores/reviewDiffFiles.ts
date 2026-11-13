import { GetEditDiffImage, GetReviewDiffFileSizes, GetReviewDiffImage } from './bindings';
import { withBackendTarget } from '../transport/backends';
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

/** What a git diff's open answered for its file reads. */
export interface OpenedDiffFiles {
  /** The server's record of the open's pinned sides; '' when there is none. */
  handle: string;
  /** Why this diff's files cannot be read while the diff can (no handle). */
  unavailable: string | null;
}

/** A git diff's file handle, reopened once when the server no longer
 * holds it (it was evicted, or the connection that opened it ended). It
 * belongs to one open of one diff: once that diff leaves the screen
 * (`close`), nothing reopens it. */
export class DiffFilesHandle {
  private reopening: Promise<OpenedDiffFiles> | null = null;
  private closed = false;

  constructor(
    private opened: OpenedDiffFiles,
    /** Opens the same diff again for a new handle (the patch is not read). */
    private readonly reopen: () => Promise<OpenedDiffFiles>,
  ) {}

  get current(): OpenedDiffFiles {
    return this.opened;
  }

  /** The handle after `stale`'s: the one another read already reopened
   * for, else one reopen shared by every read waiting on it. */
  renew(stale: OpenedDiffFiles): Promise<OpenedDiffFiles> {
    if (this.closed) return Promise.reject(new Error('The diff is no longer shown.'));
    if (this.opened !== stale) return Promise.resolve(this.opened);
    this.reopening ??= this.reopen().then(
      (next) => {
        this.opened = next;
        this.reopening = null;
        return next;
      },
      (err: unknown) => {
        this.reopening = null;
        throw err;
      },
    );
    return this.reopening;
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

/** The backend's refusal of a handle it no longer holds. */
const FILES_GONE = /no longer held/i;

/** Whether the target can read sizes: every scope but edits. */
export function diffFilesReadSizes(target: DiffFilesTarget): boolean {
  return target.kind === 'git';
}

/** Why no file of this diff can be read, or null when they can: each
 * file's row says it in its image slot. */
export function diffFilesUnavailable(target: DiffFilesTarget): string | null {
  if (target.kind !== 'git') return null;
  const { handle, unavailable } = target.files.current;
  return handle ? null : (unavailable ?? 'This diff\'s files cannot be read.');
}

/** What a refused read offers on its row: a retry of the same read (the
 * backend was busy), or a reload of the diff (the file changed since the
 * diff was read, or the server no longer holds the diff's files even
 * after one reopen, so the same read would be refused again). */
export type DiffReadAction = 'retry' | 'reload';

// The server could not pin this diff's files: every read of it answers
// the same, so the row says why and offers nothing.
const FILES_UNREADABLE = /files cannot be read/i;

export function diffReadFailure(err: unknown): { reason: string; action: DiffReadAction | null } {
  const reason = err instanceof Error ? err.message : String(err);
  if (FILES_UNREADABLE.test(reason)) return { reason, action: null };
  return { reason, action: /changed since the diff was read|no longer held/i.test(reason) ? 'reload' : 'retry' };
}

function fileRef(file: ReviewFile) {
  const oldPath = file.header?.oldPath ?? undefined;
  return oldPath ? { path: file.path, oldPath } : { path: file.path };
}

// One read on the diff's handle. A handle the server no longer holds is
// reopened once and the read made once more; a second refusal is the
// caller's. Never more.
async function onHandle<T>(target: Extract<DiffFilesTarget, { kind: 'git' }>, read: (handle: string) => Promise<T>): Promise<T> {
  const first = target.files.current;
  if (!first.handle) throw new Error(first.unavailable ?? 'This diff\'s files cannot be read.');
  try {
    return await withBackendTarget(target.backend, () => read(first.handle));
  } catch (err) {
    if (!FILES_GONE.test(err instanceof Error ? err.message : String(err))) throw err;
    const next = await target.files.renew(first);
    if (!next.handle) throw err;
    return withBackendTarget(target.backend, () => read(next.handle));
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

/** The media cache key of one side's bytes: the file in this diff
 * (`scopeKey` names the open), which no other diff's bytes share. */
export function diffImageBlobKey(target: DiffFilesTarget, scopeKey: string, file: ReviewFile, side: 'old' | 'new'): string {
  return JSON.stringify(['review-image', target.backend, scopeKey, file.path, side]);
}
