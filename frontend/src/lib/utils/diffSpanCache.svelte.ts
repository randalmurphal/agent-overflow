// Reactive span cache for the diff surfaces (inline chat diff cards +
// review pane). Replaces the per-line Shiki worker stack: highlighting
// is file-level and backend-owned (HighlightPatch RPCs parse whole
// virtual documents with tree-sitter), so the cache keys one entry per
// file content and the result lines index 1:1 with the file's
// PatchLine sequence.
//
// Keying is content-addressed (path + fnv1a of the joined patch text),
// never per-line: per-line keys are unsound with stateful highlighting
// (the same text highlights differently inside vs outside a
// docstring). Scoped (parse-primed) requests key SEPARATELY per
// (thread, scope) — primed spans depend on file content above each
// hunk, so two threads or scopes with identical patch bytes must not
// share a primed result. Theme is NOT in any key — spans are
// theme-independent class ids, so a theme toggle costs zero
// re-requests.
//
// Reactivity: a module-level `$state` generation counter bumps when
// spans land AND when entries are evicted; `getSpansForLine` reads it
// so rows re-evaluate, and the request effects read
// `diffSpanCacheGeneration()` so a mounted consumer whose entry was
// evicted (LRU pressure, same-thread reload) re-requests instead of
// staying plain.

import { HighlightPatch, HighlightPatchWithContext } from '../stores/bindings';
import { addToast } from '../stores/toast.svelte';
import { expansionPredecessor } from './diffContextExpansion';
import { contentKey } from './fnv1a';
import type { PatchFile, PatchLine } from './patchFiles';
import {
  ensureHighlightSchemaVersion,
  ensureSyntaxClassNames,
  type EncodedLine,
} from './syntaxSpans';

/** Review-pane scope fields forwarded to HighlightPatchWithContext so
 * the backend can prime parsing with the file content above each hunk.
 * Same shape as the diff-context expansion request. */
export interface PatchScopeContext {
  scope: string;
  commitSHA: string;
  headSHA: string;
}

interface SpanEntry {
  spans: EncodedLine[];
  bytes: number;
  /** Generation at last insert/touch. Entries touched in the current
   * generation window are "hot" — a mounted consumer re-requests them
   * on every bump, so evicting one would trigger an immediate refetch
   * whose insert evicts the next hot entry: a permanent RPC loop when
   * the mounted working set exceeds the byte budget. Hot entries are
   * skipped by eviction; the cache may exceed its budget by the
   * working set, which is bounded by the visible views. */
  gen: number;
  /** Set (Date.now()) when the backend flagged the result incomplete —
   * transient degradation (parse timeout, patch budget) that a retry
   * can beat. The entry still displays, but a request that touches it
   * after INCOMPLETE_RETRY_MS re-fetches instead of just touching. */
  incompleteAt?: number;
}

/** Minimum spacing between retries of an incomplete entry. Incomplete
 * inputs are usually pathological (each retry costs the backend its
 * full parse budget), so retries ride the natural request-effect
 * re-runs at a damped cadence rather than firing on every bump. */
export const INCOMPLETE_RETRY_MS = 30_000;

/** Byte budget for cached span runs. Spans are compact (plain lines
 * carry no runs at all), so this comfortably covers many large diffs;
 * the backend keeps its own content-addressed cache, so an LRU drop
 * only costs a round trip. */
export const DIFF_SPAN_CACHE_MAX_BYTES = 8 * 1024 * 1024;

// LRU by Map insertion order: touch() re-inserts on file-level access.
const entries = new Map<string, SpanEntry>();
let totalBytes = 0;

// key → threadIds that requested it. Ownership for thread eviction: a
// key shared by two threads (identical content) survives until the
// last owner is evicted. LRU eviction drops the ownership record with
// the entry (a later request re-registers); in-flight requests whose
// last owner departs are invalidated through their inFlight token.
const keyThreads = new Map<string, Set<string>>();

// key → identity token for the request in flight. A thread eviction
// deletes the key's token to invalidate the pending flight: its result
// (primed by pre-eviction workspace state) must not insert, and a
// remaining mounted consumer's re-run starts a fresh request instead
// of deduping against the doomed one.
const inFlight = new Map<string, object>();

// lines-array identity → base cache key (avoids re-joining/hashing the
// patch text on every effect re-run; parsed line arrays are immutable
// and shared via parsePatchFilesCached).
const fileKeys = new WeakMap<PatchLine[], string>();

// lines-array identity → per-line index. Scoped PER ARRAY because gap
// expansion builds a new array that reuses the original PatchLine
// objects — a single line → index map would let the expanded array's
// registration hijack the base array's render lookups.
const lineIndexes = new WeakMap<PatchLine[], Map<PatchLine, number>>();

// Once-per-extension guard for the degraded-highlight toast.
const warnedExtensions = new Set<string>();

// threadId → eviction count. seedPayloadPatchSpans captures the count
// before its class-table await and aborts if it moved: a thread
// evicted mid-await must not have entries re-registered after its
// cleanup already ran. An epoch only matters to ingests for that SAME
// thread, so evictions record one only for threads with a pending
// ingest, and a thread's last ingest deletes its entry on the way out
// — both maps are bounded by the number of concurrently pending
// ingests, not by threads ever visited.
const threadEvictEpochs = new Map<string, number>();
// threadId → count of ingests currently awaiting the class table.
const pendingIngestThreads = new Map<string, number>();
// Bumped by resetDiffSpanCacheForTest: a pre-reset ingest continuation
// must neither repopulate the reset cache (its captured epoch would
// trivially match the cleared map) nor consume a post-reset ingest's
// pending count.
let ingestGeneration = 0;

function evictEpoch(threadId: string): number {
  return threadEvictEpochs.get(threadId) ?? 0;
}

// Bumped when spans land or entries are evicted. Renderers depend on
// it via getSpansForLine; request effects depend on it via
// diffSpanCacheGeneration.
let generation = $state(0);

/** Reactive read for the request effects: an eviction bumps this, so
 * a mounted consumer re-runs and re-requests its file's spans. */
export function diffSpanCacheGeneration(): number {
  return generation;
}

function patchTextOf(file: PatchFile): string {
  let text = '';
  for (let i = 0; i < file.lines.length; i += 1) {
    if (i > 0) text += '\n';
    const line = file.lines[i];
    // Conflict-view marker and fold rows are frontend furniture, not
    // file content — sent raw, the backend would parse them as context
    // lines and an unlucky marker label (a stray backtick, quote, …)
    // could poison the virtual documents. A `\`-prefixed line is
    // wire-typed as a non-content marker (like `\ No newline`): it
    // keeps patch-line alignment and contributes nothing to any doc.
    text += line.type === 'marker' || line.fold !== undefined ? '\\ marker' : line.content;
  }
  return text;
}

function ensureFileKey(file: PatchFile): string {
  let key = fileKeys.get(file.lines);
  if (key !== undefined) return key;
  key = `${file.path} ${contentKey(patchTextOf(file))}`;
  fileKeys.set(file.lines, key);
  const index = new Map<PatchLine, number>();
  for (let i = 0; i < file.lines.length; i += 1) {
    index.set(file.lines[i], i);
  }
  lineIndexes.set(file.lines, index);
  return key;
}

function scopedKey(base: string, threadId: string, context: PatchScopeContext): string {
  // threadId is part of the identity: the backend resolves priming
  // content through the THREAD's workspace/refs, so the same
  // (scope, path, patch) primes differently across threads.
  return `${base}\0${threadId}\0${context.scope}\0${context.commitSHA}\0${context.headSHA}`;
}

function touch(key: string): void {
  const entry = entries.get(key);
  if (!entry) return;
  entry.gen = generation;
  entries.delete(key);
  entries.set(key, entry);
}

function entryBytes(spans: EncodedLine[]): number {
  let bytes = 64;
  for (const line of spans) {
    bytes += 40 + (line.r?.length ?? 0) * 8;
  }
  return bytes;
}

function insert(key: string, spans: EncodedLine[], incomplete: boolean): void {
  const prior = entries.get(key);
  if (prior) totalBytes -= prior.bytes;
  const bytes = entryBytes(spans);
  entries.delete(key);
  entries.set(key, {
    spans,
    bytes,
    gen: generation,
    ...(incomplete ? { incompleteAt: Date.now() } : {}),
  });
  totalBytes += bytes;
  if (totalBytes > DIFF_SPAN_CACHE_MAX_BYTES) {
    for (const [candidate, entry] of entries) {
      if (totalBytes <= DIFF_SPAN_CACHE_MAX_BYTES) break;
      // Skip the fresh insert and hot entries (touched since the last
      // bump — see SpanEntry.gen): evicting either would trigger an
      // immediate re-request and loop.
      if (candidate === key || entry.gen >= generation) continue;
      entries.delete(candidate);
      keyThreads.delete(candidate);
      totalBytes -= entry.bytes;
    }
  }
  generation += 1;
}

function registerThread(key: string, threadId: string): void {
  let owners = keyThreads.get(key);
  if (!owners) {
    owners = new Set();
    keyThreads.set(key, owners);
  }
  owners.add(threadId);
}

function extensionLabel(path: string): string {
  const base = path.slice(path.lastIndexOf('/') + 1);
  const dot = base.lastIndexOf('.');
  return dot > 0 ? base.slice(dot) : base;
}

function reportSpanFailure(path: string, err: unknown): void {
  // Failures degrade to plain text — already what the renderer does
  // when getSpansForLine returns null. Logged for diagnostics, plus a
  // one-shot toast per extension so the user sees a signal rather
  // than silently-uncolored diff lines.
  console.warn(`Diff highlight failed for ${path}:`, err);
  const ext = extensionLabel(path);
  if (warnedExtensions.has(ext)) return;
  warnedExtensions.add(ext);
  addToast('warning', `Syntax highlighting unavailable for ${ext} files`);
}

/**
 * Request spans for one file's diff. Fire-and-forget from render
 * effects (`void requestFileSpans(...)`); when the result lands, the
 * generation bump re-evaluates every row's `getSpansForLine` lookup.
 *
 * Review-pane callers pass `context` to get parse-priming file content
 * above each hunk (HighlightPatchWithContext, LocalOnly); on rejection
 * — the expected path for `--connect` remote clients — the request
 * degrades to the wire-safe HighlightPatch, still recorded under the
 * scoped key so the primed path is not retried within this client's
 * lifetime. Chat cards pass no context.
 *
 * Empty-success results (unknown language, over-cap input → all-plain
 * spans) ARE cached: the backend's answer is authoritative and a
 * re-request would return the same thing. Rejections are NEVER cached;
 * they retry when an effect dependency changes or another result's
 * generation bump re-runs the request effects.
 */
export async function requestFileSpans(
  file: PatchFile,
  threadId: string,
  context?: PatchScopeContext | null,
): Promise<void> {
  if (file.lines.length === 0) return;
  const base = ensureFileKey(file);
  const key = context ? scopedKey(base, threadId, context) : base;
  registerThread(key, threadId);

  const cached = entries.get(key);
  if (cached) {
    touch(key);
    // A complete entry is final for this content. An incomplete one
    // (transient backend degradation) keeps displaying but re-fetches
    // at a damped cadence; a complete retry replaces it below.
    const retry =
      cached.incompleteAt !== undefined && Date.now() - cached.incompleteAt >= INCOMPLETE_RETRY_MS;
    if (!retry) return;
  }
  if (inFlight.has(key)) return;
  const flight = {};
  inFlight.set(key, flight);

  try {
    const patch = patchTextOf(file);
    let result: { lines: EncodedLine[] | null; incomplete: boolean } | null = null;
    if (context) {
      try {
        result = await HighlightPatchWithContext(threadId, {
          scope: context.scope,
          commitSHA: context.commitSHA,
          headSHA: context.headSHA,
          path: file.path,
          patch,
        });
      } catch {
        // LocalOnly method rejected — remote client or scope failure.
        // Fall through to the wire-safe unprimed request.
      }
    }
    if (!result) {
      result = await HighlightPatch({ path: file.path, patch });
    }
    // Never render spans against an empty class-name table: the id →
    // class map loads once per page and this await is free afterwards.
    await ensureSyntaxClassNames();
    // A thread eviction mid-flight invalidates the token: the result
    // was computed against pre-eviction state (a same-thread reload
    // changes the workspace the priming read), and any surviving
    // consumer has already started a fresh request to replace this one.
    if (inFlight.get(key) !== flight) return;
    const existing = entries.get(key);
    if (result.incomplete && existing && existing.incompleteAt === undefined) {
      // A complete entry for this content-addressed key is final — a
      // late incomplete flight (a seed landed complete spans while
      // this request was out) must not downgrade it.
      touch(key);
    } else {
      insert(key, result.lines ?? [], result.incomplete);
    }
  } catch (err) {
    if (inFlight.get(key) !== flight) return;
    reportSpanFailure(file.path, err);
    const entry = entries.get(key);
    if (!entry) {
      // No entry landed: drop the ownership record too, or a
      // long-lived thread accumulates one orphaned owner set per
      // failed content key outside the byte-bounded LRU. A retry
      // re-registers.
      keyThreads.delete(key);
    } else if (entry.incompleteAt !== undefined) {
      // A failed refresh of an incomplete entry consumes its damping
      // window — leaving the old stamp would make every subsequent
      // generation bump retry immediately while the backend errors.
      entry.incompleteAt = Date.now();
    }
  } finally {
    if (inFlight.get(key) === flight) inFlight.delete(key);
  }
}

/** Wire shape of one file's backend-precomputed diff spans (Go:
 * PatchSpanSeed) — attached to diff-kind payload responses and pushed
 * on the remote-only `highlight:diff_seed` channel. `path` +
 * `contentKey` are exactly this cache's base key, computed by the
 * backend with frontend parser/hash parity; a mismatched key is simply
 * never looked up (the RPC path proceeds as usual). Seeds are complete
 * results only — the producer skips transient parse degradation. */
export interface PatchSpanSeedWire {
  path?: string;
  contentKey?: string;
  lines?: EncodedLine[] | null;
}

/**
 * Ingests backend-precomputed spans for a diff's files. The class-name
 * table is awaited first so a consumer repainting off the generation
 * bump can resolve class ids immediately. Complete entries already in
 * the cache win over a seed (nothing to gain from churning identical
 * spans); incomplete ones are replaced like any retry result.
 *
 * `expectedVersion` is the persisted-blob path (utils/persistedSpans.ts
 * with cold tables): the schema version is fetched alongside the class
 * table INSIDE the pending-ingest registration, so a thread eviction
 * during either fetch is observed by the epoch check and the seeds are
 * dropped. A fetched version that doesn't match the blob's stamp also
 * drops the seeds (stale grammar — the RPC path recomputes). Live
 * seeds (event push, payload loads) omit it: their spans come from the
 * connected backend, which IS the version authority.
 *
 * Best-effort by contract: this function never rejects. Seeds are
 * optional cache warmers riding functional paths (payload loads, event
 * ingest) that must not fail because warming did.
 */
export async function seedPayloadPatchSpans(
  threadId: string,
  files: readonly (PatchSpanSeedWire | null | undefined)[] | null | undefined,
  expectedVersion?: string,
): Promise<void> {
  if (!files || files.length === 0) return;
  const gen = ingestGeneration;
  pendingIngestThreads.set(threadId, (pendingIngestThreads.get(threadId) ?? 0) + 1);
  try {
    const epoch = evictEpoch(threadId);
    try {
      if (expectedVersion !== undefined) {
        const [version] = await Promise.all([
          ensureHighlightSchemaVersion(),
          ensureSyntaxClassNames(),
        ]);
        if (version !== expectedVersion) return;
      } else {
        await ensureSyntaxClassNames();
      }
    } catch (err) {
      // The next RPC-path request retries the table; skipping the seeds
      // just leaves their files on that path.
      console.warn('diff span seeding skipped (class table unavailable):', err);
      return;
    }
    if (ingestGeneration !== gen) {
      // The cache was reset while the table loaded (test reset); the
      // cleared epoch map would trivially match the captured epoch, so
      // the generation is the abort signal here.
      return;
    }
    if (evictEpoch(threadId) !== epoch) {
      // The thread was evicted while the table loaded (first-load race);
      // registering it again would strand entries cleanup already swept.
      return;
    }
    seedPayloadPatchSpansSync(threadId, files);
  } finally {
    // A pre-reset continuation must not consume post-reset counts —
    // the reset already cleared both maps for its generation.
    if (ingestGeneration === gen) {
      const left = (pendingIngestThreads.get(threadId) ?? 1) - 1;
      if (left <= 0) {
        pendingIngestThreads.delete(threadId);
        threadEvictEpochs.delete(threadId);
      } else {
        pendingIngestThreads.set(threadId, left);
      }
    }
  }
}

/**
 * Synchronous variant of seedPayloadPatchSpans for callers that have
 * ALREADY confirmed the class-name table is loaded (the persisted-blob
 * ingest at row init — see utils/persistedSpans.ts). With no await
 * window there is nothing for the eviction-epoch machinery to guard,
 * and the synchronous insert is what lets a cold row's children hit
 * the cache on their very first render.
 */
export function seedPayloadPatchSpansSync(
  threadId: string,
  files: readonly (PatchSpanSeedWire | null | undefined)[],
): void {
  for (const file of files) {
    if (!file || typeof file.path !== 'string' || typeof file.contentKey !== 'string') continue;
    seedPatchFileSpans(threadId, file.path, file.contentKey, file.lines ?? []);
  }
}

function seedPatchFileSpans(
  threadId: string,
  path: string,
  sourceContentKey: string,
  spans: EncodedLine[],
): void {
  if (!path || !sourceContentKey) return;
  const key = `${path} ${sourceContentKey}`;
  if (threadId) registerThread(key, threadId);
  const existing = entries.get(key);
  if (existing && existing.incompleteAt === undefined) {
    touch(key);
    return;
  }
  // insert() bumps the generation, so a consumer already mounted plain
  // (the seed push racing its render) repaints from the seeded entry.
  insert(key, spans, false);
}

/** How many superseded arrays a read may walk through (see below).
 * diffContextExpansion truncates stored chains at 3 retained
 * predecessors, and the first one with a landed entry almost always
 * answers; the extra hop is slack, not expected traversal. */
const MAX_PREDECESSOR_DEPTH = 4;

function entryForKey(
  base: string,
  threadId?: string,
  context?: PatchScopeContext | null,
): SpanEntry | undefined {
  let entry = context && threadId ? entries.get(scopedKey(base, threadId, context)) : undefined;
  entry ??= entries.get(base);
  return entry;
}

/**
 * Reactive read-side lookup shared by the diff row renderers. Returns
 * the line's encoded spans, or null when the file's result hasn't
 * landed — the renderer falls back to plain tinted text. Scoped
 * callers (review pane) resolve their own primed entry first and fall
 * back to the shared unprimed entry while the primed request is in
 * flight.
 *
 * While a rebuilt array's own result is in flight (review-pane context
 * expansion produces a new lines array on every click), lines shared
 * with a superseded array keep serving that array's spans — expanding
 * context must not flash the already-highlighted lines plain for a
 * round trip. Only the freshly fetched lines render plain until the
 * expanded result lands.
 */
export function getSpansForLine(
  file: PatchFile,
  line: PatchLine,
  threadId?: string,
  context?: PatchScopeContext | null,
): EncodedLine | null {
  void generation;
  // Register the array's key and line index synchronously (memoized
  // per array identity): a rebuilt array whose content key already has
  // a landed entry — reload followed by the same expansion — must
  // paint its spans on the FIRST render. The request effect runs after
  // render and its cache hit does not bump the generation, so there is
  // no later repaint to fix a miss here.
  const direct = entryForKey(ensureFileKey(file), threadId, context);
  if (direct) {
    const index = lineIndexes.get(file.lines)?.get(line);
    return index === undefined ? null : (direct.spans[index] ?? null);
  }
  let lines = expansionPredecessor(file.lines);
  for (let depth = 0; lines && depth < MAX_PREDECESSOR_DEPTH; depth += 1) {
    const base = fileKeys.get(lines);
    const entry = base === undefined ? undefined : entryForKey(base, threadId, context);
    if (entry) {
      const index = lineIndexes.get(lines)?.get(line);
      // Absent index = a line newer than this array (freshly fetched
      // context): plain until the expanded result lands. Older
      // predecessors are strict subsets, so there is nothing deeper.
      return index === undefined ? null : (entry.spans[index] ?? null);
    }
    lines = expansionPredecessor(lines);
  }
  return null;
}

/**
 * Thread-switch / thread-delete hook: drops every cached span entry
 * whose only requester was the named thread. Entries shared with
 * another live thread survive until their last owner is evicted.
 *
 * Bumps the generation when anything was removed: a same-thread
 * reload (a forced switchThread re-enters the switch path with the
 * same id) evicts entries whose review companion stays mounted, and
 * the bump is what makes those consumers re-request.
 */
export function evictDiffSpansForThread(threadId: string): void {
  if (!threadId) return;
  // An epoch exists to invalidate ingests for THIS thread currently
  // awaiting the class table; with none pending there is nothing to
  // invalidate and recording would grow the map per evicted thread.
  if (pendingIngestThreads.has(threadId)) {
    threadEvictEpochs.set(threadId, evictEpoch(threadId) + 1);
  }
  let removed = false;
  for (const [key, owners] of keyThreads) {
    if (!owners.delete(threadId)) continue;
    if (owners.size > 0) continue;
    keyThreads.delete(key);
    const entry = entries.get(key);
    if (entry) {
      entries.delete(key);
      totalBytes -= entry.bytes;
      removed = true;
    }
    if (inFlight.delete(key)) {
      // Invalidate the pending flight (its token no longer matches, so
      // its result is discarded on arrival) and bump: a mounted
      // consumer (same-thread reload) re-runs, and with the key free
      // it starts a FRESH request against post-reload state instead of
      // deduping into the stale one.
      removed = true;
    }
  }
  if (removed) generation += 1;
}

export function resetDiffSpanCacheForTest(): void {
  entries.clear();
  keyThreads.clear();
  inFlight.clear();
  warnedExtensions.clear();
  threadEvictEpochs.clear();
  pendingIngestThreads.clear();
  ingestGeneration += 1;
  totalBytes = 0;
  generation = 0;
}

/** Test-only inspection. */
export function __diffSpanCacheStatsForTest(): {
  entries: number;
  bytes: number;
  ownerKeys: number;
  evictEpochs: number;
  pendingIngestThreads: number;
} {
  return {
    entries: entries.size,
    bytes: totalBytes,
    ownerKeys: keyThreads.size,
    evictEpochs: threadEvictEpochs.size,
    pendingIngestThreads: pendingIngestThreads.size,
  };
}
