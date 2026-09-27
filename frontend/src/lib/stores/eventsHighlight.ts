// highlight:live and highlight:diff_seed event domain: backend-pushed syntax
// spans for streaming code fences and persisted diffs. Fan-in target of
// events.ts's setupEventListeners.
//
// Pushes only land once the origin proves it speaks this page's span
// schema; a push that fails the check or names a thread this client does
// not know is dropped, and its consumers recover through the highlight RPC
// (live code also through its sequence numbers: liveCodeSpans.svelte.ts).
// A fence's first live push carries no spans, so it lands at once.
import {
  applyLiveCode,
  resyncLiveCodeRows,
  retainLiveCodeThreads,
  stopLiveCodeFence,
  type HighlightLiveCodeEvent,
} from '../components/chat/markdown/liveCodeSpans.svelte';
import {
  seedPayloadPatchSpans,
  type PatchSpanSeedWire,
} from '../utils/diffSpanCache.svelte';
import { ensureSyntaxClassNames, syntaxClassNamesReady } from '../utils/syntaxSpans';
import { getThreadById } from './threads.svelte';
import type { EventOrigin } from '../transport/handle';
import { assertHighlightSource, provenHighlightSource, requireHighlightSchema } from '../utils/highlightService';
import { HOME_BACKEND } from '../transport/backendKey';
import { backendKeyForOrigin } from '../transport/backends';
import { onClientLeaseChange } from '../transport/lease';
import { onBackendRecovery } from './transportRecovery';
import { onWatchedThreadsComposed } from './watchedThreads';
import { wailsEventOn } from './wailsEvents';

export type { HighlightLiveCodeEvent };

/** Wire payload of `highlight:diff_seed` (Go: HighlightDiffSeedEvent):
 * patch-aligned spans for a just-persisted inline-diff tool result's
 * preview files, keyed for the diff span cache. */
export interface HighlightDiffSeedEvent {
  threadId: string;
  files: PatchSpanSeedWire[] | null;
}

function isCount(value: unknown): value is number {
  return Number.isInteger(value) && (value as number) >= 0;
}

function validLiveCodeEvent(evt: HighlightLiveCodeEvent): boolean {
  if (!evt || typeof evt.threadId !== 'string' || !evt.threadId || typeof evt.itemId !== 'string' || !evt.itemId) {
    return false;
  }
  if (!isCount(evt.fence) || !isCount(evt.from) || !isCount(evt.seq) || evt.seq === 0) return false;
  if (typeof evt.lang !== 'string' || typeof evt.final !== 'boolean') return false;
  if (evt.contentKey !== undefined && typeof evt.contentKey !== 'string') return false;
  if (evt.head !== undefined && typeof evt.head !== 'string') return false;
  const hashes = evt.lineHashes ?? [];
  const lines = evt.lines ?? [];
  return Array.isArray(hashes) && Array.isArray(lines) && hashes.length === lines.length;
}

export function applyHighlightLive(evt: HighlightLiveCodeEvent, origin?: EventOrigin): void {
  if (!validLiveCodeEvent(evt) || !getThreadById(evt.threadId)) return;
  const backend = backendKeyForOrigin(origin?.backendId ?? '');
  // A fence's first push precedes its text and names no span class: it
  // applies at once, so the fence's hosts wait for spans instead of
  // requesting them. Later pushes apply at once when the origin's schema is
  // already proven. Pushes that wait resume in arrival order, all before the
  // next event: they wait on the same promises, which settle together.
  const firstPush = evt.seq === 1 && (evt.lines ?? []).every((line) => !line.r?.length);
  if (firstPush || (syntaxClassNamesReady() && provenHighlightSource(backend))) {
    applyLiveCode(evt, backend);
    return;
  }
  void requireHighlightSchema(backend)
    .then(async (source) => {
      await ensureSyntaxClassNames();
      assertHighlightSource(source);
      if (getThreadById(evt.threadId)) applyLiveCode(evt, backend);
    })
    .catch((error) => {
      console.warn('events: live code span ingest failed', error);
      // The fence's later pushes build on this one; its hosts request spans.
      stopLiveCodeFence(evt.threadId, evt.itemId, evt.fence);
    });
}

/**
 * Subscribes live code spans and the moments their pushes may have been
 * lost: a replayed reconnect and a return from a background lease withhold
 * or drop frames without a gap on this ephemeral channel, so the rows ask
 * for keyframes; a thread leaving the watched set takes its rows along.
 */
export function setupHighlightLiveEvents(): () => void {
  const offLive = wailsEventOn<HighlightLiveCodeEvent>('highlight:live', applyHighlightLive);
  const offRecovery = onBackendRecovery((backend, phase) => {
    if (phase === 'complete') resyncLiveCodeRows(backend);
  });
  const offLease = onClientLeaseChange((state) => {
    if (state === 'active') resyncLiveCodeRows();
  });
  const offWatched = onWatchedThreadsComposed(retainLiveCodeThreads);
  return () => {
    offLive();
    offRecovery();
    offLease();
    offWatched();
  };
}

export function applyHighlightDiffSeed(evt: HighlightDiffSeedEvent, origin?: EventOrigin): void {
  if (!evt || !Array.isArray(evt.files)) return;
  // Only seed threads this client currently knows: thread deletion
  // removes the row from the store in the same pass that evicts the
  // diff span cache (threads.svelte.ts removeThread), so a seed whose
  // worker outraced the backend's delete (span write succeeded, thread
  // gone before the emit) lands here AFTER cleanup and must not
  // re-register entries for it. A live seed always originates from an
  // active turn on a known thread; a seed racing boot before the
  // thread list hydrates just misses and the RPC path covers.
  if (!getThreadById(evt.threadId ?? '')) return;
  // The push usually races the diff card's own HighlightPatch request;
  // whichever lands first inserts, the other is a no-op or an
  // identical overwrite — colors paint at the earlier of the two.
  // Ingest is best-effort by contract (it never rejects).
  void requireHighlightSchema(origin?.backendId ?? HOME_BACKEND)
    .then((source) => {
      assertHighlightSource(source);
      if (getThreadById(evt.threadId ?? '')) return seedPayloadPatchSpans(evt.threadId ?? '', evt.files);
    })
    .catch((error) => console.warn('events: highlight diff seed ingest failed', error));
}
