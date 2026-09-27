// Live syntax spans for streaming code blocks. The backend follows each
// streaming assistant_text row's fenced code with an incremental tree-sitter
// parse (internal/highlightapp/live.go) and pushes each fence's spans on
// `highlight:live` as numbered line-range deltas. This store applies them per
// (thread, item, fence), and StreamdownCodeHost paints from the fence whose
// line-hash chain verifies its text.
//
// Pushes carry no text. A fence's chain holds, per line, the fnv1a of the
// fence's lines up to and including that one (UTF-16 code units, parity
// pinned by internal/highlight/jshash_test.go). A host compares it with the
// chain of its own text, so spans paint only the lines they are proven to
// describe, plus the host's partial last line, which is a prefix of the
// fence's line at that index once the lines before it match. Until a first
// line is complete no hash proves anything, so a keyframe also carries the
// fence's first line (head), and the host checks the prefix relation itself.
//
// A fence's first push arrives before any of its text: the backend makes it
// before it emits the delta that gives the fence text. It carries no spans,
// so it applies without the backend's class table; a host that finds it waits
// for spans instead of requesting them.
//
// A fence's state converges without replay. A delta applies only on top of
// the push before it; a missing one makes the row ask the backend for a
// keyframe (ResyncLiveCode), which also answers which fence is still open, so
// fences whose final push was lost are marked stopped and their hosts request
// spans instead. Rows are dropped when their thread stops being watched, and
// rows whose fences are all final are kept only in a small LRU: a host holds
// the fence it paints from, so eviction never takes colors off a block.

import { getContext, setContext } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import type { EncodedLine } from '../../../utils/syntaxSpans';
import type { BackendKey } from '../../../transport/backendKey';
import { ResyncLiveCode } from '../../../stores/bindings';
import { seedFinalBlockSpans } from './codeSpanCache';

/** Wire payload of `highlight:live` (Go: HighlightLiveCodeEvent). */
export interface HighlightLiveCodeEvent {
  threadId: string;
  itemId: string;
  /** The row's transcript scope; the transport filters by it. */
  parentId?: string;
  /** The fence's ordinal among the row's fences. */
  fence: number;
  lang: string;
  /** Numbers the fence's pushes from 1. */
  seq: number;
  /** First line this push replaces; 0 is a keyframe. */
  from: number;
  lineHashes: number[] | null;
  lines: EncodedLine[] | null;
  /** No later push changes the fence. */
  final: boolean;
  /** Frontend contentKey of the final source, on a final push with spans. */
  contentKey?: string;
  /** The fence's first line, on a push from 0. */
  head?: string;
}

/** Rows whose fences are all final, kept for hosts still revealing them. */
export const LIVE_CODE_FINISHED_ROWS_MAX = 32;

export class LiveCodeFence {
  seq = 0;
  lineHashes: number[] = [];
  lines: EncodedLine[] = [];
  /** The fence's first line. */
  head = '';
  /** Whether the lines carry spans: the fence's first push, made before its
   * text was parsed, has none. */
  spanned = false;
  final = false;
  /** Final without spans for what follows: the backend stopped following
   * the fence, or its final push was lost. Hosts request spans. */
  stopped = false;

  constructor(
    readonly index: number,
    readonly lang: string,
  ) {}
}

export class LiveCodeRow {
  readonly fences = new Map<number, LiveCodeFence>();
  /** Bumped on every change; hosts read it to re-verify. */
  version = $state(0);
  resyncing = false;

  constructor(
    readonly key: string,
    readonly threadId: string,
    readonly itemId: string,
    readonly backend: BackendKey,
  ) {}
}

const rows = new SvelteMap<string, LiveCodeRow>();
// Insertion-ordered LRU of the keys of rows whose fences are all final.
const finished = new Set<string>();

function rowKey(threadId: string, itemId: string): string {
  return `${threadId}|${itemId}`;
}

/** The row's live state, read reactively (its appearance and each change). */
export function liveCodeRow(threadId: string, itemId: string): LiveCodeRow | undefined {
  const row = rows.get(rowKey(threadId, itemId));
  void row?.version;
  return row;
}

/** Applies one push. The caller has validated its shape. */
export function applyLiveCode(evt: HighlightLiveCodeEvent, backend: BackendKey): void {
  const key = rowKey(evt.threadId, evt.itemId);
  let row = rows.get(key);
  const fence = row?.fences.get(evt.fence);
  if (fence && evt.seq <= fence.seq) return;
  const hashes = evt.lineHashes ?? [];
  const lines = evt.lines ?? [];
  if (evt.final && !evt.contentKey && hashes.length === 0) {
    // A stop: the fence keeps the lines it has, and its hosts request the
    // rest.
    if (!row || !fence) return;
    fence.seq = evt.seq;
    fence.final = true;
    fence.stopped = true;
    changed(row);
    return;
  }
  if (!row) {
    row = new LiveCodeRow(key, evt.threadId, evt.itemId, backend);
    rows.set(key, row);
  }
  if (evt.from > 0 && (!fence || evt.seq !== fence.seq + 1 || evt.from > fence.lineHashes.length)) {
    requestResync(row);
    return;
  }
  const target = fence ?? new LiveCodeFence(evt.fence, evt.lang);
  if (evt.from === 0) {
    target.lineHashes = hashes;
    target.lines = lines;
    target.head = evt.head ?? '';
  } else {
    splice(target.lineHashes, evt.from, hashes);
    splice(target.lines, evt.from, lines);
  }
  target.seq = evt.seq;
  target.spanned = evt.seq > 1;
  target.final = evt.final;
  target.stopped = false;
  if (!fence) row.fences.set(evt.fence, target);
  if (evt.final && evt.contentKey) seedFinalBlockSpans(evt.lang, evt.contentKey, target.lines);
  changed(row);
}

/** Ends a fence whose push this client could not take, so its hosts request
 * spans instead of waiting for pushes that build on the lost one. */
export function stopLiveCodeFence(threadId: string, itemId: string, index: number): void {
  const row = rows.get(rowKey(threadId, itemId));
  const fence = row?.fences.get(index);
  if (!row || !fence || fence.stopped) return;
  fence.final = true;
  fence.stopped = true;
  changed(row);
}

function splice<T>(into: T[], from: number, items: readonly T[]): void {
  into.length = from;
  for (const item of items) into.push(item);
}

function changed(row: LiveCodeRow): void {
  row.version += 1;
  finished.delete(row.key);
  for (const fence of row.fences.values()) {
    if (!fence.final) return;
  }
  finished.add(row.key);
  while (finished.size > LIVE_CODE_FINISHED_ROWS_MAX) {
    const oldest = finished.values().next().value;
    if (oldest === undefined) break;
    finished.delete(oldest);
    rows.delete(oldest);
  }
}

function stopOpenFences(row: LiveCodeRow, except: number): void {
  for (const fence of row.fences.values()) {
    if (!fence.final && fence.index !== except) {
      fence.final = true;
      fence.stopped = true;
    }
  }
  changed(row);
}

function requestResync(row: LiveCodeRow): void {
  if (row.resyncing) return;
  row.resyncing = true;
  void ResyncLiveCode(row.threadId, row.itemId)
    .then(
      (open) => {
        // The open fence's keyframe follows; every other fence of the row
        // has ended, and a final push this client lacks was lost.
        if (rows.get(row.key) === row) stopOpenFences(row, open);
      },
      (error: unknown) => {
        console.warn('liveCodeSpans: resync failed', error);
        if (rows.get(row.key) === row) stopOpenFences(row, -1);
      },
    )
    .finally(() => {
      row.resyncing = false;
    });
}

/** Asks for a keyframe of every row with an open fence, after pushes may
 * have been withheld or lost: a reconnect, a background lease, a gap. */
export function resyncLiveCodeRows(backend?: BackendKey): void {
  for (const row of rows.values()) {
    if (backend !== undefined && row.backend !== backend) continue;
    for (const fence of row.fences.values()) {
      if (!fence.final) {
        requestResync(row);
        break;
      }
    }
  }
}

/** Drops the rows of threads this client no longer watches: their pushes
 * stop arriving, so their state could only go stale. */
export function retainLiveCodeThreads(watched: ReadonlySet<string>): void {
  for (const row of rows.values()) {
    if (watched.has(row.threadId)) continue;
    finished.delete(row.key);
    rows.delete(row.key);
  }
}

/** A host's own line-hash chain of its text, extended as the text grows. */
export class TextLineChain {
  #done: number[] = [];
  #hash = FNV_OFFSET_BASIS_32;
  #first = '';

  constructor(text: string) {
    this.append(text);
  }

  append(delta: string): void {
    if (this.#done.length === 0) {
      const end = delta.indexOf('\n');
      this.#first += end < 0 ? delta : delta.slice(0, end);
    }
    let hash = this.#hash;
    for (let i = 0; i < delta.length; i += 1) {
      const code = delta.charCodeAt(i);
      if (code === 10) this.#done.push(hash);
      hash = Math.imul(hash ^ code, FNV_PRIME_32) >>> 0;
    }
    this.#hash = hash;
  }

  reset(text: string): void {
    this.#done = [];
    this.#hash = FNV_OFFSET_BASIS_32;
    this.#first = '';
    this.append(text);
  }

  get lineCount(): number {
    return this.#done.length + 1;
  }

  /** The text's first line, whole or as far as it has arrived. */
  get firstLine(): string {
    return this.#first;
  }

  /** The chain entry of line i; the last line's covers the text so far. */
  at(i: number): number {
    return i < this.#done.length ? this.#done[i] : this.#hash;
  }
}

const FNV_OFFSET_BASIS_32 = 0x811c9dc5;
const FNV_PRIME_32 = 0x01000193;

/** Which of a fence's lines paint a host's text. */
export interface LiveCodeCover {
  readonly fence: LiveCodeFence;
  /** Leading host lines the fence's chain verifies. */
  readonly verified: number;
  /** Whether host line `verified` takes the fence's spans at that index,
   * clipped to its length: it is a prefix of the fence's line, or the
   * fence's line is a prefix of it. Past verified lines the relation follows
   * from the lines before it; on the first line the head proves it. */
  readonly clip: boolean;
  /** The clipped line is a prefix of the fence's line, so its spans color
   * all of it. */
  readonly whole: boolean;
  /** The fence is final and describes exactly the host's text. */
  readonly exact: boolean;
}

/** Leading lines on which the fence's chain and the host's agree. The
 * chains are cumulative, so agreement at a line implies it on every line
 * before it, and a binary search finds the boundary. */
function verifiedLines(fence: LiveCodeFence, chain: TextLineChain): number {
  const hashes = fence.lineHashes;
  let lo = 0;
  let hi = Math.min(hashes.length, chain.lineCount);
  while (lo < hi) {
    const mid = (lo + hi) >>> 1;
    if (hashes[mid] === chain.at(mid)) lo = mid + 1;
    else hi = mid;
  }
  return lo;
}

/** How one fence paints the host's text, or null when it does not. */
function coverOf(fence: LiveCodeFence, chain: TextLineChain, streaming: boolean): LiveCodeCover | null {
  if (fence.stopped) return null;
  const verified = verifiedLines(fence, chain);
  const hostLines = chain.lineCount;
  const fenceLines = fence.lineHashes.length;
  if (verified === hostLines) {
    return { fence, verified, clip: false, whole: false, exact: fence.final && verified === fenceLines };
  }
  if (verified < fenceLines) {
    // Host line `verified` differs from the fence's. It can still be a
    // prefix relation while one side's line is partial: a streaming host's
    // last line, or an open fence's last line.
    const hostPartial = streaming && verified === hostLines - 1;
    const fencePartial = !fence.final && verified === fenceLines - 1;
    if (!hostPartial && !fencePartial) return null;
    let whole = hostPartial && !fencePartial;
    if (verified === 0) {
      // Nothing proves the fence is this block's yet: its head does.
      const line = chain.firstLine;
      whole = hostPartial && fence.head.startsWith(line);
      if (!whole && !(fencePartial && line.startsWith(fence.head))) return null;
    }
    return { fence, verified, clip: true, whole, exact: false };
  }
  // The host has lines past every line of the fence: an open fence has not
  // reached them yet; a final fence is not this text.
  return fence.final || verified === 0 ? null : { fence, verified, clip: false, whole: false, exact: false };
}

/**
 * The fence of `row` that paints the most of the host's text in `lang`.
 * `held` is the fence the host painted from before, kept as a candidate
 * after its row is evicted. `streaming` says the host's last line may still
 * grow.
 */
export function coverLiveCode(
  row: LiveCodeRow | undefined,
  held: LiveCodeFence | undefined,
  lang: string,
  chain: TextLineChain,
  streaming: boolean,
): LiveCodeCover | null {
  let best: LiveCodeCover | null = null;
  const consider = (fence: LiveCodeFence): void => {
    if (fence.lang !== lang) return;
    const cover = coverOf(fence, chain, streaming);
    if (cover && (!best || cover.verified > best.verified)) best = cover;
  };
  if (row) for (const fence of row.fences.values()) consider(fence);
  if (held && row?.fences.get(held.index) !== held) consider(held);
  return best;
}

/** How many leading host lines a cover paints, the last maybe in part. */
export function coverPaints(cover: LiveCodeCover): number {
  return cover.fence.spanned ? cover.verified + (cover.clip ? 1 : 0) : 0;
}

/** How many leading host lines a cover colors in full. */
export function coverColorsWhole(cover: LiveCodeCover): number {
  return cover.fence.spanned ? cover.verified + (cover.clip && cover.whole ? 1 : 0) : 0;
}

/** The spans a cover paints on host line `index`. */
export function liveCoverSpans(cover: LiveCodeCover, index: number): EncodedLine | null {
  if (index < cover.verified || (cover.clip && index === cover.verified)) {
    return cover.fence.lines[index] ?? null;
  }
  return null;
}

/** The row a markdown surface renders, for its code hosts. */
export interface LiveCodeRowRef {
  threadId: string;
  itemId: string;
}

const CONTEXT_KEY = Symbol('liveCodeRow');

/** Names the streaming row whose code the surface's hosts render. */
export function setLiveCodeRowContext(get: () => LiveCodeRowRef | undefined): void {
  setContext(CONTEXT_KEY, get);
}

export function getLiveCodeRowContext(): (() => LiveCodeRowRef | undefined) | undefined {
  return getContext<(() => LiveCodeRowRef | undefined) | undefined>(CONTEXT_KEY);
}

export function resetLiveCodeSpansForTest(): void {
  rows.clear();
  finished.clear();
}

/** Test-only inspection. */
export function __liveCodeSpanStatsForTest(): { rows: number; finished: number } {
  return { rows: rows.size, finished: finished.size };
}
