// Whether a thread has a provider session live or starting, from
// `provider:session_live` and the `sessionLive` field of the computer's
// ListThreadLiveActivity snapshot (threadLiveActivity.ts), which restates it
// on every connection edge and after a gap. End Session reads it, so the
// menu never offers to end a session that is not there.
//
// Live backend state, never derived from items. One reactive box per
// thread (keyedSignalRegistry): a composer reads its own thread only.

import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { threadBackend } from '../transport/entityIndex';
import { getTransportHelloFor } from './transportStatus.svelte';

export interface SessionLivePayload {
  threadId: string;
  live: boolean;
}

const CAPABILITY = 'session-live.v1';

const liveThreads = createKeyedSignalRegistry<boolean>(false);

// Revisions tell a snapshot that crossed a frame from one that did not: a
// frame that landed while the read was out is newer and wins.
const revisions = new Map<string, number>();
let revision = 0;
export function sessionLiveRevision(threadId: string): number {
  return revisions.get(threadId) ?? 0;
}

/**
 * Tracked read: whether the thread has a session. Undefined when its
 * computer does not publish it, so a caller does not disable an action on
 * an answer nobody gave.
 */
export function threadSessionLive(threadId: string | null | undefined): boolean | undefined {
  if (!threadId) return false;
  const hello = getTransportHelloFor(threadBackend(threadId) ?? HOME_BACKEND);
  if (!hello?.capabilities.includes(CAPABILITY)) return undefined;
  return liveThreads.get(threadId);
}

function setLive(threadId: string, live: boolean): void {
  revisions.set(threadId, ++revision);
  if (live) liveThreads.set(threadId, true);
  else liveThreads.drop(threadId);
}

/** Apply a `provider:session_live` frame. */
export function applySessionLive(evt: SessionLivePayload | undefined): void {
  if (!evt?.threadId) return;
  setLive(evt.threadId, evt.live === true);
}

/** Apply the snapshot's answer unless a frame landed since `expectedRevision` was read. */
export function hydrateSessionLive(threadId: string, live: boolean, expectedRevision: number): void {
  if (!threadId || sessionLiveRevision(threadId) !== expectedRevision) return;
  setLive(threadId, live);
}

/** Drop a thread's answer: the thread was archived or deleted. */
export function clearSessionLiveForThread(threadId: string): void {
  if (!threadId) return;
  liveThreads.drop(threadId);
  revisions.delete(threadId);
}

export function resetForTest(): void {
  liveThreads.reset();
  revisions.clear();
}
