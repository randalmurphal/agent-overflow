// Ingest of persisted highlight span blobs — the cold-load half of the
// seed pipeline. Triage persists version-stamped span blobs WITH chat
// history (items.meta `codeSpans` for a settled assistant text's
// fences; payloads.preview_spans, joined as Item.payloadPreviewSpans,
// for a tool result's inline-diff previews), and rows ingest them at
// component INIT — so cold loads (thread open, scroll-up, restart)
// paint highlighted with zero highlight RPCs.
//
// Init-time matters: the code/diff hosts read their caches
// synchronously on first render and fall back to an immediate RPC on a
// miss, so a post-render $effect ingest loses that race every time.
// Once the schema version and class-name table are memoized (every
// load after the first), ingest runs fully synchronously at row init —
// before any child mounts — and the children's first reads hit. On the
// very first ingest of a session the two fetches make it async and the
// RPC path covers; that's a one-time cost per page load.
//
// Ingest is deliberately NOT memoized per blob: the target caches
// evict (thread switch, LRU pressure), and a memo would pin those
// entries out forever — the evict → remount transition is exactly when
// re-seeding pays. Re-ingest of a warm blob is cheap and idempotent:
// parseJsonObject memoizes the parse by string, and the seeds are
// content-addressed inserts of identical values.
//
// Same fail-safe discipline as the live seed push: blobs are
// content-addressed per fence/file, so one that doesn't match what the
// row actually renders is inert and the RPC path recomputes. The `hv`
// stamp is compared against the CONNECTED backend's schema version —
// which is also the RPC server that would recompute — so spans written
// by another build's grammar set degrade to the RPC path instead of
// coloring by an old opinion.

import { seedFinalBlockSpans } from '../components/chat/markdown/codeSpanCache';
import {
  seedPayloadPatchSpans,
  seedPayloadPatchSpansSync,
  type PatchSpanSeedWire,
} from './diffSpanCache.svelte';
import { parseJsonObject } from './parseJsonObject';
import {
  ensureHighlightSchemaVersion,
  ensureSyntaxClassNames,
  highlightSchemaVersionSync,
  syntaxClassNamesReady,
  type EncodedLine,
} from './syntaxSpans';

/** One fence's spans inside the items.meta `codeSpans` value (Go:
 * PersistedCodeSpan). `lang` + `contentKey` are exactly the
 * codeSpanCache key, computed by the backend with frontend hash
 * parity. */
interface PersistedCodeSpanWire {
  lang?: string;
  contentKey?: string;
  lines?: EncodedLine[] | null;
}

/**
 * True when both tables are memoized, i.e. an init-time ingest can
 * seed synchronously and beat its children's first cache reads.
 */
function tablesReady(): boolean {
  return highlightSchemaVersionSync() !== null && syntaxClassNamesReady();
}

/**
 * Code-span path only: runs `seed` synchronously when the version +
 * class table are already loaded, else fetches both and seeds when
 * they land. The code span cache is content-addressed and global (no
 * thread ownership), so a late continuation needs no eviction guard —
 * unlike the patch path, which routes its cold-table case through
 * seedPayloadPatchSpans' pending-ingest registration instead. Fetch
 * failures are logged and dropped — the next ingest retries the
 * memoized-clear fetch, and the RPC path covers meanwhile.
 */
function withSchemaVersion(blobVersion: string, seed: () => void): void {
  if (tablesReady()) {
    if (highlightSchemaVersionSync() === blobVersion) seed();
    return;
  }
  void Promise.all([ensureHighlightSchemaVersion(), ensureSyntaxClassNames()])
    .then(([fetched]) => {
      if (fetched === blobVersion) seed();
    })
    .catch((err) => {
      console.warn('persisted span ingest failed:', err);
    });
}

/**
 * Ingests an assistant-text row's persisted fence spans (items.meta
 * `codeSpans`) into the markdown code span cache. Call at component
 * init (cold-mount sync path) and from a meta-tracking $effect
 * (settle-time updates); never throws. Rows without the key (streaming
 * rows, pre-persistence history) are a cheap no-op — parseJsonObject
 * memoizes by string, so the per-remount cost is a Map hit.
 */
export function ingestPersistedCodeSpans(meta: string | null | undefined): void {
  if (!meta) return;
  const spans = parseJsonObject(meta)?.codeSpans as
    | { hv?: string; blocks?: (PersistedCodeSpanWire | null)[] | null }
    | undefined;
  if (
    !spans ||
    typeof spans !== 'object' ||
    typeof spans.hv !== 'string' ||
    !Array.isArray(spans.blocks) ||
    spans.blocks.length === 0
  ) {
    return;
  }
  const blocks = spans.blocks;
  withSchemaVersion(spans.hv, () => {
    for (const block of blocks) {
      if (!block || typeof block.lang !== 'string' || typeof block.contentKey !== 'string') {
        continue;
      }
      seedFinalBlockSpans(block.lang, block.contentKey, block.lines ?? []);
    }
  });
}

/**
 * Ingests a tool-result row's persisted inline-diff preview spans
 * (Item.payloadPreviewSpans, Go: PersistedPatchSpans) into the diff
 * span cache. Same call contract as ingestPersistedCodeSpans.
 *
 * The warm-table path seeds synchronously — no await window, so
 * nothing for the eviction-epoch machinery to guard. The cold-table
 * path (first ingest of a page load) goes through the async
 * seedPayloadPatchSpans with the blob's version stamp: its
 * pending-ingest registration spans the table fetches, so a thread
 * switched away or deleted mid-fetch drops the seeds instead of
 * repopulating entries cleanup already swept.
 */
export function ingestPersistedPatchSpans(
  threadId: string,
  blob: string | null | undefined,
): void {
  if (!blob) return;
  const spans = parseJsonObject(blob);
  const files = spans?.files as (PatchSpanSeedWire | null)[] | undefined;
  if (typeof spans?.hv !== 'string' || !Array.isArray(files) || files.length === 0) return;
  if (tablesReady()) {
    if (highlightSchemaVersionSync() === spans.hv) seedPayloadPatchSpansSync(threadId, files);
    return;
  }
  void seedPayloadPatchSpans(threadId, files, spans.hv);
}
