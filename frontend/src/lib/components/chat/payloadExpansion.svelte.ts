import { GetPayloadChunk, GetPayloadPreview } from '../../stores/bindings';

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

export interface PayloadExpansionOptions {
  previewBytes?: number;
  chunkBytes?: number;
  requestTimeoutMs?: number;
  payloadVersion?: PayloadVersionSource;
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

  let expanded = $state(false);
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
  let loadedPayloadID: string | null = null;
  let loadedPayloadVersion: unknown;
  let overridePayloadVersion = $state<unknown>(undefined);
  let hasOverridePayloadVersion = $state(false);

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

  function setPayloadVersion(version: unknown): void {
    overridePayloadVersion = version;
    hasOverridePayloadVersion = true;
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
  }

  async function loadPreview(): Promise<boolean> {
    const id = currentPayloadID();
    const ownerThreadID = currentThreadID();
    if (!id) return false;
    const version = currentPayloadVersion();
    if (chunks.length > 0 && loadedPayloadID === id && Object.is(loadedPayloadVersion, version)) {
      return false;
    }
    if (chunks.length > 0) clearLoadedData();
    if (!ownerThreadID) {
      error = 'Missing thread context for payload read';
      return false;
    }

    const generation = cancelInflight();
    loadingPreview = true;
    loadingFull = false;
    error = null;

    try {
      const result = await withTimeout(
        GetPayloadPreview(ownerThreadID, id, previewBytes),
        requestTimeoutMs,
        'Loading payload preview timed out',
      );
      if (generation !== requestGeneration || !expanded) return false;
      chunks = [result.data];
      hasFullChunks = false;
      totalSize = result.totalSize;
      isComplete = result.isComplete;
      loadedBytes = result.nextOffset;
      loadedPayloadID = id;
      loadedPayloadVersion = version;
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
  }

  async function expand(): Promise<void> {
    if (!expanded) expanded = true;
    await ensureLoaded();
  }

  async function ensureLoaded(): Promise<boolean> {
    if (!expanded || loadingPreview || loadingFull || error !== null) return false;
    return loadPreview();
  }

  function collapse(): void {
    expanded = false;
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
    if (!ownerThreadID) {
      error = 'Missing thread context for payload read';
      return;
    }

    const generation = cancelInflight();
    loadingPreview = false;
    loadingFull = true;
    error = null;

    try {
      const offset = loadedBytes;
      const content = await withTimeout(
        GetPayloadChunk(ownerThreadID, id, offset, chunkBytes),
        requestTimeoutMs,
        'Loading payload chunk timed out',
      );
      if (generation !== requestGeneration || !expanded) return;
      // Append the chunk to the buffer instead of re-allocating a
      // cumulative string. Writing `[...chunks, ...]` produces a
      // new array reference so $derived re-evaluates `displayData`.
      chunks = [...chunks, content.data];
      hasFullChunks = true;
      totalSize = content.totalSize;
      loadedBytes = content.nextOffset;
      isComplete = content.isComplete;
    } catch (err) {
      if (generation !== requestGeneration || !expanded) return;
      error = err instanceof Error ? err.message : String(err);
    } finally {
      if (generation === requestGeneration) {
        loadingFull = false;
      }
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
  if (value.length <= 128) return value;
  return `${value.length}:${value.slice(0, 64)}:${value.slice(-64)}`;
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

export function formatPayloadSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
