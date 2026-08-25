import { onDestroy, untrack } from 'svelte';
import type {
  PaneSession,
  RowUiRegistry,
} from '../../stores/threadPaneRoles';
import type {
  PayloadExpansionStateOptions,
  RowExpansionStateOptions,
} from '../../stores/threadRowUiState.svelte';
import type { Item } from '../../types/models';
import { compositeKey } from '../../utils/compositeKey';
import { payloadVersionKey as cachePayloadVersionKey } from '../../utils/payloadDataCache';
import type { PayloadExpansionHandle } from '../../utils/payloadExpansion.svelte';

/** What a leased expansion handle needs: the row-UI registry, plus the pane id the lease is keyed by. */
type LeaseHost = PaneSession & RowUiRegistry;

interface LeasedExpansionRef {
  readonly current: PayloadExpansionHandle | null;
}

interface LeasedItemExpansionOptions {
  getPane(): LeaseHost | undefined;
  getItem(): Item;
  getFallback(): PayloadExpansionHandle | null;
  getOptions?(): RowExpansionStateOptions;
  enabled?(): boolean;
}

interface LeasedPayloadExpansionOptions {
  getPane(): LeaseHost | undefined;
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
      : compositeKey(
        'fallback-item',
        enabled,
        item.threadId,
        item.id,
        ...itemOptionsKeyParts(itemOptions),
      );
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
      : compositeKey(
        'fallback-payload',
        enabled,
        threadId,
        payloadId ?? '',
        ...payloadOptionsKeyParts(payloadOptions),
      );
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

/**
 * The options half of a lease key, as PARTS rather than a joined string.
 *
 * Parts, because both the leased key and the fallback key end with these
 * and a pre-joined sub-key would be nested inside the outer join on the
 * same separator — which is exactly the ambiguity the separator is there
 * to avoid (`['a', 'b\u0000c']` and `['a\u0000b', 'c']` are different
 * tuples with one key). Spreading keeps every tuple flat, and it is what
 * lets the leased and fallback keys share one description of "the options
 * that make two handles different".
 */
/**
 * The fields both option shapes contribute to a key. `cacheEnabled` is
 * widened because the two types disagree on its signature and the key only
 * cares which of three shapes it is (see `cacheEnabledKey`).
 */
type ExpansionKeyOptions = Pick<
  RowExpansionStateOptions,
  'loadMode' | 'loadOnMount' | 'stateKey' | 'previewBytes' | 'chunkBytes' | 'requestTimeoutMs'
> & { cacheEnabled?: unknown };

function itemOptionsKeyParts(options: ExpansionKeyOptions): (string | number)[] {
  return [
    options.loadMode ?? 'preview',
    options.loadOnMount ? 'auto' : 'manual',
    options.stateKey ?? 'default',
    options.previewBytes ?? 'preview-default',
    options.chunkBytes ?? 'chunk-default',
    options.requestTimeoutMs ?? 'timeout-default',
    cacheEnabledKey(options.cacheEnabled),
  ];
}

function payloadOptionsKeyParts(
  optionsOrPayloadVersion: PayloadExpansionStateOptions | unknown,
): (string | number)[] {
  const options = normalizePayloadOptions(optionsOrPayloadVersion);
  return [
    ...itemOptionsKeyParts(options),
    payloadVersionLeaseKey(options.payloadVersion),
  ];
}

function itemLeaseKey(
  paneId: string,
  item: Item,
  options: RowExpansionStateOptions,
): string {
  return compositeKey(
    'item',
    paneId,
    item.threadId,
    item.id,
    ...itemOptionsKeyParts(options),
  );
}

function payloadLeaseKey(
  paneId: string,
  threadId: string,
  payloadId: string,
  optionsOrPayloadVersion: PayloadExpansionStateOptions | unknown,
): string {
  return compositeKey(
    'payload',
    paneId,
    threadId,
    payloadId,
    ...payloadOptionsKeyParts(optionsOrPayloadVersion),
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
