// highlight:seed event domain: backend-pushed syntax spans for
// streaming code fences (remote clients only — the transport filters
// the channel away from loopback origins, and the backend producer is
// gated on HasRemoteClient). Fan-in target of events.ts's
// setupEventListeners.
//
// Seeds are cache-warmers, never authority: a final seed lands in
// codeSpanCache under the backend-computed contentKey so a settled
// block's mount is a synchronous hit, and every seed (live or final)
// feeds the live-seed match table StreamdownCodeHost consults before
// falling back to the RPC path. Malformed or diverged seeds degrade to
// that RPC path — never to misaligned colors.
import {
  putLiveCodeSeed,
  type HighlightSeedEvent,
} from '../components/chat/markdown/liveCodeSeeds.svelte';
import { seedFinalBlockSpans } from '../components/chat/markdown/codeSpanCache';
import {
  seedPayloadPatchSpans,
  type PatchSpanSeedWire,
} from '../utils/diffSpanCache.svelte';
import { ensureSyntaxClassNames } from '../utils/syntaxSpans';
import { getThreadById } from './threads.svelte';
import type { EventOrigin } from '../transport/handle';
import { assertHighlightSource, requireHighlightSchema } from '../utils/highlightService';
import { HOME_BACKEND } from '../transport/backendKey';

export type { HighlightSeedEvent };

/** Wire payload of `highlight:diff_seed` (Go: HighlightDiffSeedEvent):
 * patch-aligned spans for a just-persisted inline-diff tool result's
 * preview files, keyed for the diff span cache. */
export interface HighlightDiffSeedEvent {
  threadId: string;
  files: PatchSpanSeedWire[] | null;
}

export function applyHighlightSeed(evt: HighlightSeedEvent, origin?: EventOrigin): void {
  if (!evt || typeof evt.lang !== 'string' || !Array.isArray(evt.lineHashes)) return;
  // Never surface spans before the classId → class-name table exists;
  // it loads once per page load, so this await is a no-op after boot.
  void requireHighlightSchema(origin?.backendId ?? HOME_BACKEND)
    .then(async (source) => {
      await ensureSyntaxClassNames();
      assertHighlightSource(source);
      if (evt.final && evt.contentKey) {
        seedFinalBlockSpans(evt.lang, evt.contentKey, evt.lines ?? []);
      }
      putLiveCodeSeed(evt.threadId ?? '', evt.itemId ?? '', evt.lang, evt.lineHashes ?? [], evt.lines ?? []);
    })
    .catch((error) => {
      console.warn('events: highlight seed ingest failed', error);
    });
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
