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
import { subagentGroupKeysFor } from '../utils/subagentGrouping';
import { getSettings } from './settings.svelte';

/** What an inline diff card renders when no reader override applies. */
function defaultDiffCardExpanded(): boolean {
  return !getSettings().collapseDiffPreviews;
}

/**
 * A stored diff-card override, seen against the CURRENT default — the one
 * chokepoint every read of the override map goes through.
 *
 * An override means "the reader pinned this card away from the default", so
 * a value that agrees with the default is not one, whatever put it there.
 * That single comparison is the whole invalidation strategy: `collapseDiffPreviews`
 * flipping retires every override recorded under the old default during the
 * next render, with no `$effect`, no sweep and no generation counter — and
 * flipping back brings the reader's pin straight back, because the entry was
 * never destroyed. Keeping it a value test rather than a map-wide stamp is
 * what makes it caller-proof: a redundant entry can never be mistaken for a
 * deviation, and a real one can never be retired with the batch.
 */
function liveDiffOverride(stored: boolean | undefined): boolean | undefined {
  return stored === defaultDiffCardExpanded() ? undefined : stored;
}

interface ThreadRowUiStateOptions {
  getItemById(itemId: string): Item | undefined;
  isPayloadReferenced?(threadId: string, payloadId: string): boolean;
}

export interface ThreadRowUiState {
  expansionStateFor(
    item: Item,
    options?: RowExpansionStateOptions,
  ): PayloadExpansionHandle;
  retainExpansionStateFor(
    item: Item,
    options?: RowExpansionStateOptions,
  ): PayloadExpansionLease;
  expansionStateForPayload(
    payloadId: string,
    threadId: string,
    options?: PayloadExpansionStateOptions | unknown,
  ): PayloadExpansionHandle;
  retainExpansionStateForPayload(
    payloadId: string,
    threadId: string,
    options?: PayloadExpansionStateOptions | unknown,
  ): PayloadExpansionLease;
  appendLivePayloadDeltaForItem(
    itemId: string,
    stateKey: string,
    delta: string,
    payloadVersion?: unknown,
    previousLiveTail?: string,
  ): void;
  isSubagentGroupExpanded(groupKey: string): boolean;
  toggleSubagentGroupExpanded(groupKey: string): boolean;
  /** Whether the reader expanded this user message's clamped text. */
  isUserMessageExpanded(itemId: string): boolean;
  /** Record the state the reader put the message's text in. Every user
   *  message defaults to collapsed and no setting moves that default, so
   *  the registry stores the deviation only — an expanded id — and
   *  collapsing forgets it rather than storing `false`. */
  setUserMessageExpanded(itemId: string, expanded: boolean): void;
  /** The card's deviation from the current `collapseDiffPreviews` default,
   *  or undefined when it follows it. See `liveDiffOverride`. */
  diffCardExpandedOverride(itemId: string, filePath: string): boolean | undefined;
  /** Record the state the reader put the card in. Matching the current
   *  `collapseDiffPreviews` default clears instead of storing, so the card
   *  keeps following future flips of that setting — the registry decides
   *  that, never the caller. */
  setDiffCardExpanded(itemId: string, filePath: string, expanded: boolean): void;
  /**
   * A stable string of the thread's non-default row-UI expansion state —
   * the validity stamp for replaying a measured-size priors snapshot across
   * a thread switch (see utils/virtual/priors.ts). Empty when every
   * row is at its default expansion, which is the state `clear()` resets to
   * on switch-in, so a snapshot captured with anything expanded (taller
   * rows) cannot match a freshly-mounted timeline and is correctly refused.
   */
  expansionSignature(): string;
  /**
   * Whether the reader has EXPLICITLY expanded anything belonging to these
   * items: a diff card overridden to expanded, a subagent / wait / read
   * group, a clamped user message opened with "Show more", or a payload
   * body whose default is collapsed. The same "user
   * deviations from default" contract as `expansionSignature` — an expansion
   * a setting handed them stamps nothing this can mistake for a reader: a
   * `collapseDiffPreviews` default-open diff leaves no override (and an
   * override that setting has since caught up with reads as none — see
   * `liveDiffOverride`), a
   * `loadOnMount` entry (auto-loaded diff bodies, plan cards) is skipped
   * wholesale via `autoExpands`, and a diff card overridden back to
   * COLLAPSED is an answer, not engagement. Non-creating: pure reads, no
   * registry entries minted. The activity-run auto-collapse gate is the
   * caller — a run the reader opened something inside must not close under
   * them.
   */
  hasUserExpansionWithin(itemIds: Iterable<string>): boolean;
  attachmentCacheFor(itemId: string): AttachmentPreviewCache;
  disposeItems(items: Iterable<Item>): void;
  pruneRowUiState(retention: RowUiStateRetention): void;
  clear(): void;
  debugStats(): {
    expansionStates: number;
    itemExpansionStates: number;
    payloadExpansionStates: number;
    subagentGroups: number;
    expandedUserMessages: number;
    diffCardOverrideItems: number;
    attachmentItems: number;
  };
}

export interface PayloadExpansionRetentionKey {
  threadId: string;
  payloadId: string;
}

export interface PayloadExpansionLease {
  handle: PayloadExpansionHandle;
  release(): void;
}

// Concrete re-iterable collections on purpose — this retention is
// consumed by more than one pruner (rowUiState's own prune plus the
// settled-thinking-tail prune in thread.svelte.ts), and a one-shot
// iterator here would silently starve the second consumer.
export interface RowUiStateRetention {
  itemIds: ReadonlySet<string>;
  payloads: readonly PayloadExpansionRetentionKey[];
  groupKeys: ReadonlySet<string>;
}

/**
 * Registry entries outlive the row component that creates them and retain
 * these options for the entry's whole lifetime. Function-valued options
 * (`payloadVersion`, `cacheEnabled`) must be module-scope helpers that
 * derive everything from the `item` argument — a closure capturing a
 * component's props or scope pins that component instance (and its
 * detached DOM) until the item leaves retention.
 */
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
  /**
   * Current registry key. Starts as the `expansionStates` key and is
   * reassigned when a still-leased entry is parked under a
   * `leased-pruned:` key, so release() can unindex without scanning.
   */
  key: string;
  leases: number;
  disposeRequested: boolean;
  /**
   * The row asked for `loadOnMount`, so `handle.expanded` flips true with no
   * reader involved (auto-loaded diffs, plan cards). `hasUserExpansionWithin`
   * skips these wholesale: on an auto entry the expanded bit is the setting's
   * doing, so it can never witness a reader deviation.
   */
  autoExpands: boolean;
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
 * Per-row UI registries live outside row components so windowing remounts
 * do not drop loaded payload chunks, attachment thumbnails, or group
 * expansion state while the user scrolls around a thread.
 */
export function createThreadRowUiState(options: ThreadRowUiStateOptions): ThreadRowUiState {
  const expansionStates = new Map<string, ExpansionRegistryEntry>();
  const leasedPrunedExpansionStates = new Map<string, ExpansionRegistryEntry>();
  const itemExpansionKeysByState = new Map<string, Map<string, Set<string>>>();
  const payloadExpansionKeysByPayload = new Map<string, Set<string>>();
  let nextLeasedPrunedExpansionKey = 1;
  let subagentGroupExpanded: Set<string> = $state(new Set());
  // Item ids whose user-message text the reader unclamped ("Show more").
  // A Set, not an override map like the diff cards': every user message
  // defaults to clamped and no setting moves that default, so membership IS
  // the deviation and forgetting an id restores the default exactly.
  // Reassigned copy-on-write like subagentGroupExpanded.
  let userMessageExpanded: Set<string> = $state(new Set());
  // Per-card expand/collapse overrides for inline diff file blocks,
  // keyed itemId → filePath. An absent entry means "follow the
  // collapseDiffPreviews setting default"; a stored boolean is a reader
  // toggle, and only ever the negation of the default in force when it
  // was written (setDiffCardExpanded clears instead of storing a value
  // that matches). Every read goes through `liveDiffOverride`, so an
  // entry the setting has since caught up with reads as absent without
  // being erased. Reassigned copy-on-write like subagentGroupExpanded.
  let diffCardExpandedOverrides: ReadonlyMap<string, ReadonlyMap<string, boolean>> = $state(
    new Map(),
  );
  const attachmentBlobs = new Map<string, Map<string, ImagePreviewItem>>();
  let attachmentClearGeneration = 0;

  function expansionStateFor(
    item: Item,
    rowOptions: RowExpansionStateOptions = {},
  ): PayloadExpansionHandle {
    return getOrCreateItemExpansion(item, rowOptions).handle;
  }

  function retainExpansionStateFor(
    item: Item,
    rowOptions: RowExpansionStateOptions = {},
  ): PayloadExpansionLease {
    const entry = getOrCreateItemExpansion(item, rowOptions);
    return retainExpansionEntry(entry);
  }

  function itemExpansionKey(
    item: Item,
    rowOptions: RowExpansionStateOptions,
  ): string {
    const loadMode = rowOptions.loadMode ?? 'preview';
    const stateKey = rowOptions.stateKey ?? 'default';
    return expansionRegistryKey([
      'i',
      item.threadId,
      item.id,
      loadMode,
      rowOptions.loadOnMount ? 'auto' : 'manual',
      stateKey,
      rowOptions.previewBytes ?? 'preview-default',
      rowOptions.chunkBytes ?? 'chunk-default',
      rowOptions.requestTimeoutMs ?? 'timeout-default',
      cacheEnabledRegistryKey(rowOptions.cacheEnabled),
    ]);
  }

  function getOrCreateItemExpansion(
    item: Item,
    rowOptions: RowExpansionStateOptions,
  ): ExpansionRegistryEntry {
    const loadMode = rowOptions.loadMode ?? 'preview';
    const stateKey = rowOptions.stateKey ?? 'default';
    const key = itemExpansionKey(item, rowOptions);
    let cached = expansionStates.get(key);
    if (cached) return cached;

    const itemId = item.id;
    const itemThreadId = item.threadId;
    const getCurrentItem = (): Item | undefined => {
      const currentItem = options.getItemById(itemId);
      return currentItem?.threadId === itemThreadId ? currentItem : undefined;
    };
    const currentPayloadVersion = rowOptions.payloadVersion ?? payloadVersionForItem;
    const currentCacheEnabled = (): boolean => rowCacheEnabled(rowOptions.cacheEnabled, getCurrentItem());
    cached = createRegistryExpansion(
      key,
      {
        kind: 'item',
        itemId,
        stateKey,
      },
      rowOptions.loadOnMount === true,
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
    return cached;
  }

  function expansionStateForPayload(
    payloadId: string,
    threadId: string,
    optionsOrPayloadVersion?: PayloadExpansionStateOptions | unknown,
  ): PayloadExpansionHandle {
    return getOrCreatePayloadExpansion(payloadId, threadId, optionsOrPayloadVersion).handle;
  }

  function retainExpansionStateForPayload(
    payloadId: string,
    threadId: string,
    optionsOrPayloadVersion?: PayloadExpansionStateOptions | unknown,
  ): PayloadExpansionLease {
    const entry = getOrCreatePayloadExpansion(payloadId, threadId, optionsOrPayloadVersion);
    return retainExpansionEntry(entry);
  }

  function payloadExpansionKey(
    payloadId: string,
    threadId: string,
    payloadOptions: PayloadExpansionStateOptions,
  ): string {
    const loadMode = payloadOptions.loadMode ?? 'preview';
    return expansionRegistryKey([
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
  }

  function getOrCreatePayloadExpansion(
    payloadId: string,
    threadId: string,
    optionsOrPayloadVersion?: PayloadExpansionStateOptions | unknown,
  ): ExpansionRegistryEntry {
    const payloadOptions = normalizePayloadExpansionStateOptions(optionsOrPayloadVersion);
    const loadMode = payloadOptions.loadMode ?? 'preview';
    const key = payloadExpansionKey(payloadId, threadId, payloadOptions);
    let cached = expansionStates.get(key);
    if (cached) {
      cached.handle.setPayloadVersion(payloadOptions.payloadVersion);
      return cached;
    }

    cached = createRegistryExpansion(
      key,
      {
        kind: 'payload',
        threadId,
        payloadId,
      },
      payloadOptions.loadOnMount === true,
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
    return cached;
  }

  function createRegistryExpansion(
    key: string,
    owner: ExpansionRegistryOwner,
    autoExpands: boolean,
    create: () => PayloadExpansionHandle,
  ): ExpansionRegistryEntry {
    let handle: PayloadExpansionHandle | undefined;
    // Entries are created lazily from whichever row component first asks,
    // but live until the item leaves retention. The svelte patch's
    // "ownerless-roots" hunk (patches/svelte@5.56.8.patch) keeps this
    // root from inheriting the creating row's component context — without
    // it, every entry pins that row instance's props, scopes, and
    // detached DOM for the entry's whole lifetime.
    const dispose = $effect.root(() => {
      handle = create();
    });
    if (!handle) {
      dispose();
      throw new Error('Failed to create payload expansion state');
    }
    return {
      handle,
      dispose,
      owner,
      key,
      leases: 0,
      disposeRequested: false,
      autoExpands,
    };
  }

  function retainExpansionEntry(entry: ExpansionRegistryEntry): PayloadExpansionLease {
    entry.leases += 1;
    let released = false;
    return {
      handle: entry.handle,
      release() {
        if (released) return;
        released = true;
        entry.leases -= 1;
        if (entry.leases > 0 || !entry.disposeRequested) return;
        leasedPrunedExpansionStates.delete(entry.key);
        unindexExpansionKey(entry.key, entry.owner);
        disposeExpansionEntry(entry);
      },
    };
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
      const entry = expansionStates.get(key) ?? leasedPrunedExpansionStates.get(key);
      entry?.handle.appendLiveDelta(delta, payloadVersion, previousLiveTail);
    }
  }

  function disposeExpansionKey(key: string): void {
    const entry = expansionStates.get(key) ?? leasedPrunedExpansionStates.get(key);
    if (!entry) return;
    if (entry.leases > 0) {
      if (expansionStates.delete(key)) {
        unindexExpansionKey(key, entry.owner);
        const leasedKey = `leased-pruned:${nextLeasedPrunedExpansionKey}`;
        nextLeasedPrunedExpansionKey += 1;
        leasedPrunedExpansionStates.set(leasedKey, entry);
        indexExpansionKey(leasedKey, entry.owner);
        entry.key = leasedKey;
      }
      entry.disposeRequested = true;
      return;
    }
    expansionStates.delete(key);
    leasedPrunedExpansionStates.delete(key);
    unindexExpansionKey(key, entry.owner);
    disposeExpansionEntry(entry);
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
    let nextUserMessages: Set<string> | null = null;
    let nextDiffOverrides: Map<string, ReadonlyMap<string, boolean>> | null = null;
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
      for (const groupKey of subagentGroupKeysFor(itemId)) {
        if (!subagentGroupExpanded.has(groupKey)) continue;
        if (!nextGroupExpanded) nextGroupExpanded = new Set(subagentGroupExpanded);
        nextGroupExpanded.delete(groupKey);
      }
      if (userMessageExpanded.has(itemId)) {
        if (!nextUserMessages) nextUserMessages = new Set(userMessageExpanded);
        nextUserMessages.delete(itemId);
      }
      if (diffCardExpandedOverrides.has(itemId)) {
        if (!nextDiffOverrides) nextDiffOverrides = new Map(diffCardExpandedOverrides);
        nextDiffOverrides.delete(itemId);
      }
    }
    if (nextGroupExpanded) subagentGroupExpanded = nextGroupExpanded;
    if (nextUserMessages) userMessageExpanded = nextUserMessages;
    if (nextDiffOverrides) diffCardExpandedOverrides = nextDiffOverrides;
  }

  function pruneRowUiState(retention: RowUiStateRetention): void {
    const retainedItemIds = retention.itemIds;
    const retainedPayloads = new Set<string>();
    for (const payload of retention.payloads) {
      retainedPayloads.add(payloadExpansionRegistryKey(payload.threadId, payload.payloadId));
    }
    const retainedGroupKeys = retention.groupKeys;

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

    let nextUserMessages: Set<string> | null = null;
    for (const itemId of userMessageExpanded) {
      if (retainedItemIds.has(itemId)) continue;
      if (!nextUserMessages) nextUserMessages = new Set(userMessageExpanded);
      nextUserMessages.delete(itemId);
    }
    if (nextUserMessages) userMessageExpanded = nextUserMessages;

    let nextDiffOverrides: Map<string, ReadonlyMap<string, boolean>> | null = null;
    for (const itemId of diffCardExpandedOverrides.keys()) {
      if (retainedItemIds.has(itemId)) continue;
      if (!nextDiffOverrides) nextDiffOverrides = new Map(diffCardExpandedOverrides);
      nextDiffOverrides.delete(itemId);
    }
    if (nextDiffOverrides) diffCardExpandedOverrides = nextDiffOverrides;
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

  function isUserMessageExpanded(itemId: string): boolean {
    return userMessageExpanded.has(itemId);
  }

  function setUserMessageExpanded(itemId: string, expanded: boolean): void {
    if (expanded === userMessageExpanded.has(itemId)) return;
    const next = new Set(userMessageExpanded);
    if (expanded) next.add(itemId);
    else next.delete(itemId);
    userMessageExpanded = next;
  }

  function diffCardExpandedOverride(itemId: string, filePath: string): boolean | undefined {
    return liveDiffOverride(diffCardExpandedOverrides.get(itemId)?.get(filePath));
  }

  function setDiffCardExpanded(itemId: string, filePath: string, expanded: boolean): void {
    const inner = new Map(diffCardExpandedOverrides.get(itemId) ?? []);
    if (expanded === defaultDiffCardExpanded()) {
      inner.delete(filePath);
    } else {
      inner.set(filePath, expanded);
    }
    const next = new Map(diffCardExpandedOverrides);
    if (inner.size === 0) {
      next.delete(itemId);
    } else {
      next.set(itemId, inner);
    }
    diffCardExpandedOverrides = next;
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
    for (const key of [...expansionStates.keys()]) {
      disposeExpansionKey(key);
    }
    for (const key of [...leasedPrunedExpansionStates.keys()]) {
      disposeExpansionKey(key);
    }
    expansionStates.clear();
    leasedPrunedExpansionStates.clear();
    itemExpansionKeysByState.clear();
    payloadExpansionKeysByPayload.clear();
    subagentGroupExpanded = new Set();
    userMessageExpanded = new Set();
    diffCardExpandedOverrides = new Map();
    attachmentClearGeneration += 1;
    disposeAttachmentBlobs();
  }

  // See the interface doc. Serializes only USER deviations from default —
  // the collapseDiffPreviews setting and a payload's default-collapsed height
  // are identical at capture and restore, so defaults never change row
  // heights across a switch and are not stamped. Read imperatively (capture
  // on settle, restore on mount), never inside a reactive scope.
  function expansionSignature(): string {
    const parts: string[] = [];
    if (subagentGroupExpanded.size > 0) {
      parts.push('g:' + [...subagentGroupExpanded].sort().join(','));
    }
    // Unclamping a user message is a row-height deviation like any other, so
    // a priors snapshot captured with one open must not replay onto a
    // freshly-mounted (all-clamped) timeline.
    if (userMessageExpanded.size > 0) {
      parts.push('u:' + [...userMessageExpanded].sort().join(','));
    }
    const diffs: string[] = [];
    for (const [itemId, files] of diffCardExpandedOverrides) {
      for (const [filePath, stored] of files) {
        const expanded = liveDiffOverride(stored);
        if (expanded === undefined) continue;
        diffs.push(`${itemId}/${filePath}=${expanded ? 1 : 0}`);
      }
    }
    if (diffs.length > 0) parts.push('d:' + diffs.sort().join(','));
    const expandedPayloads: string[] = [];
    for (const entry of expansionStates.values()) {
      if (!entry.handle.expanded) continue;
      const owner = entry.owner;
      expandedPayloads.push(
        owner.kind === 'item' ? `i:${owner.itemId}:${owner.stateKey}` : `y:${owner.payloadId}`,
      );
    }
    if (expandedPayloads.length > 0) {
      parts.push('p:' + expandedPayloads.sort().join(','));
    }
    return parts.join('|');
  }

  // See the interface doc. Each structure is checked through its own index so
  // the cost scales with the queried items, not with the thread: subagent and
  // diff maps by direct lookup, expansion entries through
  // `itemExpansionKeysByState` / `payloadExpansionKeysByPayload`. Both entry
  // maps are consulted because a leased-pruned entry is still live user state
  // — a row holding its lease keeps rendering the expansion this reports.
  function hasUserExpansionWithin(itemIds: Iterable<string>): boolean {
    for (const itemId of itemIds) {
      for (const groupKey of subagentGroupKeysFor(itemId)) {
        if (subagentGroupExpanded.has(groupKey)) return true;
      }
      // A user message can never be an activity run's child, so this branch
      // cannot fire for today's only caller. It is here because the
      // contract above is stated over ITEMS, not over run membership: a
      // future caller asking about a user_text id must get the truth.
      if (userMessageExpanded.has(itemId)) return true;
      const files = diffCardExpandedOverrides.get(itemId);
      if (files) {
        for (const stored of files.values()) {
          if (liveDiffOverride(stored) === true) return true;
        }
      }
      const states = itemExpansionKeysByState.get(itemId);
      if (states) {
        for (const keys of states.values()) {
          for (const key of keys) {
            if (userExpandedEntry(key)) return true;
          }
        }
      }
      // Payload-keyed expansions reach the same body through the payload id
      // rather than the item, so the item's payload is checked too — resolved
      // here rather than passed in, because which id a row keyed its state by
      // is this registry's implementation detail, not the caller's.
      const item = options.getItemById(itemId);
      if (!item?.payloadId) continue;
      const payloadKeys = payloadExpansionKeysByPayload.get(
        payloadExpansionRegistryKey(item.threadId, item.payloadId),
      );
      if (!payloadKeys) continue;
      for (const key of payloadKeys) {
        if (userExpandedEntry(key)) return true;
      }
    }
    return false;
  }

  // An `autoExpands` entry's expanded bit is `loadOnMount`'s doing (see the
  // field's declaration), so only a manual-mode entry can witness a reader.
  function userExpandedEntry(key: string): boolean {
    const entry = expansionStates.get(key) ?? leasedPrunedExpansionStates.get(key);
    if (!entry || entry.autoExpands) return false;
    return entry.handle.expanded;
  }

  return {
    expansionStateFor,
    retainExpansionStateFor,
    expansionStateForPayload,
    retainExpansionStateForPayload,
    appendLivePayloadDeltaForItem,
    isSubagentGroupExpanded,
    toggleSubagentGroupExpanded,
    isUserMessageExpanded,
    setUserMessageExpanded,
    diffCardExpandedOverride,
    setDiffCardExpanded,
    expansionSignature,
    hasUserExpansionWithin,
    attachmentCacheFor,
    disposeItems,
    pruneRowUiState,
    clear,
    debugStats() {
      let payloadExpansionStates = 0;
      for (const entry of expansionStates.values()) {
        if (entry.owner.kind === 'payload') payloadExpansionStates += 1;
      }
      let itemExpansionStates = 0;
      for (const entry of expansionStates.values()) {
        if (entry.owner.kind === 'item') itemExpansionStates += 1;
      }
      return {
        expansionStates: expansionStates.size,
        itemExpansionStates,
        payloadExpansionStates,
        subagentGroups: subagentGroupExpanded.size,
        expandedUserMessages: userMessageExpanded.size,
        diffCardOverrideItems: diffCardExpandedOverrides.size,
        attachmentItems: attachmentBlobs.size,
      };
    },
  };
}
