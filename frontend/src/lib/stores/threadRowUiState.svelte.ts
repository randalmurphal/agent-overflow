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
  isPayloadReferenced?(threadId: string, payloadId: string): boolean;
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
  appendLivePayloadDeltaForItem(
    itemId: string,
    stateKey: string,
    delta: string,
    payloadVersion?: unknown,
    previousLiveTail?: string,
  ): void;
  isSubagentGroupExpanded(groupKey: string): boolean;
  toggleSubagentGroupExpanded(groupKey: string): boolean;
  attachmentCacheFor(itemId: string): AttachmentPreviewCache;
  disposeItems(items: Iterable<Item>): void;
  pruneRowUiState(retention: RowUiStateRetention): void;
  clear(): void;
  debugStats(): {
    expansionStates: number;
    itemExpansionStates: number;
    payloadExpansionStates: number;
    subagentGroups: number;
    attachmentItems: number;
  };
}

export interface PayloadExpansionRetentionKey {
  threadId: string;
  payloadId: string;
}

export interface RowUiStateRetention {
  itemIds: Iterable<string>;
  payloads: Iterable<PayloadExpansionRetentionKey>;
  groupKeys: Iterable<string>;
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
  cacheEnabled?: boolean | ((item: Item | undefined) => boolean);
}

export interface PayloadExpansionStateOptions
  extends Pick<
    PayloadExpansionOptions,
    'loadMode' | 'loadOnMount' | 'previewBytes' | 'chunkBytes' | 'requestTimeoutMs' | 'cacheEnabled'
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
      || 'cacheEnabled' in optionsOrPayloadVersion
    )
  ) {
    return optionsOrPayloadVersion as PayloadExpansionStateOptions;
  }
  return { payloadVersion: optionsOrPayloadVersion };
}

function expansionRegistryKey(parts: readonly unknown[]): string {
  return JSON.stringify(parts);
}

function rowCacheEnabled(
  cacheEnabled: RowExpansionStateOptions['cacheEnabled'],
  item: Item | undefined,
): boolean {
  if (cacheEnabled === undefined) return true;
  return typeof cacheEnabled === 'function' ? cacheEnabled(item) : cacheEnabled;
}

function cacheEnabledRegistryKey(cacheEnabled: unknown): string {
  if (cacheEnabled === undefined) return 'cache-default';
  if (typeof cacheEnabled === 'function') return 'cache-dynamic';
  return cacheEnabled ? 'cache-on' : 'cache-off';
}

interface ExpansionRegistryEntry {
  handle: PayloadExpansionHandle;
  dispose: () => void;
  owner: ExpansionRegistryOwner;
}

type ExpansionRegistryOwner =
  | {
      kind: 'item';
      itemId: string;
      stateKey: string;
    }
  | {
      kind: 'payload';
      threadId: string;
      payloadId: string;
    };

/**
 * Per-row UI registries live outside row components so virtua remounts
 * do not drop loaded payload chunks, attachment thumbnails, or group
 * expansion state while the user scrolls around a thread.
 */
export function createThreadRowUiState(options: ThreadRowUiStateOptions): ThreadRowUiState {
  const expansionStates = new Map<string, ExpansionRegistryEntry>();
  const itemExpansionKeysByState = new Map<string, Map<string, Set<string>>>();
  const payloadExpansionKeysByPayload = new Map<string, Set<string>>();
  let subagentGroupExpanded: Set<string> = $state(new Set());
  const attachmentBlobs = new Map<string, Map<string, ImagePreviewItem>>();
  let attachmentClearGeneration = 0;

  function expansionStateFor(
    item: Item,
    rowOptions: RowExpansionStateOptions = {},
  ): PayloadExpansionHandle {
    const loadMode = rowOptions.loadMode ?? 'preview';
    const stateKey = rowOptions.stateKey ?? 'default';
    const key = expansionRegistryKey([
      'i',
      item.id,
      loadMode,
      rowOptions.loadOnMount ? 'auto' : 'manual',
      stateKey,
      rowOptions.previewBytes ?? 'preview-default',
      rowOptions.chunkBytes ?? 'chunk-default',
      rowOptions.requestTimeoutMs ?? 'timeout-default',
      cacheEnabledRegistryKey(rowOptions.cacheEnabled),
    ]);
    let cached = expansionStates.get(key);
    if (cached) return cached.handle;

    const itemId = item.id;
    const getCurrentItem = (): Item | undefined => options.getItemById(itemId);
    const currentPayloadVersion = rowOptions.payloadVersion ?? payloadVersionForItem;
    const currentCacheEnabled = (): boolean => rowCacheEnabled(rowOptions.cacheEnabled, getCurrentItem());
    cached = createRegistryExpansion(
      {
        kind: 'item',
        itemId,
        stateKey,
      },
      () => createPayloadExpansion(
        () => getCurrentItem()?.payloadId,
        () => getCurrentItem()?.threadId,
        {
          payloadVersion: () => currentPayloadVersion(getCurrentItem()),
          loadMode,
          loadOnMount: rowOptions.loadOnMount,
          previewBytes: rowOptions.previewBytes,
          chunkBytes: rowOptions.chunkBytes,
          requestTimeoutMs: rowOptions.requestTimeoutMs,
          cacheEnabled: currentCacheEnabled,
        },
      ),
    );
    expansionStates.set(key, cached);
    indexExpansionKey(key, cached.owner);
    return cached.handle;
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
      cacheEnabledRegistryKey(payloadOptions.cacheEnabled),
    ]);
    let cached = expansionStates.get(key);
    if (cached) {
      cached.handle.setPayloadVersion(payloadOptions.payloadVersion);
      return cached.handle;
    }

    cached = createRegistryExpansion(
      {
        kind: 'payload',
        threadId,
        payloadId,
      },
      () => createPayloadExpansion(
        () => payloadId,
        () => threadId,
        {
          payloadVersion: payloadOptions.payloadVersion,
          loadMode,
          loadOnMount: payloadOptions.loadOnMount,
          previewBytes: payloadOptions.previewBytes,
          chunkBytes: payloadOptions.chunkBytes,
          requestTimeoutMs: payloadOptions.requestTimeoutMs,
          cacheEnabled: payloadOptions.cacheEnabled,
        },
      ),
    );
    expansionStates.set(key, cached);
    indexExpansionKey(key, cached.owner);
    return cached.handle;
  }

  function createRegistryExpansion(
    owner: ExpansionRegistryOwner,
    create: () => PayloadExpansionHandle,
  ): ExpansionRegistryEntry {
    let handle: PayloadExpansionHandle | undefined;
    const dispose = $effect.root(() => {
      handle = create();
    });
    if (!handle) {
      dispose();
      throw new Error('Failed to create payload expansion state');
    }
    return { handle, dispose, owner };
  }

  function appendLivePayloadDeltaForItem(
    itemId: string,
    stateKey: string,
    delta: string,
    payloadVersion?: unknown,
    previousLiveTail?: string,
  ): void {
    const keys = itemExpansionKeysByState.get(itemId)?.get(stateKey);
    if (!keys || keys.size === 0) return;
    for (const key of keys) {
      const entry = expansionStates.get(key);
      entry?.handle.appendLiveDelta(delta, payloadVersion, previousLiveTail);
    }
  }

  function disposeExpansionKey(key: string): void {
    const entry = expansionStates.get(key);
    if (!entry) return;
    disposeExpansionEntry(entry);
    expansionStates.delete(key);
    unindexExpansionKey(key, entry.owner);
  }

  function disposeExpansionEntry(entry: ExpansionRegistryEntry): void {
    entry.handle.reset();
    entry.dispose();
  }

  function indexExpansionKey(key: string, owner: ExpansionRegistryOwner): void {
    if (owner.kind === 'item') {
      let stateKeys = itemExpansionKeysByState.get(owner.itemId);
      if (!stateKeys) {
        stateKeys = new Map();
        itemExpansionKeysByState.set(owner.itemId, stateKeys);
      }
      let keysForState = stateKeys.get(owner.stateKey);
      if (!keysForState) {
        keysForState = new Set();
        stateKeys.set(owner.stateKey, keysForState);
      }
      keysForState.add(key);
      return;
    }

    const payloadKey = payloadExpansionRegistryKey(owner.threadId, owner.payloadId);
    let keys = payloadExpansionKeysByPayload.get(payloadKey);
    if (!keys) {
      keys = new Set();
      payloadExpansionKeysByPayload.set(payloadKey, keys);
    }
    keys.add(key);
  }

  function unindexExpansionKey(key: string, owner: ExpansionRegistryOwner): void {
    if (owner.kind === 'item') {
      const stateKeys = itemExpansionKeysByState.get(owner.itemId);
      const keysForState = stateKeys?.get(owner.stateKey);
      keysForState?.delete(key);
      if (keysForState && keysForState.size === 0) stateKeys?.delete(owner.stateKey);
      if (stateKeys && stateKeys.size === 0) itemExpansionKeysByState.delete(owner.itemId);
      return;
    }

    const payloadKey = payloadExpansionRegistryKey(owner.threadId, owner.payloadId);
    const keys = payloadExpansionKeysByPayload.get(payloadKey);
    keys?.delete(key);
    if (keys && keys.size === 0) payloadExpansionKeysByPayload.delete(payloadKey);
  }

  function payloadExpansionRegistryKey(threadId: string, payloadId: string): string {
    return expansionRegistryKey([threadId, payloadId]);
  }

  function disposeItemExpansionStates(itemId: string): void {
    const states = itemExpansionKeysByState.get(itemId);
    if (!states) return;
    for (const keys of states.values()) {
      for (const key of [...keys]) {
        disposeExpansionKey(key);
      }
    }
  }

  function disposePayloadExpansionStates(threadId: string, payloadId: string): void {
    const keys = payloadExpansionKeysByPayload.get(
      payloadExpansionRegistryKey(threadId, payloadId),
    );
    if (!keys) return;
    for (const key of [...keys]) disposeExpansionKey(key);
  }

  function disposeAttachmentBlobsForItem(itemId: string): void {
    const inner = attachmentBlobs.get(itemId);
    if (!inner) return;
    for (const preview of inner.values()) {
      revokePreview(preview);
    }
    inner.clear();
    attachmentBlobs.delete(itemId);
  }

  function disposeItems(items: Iterable<Item>): void {
    let nextGroupExpanded: Set<string> | null = null;
    for (const item of items) {
      const itemId = item.id;
      disposeItemExpansionStates(itemId);
      if (
        item.payloadId
        && !options.isPayloadReferenced?.(item.threadId, item.payloadId)
      ) {
        disposePayloadExpansionStates(item.threadId, item.payloadId);
      }
      disposeAttachmentBlobsForItem(itemId);
      const groupKeys = [itemId, `wait:${itemId}`, `reads:${itemId}`];
      for (const groupKey of groupKeys) {
        if (!subagentGroupExpanded.has(groupKey)) continue;
        if (!nextGroupExpanded) nextGroupExpanded = new Set(subagentGroupExpanded);
        nextGroupExpanded.delete(groupKey);
      }
    }
    if (nextGroupExpanded) subagentGroupExpanded = nextGroupExpanded;
  }

  function pruneRowUiState(retention: RowUiStateRetention): void {
    const retainedItemIds = new Set(retention.itemIds);
    const retainedPayloads = new Set<string>();
    for (const payload of retention.payloads) {
      retainedPayloads.add(payloadExpansionRegistryKey(payload.threadId, payload.payloadId));
    }
    const retainedGroupKeys = new Set(retention.groupKeys);

    for (const [key, entry] of expansionStates) {
      if (entry.owner.kind === 'item') {
        if (!retainedItemIds.has(entry.owner.itemId)) disposeExpansionKey(key);
        continue;
      }

      const payloadKey = payloadExpansionRegistryKey(entry.owner.threadId, entry.owner.payloadId);
      if (!retainedPayloads.has(payloadKey)) disposeExpansionKey(key);
    }

    for (const itemId of attachmentBlobs.keys()) {
      if (retainedItemIds.has(itemId)) continue;
      disposeAttachmentBlobsForItem(itemId);
    }

    let nextGroupExpanded: Set<string> | null = null;
    for (const groupKey of subagentGroupExpanded) {
      if (retainedGroupKeys.has(groupKey)) continue;
      if (!nextGroupExpanded) nextGroupExpanded = new Set(subagentGroupExpanded);
      nextGroupExpanded.delete(groupKey);
    }
    if (nextGroupExpanded) subagentGroupExpanded = nextGroupExpanded;
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
    const clearGeneration = attachmentClearGeneration;
    let inner = attachmentBlobs.get(itemId);
    if (!inner) {
      inner = new Map<string, ImagePreviewItem>();
      attachmentBlobs.set(itemId, inner);
    }

    const innerRef = inner;
    return {
      get(attachmentId: string): ImagePreviewItem | undefined {
        if (clearGeneration !== attachmentClearGeneration) return undefined;
        if (attachmentBlobs.get(itemId) !== innerRef) return undefined;
        return innerRef.get(attachmentId);
      },
      set(attachmentId: string, preview: ImagePreviewItem): void {
        if (
          clearGeneration !== attachmentClearGeneration
          || attachmentBlobs.get(itemId) !== innerRef
        ) {
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
    for (const entry of expansionStates.values()) {
      disposeExpansionEntry(entry);
    }
    expansionStates.clear();
    itemExpansionKeysByState.clear();
    payloadExpansionKeysByPayload.clear();
    subagentGroupExpanded = new Set();
    attachmentClearGeneration += 1;
    disposeAttachmentBlobs();
  }

  return {
    expansionStateFor,
    expansionStateForPayload,
    appendLivePayloadDeltaForItem,
    isSubagentGroupExpanded,
    toggleSubagentGroupExpanded,
    attachmentCacheFor,
    disposeItems,
    pruneRowUiState,
    clear,
    debugStats() {
      let payloadExpansionStates = 0;
      for (const entry of expansionStates.values()) {
        if (entry.owner.kind === 'payload') payloadExpansionStates += 1;
      }
      return {
        expansionStates: expansionStates.size,
        itemExpansionStates: itemExpansionKeysByState.size,
        payloadExpansionStates,
        subagentGroups: subagentGroupExpanded.size,
        attachmentItems: attachmentBlobs.size,
      };
    },
  };
}
