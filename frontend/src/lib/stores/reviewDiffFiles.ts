import { GetEditDiffImage, GetReviewDiffFileSizes, GetReviewDiffImage } from './bindings';
import { withBackendTarget } from '../transport/backends';
import type { BackendKey } from '../transport/backendKey';
import type { WorkspaceRef } from '../types/git';
import type { ReviewFile } from '../utils/patchStore';
import type { PRRevision } from '../utils/prRevision';

// A review diff's files read by the diff they belong to: their two sides'
// byte sizes and, for an image, the bytes behind a single-use URL
// (docs/architecture/review-diff-streaming.md, "Reading a diff's files").
// The one place that names a file to the backend: every call to
// GetReviewDiffFileSizes, GetReviewDiffImage and GetEditDiffImage, the
// arguments they take, and what a refusal means for the row, are decided
// here from the opened diff and a file of it.

/** What the backend's open of a diff says its file reads name. */
export interface OpenedDiffFiles {
  /** The source the Open call took. */
  source: GitDiffSource;
  /** Why no file of this diff can be read (the open could not pin its
   * sides), as one sentence; null when they can. */
  unavailable: string | null;
}

/** The diff a pane has on screen, as it was opened. */
export type DiffFilesTarget =
  | {
    kind: 'git';
    backend: BackendKey;
    workspace: WorkspaceRef;
    opened: OpenedDiffFiles;
  }
  | {
    kind: 'edits';
    backend: BackendKey;
    threadId: string;
    editPayloadId: string;
    editTurnIndex: number;
    /** The file's patch text, which the backend verifies the bytes by. */
    verifyPatch: (file: ReviewFile) => Promise<string>;
  };

/** `ReviewDiffSource`: workspace, branch, commit, or pr with its revision
 * and run (both ends empty for the whole PR). */
export type GitDiffSource =
  | { scope: 'workspace' }
  | { scope: 'branch'; baseBranch: string }
  | { scope: 'commit'; commitSha: string }
  | { scope: 'pr'; revision: PRRevision; firstSha?: string; lastSha?: string };

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
  /** The bytes cannot be served, with the one-sentence reason. */
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

const ZERO_OID = '0000000';

/** Whether the target can read sizes: every scope but edits. */
export function diffFilesReadSizes(target: DiffFilesTarget): boolean {
  return target.kind === 'git';
}

/** Why `file`'s sides cannot be read in this diff, or null when they can:
 * its row is the ordinary binary row with this sentence. */
export function diffFileUnreadable(target: DiffFilesTarget, _file: ReviewFile): string | null {
  return target.kind === 'git' ? target.opened.unavailable : null;
}

/** What a refused read offers on its row: a retry of the same read (the
 * backend was busy), or a reload of the diff (the file changed since the
 * diff was read, so the same read would be refused again). */
export type DiffReadAction = 'retry' | 'reload';

export function diffReadFailure(err: unknown): { reason: string; action: DiffReadAction } {
  const reason = err instanceof Error ? err.message : String(err);
  return { reason, action: /changed since the diff was read/i.test(reason) ? 'reload' : 'retry' };
}

// The file as its patch names it: paths and the index line's ids, all
// zeros for a side that does not exist, both empty with no index line.
function fileRef(file: ReviewFile) {
  const header = file.header;
  const oldPath = header?.oldPath ?? undefined;
  if (!header?.indexed) return { path: file.path, oldPath, oldOid: '', newOid: '' };
  return { path: file.path, oldPath, oldOid: header.oldBlob ?? ZERO_OID, newOid: header.newBlob ?? ZERO_OID };
}

function wireSource(opened: OpenedDiffFiles) {
  return { ...opened.source };
}

/** The sizes of up to DIFF_FILE_SIZES_BATCH files, by path. A failure to
 * resolve the diff rejects; a file's own failure is its `error`. */
export async function readDiffFileSizes(target: DiffFilesTarget, files: readonly ReviewFile[]): Promise<Map<string, DiffFileSizes>> {
  if (target.kind !== 'git') throw new Error('An edit\'s file sizes are not recorded.');
  if (files.length > DIFF_FILE_SIZES_BATCH) throw new Error(`At most ${DIFF_FILE_SIZES_BATCH} files per size read.`);
  const answers = (await withBackendTarget(target.backend, () =>
    GetReviewDiffFileSizes(target.workspace, wireSource(target.opened), files.map(fileRef)))) ?? [];
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

/** Both sides of an image file (edits: the new side only). */
export async function readDiffImage(target: DiffFilesTarget, file: ReviewFile): Promise<DiffImageAnswer> {
  const answer = target.kind === 'git'
    ? await withBackendTarget(target.backend, () => GetReviewDiffImage(target.workspace, wireSource(target.opened), fileRef(file)))
    : await withBackendTarget(target.backend, async () => GetEditDiffImage(target.threadId, {
      scope: 'edits',
      commitSHA: '',
      headSHA: '',
      path: file.path,
      startLine: 0,
      endLine: 0,
      verifyPatch: await target.verifyPatch(file),
      editPayloadId: target.editPayloadId,
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

/**
 * The media cache key of one side's bytes: its blob id where the patch
 * names one, so every view of the same bytes shares them; else the file
 * in this diff (`scopeKey` names the diff).
 */
export function diffImageBlobKey(target: DiffFilesTarget, scopeKey: string, file: ReviewFile, side: 'old' | 'new'): string {
  const header = file.header;
  const oid = side === 'old' ? header?.oldBlob : header?.newBlob;
  // An abbreviated id is unique within its repository only.
  if (target.kind === 'git' && header?.indexed && oid) return JSON.stringify(['review-image', target.backend, target.workspace.projectId, oid]);
  return JSON.stringify(['review-image', target.backend, scopeKey, file.path, side]);
}
