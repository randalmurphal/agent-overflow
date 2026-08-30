// Persisted shape of one thread's cached timeline window
// (docs/architecture/thread-replica-sync.md §6) plus the validation that
// decides whether a stored record may be painted.
//
// Two rules this file exists to enforce:
//
//  - **Never migrate, always drop.** Any version, schema or generation
//    mismatch discards the record. The replica is a paint accelerator
//    over a backend that remains the only queryable truth, so a stale
//    or unreadable record costs one window fetch — the same price a
//    miss costs — while a migration would be permanent code carrying a
//    permanent risk of resurrecting rows the backend cut.
//  - **Store plain data only.** Pane items reach here through a Svelte
//    `$state` proxy, and the structured-clone algorithm throws
//    `DataCloneError` on a Proxy. `normalizeBody` rebuilds every value
//    as a plain object, so a caller cannot hand IndexedDB something it
//    will reject at write time.
import type { Item } from '../types/models';
import type { SubagentFoldSnapshot } from '../utils/subagentFold';
import type { TimelineCursorLike } from '../stores/threadItems';
import type { SettledTurn } from '../stores/threadTurnProjection';

/**
 * Envelope format version. Bumped when the envelope framing changes
 * (`cipher`, where the body lives). Independent of SCHEMA_VERSION,
 * which describes the body's own field set.
 */
export const REPLICA_ENVELOPE_VERSION = 1;

/**
 * Body schema version, stamped into the database's `meta` record.
 * Bump whenever `ReplicaBody`'s field set or the meaning of a field
 * changes — including a change to the wire `Item` DTO, since items ride
 * the envelope verbatim. A mismatch drops the whole database.
 */
export const REPLICA_SCHEMA_VERSION = 1;

/**
 * Per-envelope caps, deliberately the same numbers `threadItemCache`
 * applies to its in-memory snapshots: an envelope holds the same window
 * the L1 snapshot holds, so a window worth caching in memory is exactly
 * the window worth persisting.
 */
export const MAX_ENVELOPE_ITEMS = 1000;
export const MAX_ENVELOPE_CHARS = 2 * 1024 * 1024;

/** Replica-wide bounds, swept LRU-by-`savedAt` on every write. */
export const MAX_REPLICA_THREADS = 50;
export const MAX_REPLICA_CHARS = 32 * 1024 * 1024;

/**
 * The attested stamp pair a body carries. Persisted stamps come ONLY
 * from a `SyncThreadWindow` response (§3.4) — the one place stamps and
 * rows are read in a single transaction.
 */
export interface ReplicaStamp {
  epoch: number;
  rev: number;
}

/**
 * The persisted window. Field names follow the spec's envelope rather
 * than `ThreadItemSnapshot`'s pane-internal names: this is a stored
 * format with its own version, and it must not silently follow a
 * refactor of the in-memory type.
 *
 * Payload bodies are never stored (core principle 4). `payloadMeta` /
 * `payloadPreviewSpans` ride the item rows exactly as they do on the
 * wire.
 */
export interface ReplicaBody {
  epoch: number;
  rev: number;
  savedAt: number;
  items: Item[];
  oldestCursor: TimelineCursorLike | null;
  newestCursor: TimelineCursorLike | null;
  hasMoreOlder: boolean;
  hasMoreNewer: boolean;
  /** Paint-only; `ListRecentTurns` re-fetches it on every open. */
  latestSettledTurn: SettledTurn | null;
  subagentFolds: SubagentFoldSnapshot | null;
}

export interface ReplicaEnvelope {
  v: number;
  /**
   * Encryption scheme naming the body's encoding. Always `'none'` while
   * the replica is desktop-only (same OS-user boundary as the SQLite
   * store). Remote-device encryption (remote-access §12) turns `body`
   * into an opaque blob and names its scheme here.
   */
  cipher: 'none';
  body: ReplicaBody;
}

export interface ReplicaMeta {
  generation: string;
  schemaVersion: number;
}

function isFiniteNumber(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value);
}

function plainCursor(cursor: TimelineCursorLike | null | undefined): TimelineCursorLike | null {
  if (!cursor || !isFiniteNumber(cursor.turnIndex) || !isFiniteNumber(cursor.itemIndex)) {
    return null;
  }
  const plain: TimelineCursorLike = {
    turnIndex: cursor.turnIndex,
    itemIndex: cursor.itemIndex,
  };
  if (typeof cursor.itemId === 'string') plain.itemId = cursor.itemId;
  return plain;
}

function plainFolds(folds: SubagentFoldSnapshot | null | undefined): SubagentFoldSnapshot | null {
  if (!folds || !Array.isArray(folds.anchors)) return null;
  return {
    anchors: folds.anchors.map((anchor) => ({
      anchorId: anchor.anchorId,
      evictedIds: [...anchor.evictedIds],
      terminalPreview: anchor.terminalPreview,
      terminalTurnIndex: anchor.terminalTurnIndex,
      terminalItemIndex: anchor.terminalItemIndex,
    })),
  };
}

function plainSettledTurn(turn: SettledTurn | null | undefined): SettledTurn | null {
  if (!turn) return null;
  return {
    ...turn,
    tokenUsage: turn.tokenUsage ? { ...turn.tokenUsage } : null,
  };
}

/**
 * Rebuild `input` as structured-clone-safe plain data. Every reactive
 * proxy, class instance and shared reference the pane may hold is
 * flattened here, so `put` cannot be handed a value IndexedDB refuses.
 */
export function normalizeBody(input: ReplicaBody): ReplicaBody {
  return {
    epoch: input.epoch,
    rev: input.rev,
    savedAt: input.savedAt,
    items: input.items.map((item) => ({ ...item })),
    oldestCursor: plainCursor(input.oldestCursor),
    newestCursor: plainCursor(input.newestCursor),
    hasMoreOlder: input.hasMoreOlder === true,
    hasMoreNewer: input.hasMoreNewer === true,
    latestSettledTurn: plainSettledTurn(input.latestSettledTurn),
    subagentFolds: plainFolds(input.subagentFolds),
  };
}

/**
 * Chars an envelope contributes to the replica-wide budget. Term for
 * term the accounting `threadItemCache.estimateSnapshotChars` uses
 * (summary + meta + payloadMeta + payloadPreviewSpans per row, plus the
 * fold's preview and id strings), so the in-memory and durable tiers —
 * which share the per-window caps above — are measured on one scale.
 * Changing either estimator without the other silently desynchronises
 * what the two tiers will accept.
 */
export function estimateBodyChars(body: ReplicaBody): number {
  let chars = 0;
  for (const item of body.items) {
    chars += item.summary?.length ?? 0;
    chars += item.meta?.length ?? 0;
    chars += item.payloadMeta?.length ?? 0;
    chars += item.payloadPreviewSpans?.length ?? 0;
  }
  for (const anchor of body.subagentFolds?.anchors ?? []) {
    chars += anchor.terminalPreview.length;
    for (const id of anchor.evictedIds) chars += id.length;
  }
  return chars;
}

/** Does this window fit the per-envelope caps? */
export function bodyFitsCaps(body: ReplicaBody): boolean {
  return body.items.length <= MAX_ENVELOPE_ITEMS && estimateBodyChars(body) <= MAX_ENVELOPE_CHARS;
}

export function wrapEnvelope(body: ReplicaBody): ReplicaEnvelope {
  return { v: REPLICA_ENVELOPE_VERSION, cipher: 'none', body };
}

/**
 * Read a stored record back. Returns null for anything this build
 * cannot paint verbatim — wrong envelope version, unknown cipher, or a
 * body whose required fields are missing or mistyped. Callers treat
 * null as a miss; the record is dropped, never repaired.
 */
export function readEnvelope(raw: unknown): ReplicaBody | null {
  if (!raw || typeof raw !== 'object') return null;
  const envelope = raw as Partial<ReplicaEnvelope>;
  if (envelope.v !== REPLICA_ENVELOPE_VERSION) return null;
  if (envelope.cipher !== 'none') return null;
  const body = envelope.body as Partial<ReplicaBody> | undefined;
  if (!body || typeof body !== 'object') return null;
  if (!isFiniteNumber(body.epoch) || !isFiniteNumber(body.rev)) return null;
  if (!isFiniteNumber(body.savedAt)) return null;
  if (!Array.isArray(body.items)) return null;
  for (const item of body.items) {
    if (!item || typeof item !== 'object') return null;
    if (typeof (item as Item).id !== 'string') return null;
    if (!isFiniteNumber((item as Item).turnIndex) || !isFiniteNumber((item as Item).itemIndex)) {
      return null;
    }
  }
  return {
    epoch: body.epoch,
    rev: body.rev,
    savedAt: body.savedAt,
    items: body.items as Item[],
    oldestCursor: plainCursor(body.oldestCursor),
    newestCursor: plainCursor(body.newestCursor),
    hasMoreOlder: body.hasMoreOlder === true,
    hasMoreNewer: body.hasMoreNewer === true,
    latestSettledTurn: (body.latestSettledTurn as SettledTurn | null) ?? null,
    subagentFolds: plainFolds(body.subagentFolds),
  };
}

/** Is a stored meta record usable by this build against `generation`? */
export function metaMatches(raw: unknown, generation: string): boolean {
  if (!raw || typeof raw !== 'object') return false;
  const meta = raw as Partial<ReplicaMeta>;
  return meta.schemaVersion === REPLICA_SCHEMA_VERSION && meta.generation === generation;
}
