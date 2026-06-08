import { onDestroy, untrack } from 'svelte';
import type { ThreadPane } from '../../stores/thread.svelte';
import type {
  PayloadExpansionStateOptions,
  RowExpansionStateOptions,
} from '../../stores/threadRowUiState.svelte';
import type { Item } from '../../types/models';
import { payloadVersionKey as cachePayloadVersionKey } from '../../utils/payloadDataCache';
import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';

interface LeasedExpansionRef {
  readonly current: PayloadExpansionHandle | null;
}

interface LeasedItemExpansionOptions {
  getPane(): ThreadPane | undefined;
  getItem(): Item;
  getFallback(): PayloadExpansionHandle | null;
  getOptions?(): RowExpansionStateOptions;
  enabled?(): boolean;
}

interface LeasedPayloadExpansionOptions {
  getPane(): ThreadPane | undefined;
  getPayloadId(): string | undefined;
  getThreadId(): string;
  getFallback(): PayloadExpansionHandle | null;
  getOptions?(): PayloadExpansionStateOptions | unknown;
  enabled?(): boolean;
}

export function useLeasedItemExpansion(
  options: LeasedItemExpansionOptions,
): LeasedExpansionRef {
  let current = $state<PayloadExpansionHandle | null>(null);
  let leaseKey = '';
  let releaseLease: (() => void) | null = null;

  function syncLease(): void {
    const enabled = options.enabled?.() ?? true;
    const pane = options.getPane();
    const item = options.getItem();
    const itemOptions = options.getOptions?.() ?? {};
    const key = enabled && pane
      ? itemLeaseKey(pane.paneId, item, itemOptions)
      : stableLeaseKey('fallback-item', enabled, item.threadId, item.id, itemOptionsKey(itemOptions));
    if (key === leaseKey) return;

    releaseLease?.();
    releaseLease = null;
    leaseKey = key;

    if (!enabled || !pane) {
      current = enabled ? options.getFallback() : null;
      return;
    }

    if (typeof pane.retainExpansionStateFor !== 'function') {
      current = pane.expansionStateFor(
        untrack(options.getItem),
        untrack(() => options.getOptions?.() ?? {}),
      );
      return;
    }

    const lease = pane.retainExpansionStateFor(
      untrack(options.getItem),
      untrack(() => itemOptions),
    );
    current = lease.handle;
    releaseLease = lease.release;
  }

  syncLease();

  $effect(syncLease);

  onDestroy(() => {
    releaseLease?.();
    releaseLease = null;
  });

  return {
    get current() {
      return current;
    },
  };
}

export function useLeasedPayloadExpansion(
  options: LeasedPayloadExpansionOptions,
): LeasedExpansionRef {
  let current = $state<PayloadExpansionHandle | null>(null);
  let leaseKey = '';
  let releaseLease: (() => void) | null = null;

  function syncLease(): void {
    const enabled = options.enabled?.() ?? true;
    const pane = options.getPane();
    const payloadId = options.getPayloadId();
    const threadId = options.getThreadId();
    const payloadOptions = options.getOptions?.();
    const key = enabled && pane && payloadId
      ? payloadLeaseKey(pane.paneId, threadId, payloadId, payloadOptions)
      : stableLeaseKey('fallback-payload', enabled, threadId, payloadId ?? '', payloadOptionsKey(payloadOptions));
    if (key === leaseKey) return;

    releaseLease?.();
    releaseLease = null;
    leaseKey = key;

    if (!enabled || !pane || !payloadId) {
      current = enabled ? options.getFallback() : null;
      return;
    }

    if (typeof pane.retainExpansionStateForPayload !== 'function') {
      current = pane.expansionStateForPayload(
        payloadId,
        threadId,
        untrack(() => options.getOptions?.()),
      );
      return;
    }

    const lease = pane.retainExpansionStateForPayload(
      payloadId,
      threadId,
      untrack(() => payloadOptions),
    );
    current = lease.handle;
    releaseLease = lease.release;
  }

  syncLease();

  $effect(syncLease);

  onDestroy(() => {
    releaseLease?.();
    releaseLease = null;
  });

  return {
    get current() {
      return current;
    },
  };
}

function stableLeaseKey(...parts: readonly unknown[]): string {
  return JSON.stringify(parts);
}

function itemLeaseKey(
  paneId: string,
  item: Item,
  options: RowExpansionStateOptions,
): string {
  const loadMode = options.loadMode ?? 'preview';
  const stateKey = options.stateKey ?? 'default';
  return stableLeaseKey(
    'item',
    paneId,
    item.threadId,
    item.id,
    loadMode,
    options.loadOnMount ? 'auto' : 'manual',
    stateKey,
    options.previewBytes ?? 'preview-default',
    options.chunkBytes ?? 'chunk-default',
    options.requestTimeoutMs ?? 'timeout-default',
    cacheEnabledKey(options.cacheEnabled),
  );
}

function payloadLeaseKey(
  paneId: string,
  threadId: string,
  payloadId: string,
  optionsOrPayloadVersion: PayloadExpansionStateOptions | unknown,
): string {
  const options = normalizePayloadOptions(optionsOrPayloadVersion);
  const loadMode = options.loadMode ?? 'preview';
  return stableLeaseKey(
    'payload',
    paneId,
    threadId,
    payloadId,
    loadMode,
    options.loadOnMount ? 'auto' : 'manual',
    options.stateKey ?? 'default',
    options.previewBytes ?? 'preview-default',
    options.chunkBytes ?? 'chunk-default',
    options.requestTimeoutMs ?? 'timeout-default',
    cacheEnabledKey(options.cacheEnabled),
    payloadVersionLeaseKey(options.payloadVersion),
  );
}

function itemOptionsKey(options: RowExpansionStateOptions): string {
  return stableLeaseKey(
    options.loadMode ?? 'preview',
    options.loadOnMount ? 'auto' : 'manual',
    options.stateKey ?? 'default',
    options.previewBytes ?? 'preview-default',
    options.chunkBytes ?? 'chunk-default',
    options.requestTimeoutMs ?? 'timeout-default',
    cacheEnabledKey(options.cacheEnabled),
  );
}

function payloadOptionsKey(optionsOrPayloadVersion: PayloadExpansionStateOptions | unknown): string {
  const options = normalizePayloadOptions(optionsOrPayloadVersion);
  return stableLeaseKey(
    options.loadMode ?? 'preview',
    options.loadOnMount ? 'auto' : 'manual',
    options.stateKey ?? 'default',
    options.previewBytes ?? 'preview-default',
    options.chunkBytes ?? 'chunk-default',
    options.requestTimeoutMs ?? 'timeout-default',
    cacheEnabledKey(options.cacheEnabled),
    payloadVersionLeaseKey(options.payloadVersion),
  );
}

function normalizePayloadOptions(
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

function cacheEnabledKey(cacheEnabled: unknown): string {
  if (cacheEnabled === undefined) return 'cache-default';
  if (typeof cacheEnabled === 'function') return 'cache-dynamic';
  return cacheEnabled ? 'cache-on' : 'cache-off';
}

function payloadVersionLeaseKey(value: unknown): string {
  if (typeof value === 'function') return 'version-dynamic';
  return cachePayloadVersionKey(value);
}
