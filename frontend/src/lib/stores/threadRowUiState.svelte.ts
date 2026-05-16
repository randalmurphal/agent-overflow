import type { Item } from '../types/models';
import { payloadVersionForItem } from '../utils/payloadVersion';
import {
  createPayloadExpansion,
  type PayloadExpansionHandle,
  type PayloadExpansionOptions,
} from '../utils/payloadExpansion.svelte';
import type {
  AttachmentPreviewCache,
  ImagePreviewItem,
} from '../utils/attachmentPreview.svelte';

interface ThreadRowUiStateOptions {
  getItemById(itemId: string): Item | undefined;
}

export interface ThreadRowUiState {
  expansionStateFor(
    item: Item,
    options?: RowExpansionStateOptions,
  ): PayloadExpansionHandle;
  expansionStateForPayload(
    payloadId: string,
    threadId: string,
    options?: PayloadExpansionStateOptions | unknown,
  ): PayloadExpansionHandle;
  isSubagentGroupExpanded(groupKey: string): boolean;
  toggleSubagentGroupExpanded(groupKey: string): boolean;
  attachmentCacheFor(itemId: string): AttachmentPreviewCache;
  clear(): void;
}

export interface RowExpansionStateOptions
  extends Pick<
    PayloadExpansionOptions,
    'loadMode' | 'loadOnMount' | 'previewBytes' | 'chunkBytes' | 'requestTimeoutMs'
  > {
  /**
   * Disambiguates independent payload consumers for the same item. Most rows
   * have one payload body and can use the default; rows with multiple payload
   * interpretations should give each one a stable key.
   */
  stateKey?: string;
  /**
   * Version used for cache invalidation and remount-safe auto-loads. The
   * default comes from `payloadVersionForItem`, which prefers explicit
   * payload signatures before falling back to ids/meta/updatedAt. Callers
   * with richer row-local signatures can still provide one so UI-only item
   * changes do not invalidate loaded payload content.
   */
  payloadVersion?: (item: Item | undefined) => unknown;
}

export interface PayloadExpansionStateOptions
  extends Pick<
    PayloadExpansionOptions,
    'loadMode' | 'loadOnMount' | 'previewBytes' | 'chunkBytes' | 'requestTimeoutMs'
  > {
  stateKey?: string;
  payloadVersion?: unknown;
}

function normalizePayloadExpansionStateOptions(
  optionsOrPayloadVersion: PayloadExpansionStateOptions | unknown,
): PayloadExpansionStateOptions {
  if (
    optionsOrPayloadVersion
    && typeof optionsOrPayloadVersion === 'object'
    && (
      'payloadVersion' in optionsOrPayloadVersion
      || 'stateKey' in optionsOrPayloadVersion
      || 'loadMode' in optionsOrPayloadVersion
      || 'loadOnMount' in optionsOrPayloadVersion
      || 'previewBytes' in optionsOrPayloadVersion
      || 'chunkBytes' in optionsOrPayloadVersion
      || 'requestTimeoutMs' in optionsOrPayloadVersion
    )
  ) {
    return optionsOrPayloadVersion as PayloadExpansionStateOptions;
  }
  return { payloadVersion: optionsOrPayloadVersion };
}

function expansionRegistryKey(parts: readonly unknown[]): string {
  return JSON.stringify(parts);
}

/**
 * Per-row UI registries live outside row components so virtua remounts
 * do not drop loaded payload chunks, attachment thumbnails, or group
 * expansion state while the user scrolls around a thread.
 */
export function createThreadRowUiState(options: ThreadRowUiStateOptions): ThreadRowUiState {
  const expansionStates = new Map<string, PayloadExpansionHandle>();
  let subagentGroupExpanded: Set<string> = $state(new Set());
  const attachmentBlobs = new Map<string, Map<string, ImagePreviewItem>>();
  let attachmentCacheEpoch = 0;

  function expansionStateFor(
    item: Item,
    rowOptions: RowExpansionStateOptions = {},
  ): PayloadExpansionHandle {
    const loadMode = rowOptions.loadMode ?? 'preview';
    const key = expansionRegistryKey([
      'i',
      item.id,
      loadMode,
      rowOptions.loadOnMount ? 'auto' : 'manual',
      rowOptions.stateKey ?? 'default',
      rowOptions.previewBytes ?? 'preview-default',
      rowOptions.chunkBytes ?? 'chunk-default',
      rowOptions.requestTimeoutMs ?? 'timeout-default',
    ]);
    let cached = expansionStates.get(key);
    if (cached) return cached;

    const itemId = item.id;
    const getCurrentItem = (): Item | undefined => options.getItemById(itemId);
    const currentPayloadVersion = rowOptions.payloadVersion ?? payloadVersionForItem;
    cached = createPayloadExpansion(
      () => getCurrentItem()?.payloadId,
      () => getCurrentItem()?.threadId,
      {
        payloadVersion: () => currentPayloadVersion(getCurrentItem()),
        loadMode,
        loadOnMount: rowOptions.loadOnMount,
        previewBytes: rowOptions.previewBytes,
        chunkBytes: rowOptions.chunkBytes,
        requestTimeoutMs: rowOptions.requestTimeoutMs,
      },
    );
    expansionStates.set(key, cached);
    return cached;
  }

  function expansionStateForPayload(
    payloadId: string,
    threadId: string,
    optionsOrPayloadVersion?: PayloadExpansionStateOptions | unknown,
  ): PayloadExpansionHandle {
    const payloadOptions = normalizePayloadExpansionStateOptions(optionsOrPayloadVersion);
    const loadMode = payloadOptions.loadMode ?? 'preview';
    const key = expansionRegistryKey([
      'p',
      threadId,
      payloadId,
      loadMode,
      payloadOptions.loadOnMount ? 'auto' : 'manual',
      payloadOptions.stateKey ?? 'default',
      payloadOptions.previewBytes ?? 'preview-default',
      payloadOptions.chunkBytes ?? 'chunk-default',
      payloadOptions.requestTimeoutMs ?? 'timeout-default',
    ]);
    let cached = expansionStates.get(key);
    if (cached) {
      cached.setPayloadVersion(payloadOptions.payloadVersion);
      return cached;
    }

    cached = createPayloadExpansion(
      () => payloadId,
      () => threadId,
      {
        payloadVersion: payloadOptions.payloadVersion,
        loadMode,
        loadOnMount: payloadOptions.loadOnMount,
        previewBytes: payloadOptions.previewBytes,
        chunkBytes: payloadOptions.chunkBytes,
        requestTimeoutMs: payloadOptions.requestTimeoutMs,
      },
    );
    expansionStates.set(key, cached);
    return cached;
  }

  function isSubagentGroupExpanded(groupKey: string): boolean {
    return subagentGroupExpanded.has(groupKey);
  }

  function toggleSubagentGroupExpanded(groupKey: string): boolean {
    const next = new Set(subagentGroupExpanded);
    const willExpand = !next.has(groupKey);
    if (willExpand) {
      next.add(groupKey);
    } else {
      next.delete(groupKey);
    }
    subagentGroupExpanded = next;
    return willExpand;
  }

  function attachmentCacheFor(itemId: string): AttachmentPreviewCache {
    const cacheEpoch = attachmentCacheEpoch;
    let inner = attachmentBlobs.get(itemId);
    if (!inner) {
      inner = new Map<string, ImagePreviewItem>();
      attachmentBlobs.set(itemId, inner);
    }

    const innerRef = inner;
    return {
      get(attachmentId: string): ImagePreviewItem | undefined {
        if (cacheEpoch !== attachmentCacheEpoch) return undefined;
        return innerRef.get(attachmentId);
      },
      set(attachmentId: string, preview: ImagePreviewItem): void {
        if (cacheEpoch !== attachmentCacheEpoch) {
          revokePreview(preview);
          return;
        }
        innerRef.set(attachmentId, preview);
      },
    };
  }

  function revokePreview(preview: ImagePreviewItem): void {
    if (preview.url.startsWith('blob:')) URL.revokeObjectURL(preview.url);
  }

  function disposeAttachmentBlobs(): void {
    for (const inner of attachmentBlobs.values()) {
      for (const preview of inner.values()) {
        revokePreview(preview);
      }
      inner.clear();
    }
    attachmentBlobs.clear();
  }

  function clear(): void {
    expansionStates.clear();
    subagentGroupExpanded = new Set();
    attachmentCacheEpoch += 1;
    disposeAttachmentBlobs();
  }

  return {
    expansionStateFor,
    expansionStateForPayload,
    isSubagentGroupExpanded,
    toggleSubagentGroupExpanded,
    attachmentCacheFor,
    clear,
  };
}
