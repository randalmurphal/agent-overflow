import type { Item } from '../types/models';
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
    options?: Pick<PayloadExpansionOptions, 'loadMode'>,
  ): PayloadExpansionHandle;
  expansionStateForPayload(
    payloadId: string,
    threadId: string,
    payloadVersion?: unknown,
  ): PayloadExpansionHandle;
  isSubagentGroupExpanded(groupKey: string): boolean;
  toggleSubagentGroupExpanded(groupKey: string): boolean;
  attachmentCacheFor(itemId: string): AttachmentPreviewCache;
  clear(): void;
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
    rowOptions: Pick<PayloadExpansionOptions, 'loadMode'> = {},
  ): PayloadExpansionHandle {
    const key = 'i:' + item.id + ':' + (rowOptions.loadMode ?? 'preview');
    let cached = expansionStates.get(key);
    if (cached) return cached;

    const itemId = item.id;
    const getCurrentItem = (): Item | undefined => options.getItemById(itemId);
    cached = createPayloadExpansion(
      () => getCurrentItem()?.payloadId,
      () => getCurrentItem()?.threadId,
      {
        payloadVersion: () => getCurrentItem()?.updatedAt,
        loadMode: rowOptions.loadMode,
      },
    );
    expansionStates.set(key, cached);
    return cached;
  }

  function expansionStateForPayload(
    payloadId: string,
    threadId: string,
    payloadVersion?: unknown,
  ): PayloadExpansionHandle {
    const key = 'p:' + payloadId;
    let cached = expansionStates.get(key);
    if (cached) {
      cached.setPayloadVersion(payloadVersion);
      return cached;
    }

    cached = createPayloadExpansion(
      () => payloadId,
      () => threadId,
    );
    cached.setPayloadVersion(payloadVersion);
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
