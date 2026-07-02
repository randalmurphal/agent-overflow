import { GetPayloadChunk, GetPayloadData, GetPayloadPreview } from '../stores/bindings';
import { payloadVersionKey, readPayloadCache, writePayloadCache } from './payloadDataCache';
import { boundedPayloadVersionString } from './payloadVersion';
import { revealedSuffix } from './textOverlap';

export const DEFAULT_PAYLOAD_PREVIEW_BYTES = 32 * 1024;
export const DEFAULT_PAYLOAD_CHUNK_BYTES = 256 * 1024;
export const DEFAULT_PAYLOAD_REQUEST_TIMEOUT_MS = 35_000;

export interface PayloadExpansionHandle {
  readonly expanded: boolean;
  readonly loading: boolean;
  readonly error: string | null;
  readonly previewData: string | null;
  readonly fullData: string | null;
  readonly totalSize: number;
  readonly isComplete: boolean;
  readonly hasMore: boolean;
  readonly payloadVersion: unknown;
  /**
   * Raw payload bytes. Used for clipboard copy, save-to-file, and
   * any caller that needs to run a transform before rendering.
   */
  readonly displayData: string | null;
  /**
   * Append a provider delta into an already-expanded full payload.
   * The delta is ignored while collapsed, queued while the initial full
   * load is still pending, and applied only to the currently loaded
   * payload id. Preview-only handles do not accept live appends.
   * `previousLiveTail` lets a newly-expanded streaming payload repair a
   * stale initial snapshot using only the already-bounded row summary.
   */
  appendLiveDelta(delta: string, payloadVersion?: unknown, previousLiveTail?: string): void;
  toggle(): Promise<void>;
  expand(): Promise<void>;
  ensureLoaded(): Promise<boolean>;
  collapse(): void;
  showFull(): Promise<void>;
  retry(): Promise<void>;
  reset(): void;
  setPayloadVersion(version: unknown): void;
}

type PayloadIDSource = string | undefined | (() => string | undefined);
type ThreadIDSource = string | undefined | (() => string | undefined);
type PayloadVersionSource = unknown | (() => unknown);
type PayloadAvailableSource = boolean | (() => boolean);
type PayloadExpansionSource = PayloadExpansionHandle | (() => PayloadExpansionHandle);
type PayloadCacheEnabledSource = boolean | (() => boolean);

export interface PayloadExpansionOptions {
  previewBytes?: number;
  chunkBytes?: number;
  requestTimeoutMs?: number;
  payloadVersion?: PayloadVersionSource;
  loadMode?: 'preview' | 'full';
  /**
   * Controls the module-level payload cache. Most payloads are immutable
   * once linked to an item and should use the default cache. Live payloads
   * that grow behind a stable id, such as streaming thinking blocks, should
   * disable caching until they settle so collapse/re-expand fetches the
   * current body rather than an earlier snapshot.
   */
  cacheEnabled?: PayloadCacheEnabledSource;
  /**
   * When true, the expansion auto-expands as soon as the payloadID is
   * available — at construction if already set, otherwise via an
   * internal $effect when it arrives. Used by callers like
   * DiffFileStack whose body always renders open. Combined with the
   * module-level payload cache, a cache-hit at construction time
   * produces a fully-rendered row at first paint (no empty-then-loaded
   * flash on thread re-entry). Default false — toggle-style consumers
   * keep their explicit expand-on-click contract.
   */
  loadOnMount?: boolean;
}

export function createPayloadExpansion(
  payloadID: PayloadIDSource,
  threadID: ThreadIDSource,
  options: PayloadExpansionOptions = {},
): PayloadExpansionHandle {
  const previewBytes = options.previewBytes ?? DEFAULT_PAYLOAD_PREVIEW_BYTES;
  const chunkBytes = options.chunkBytes ?? DEFAULT_PAYLOAD_CHUNK_BYTES;
  const requestTimeoutMs = options.requestTimeoutMs ?? DEFAULT_PAYLOAD_REQUEST_TIMEOUT_MS;
  const payloadVersion = options.payloadVersion;
  const loadMode = options.loadMode ?? 'preview';
  const cacheEnabled = options.cacheEnabled;

  let expanded = $state(false);

  // Single write path for the open/closed flag: every transition
  // (expand, collapse, load-on-mount hydrate) funnels through one
  // change-guarded assignment.
  function setExpandedFlag(next: boolean): void {
    if (expanded === next) return;
    expanded = next;
  }
  let loadingPreview = $state(false);
  let loadingFull = $state(false);
  let error = $state<string | null>(null);
  // String accumulator. The first segment is the preview; further
  // segments are appended by `showFull`. Joining is deferred to
  // `displayData` (a $derived) so multi-chunk fetches don't pay the
  // O(N²) cost of re-concatenating the cumulative string per fetch.
  let chunks: string[] = $state([]);
  let hasFullChunks = $state(false);
  let totalSize = $state(0);
  let isComplete = $state(true);
  let loadedBytes = $state(0);

  let requestGeneration = 0;
  let activePreviewLoad: Promise<boolean> | null = null;
  let activeFullLoad: Promise<void> | null = null;
  let loadedPayloadID: string | null = null;
  let loadedPayloadVersion: unknown;
  let pendingLiveDeltas: Array<{
    payloadID: string;
    delta: string;
    payloadVersion: unknown;
    previousLiveTail?: string;
  }> = [];
  let overridePayloadVersion = $state<unknown>(undefined);
  let hasOverridePayloadVersion = $state(false);

  // Synchronous cache hydration. Runs during construction so the very
  // first paint after mount sees `chunks` populated, and the row that
  // hosts this expansion renders at its full height from frame 0
  // instead of the empty-then-loaded oscillation that breaks
  // virtua's per-row size cache on thread re-entry. The corresponding
  // write happens after a successful fetch in `loadPreview` /
  // `showFull`.
  //
  // We only flip `expanded = true` on cache hit when `loadOnMount` is
  // set. Otherwise we hydrate the data without changing the open/closed
  // intent — toggle-style consumers (GenericToolCallRow, ThinkingBlock)
  // expect their thread-switch reset of `expanded=false` to survive the
  // cache hit; the data simply flashes in instantly when the user later
  // clicks expand.
  hydrateFromCache({ expandOnHit: options.loadOnMount === true });

  // loadOnMount: drive expand() as soon as a payloadID becomes
  // available. Replaces the per-consumer `$effect(() => {
  // if (!payloadId) return; void expansion.expand(); })` boilerplate
  // and lets the synchronous cache check above own setting `expanded`
  // before the first paint. On cache hit, expand() short-circuits at
  // the loadPreview early-return because chunks are already populated.
  //
  // The fire-once-per-content guard (`loadOnMountInvokedFor`) keeps the
  // effect from re-arming if a future consumer ever exposes a collapse
  // path: the user collapses → `expanded = false` flips → without the
  // guard this effect would re-fire on the next reactive tick and
  // resurrect `expanded = true` against the user's intent. Tracking by
  // payloadID plus version means a genuine content replacement DOES
  // re-fire even when the backend keeps the same payloadID.
  let loadOnMountInvokedFor: string | null = null;
  if (options.loadOnMount) {
    $effect(() => {
      const id = currentPayloadID();
      if (!id) return;
      const invokeKey = `${id}\0${payloadVersionKey(currentPayloadVersion())}`;
      if (invokeKey === loadOnMountInvokedFor) return;
      if (loadingPreview || loadingFull) return;
      loadOnMountInvokedFor = invokeKey;
      void expand();
    });
  }

  // $derived caches the join — re-runs only when `chunks` actually
  // changes (Svelte 5's reactivity, not on every read).
  const displayData = $derived.by<string | null>(() => {
    if (chunks.length === 0) return null;
    if (chunks.length === 1) return chunks[0] ?? null;
    return chunks.join('');
  });

  function currentPayloadID(): string | undefined {
    return typeof payloadID === 'function' ? payloadID() : payloadID;
  }

  function currentThreadID(): string | undefined {
    return typeof threadID === 'function' ? threadID() : threadID;
  }

  function currentPayloadVersion(): unknown {
    if (hasOverridePayloadVersion) return overridePayloadVersion;
    const version = typeof payloadVersion === 'function' ? payloadVersion() : payloadVersion;
    return version;
  }

  function currentCacheEnabled(): boolean {
    if (cacheEnabled === undefined) return true;
    return typeof cacheEnabled === 'function' ? cacheEnabled() : cacheEnabled;
  }

  function setPayloadVersion(version: unknown): void {
    if (Object.is(currentPayloadVersion(), version)) return;
    overridePayloadVersion = version;
    hasOverridePayloadVersion = true;
    cancelInflight();
    activePreviewLoad = null;
    activeFullLoad = null;
    loadingPreview = false;
    loadingFull = false;
    clearLoadedData();
    hydrateFromCache({ expandOnHit: false });
  }

  function cancelInflight(): number {
    requestGeneration += 1;
    return requestGeneration;
  }

  function clearLoadedData(): void {
    error = null;
    chunks = [];
    hasFullChunks = false;
    totalSize = 0;
    isComplete = true;
    loadedBytes = 0;
    loadedPayloadID = null;
    loadedPayloadVersion = undefined;
    pendingLiveDeltas = [];
  }

  function hydrateFromCache(opts: { expandOnHit: boolean }): boolean {
    if (!currentCacheEnabled()) return false;
    const id = currentPayloadID();
    const tid = currentThreadID();
    if (!id || !tid) return false;
    const version = currentPayloadVersion();
    const cached = readPayloadCache(tid, id, version);
    if (!cached) return false;
    chunks = cached.chunks;
    hasFullChunks = cached.hasFullChunks;
    totalSize = cached.totalSize;
    isComplete = cached.isComplete;
    loadedBytes = cached.loadedBytes;
    loadedPayloadID = id;
    loadedPayloadVersion = version;
    error = null;
    if (opts.expandOnHit) setExpandedFlag(true);
    return true;
  }

  async function loadPreview(): Promise<boolean> {
    const id = currentPayloadID();
    const ownerThreadID = currentThreadID();
    if (!id) return false;
    const version = currentPayloadVersion();
    if (chunks.length > 0 && loadedPayloadID === id && Object.is(loadedPayloadVersion, version)) {
      return false;
    }
    if (hydrateFromCache({ expandOnHit: false })) return false;
    // Only drop loaded content when the payload identity actually changes. A
    // same-id reload — e.g. a thinking row's streaming->settled version relabel,
    // which carries identical bytes — keeps its current chunks visible and lets
    // the fetch below overwrite them in place (no null frame). Clearing here
    // would blink the expanded body to empty for a frame, collapsing its height
    // and clamping the timeline scrollTop: the reported "expanded block jumps to
    // the top, then springs back to the bottom when the next item arrives".
    if (chunks.length > 0 && loadedPayloadID !== id) clearLoadedData();
    if (!ownerThreadID) {
      error = 'Missing thread context for payload read';
      return false;
    }

    const generation = cancelInflight();
    loadingPreview = true;
    loadingFull = false;
    error = null;

    const request = (async (): Promise<boolean> => {
      try {
        const result = await loadInitialPayload(ownerThreadID, id);
        if (
          generation !== requestGeneration
          || !expanded
          || !Object.is(currentPayloadVersion(), version)
        ) return false;
        chunks = [result.data];
        hasFullChunks = loadMode === 'full';
        totalSize = result.totalSize;
        isComplete = result.isComplete;
        loadedBytes = result.nextOffset;
        loadedPayloadID = id;
        loadedPayloadVersion = version;
        replayPendingLiveDeltas();
        writeLoadedPayloadCache(ownerThreadID, id, version);
        return true;
      } catch (err) {
        if (generation !== requestGeneration || !expanded) return false;
        error = err instanceof Error ? err.message : String(err);
      } finally {
        if (generation === requestGeneration) {
          loadingPreview = false;
        }
      }
      return false;
    })();
    activePreviewLoad = request;
    try {
      return await request;
    } finally {
      if (activePreviewLoad === request) activePreviewLoad = null;
    }
  }

  async function loadInitialPayload(ownerThreadID: string, id: string): Promise<{
    data: string;
    nextOffset: number;
    totalSize: number;
    isComplete: boolean;
  }> {
    if (loadMode === 'full') {
      const result = await withTimeout(
        GetPayloadData(ownerThreadID, id),
        requestTimeoutMs,
        'Loading payload timed out',
      );
      const data = payloadTextFromBindingData(result.data, 'GetPayloadData');
      return {
        data,
        nextOffset: data.length,
        totalSize: data.length,
        isComplete: true,
      };
    }
    const preview = await withTimeout(
      GetPayloadPreview(ownerThreadID, id, previewBytes),
      requestTimeoutMs,
      'Loading payload preview timed out',
    );
    return {
      ...preview,
      data: payloadTextFromBindingData(preview.data, 'GetPayloadPreview'),
    };
  }

  async function expand(): Promise<void> {
    setExpandedFlag(true);
    await ensureLoaded();
  }

  async function ensureLoaded(): Promise<boolean> {
    if (!expanded || error !== null) return false;
    if (loadingPreview || loadingFull) {
      await activePreviewLoad;
      await activeFullLoad;
      if (!expanded || error !== null) return false;
    }
    return loadPreview();
  }

  function writeLoadedPayloadCache(ownerThreadID: string, id: string, version: unknown): void {
    if (!currentCacheEnabled()) return;
    writePayloadCache(ownerThreadID, id, version, {
      chunks,
      hasFullChunks,
      totalSize,
      isComplete,
      loadedBytes,
    });
  }

  function appendLoadedLiveDelta(delta: string, nextPayloadVersion: unknown): void {
    if (!delta) {
      loadedPayloadVersion = nextPayloadVersion;
      return;
    }
    chunks = [...chunks, delta];
    totalSize += delta.length;
    loadedBytes += delta.length;
    isComplete = true;
    loadedPayloadVersion = nextPayloadVersion;
  }

  // Merge the smoother's freshly-revealed text into the loaded body.
  // `previousLiveTail + delta` is everything the smoother has revealed through
  // this delta; `displayData` is what we already hold (the flushed
  // GetPayloadData snapshot plus prior live appends). Both are prefixes of the
  // SAME canonical payload — a streaming thinking row is seeded with an empty
  // summary and fed full provider deltas (stream_items.go blanks the
  // block-start summary and ships evt.Content per delta), so each revealed
  // window is untrimmed text from offset 0. revealedSuffix (textOverlap.ts)
  // owns the containment-aware merge and documents why the prefix guard is
  // load-bearing here (flush-before-read leaves the snapshot ahead of the
  // reveal) and where it stops being exact (reconnect interior windows).
  function appendRevealedSuffix(
    previousLiveTail: string | undefined,
    delta: string,
    nextPayloadVersion: unknown,
  ): void {
    const revealed = (previousLiveTail ?? '') + delta;
    appendLoadedLiveDelta(revealedSuffix(displayData ?? '', revealed), nextPayloadVersion);
  }

  function replayPendingLiveDeltas(): void {
    if (pendingLiveDeltas.length === 0) return;
    const pending = pendingLiveDeltas;
    pendingLiveDeltas = [];
    for (const live of pending) {
      if (live.payloadID !== loadedPayloadID) continue;
      appendRevealedSuffix(live.previousLiveTail, live.delta, live.payloadVersion);
    }
  }

  function appendLiveDelta(
    delta: string,
    nextPayloadVersion: unknown = currentPayloadVersion(),
    previousLiveTail?: string,
  ): void {
    if (!delta || !expanded || error !== null) return;
    const id = currentPayloadID();
    if (!id) return;
    if (chunks.length === 0 || loadingPreview || loadingFull) {
      pendingLiveDeltas.push({
        payloadID: id,
        delta,
        payloadVersion: nextPayloadVersion,
        previousLiveTail,
      });
      return;
    }
    if (loadedPayloadID !== id) return;
    if (!hasFullChunks) return;
    appendRevealedSuffix(previousLiveTail, delta, nextPayloadVersion);
  }

  function collapse(): void {
    setExpandedFlag(false);
    loadingPreview = false;
    loadingFull = false;
    cancelInflight();
    clearLoadedData();
  }

  async function toggle(): Promise<void> {
    if (expanded) {
      collapse();
      return;
    }
    await expand();
  }

  async function showFull(): Promise<void> {
    const id = currentPayloadID();
    const ownerThreadID = currentThreadID();
    if (!expanded || !id || isComplete) return;
    const version = currentPayloadVersion();
    if (!ownerThreadID) {
      error = 'Missing thread context for payload read';
      return;
    }

    const generation = cancelInflight();
    loadingPreview = false;
    loadingFull = true;
    error = null;

    const request = (async (): Promise<void> => {
      try {
        const offset = loadedBytes;
        const rawContent = await withTimeout(
          GetPayloadChunk(ownerThreadID, id, offset, chunkBytes),
          requestTimeoutMs,
          'Loading payload chunk timed out',
        );
        const content = {
          ...rawContent,
          data: payloadTextFromBindingData(rawContent.data, 'GetPayloadChunk'),
        };
        if (
          generation !== requestGeneration
          || !expanded
          || !Object.is(currentPayloadVersion(), version)
        ) return;
        // Append the chunk to the buffer instead of re-allocating a
        // cumulative string. Writing `[...chunks, ...]` produces a
        // new array reference so $derived re-evaluates `displayData`.
        chunks = [...chunks, content.data];
        hasFullChunks = true;
        totalSize = content.totalSize;
        loadedBytes = content.nextOffset;
        isComplete = content.isComplete;
        writeLoadedPayloadCache(ownerThreadID, id, version);
      } catch (err) {
        if (generation !== requestGeneration || !expanded) return;
        error = err instanceof Error ? err.message : String(err);
      } finally {
        if (generation === requestGeneration) {
          loadingFull = false;
        }
      }
    })();
    activeFullLoad = request;
    try {
      await request;
    } finally {
      if (activeFullLoad === request) activeFullLoad = null;
    }
  }

  function reset(): void {
    collapse();
  }

  async function retry(): Promise<void> {
    if (!expanded) {
      await expand();
      return;
    }
    clearLoadedData();
    await loadPreview();
  }

  return {
    get expanded() { return expanded; },
    get loading() { return loadingPreview || loadingFull; },
    get error() { return error; },
    // previewData/fullData are exposed for callers that distinguish
    // "preview-only" from "fully loaded" (e.g. testid switching in
    // LazyContentBlock). They derive directly from the chunk buffer
    // so tests + components can tell which state we're in without
    // needing a separate flag.
    get previewData() {
      return hasFullChunks ? null : (chunks[0] ?? null);
    },
    get fullData() {
      return hasFullChunks ? displayData : null;
    },
    get totalSize() { return totalSize; },
    get isComplete() { return isComplete; },
    get payloadVersion() { return currentPayloadVersion(); },
    get hasMore() {
      return expanded && chunks.length > 0 && !isComplete;
    },
    get displayData() { return displayData; },
    appendLiveDelta,
    toggle,
    expand,
    ensureLoaded,
    collapse,
    showFull,
    retry,
    reset,
    setPayloadVersion,
  };
}

export function keepExpandedPayloadFresh(
  expansion: PayloadExpansionSource,
  hasPayload: PayloadAvailableSource,
): void {
  $effect(() => {
    const handle = typeof expansion === 'function' ? expansion() : expansion;
    const available = typeof hasPayload === 'function' ? hasPayload() : hasPayload;
    void handle.payloadVersion;
    if (!available || !handle.expanded || handle.loading || handle.error !== null) return;
    void handle.ensureLoaded();
  });
}

export function compactPayloadVersion(value: string | undefined): string {
  if (!value) return '';
  return boundedPayloadVersionString(value);
}

async function withTimeout<T>(
  promise: Promise<T>,
  timeoutMs: number,
  message: string,
): Promise<T> {
  if (timeoutMs <= 0) return promise;
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      promise,
      new Promise<T>((_, reject) => {
        timer = setTimeout(() => reject(new Error(message)), timeoutMs);
      }),
    ]);
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}

function payloadTextFromBindingData(data: unknown, operation: string): string {
  if (typeof data === 'string') return data;
  const kind = Array.isArray(data) ? 'array' : data === null ? 'null' : typeof data;
  throw new Error(`${operation} returned non-string payload data (${kind})`);
}

export function formatPayloadSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
