import { GetPayloadData, GetPayloadPreview } from '../../stores/bindings';

export const DEFAULT_PAYLOAD_PREVIEW_BYTES = 32 * 1024;

export interface PayloadExpansionHandle {
  readonly expanded: boolean;
  readonly loading: boolean;
  readonly error: string | null;
  readonly previewData: string | null;
  readonly fullData: string | null;
  readonly totalSize: number;
  readonly isComplete: boolean;
  readonly hasMore: boolean;
  readonly displayData: string | null;
  toggle(): Promise<void>;
  expand(): Promise<void>;
  collapse(): void;
  showFull(): Promise<void>;
  reset(): void;
}

type PayloadIDSource = string | undefined | (() => string | undefined);

export function createPayloadExpansion(
  payloadID: PayloadIDSource,
  previewBytes = DEFAULT_PAYLOAD_PREVIEW_BYTES,
): PayloadExpansionHandle {
  let expanded = $state(false);
  let loadingPreview = $state(false);
  let loadingFull = $state(false);
  let error = $state<string | null>(null);
  let previewData = $state<string | null>(null);
  let fullData = $state<string | null>(null);
  let totalSize = $state(0);
  let isComplete = $state(true);

  let requestGeneration = 0;

  function currentPayloadID(): string | undefined {
    return typeof payloadID === 'function' ? payloadID() : payloadID;
  }

  function cancelInflight(): number {
    requestGeneration += 1;
    return requestGeneration;
  }

  function clearLoadedData(): void {
    error = null;
    previewData = null;
    fullData = null;
    totalSize = 0;
    isComplete = true;
  }

  async function loadPreview(): Promise<void> {
    const id = currentPayloadID();
    if (!id || previewData !== null || fullData !== null) return;

    const generation = cancelInflight();
    loadingPreview = true;
    loadingFull = false;
    error = null;

    try {
      const result = await GetPayloadPreview(id, previewBytes);
      if (generation !== requestGeneration || !expanded) return;
      previewData = result.data;
      totalSize = result.totalSize;
      isComplete = result.isComplete;
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
    if (!expanded || !id || fullData !== null || isComplete) return;

    const generation = cancelInflight();
    loadingPreview = false;
    loadingFull = true;
    error = null;

    try {
      const data = await GetPayloadData(id);
      if (generation !== requestGeneration || !expanded) return;
      fullData = data;
      totalSize = Math.max(totalSize, data.length);
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
    get previewData() { return previewData; },
    get fullData() { return fullData; },
    get totalSize() { return totalSize; },
    get isComplete() { return isComplete; },
    get hasMore() {
      return expanded && previewData !== null && !isComplete && fullData === null;
    },
    get displayData() { return fullData ?? previewData; },
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
