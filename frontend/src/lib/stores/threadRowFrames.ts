// Which thread:updated frames reached this client since an RPC started, so
// the row the RPC returns does not undo them.
//
// The backend reads an RPC's returned row before that write's own frame
// leaves, and other writers' frames can arrive between the call and its
// reply. A frame that arrived after the call started is at least as new as
// anything the client knew when it sent the call, and every later write
// announces its own frame, so the client converges by keeping what the
// frame set: a row frame supersedes the whole reply, a patch the fields it
// names. This is a leaf: it imports no store.
import type { Thread } from '../types/models';

type PatchedField = 'title' | 'model' | 'sessionRef';

const PATCHED_FIELDS: readonly PatchedField[] = ['title', 'model', 'sessionRef'];

interface ThreadFrames {
  /** Sequence of the last frame carrying the whole row. */
  row: number;
  /** Sequence of the last patch naming each field. */
  fields: Partial<Record<PatchedField, number>>;
}

let sequence = 0;
// One entry per thread a row-changing frame named, dropped when the thread
// is deleted: a deleted row has no surface left for a reply to reach.
const framesByThread = new Map<string, ThreadFrames>();

/** The point an RPC starts from. Take it before sending the call. */
export function threadRowFrameMark(): number {
  return sequence;
}

/** Records a frame carrying the thread's whole row (full, listed, unlisted). */
export function noteThreadRowFrame(threadId: string): void {
  const frames = framesFor(threadId);
  frames.row = ++sequence;
}

/** Records a patch frame. An activity-only patch changes no row field. */
export function noteThreadPatchFrame(threadId: string, patch: Partial<Record<PatchedField, unknown>>): void {
  let frames: ThreadFrames | undefined;
  for (const field of PATCHED_FIELDS) {
    if (patch[field] === undefined) continue;
    frames ??= framesFor(threadId);
    frames.fields[field] = ++sequence;
  }
}

/** Drops a deleted thread's record. */
export function forgetThreadRowFrames(threadId: string): void {
  framesByThread.delete(threadId);
}

/**
 * The row to apply for `reply`, an RPC answer for a call started at `mark`:
 * null when a whole-row frame arrived since, otherwise `reply` with each
 * field a later patch named taken from `current`, the row the client holds.
 */
export function threadReplyRow(reply: Thread, mark: number, current: Thread | null | undefined): Thread | null {
  const frames = framesByThread.get(reply.id);
  if (!frames) return reply;
  if (frames.row > mark) return null;
  if (!current) return reply;
  const kept: Partial<Pick<Thread, PatchedField>> = {};
  for (const field of PATCHED_FIELDS) {
    if ((frames.fields[field] ?? 0) > mark) Object.assign(kept, { [field]: current[field] });
  }
  return Object.keys(kept).length === 0 ? reply : { ...reply, ...kept };
}

function framesFor(threadId: string): ThreadFrames {
  let frames = framesByThread.get(threadId);
  if (!frames) {
    frames = { row: 0, fields: {} };
    framesByThread.set(threadId, frames);
  }
  return frames;
}
