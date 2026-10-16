// The live tail of a running background command's output, keyed by the
// launch (thread + launch row id). The tray row a user expands attaches
// here; the backend follows the command's output file for as long as a
// watch holds (internal/tasktail) and streams what it gains as
// `provider:background_task_output` frames, which this store applies to
// the tail it holds for the key. Two surfaces expanding one launch share
// one watch.
//
// The tail is bounded here as it is on the backend: the last TAIL_BYTES,
// cut at a line boundary, so a run that prints for an hour costs the page
// one block of text and the morph of its newest lines. A frame whose seq
// does not continue the held tail's is a missed one, and the key is
// re-sourced: the watch reply carries the whole tail again. A frame that
// lands while the watch reply is still in flight is held and replayed
// over the reply, so an append the backend sent between its seed read
// and the reply's arrival is not lost; a transport gap on the channel
// re-sources every held tail (eventsTransportGap.ts).

import type { BackgroundTaskOutputEvent } from '../types/events';
import {
  UnwatchBackgroundTaskOutput,
  WatchBackgroundTaskOutput,
  type BackgroundTaskOutputSnapshot,
} from './bindings';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { onBackgroundTaskOutputEvent } from './eventsBackgroundTaskOutput';
import { isTransportClassError } from './transportStatus.svelte';
import { withBackendTarget } from '../transport/backends';
import { threadBackend } from '../transport/entityIndex';
import { HOME_BACKEND, type BackendKey } from '../transport/backendKey';

/** The bound kept here, the backend's DefaultTailBytes. */
export const TAIL_BYTES = 64 * 1024;

export interface BackgroundTaskOutputTail {
  /** The tail text as it stands. */
  readonly text: string;
  /** The tail holds less than the file: earlier output was dropped. */
  readonly truncated: boolean;
  /** The file's size at the last read. */
  readonly size: number;
  /** The last frame applied; the next must carry seq + 1. */
  readonly seq: number;
  /** The task ended, or the file became unreadable (`error`): no more frames. */
  readonly ended: boolean;
  readonly error: string | null;
  /** `performance.now()` of the last append, for the follow spring's live gate. */
  readonly lastAppendAt: number;
}

export function backgroundTaskOutputKey(threadId: string, launchId: string): string {
  return `${threadId}\u0000${launchId}`;
}

export function splitBackgroundTaskOutputKey(key: string): { threadId: string; launchId: string } {
  const at = key.indexOf('\u0000');
  return { threadId: key.slice(0, at), launchId: key.slice(at + 1) };
}

/**
 * Keeps the last `limit` bytes of `text` (as UTF-16 code units, the one
 * measure the page has; the backend's bound is bytes, so the page holds
 * at most what the backend does). The cut lands at the next line start
 * when a line straddles it, as the backend's does.
 */
export function boundTail(text: string, limit: number): { text: string; cut: boolean } {
  if (text.length <= limit) return { text, cut: false };
  let cut = text.length - limit;
  const nl = text.indexOf('\n', cut);
  if (nl >= 0 && nl + 1 < text.length) cut = nl + 1;
  // A cut on the low half of a surrogate pair would split the character.
  else if ((text.charCodeAt(cut) & 0xfc00) === 0xdc00) cut += 1;
  return { text: text.slice(cut), cut: true };
}

// The watch each live key holds, by key: its id for the release and the
// backend it was minted on, which is the only connection the id means
// anything to. Written by the source run that minted it and read by the
// event listener to route frames; stamped with the run so a superseded
// run's cleanup cannot drop the live run's watch.
interface LiveWatch {
  watchId: string;
  backend: BackendKey;
  owner: symbol;
}
const liveWatches = new Map<string, LiveWatch>();

// Frames that arrived for a key whose watch reply is still in flight,
// replayed over the reply in seq order. Bounded: past the bound the
// oldest frame goes, and the seq skip that leaves re-sources the key.
const PENDING_FRAMES_MAX = 512;
const pendingFrames = new Map<string, Array<{ evt: BackgroundTaskOutputEvent; origin: BackendKey | undefined }>>();

function tailFromSnapshot(snapshot: BackgroundTaskOutputSnapshot): BackgroundTaskOutputTail {
  const bounded = boundTail(snapshot.data ?? '', TAIL_BYTES);
  return {
    text: bounded.text,
    truncated: Boolean(snapshot.truncated) || bounded.cut,
    size: snapshot.size ?? 0,
    seq: snapshot.seq ?? 0,
    ended: Boolean(snapshot.ended),
    error: snapshot.error ? snapshot.error : null,
    lastAppendAt: 0,
  };
}

const store = createEntityStore<BackgroundTaskOutputTail, void>({
  name: 'backgroundTaskOutput',
  backendForKey: (key) => threadBackend(splitBackgroundTaskOutputKey(key).threadId),
  rawValue: true,
  source: async ({ key, apply, signal }) => {
    const { threadId, launchId } = splitBackgroundTaskOutputKey(key);
    const backend = threadBackend(threadId);
    const owner = Symbol(key);
    const issue = () => WatchBackgroundTaskOutput(threadId, launchId);
    pendingFrames.set(key, []);
    let snapshot: BackgroundTaskOutputSnapshot;
    try {
      snapshot = (await (backend === undefined ? issue() : withBackendTarget(backend, issue))) as BackgroundTaskOutputSnapshot;
    } catch (err) {
      pendingFrames.delete(key);
      throw err;
    }
    const watchId = snapshot.watchId ?? '';
    // A thread the index has not attributed yet routed by the default
    // thread route, which is the home computer's.
    const minted = backend ?? HOME_BACKEND;
    const held = pendingFrames.get(key) ?? [];
    pendingFrames.delete(key);
    if (signal.aborted) {
      return async () => {
        if (watchId) await releaseWatch(minted, threadId, watchId);
      };
    }
    if (watchId) liveWatches.set(key, { watchId, backend: minted, owner });
    apply(tailFromSnapshot(snapshot));
    // Frames the backend sent after its seed read and before this reply
    // landed continue the snapshot's seq; the rest it already carried.
    held.sort((a, b) => a.evt.seq - b.evt.seq);
    for (const { evt, origin } of held) {
      if (origin !== undefined && origin !== minted) continue;
      const current = store.snapshot(key);
      if (current === null || current.ended || evt.seq !== current.seq + 1) continue;
      store.apply(key, appendFrame(current, evt));
    }
    return async () => {
      const live = liveWatches.get(key);
      if (live?.owner === owner) liveWatches.delete(key);
      // Every watch the reply named is released, an ended one included:
      // the backend answers an id it no longer holds with nothing, and
      // the id must not stay counted against the cap.
      if (!watchId) return;
      await releaseWatch(minted, threadId, watchId);
    };
  },
  onDrop: (key) => {
    liveWatches.delete(key);
    pendingFrames.delete(key);
  },
});

async function releaseWatch(backend: BackendKey, threadId: string, watchId: string): Promise<void> {
  try {
    await withBackendTarget(backend, () => UnwatchBackgroundTaskOutput(threadId, watchId));
  } catch (err) {
    // A dead wire releases every watch its connection held; anything
    // else is a real failure to release and must be seen.
    if (!isTransportClassError(err)) throw err;
  }
}

function appendFrame(current: BackgroundTaskOutputTail, evt: BackgroundTaskOutputEvent): BackgroundTaskOutputTail {
  const append = evt.append ?? '';
  const joined = evt.reset ? append : current.text + append;
  const bounded = boundTail(joined, TAIL_BYTES);
  return {
    text: bounded.text,
    truncated: Boolean(evt.truncated) || bounded.cut || (!evt.reset && current.truncated),
    size: evt.size ?? current.size,
    seq: evt.seq,
    ended: Boolean(evt.ended),
    error: evt.error ? evt.error : null,
    lastAppendAt: append ? performance.now() : current.lastAppendAt,
  };
}

/**
 * Applies one frame to the tail it names. A frame from another computer's
 * launch of the same ids, or one that does not continue the held seq, is
 * not applied: the first is routed away by the backend the watch was
 * minted on, the second re-sources the key so the whole tail is read again.
 */
export function applyBackgroundTaskOutputFrame(
  evt: BackgroundTaskOutputEvent,
  origin: BackendKey | undefined,
): void {
  const key = backgroundTaskOutputKey(evt.threadId, evt.launchId);
  const live = liveWatches.get(key);
  if (!live) {
    const held = pendingFrames.get(key);
    if (held) {
      held.push({ evt, origin });
      if (held.length > PENDING_FRAMES_MAX) held.shift();
    }
    return;
  }
  if (origin !== undefined && origin !== live.backend) return;
  const current = store.snapshot(key);
  if (current === null || current.ended) return;
  if (evt.seq !== current.seq + 1) {
    if (evt.seq > current.seq + 1) store.invalidate(key);
    return;
  }
  store.apply(key, appendFrame(current, evt));
}

/**
 * Re-sources every held tail of the threads named (every thread when none
 * is), on the backend named: a transport gap on the channel may have
 * dropped an append, and a tail whose command then goes quiet would show
 * a stale end until the next frame. The held text survives the re-watch.
 */
export function resourceBackgroundTaskOutput(threads: readonly string[] | undefined, backend: BackendKey): void {
  const wanted = threads === undefined ? null : new Set(threads);
  for (const key of store.keys()) {
    const { threadId } = splitBackgroundTaskOutputKey(key);
    if (wanted !== null && !wanted.has(threadId)) continue;
    const live = liveWatches.get(key);
    if (live !== undefined && live.backend !== backend) continue;
    store.invalidate(key);
  }
}

onBackgroundTaskOutputEvent((evt, origin) => {
  applyBackgroundTaskOutputFrame(evt, origin);
});

/**
 * Refcounted attach for a launch's tail. Release when the consumer
 * collapses or unmounts; the backend's follow ends with the last release.
 */
export function attachBackgroundTaskOutput(
  threadId: string,
  launchId: string,
): EntityAttachment<BackgroundTaskOutputTail> {
  return store.attach(backgroundTaskOutputKey(threadId, launchId), undefined);
}

/** Diagnostics and tests: the launches currently held. */
export function backgroundTaskOutputKeys(): string[] {
  return store.keys();
}

/** Test seam: drop every entry and watch, as a fresh module load would. */
export function __resetBackgroundTaskOutputForTest(): void {
  store.suspend();
  store.resetAll();
  liveWatches.clear();
  pendingFrames.clear();
}
