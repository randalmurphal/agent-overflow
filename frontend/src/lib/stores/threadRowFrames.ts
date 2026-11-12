// Which thread:updated frames reached this client since an RPC started, so
// the row the RPC returns does not undo them.
//
// The backend announces a write before the RPC that made it returns, and
// the transport writes every event queued for a connection ahead of that
// connection's reply. So the frame of any write whose reply this client
// already had arrived before that reply, and a frame arriving after a call
// started belongs to a write made during or after the call: a write by this
// call itself, by another client, or by background work. Every later write
// announces its own frame, so the client converges by keeping what the
// frame set: a row frame supersedes the whole reply, a patch the fields it
// names. One exception: during reconnect replay the WS client holds live
// frames until replay completes, so a reply can be handled before a frame
// the backend sent ahead of it. This is a leaf: it imports no store.
//
// One reply is known to be newer than a row frame from its own call: a fork
// is announced as preparing while the call runs and as ready when it ends,
// and the answer can overtake the ready frame. Readiness only moves forward,
// so an answer that says ready replaces a held row that says preparing.
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
 * Whether a frame carrying the thread's whole row arrived after `mark`, so a
 * reply from a call started at `mark` is older than the row the client holds.
 */
export function threadRowFramedSince(threadId: string, mark: number): boolean {
  return (framesByThread.get(threadId)?.row ?? 0) > mark;
}

/**
 * The row to apply for `reply`, an RPC answer for a call started at `mark`:
 * null when a whole-row frame arrived since, otherwise `reply` with each
 * field a later patch named taken from `current`, the row the client holds.
 * A whole-row frame that left `current` a preparing fork does not hold back
 * an answer that says the fork is ready.
 */
export function threadReplyRow(reply: Thread, mark: number, current: Thread | null | undefined): Thread | null {
  const frames = framesByThread.get(reply.id);
  if (!frames) return reply;
  const readied = !!current?.forkPreparing && !reply.forkPreparing;
  if (frames.row > mark && !readied) return null;
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
