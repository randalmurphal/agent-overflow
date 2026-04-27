import { GetPayloadChunk, GetPayloadPreview } from '../../stores/bindings';

export const DEFAULT_PAYLOAD_PREVIEW_BYTES = 32 * 1024;
export const DEFAULT_PAYLOAD_CHUNK_BYTES = 256 * 1024;

export interface PayloadExpansionHandle {
  readonly expanded: boolean;
  readonly loading: boolean;
  readonly error: string | null;
  readonly previewData: string | null;
  readonly fullData: string | null;
  readonly totalSize: number;
  readonly isComplete: boolean;
  readonly hasMore: boolean;
  /**
   * Raw payload bytes. Used for clipboard copy, save-to-file, and
   * any caller that needs to run a transform before rendering.
   */
  readonly displayData: string | null;
  toggle(): Promise<void>;
  expand(): Promise<void>;
  collapse(): void;
  showFull(): Promise<void>;
  reset(): void;
}

type PayloadIDSource = string | undefined | (() => string | undefined);
type ThreadIDSource = string | undefined | (() => string | undefined);

export function createPayloadExpansion(
  payloadID: PayloadIDSource,
  threadID: ThreadIDSource,
  previewBytes = DEFAULT_PAYLOAD_PREVIEW_BYTES,
  chunkBytes = DEFAULT_PAYLOAD_CHUNK_BYTES,
): PayloadExpansionHandle {
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
  }

  async function loadPreview(): Promise<void> {
    const id = currentPayloadID();
    const ownerThreadID = currentThreadID();
    if (!id || chunks.length > 0) return;
    if (!ownerThreadID) {
      error = 'Missing thread context for payload read';
      return;
    }

    const generation = cancelInflight();
    loadingPreview = true;
    loadingFull = false;
    error = null;

    try {
      const result = await GetPayloadPreview(ownerThreadID, id, previewBytes);
      if (generation !== requestGeneration || !expanded) return;
      chunks = [result.data];
      hasFullChunks = false;
      totalSize = result.totalSize;
      isComplete = result.isComplete;
      loadedBytes = result.nextOffset;
    } catch (err) {
      if (generation !== requestGeneration || !expanded) return;
      error = err instanceof Error ? err.message : String(err);
    } finally {
      if (generation === requestGeneration) {
        loadingPreview = false;
      }
    }
  }

  async function expand(): Promise<void> {
    if (expanded) return;
    expanded = true;
    await loadPreview();
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
      const content = await GetPayloadChunk(ownerThreadID, id, offset, chunkBytes);
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
    get hasMore() {
      return expanded && chunks.length > 0 && !isComplete;
    },
    get displayData() { return displayData; },
    toggle,
    expand,
    collapse,
    showFull,
    reset,
  };
}

export function formatPayloadSize(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}
