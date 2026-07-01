import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import { makeItem } from '../../test/helpers/chat';
import {
  __resetPayloadCacheForTest,
  readPayloadCache,
  writePayloadCache,
} from '../utils/payloadDataCache';
import { THINKING_PAYLOAD_EXPANSION_STATE_KEY } from '../utils/payloadVersion';
import { createThreadRowUiState } from './threadRowUiState.svelte';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

describe('createThreadRowUiState', () => {
  beforeEach(() => {
    resetBindingMocks();
    __resetPayloadCacheForTest();
  });

  it('keeps item-keyed expansion handles stable while reading the latest item reference', () => {
    const items = new Map<string, Item>();
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return items.get(itemId);
      },
    });
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'payload-a',
      threadId: 'thread-a',
      updatedAt: 1,
    });
    items.set(item.id, item);

    const first = rowUiState.expansionStateFor(item);
    const updated = { ...item, payloadId: 'payload-b', updatedAt: 2 };
    items.set(item.id, updated);

    expect(rowUiState.expansionStateFor(updated)).toBe(first);
    expect(first.payloadVersion).toBe('payload-b');
  });

  it('does not bind cached expansion handles to a transient row derived owner', () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      const items = new Map<string, Item>();
      const rowUiState = createThreadRowUiState({
        getItemById(itemId: string): Item | undefined {
          return items.get(itemId);
        },
      });
      const item = makeItem({
        id: 'tool:derived-owner',
        kind: 'tool_call',
        payloadId: 'payload-derived-owner',
        threadId: 'thread-a',
        updatedAt: 1,
      });
      items.set(item.id, item);

      let expansion = rowUiState.expansionStateFor(item);
      const dispose = $effect.root(() => {
        const fromRowDerived = $derived(rowUiState.expansionStateFor(item));
        $effect(() => {
          expansion = fromRowDerived;
        });
      });
      dispose();

      expect(expansion.displayData).toBeNull();
      const warnedDerivedInert = warn.mock.calls
        .flat()
        .some((arg) => String(arg).includes('derived_inert'));
      expect(warnedDerivedInert).toBe(false);
    } finally {
      warn.mockRestore();
    }
  });

  it('lets item-keyed expansion handles use a payload-specific version', () => {
    const items = new Map<string, Item>();
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return items.get(itemId);
      },
    });
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'payload-a',
      threadId: 'thread-a',
      payloadMeta: 'signature-a',
      updatedAt: 1,
    });
    items.set(item.id, item);

    const first = rowUiState.expansionStateFor(item, {
      stateKey: 'proposed-plan-history',
      payloadVersion: (currentItem) => currentItem?.payloadMeta,
    });
    const updated = {
      ...item,
      meta: JSON.stringify({ planImplementedAt: 123 }),
      payloadMeta: 'signature-a',
      updatedAt: 2,
    };
    items.set(item.id, updated);

    expect(rowUiState.expansionStateFor(updated, {
      stateKey: 'proposed-plan-history',
      payloadVersion: (currentItem) => currentItem?.payloadMeta,
    })).toBe(first);
    expect(first.payloadVersion).toBe('signature-a');
  });

  it('keeps payload-keyed expansion handles stable and refreshes their version', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });

    const first = rowUiState.expansionStateForPayload('payload-a', 'thread-a', 1);
    const second = rowUiState.expansionStateForPayload('payload-a', 'thread-a', 2);

    expect(second).toBe(first);
    expect(second.payloadVersion).toBe(2);
  });

  it('does not cancel an in-flight payload load when reacquired with the same version', async () => {
    let resolvePreview: ((value: {
      data: string;
      nextOffset: number;
      totalSize: number;
      isComplete: boolean;
    }) => void) | undefined;
    const preview = setBindingMock('GetPayloadPreview', () => new Promise((resolve) => {
      resolvePreview = resolve;
    }));
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });

    const first = rowUiState.expansionStateForPayload('payload-a', 'thread-a', 'version-a');
    const expand = first.expand();
    await vi.waitFor(() => expect(preview).toHaveBeenCalledTimes(1));

    const second = rowUiState.expansionStateForPayload('payload-a', 'thread-a', 'version-a');
    expect(second).toBe(first);
    resolvePreview?.({
      data: 'loaded payload',
      nextOffset: 14,
      totalSize: 14,
      isComplete: true,
    });

    await expand;
    expect(first.displayData).toBe('loaded payload');
    expect(preview).toHaveBeenCalledTimes(1);
  });

  it('appends live payload deltas to existing item-keyed handles only', async () => {
    const items = new Map<string, Item>();
    const item = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      payloadId: 'thinking-payload',
      threadId: 'thread-a',
      updatedAt: 1,
    });
    items.set(item.id, item);
    const getPayloadData = setBindingMock('GetPayloadData', async () => ({ data: 'seed' }));
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return items.get(itemId);
      },
    });

    rowUiState.appendLivePayloadDeltaForItem(
      item.id,
      THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      ' ignored',
      2,
    );

    const expansion = rowUiState.expansionStateFor(item, {
      loadMode: 'full',
      stateKey: THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      payloadVersion: (currentItem) => currentItem?.updatedAt,
    });
    await expansion.expand();
    expect(expansion.displayData).toBe('seed');

    const updated = { ...item, updatedAt: 2 };
    items.set(item.id, updated);
    rowUiState.appendLivePayloadDeltaForItem(
      item.id,
      THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      ' live',
      updated.updatedAt,
    );

    expect(expansion.displayData).toBe('seed live');
    await expansion.ensureLoaded();
    expect(getPayloadData).toHaveBeenCalledTimes(1);
  });

  it('evaluates item-keyed cache policy against the latest item reference', async () => {
    const items = new Map<string, Item>();
    const item = makeItem({
      id: 'think:0:0',
      kind: 'thinking',
      status: 'streaming',
      payloadId: 'thinking-payload',
      threadId: 'thread-a',
      updatedAt: 1,
    });
    items.set(item.id, item);
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return items.get(itemId);
      },
    });

    const expansion = rowUiState.expansionStateFor(item, {
      loadMode: 'full',
      stateKey: THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      payloadVersion: (currentItem) => currentItem?.updatedAt,
      cacheEnabled: (currentItem) => currentItem?.status !== 'streaming',
    });

    const completed = { ...item, status: 'completed' as const, updatedAt: 2 };
    items.set(item.id, completed);
    writePayloadCache('thread-a', 'thinking-payload', 2, {
      chunks: ['cached complete'],
      hasFullChunks: true,
      totalSize: 15,
      isComplete: true,
      loadedBytes: 15,
    });
    const getPayloadData = setBindingMock('GetPayloadData', async () => {
      throw new Error('completed thinking should hydrate from cache');
    });

    await expansion.expand();

    expect(expansion.displayData).toBe('cached complete');
    expect(getPayloadData).not.toHaveBeenCalled();
  });

  it('keeps payload-keyed cache policies isolated', async () => {
    writePayloadCache('thread-a', 'payload-a', 'version-a', {
      chunks: ['cached payload'],
      hasFullChunks: true,
      totalSize: 14,
      isComplete: true,
      loadedBytes: 14,
    });
    const getPayloadData = setBindingMock('GetPayloadData', async () => ({ data: 'fresh payload' }));
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });

    const cached = rowUiState.expansionStateForPayload(
      'payload-a',
      'thread-a',
      { payloadVersion: 'version-a', loadMode: 'full', cacheEnabled: true },
    );
    const uncached = rowUiState.expansionStateForPayload(
      'payload-a',
      'thread-a',
      { payloadVersion: 'version-a', loadMode: 'full', cacheEnabled: false },
    );

    expect(uncached).not.toBe(cached);
    await uncached.expand();
    expect(uncached.displayData).toBe('fresh payload');
    expect(getPayloadData).toHaveBeenCalledTimes(1);
  });

  it('does not collide item-keyed expansion handles when ids contain key delimiters', () => {
    const items = new Map<string, Item>();
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return items.get(itemId);
      },
    });
    const itemWithColon = makeItem({
      id: 'item:preview:manual:default',
      payloadId: 'payload-a',
    });
    const itemWithoutColon = makeItem({
      id: 'item',
      payloadId: 'payload-b',
    });
    items.set(itemWithColon.id, itemWithColon);
    items.set(itemWithoutColon.id, itemWithoutColon);

    const first = rowUiState.expansionStateFor(itemWithColon);
    const second = rowUiState.expansionStateFor(itemWithoutColon, {
      stateKey: 'default:preview-default:chunk-default:timeout-default',
    });

    expect(second).not.toBe(first);
  });

  it('does not reuse item-keyed expansion handles across threads with the same item id', () => {
    const items = new Map<string, Item>();
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return items.get(itemId);
      },
    });
    const firstThreadItem = makeItem({
      id: 'tool:same-id',
      payloadId: 'payload-a',
      threadId: 'thread-a',
    });
    const secondThreadItem = makeItem({
      id: 'tool:same-id',
      payloadId: 'payload-b',
      threadId: 'thread-b',
    });
    items.set(firstThreadItem.id, firstThreadItem);

    const first = rowUiState.expansionStateFor(firstThreadItem);
    items.set(secondThreadItem.id, secondThreadItem);
    const second = rowUiState.expansionStateFor(secondThreadItem);

    expect(second).not.toBe(first);
    expect(first.payloadVersion).toBeUndefined();
    expect(second.payloadVersion).toBe('payload-b');
  });

  it('hydrates payload-keyed expansion handles from the versioned cache on first creation', () => {
    writePayloadCache('thread-a', 'payload-a', 'version-a', {
      chunks: ['cached payload'],
      hasFullChunks: true,
      totalSize: 14,
      isComplete: true,
      loadedBytes: 14,
    });
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });

    const dispose = $effect.root(() => {
      const expansion = rowUiState.expansionStateForPayload(
        'payload-a',
        'thread-a',
        { payloadVersion: 'version-a', loadOnMount: true },
      );

      expect(expansion.expanded).toBe(true);
      expect(expansion.displayData).toBe('cached payload');
    });
    dispose();
  });

  it('does not collide payload-keyed expansion handles when ids contain key delimiters', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });

    const first = rowUiState.expansionStateForPayload(
      'payload:preview:manual:default',
      'thread',
    );
    const second = rowUiState.expansionStateForPayload(
      'payload',
      'thread:payload',
      { stateKey: 'default:preview-default:chunk-default:timeout-default' },
    );

    expect(second).not.toBe(first);
  });

  it('tracks subagent group expansion by group key', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });

    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
    expect(rowUiState.toggleSubagentGroupExpanded('group-a')).toBe(true);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(true);
    expect(rowUiState.isSubagentGroupExpanded('group-b')).toBe(false);
    expect(rowUiState.toggleSubagentGroupExpanded('group-a')).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
  });

  it('tracks diff card expanded overrides by item id and file path', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });

    // Absent = follow the collapseDiffPreviews setting default.
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBeUndefined();

    rowUiState.setDiffCardExpanded('item-a', 'src/foo.ts', false);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(false);
    rowUiState.setDiffCardExpanded('item-a', 'src/foo.ts', true);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(true);

    // Files under the same item stay independent, as do other items.
    rowUiState.setDiffCardExpanded('item-a', 'src/bar.ts', false);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/bar.ts')).toBe(false);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(true);
    expect(rowUiState.diffCardExpandedOverride('item-b', 'src/foo.ts')).toBeUndefined();

    // Clearing (undefined) removes the override so the card re-follows
    // the setting default; clearing the last path drops the item entry.
    rowUiState.setDiffCardExpanded('item-a', 'src/bar.ts', undefined);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/bar.ts')).toBeUndefined();
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(true);
    rowUiState.setDiffCardExpanded('item-a', 'src/foo.ts', undefined);
    expect(rowUiState.debugStats().diffCardOverrideItems).toBe(0);
  });

  it('keeps attachment cache entries isolated by item and clears them with blob revocation', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});

    const firstItemCache = rowUiState.attachmentCacheFor('item-a');
    firstItemCache.set('blob-preview', {
      id: 'blob-preview',
      filename: 'blob.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:preview-a',
    });
    firstItemCache.set('data-preview', {
      id: 'data-preview',
      filename: 'data.png',
      mimeType: 'image/png',
      size: 1,
      url: 'data:image/png;base64,abc',
    });

    expect(rowUiState.attachmentCacheFor('item-a').get('blob-preview')).toBeTruthy();
    expect(rowUiState.attachmentCacheFor('item-b').get('blob-preview')).toBeUndefined();

    rowUiState.clear();

    expect(revoke).toHaveBeenCalledExactlyOnceWith('blob:preview-a');
    expect(rowUiState.attachmentCacheFor('item-a').get('blob-preview')).toBeUndefined();
    revoke.mockRestore();
  });

  it('rejects stale attachment cache handles after clear', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});
    const staleCache = rowUiState.attachmentCacheFor('item-a');
    staleCache.set('before-clear', {
      id: 'before-clear',
      filename: 'before.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:before-clear',
    });

    rowUiState.clear();
    staleCache.set('after-clear', {
      id: 'after-clear',
      filename: 'after.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:after-clear',
    });

    expect(staleCache.get('before-clear')).toBeUndefined();
    expect(staleCache.get('after-clear')).toBeUndefined();
    expect(rowUiState.attachmentCacheFor('item-a').get('after-clear')).toBeUndefined();
    expect(revoke).toHaveBeenCalledWith('blob:before-clear');
    expect(revoke).toHaveBeenCalledWith('blob:after-clear');
    revoke.mockRestore();
  });

  it('caches timeline row heights by key, signature, and width', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
    const key = {
      key: 'l:thread-a:item-a',
      signature: 'signature-a',
      width: 420,
      ownerItemIds: ['item-a'],
    };

    expect(rowUiState.cachedTimelineRowHeight(key)).toBeUndefined();

    rowUiState.rememberTimelineRowHeight(key, 123.4);

    // Exact fractional round-trip — cached heights back the remount
    // min-height floor, so rounding to 123 would release each floor with a
    // 0.4px residue (the settle-flicker amplifier,
    // docs/architecture/settle-flicker-analysis.md).
    expect(rowUiState.cachedTimelineRowHeight(key)).toBe(123.4);
    expect(rowUiState.cachedTimelineRowHeight({
      ...key,
      signature: 'signature-b',
    })).toBeUndefined();
    expect(rowUiState.cachedTimelineRowHeight({
      ...key,
      width: 421,
    })).toBeUndefined();

    const widerKey = { ...key, width: 900 };
    rowUiState.rememberTimelineRowHeight(widerKey, 200);
    expect(rowUiState.cachedTimelineRowHeight(key)).toBe(123.4);
    expect(rowUiState.cachedTimelineRowHeight(widerKey)).toBe(200);

    rowUiState.rememberTimelineRowHeight({
      ...key,
      signature: 'signature-c',
    }, 300);
    expect(rowUiState.cachedTimelineRowHeight(key)).toBeUndefined();
    expect(rowUiState.cachedTimelineRowHeight(widerKey)).toBeUndefined();
  });

  it('drops preserved row geometry when a diff card is toggled, sparing other rows', () => {
    const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
    const key = {
      key: 'l:thread-a:item-a',
      signature: 'sig-a',
      width: 800,
      ownerItemIds: ['item-a'],
    };
    const otherKey = {
      key: 'l:thread-a:item-b',
      signature: 'sig-b',
      width: 800,
      ownerItemIds: ['item-b'],
    };
    rowUiState.rememberTimelineRowHeight(key, 200);
    rowUiState.rememberTimelineRowHeight(otherKey, 90);

    // Collapsing the card is a deliberate height change; its stale
    // expanded geometry must not survive to be reserved on a remount.
    rowUiState.setDiffCardExpanded('item-a', 'src/foo.ts', false);

    expect(rowUiState.cachedTimelineRowHeight(key)).toBeUndefined();
    expect(rowUiState.cachedTimelineRowHeight(otherKey)).toBe(90);
  });

  it('drops preserved row geometry when an item expansion handle collapses', async () => {
    setBindingMock('GetPayloadPreview', async () => ({
      data: 'body',
      nextOffset: 4,
      totalSize: 4,
      isComplete: true,
    }));
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'payload-a',
      threadId: 'thread-a',
    });
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return itemId === item.id ? item : undefined;
      },
    });
    const key = {
      key: `l:${item.threadId}:${item.id}`,
      signature: 'sig',
      width: 800,
      ownerItemIds: [item.id],
    };

    const handle = rowUiState.expansionStateFor(item);
    await handle.expand();
    rowUiState.rememberTimelineRowHeight(key, 200);
    expect(rowUiState.cachedTimelineRowHeight(key)).toBe(200);

    // The user collapses the row: its preserved expanded height is dropped
    // synchronously, before any collapse-driven remount can reserve it.
    handle.collapse();
    expect(rowUiState.cachedTimelineRowHeight(key)).toBeUndefined();
  });

  it('disposes per-item expansion and attachment state', () => {
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'payload-a',
      threadId: 'thread-a',
    });
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return itemId === item.id ? item : undefined;
      },
    });
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});

    const beforeDispose = rowUiState.expansionStateFor(item);
    const beforePayloadDispose = rowUiState.expansionStateForPayload(
      'payload-a',
      'thread-a',
    );
    rowUiState.toggleSubagentGroupExpanded(item.id);
    rowUiState.toggleSubagentGroupExpanded(`wait:${item.id}`);
    const staleCache = rowUiState.attachmentCacheFor(item.id);
    staleCache.set('before-dispose', {
      id: 'before-dispose',
      filename: 'before.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:before-dispose',
    });

    rowUiState.setDiffCardExpanded(item.id, 'src/foo.ts', true);
    rowUiState.setDiffCardExpanded('item-other', 'src/keep.ts', false);
    const rowGeometryKey = {
      key: `l:${item.threadId}:${item.id}`,
      signature: 'row-signature',
      width: 800,
      ownerItemIds: [item.id],
    };
    rowUiState.rememberTimelineRowHeight(rowGeometryKey, 144);

    expect(rowUiState.debugStats().itemExpansionStates).toBe(1);
    expect(rowUiState.debugStats().payloadExpansionStates).toBe(1);
    expect(rowUiState.debugStats().rowGeometryItems).toBe(1);
    rowUiState.disposeItems([item]);

    expect(rowUiState.debugStats().itemExpansionStates).toBe(0);
    expect(rowUiState.debugStats().payloadExpansionStates).toBe(0);
    expect(rowUiState.debugStats().attachmentItems).toBe(0);
    expect(rowUiState.debugStats().rowGeometryItems).toBe(0);
    expect(rowUiState.cachedTimelineRowHeight(rowGeometryKey)).toBeUndefined();
    expect(rowUiState.isSubagentGroupExpanded(item.id)).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded(`wait:${item.id}`)).toBe(false);
    // Diff card overrides for the disposed item drop; others survive.
    expect(rowUiState.diffCardExpandedOverride(item.id, 'src/foo.ts')).toBeUndefined();
    expect(rowUiState.diffCardExpandedOverride('item-other', 'src/keep.ts')).toBe(false);
    expect(revoke).toHaveBeenCalledWith('blob:before-dispose');

    staleCache.set('after-dispose', {
      id: 'after-dispose',
      filename: 'after.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:after-dispose',
    });
    expect(revoke).toHaveBeenCalledWith('blob:after-dispose');

    const afterDispose = rowUiState.expansionStateFor(item);
    expect(Object.is(afterDispose, beforeDispose)).toBe(false);
    expect(rowUiState.expansionStateForPayload('payload-a', 'thread-a')).not.toBe(beforePayloadDispose);
    revoke.mockRestore();
  });

  it('cancels in-flight payload loads when pruning an expansion handle', async () => {
    let resolvePreview: ((value: {
      data: string;
      nextOffset: number;
      totalSize: number;
      isComplete: boolean;
    }) => void) | undefined;
    setBindingMock('GetPayloadPreview', () => new Promise((resolve) => {
      resolvePreview = resolve;
    }));
    const item = makeItem({
      id: 'tool:inflight',
      kind: 'tool_call',
      payloadId: 'payload-inflight',
      threadId: 'thread-a',
    });
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return itemId === item.id ? item : undefined;
      },
    });

    const expansion = rowUiState.expansionStateFor(item);
    const load = expansion.expand();
    await vi.waitFor(() => expect(resolvePreview).toBeDefined());

    rowUiState.disposeItems([item]);
    resolvePreview?.({
      data: 'late payload',
      nextOffset: 12,
      totalSize: 12,
      isComplete: true,
    });
    await load;

    expect(readPayloadCache('thread-a', 'payload-inflight', 'payload-inflight')).toBeUndefined();
    const replacement = rowUiState.expansionStateFor(item);
    expect(replacement).not.toBe(expansion);
    expect(replacement.displayData).toBeNull();
  });

  it('keeps pruned leased expansion handles readable until release', async () => {
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
    try {
      let resolvePreview: ((value: {
        data: string;
        nextOffset: number;
        totalSize: number;
        isComplete: boolean;
      }) => void) | undefined;
      setBindingMock('GetPayloadPreview', () => new Promise((resolve) => {
        resolvePreview = resolve;
      }));
      const item = makeItem({
        id: 'tool:leased-prune',
        kind: 'tool_call',
        payloadId: 'payload-leased-prune',
        threadId: 'thread-a',
      });
      const rowUiState = createThreadRowUiState({
        getItemById(itemId: string): Item | undefined {
          return itemId === item.id ? item : undefined;
        },
      });

      const lease = rowUiState.retainExpansionStateFor(item);
      const load = lease.handle.expand();
      await vi.waitFor(() => expect(resolvePreview).toBeDefined());

      rowUiState.pruneRowUiState({
        itemIds: [],
        payloads: [],
        groupKeys: [],
        rowGeometryKeys: [],
      });

      expect(rowUiState.debugStats().expansionStates).toBe(0);
      expect(rowUiState.expansionStateFor(item)).not.toBe(lease.handle);

      resolvePreview?.({
        data: 'loaded while leased',
        nextOffset: 19,
        totalSize: 19,
        isComplete: true,
      });
      await load;

      expect(lease.handle.displayData).toBe('loaded while leased');
      const warnedDerivedInert = warn.mock.calls
        .flat()
        .some((arg) => String(arg).includes('derived_inert'));
      expect(warnedDerivedInert).toBe(false);

      lease.release();
    } finally {
      warn.mockRestore();
    }
  });

  it('routes live deltas to pruned leased expansion handles until release', async () => {
    const item = makeItem({
      id: 'think:leased-prune',
      kind: 'thinking',
      payloadId: 'thinking-payload',
      threadId: 'thread-a',
      updatedAt: 1,
    });
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return itemId === item.id ? item : undefined;
      },
    });
    const getPayloadData = setBindingMock('GetPayloadData', async () => ({ data: 'seed' }));

    const lease = rowUiState.retainExpansionStateFor(item, {
      loadMode: 'full',
      stateKey: THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      payloadVersion: (currentItem) => currentItem?.updatedAt,
    });
    await lease.handle.expand();

    rowUiState.pruneRowUiState({
      itemIds: [],
      payloads: [],
      groupKeys: [],
      rowGeometryKeys: [],
    });
    rowUiState.appendLivePayloadDeltaForItem(
      item.id,
      THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      ' live',
      2,
    );

    expect(lease.handle.displayData).toBe('seed live');
    expect(getPayloadData).toHaveBeenCalledTimes(1);

    lease.release();
    rowUiState.appendLivePayloadDeltaForItem(
      item.id,
      THINKING_PAYLOAD_EXPANSION_STATE_KEY,
      ' ignored',
      3,
    );
    expect(lease.handle.displayData).toBe('seed live');
  });

  it('prunes old expansion, attachment, and group state outside the retained row window', () => {
    const items = new Map<string, Item>();
    const oldItem = makeItem({
      id: 'tool:old',
      kind: 'tool_call',
      payloadId: 'payload-old',
      threadId: 'thread-a',
    });
    const retainedItem = makeItem({
      id: 'tool:retained',
      kind: 'tool_call',
      payloadId: 'payload-retained',
      threadId: 'thread-a',
    });
    items.set(oldItem.id, oldItem);
    items.set(retainedItem.id, retainedItem);
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return items.get(itemId);
      },
    });
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {});

    const oldItemExpansion = rowUiState.expansionStateFor(oldItem);
    const retainedItemExpansion = rowUiState.expansionStateFor(retainedItem);
    const oldPayloadExpansion = rowUiState.expansionStateForPayload(
      'payload-old',
      'thread-a',
    );
    const retainedPayloadExpansion = rowUiState.expansionStateForPayload(
      'payload-retained',
      'thread-a',
    );
    rowUiState.toggleSubagentGroupExpanded('group-old');
    rowUiState.toggleSubagentGroupExpanded('group-retained');
    rowUiState.setDiffCardExpanded(oldItem.id, 'src/old.ts', true);
    rowUiState.setDiffCardExpanded(retainedItem.id, 'src/retained.ts', false);
    const oldAttachmentCache = rowUiState.attachmentCacheFor(oldItem.id);
    const retainedAttachmentCache = rowUiState.attachmentCacheFor(retainedItem.id);
    oldAttachmentCache.set('old-attachment', {
      id: 'old-attachment',
      filename: 'old.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:old-attachment',
    });
    retainedAttachmentCache.set('retained-attachment', {
      id: 'retained-attachment',
      filename: 'retained.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:retained-attachment',
    });
    const oldRowGeometryKey = {
      key: `l:${oldItem.threadId}:${oldItem.id}`,
      signature: 'old-row-signature',
      width: 800,
      ownerItemIds: [oldItem.id],
    };
    const retainedRowGeometryKey = {
      key: `l:${retainedItem.threadId}:${retainedItem.id}`,
      signature: 'retained-row-signature',
      width: 800,
      ownerItemIds: [retainedItem.id],
    };
    rowUiState.rememberTimelineRowHeight(oldRowGeometryKey, 111);
    rowUiState.rememberTimelineRowHeight(retainedRowGeometryKey, 222);

    expect(rowUiState.debugStats()).toMatchObject({
      expansionStates: 4,
      itemExpansionStates: 2,
      payloadExpansionStates: 2,
      subagentGroups: 2,
      diffCardOverrideItems: 2,
      attachmentItems: 2,
      rowGeometryItems: 2,
    });

    rowUiState.pruneRowUiState({
      itemIds: [retainedItem.id],
      payloads: [{ threadId: 'thread-a', payloadId: 'payload-retained' }],
      groupKeys: ['group-retained'],
      rowGeometryKeys: [retainedRowGeometryKey.key],
    });

    expect(rowUiState.debugStats()).toMatchObject({
      expansionStates: 2,
      itemExpansionStates: 1,
      payloadExpansionStates: 1,
      subagentGroups: 1,
      diffCardOverrideItems: 1,
      attachmentItems: 1,
      rowGeometryItems: 1,
    });
    expect(revoke).toHaveBeenCalledWith('blob:old-attachment');
    expect(rowUiState.isSubagentGroupExpanded('group-old')).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded('group-retained')).toBe(true);
    expect(rowUiState.diffCardExpandedOverride(oldItem.id, 'src/old.ts')).toBeUndefined();
    expect(rowUiState.diffCardExpandedOverride(retainedItem.id, 'src/retained.ts')).toBe(false);
    expect(retainedAttachmentCache.get('retained-attachment')).toBeTruthy();
    expect(oldAttachmentCache.get('old-attachment')).toBeUndefined();
    expect(rowUiState.cachedTimelineRowHeight(oldRowGeometryKey)).toBeUndefined();
    expect(rowUiState.cachedTimelineRowHeight(retainedRowGeometryKey)).toBe(222);

    retainedAttachmentCache.set('retained-after-prune', {
      id: 'retained-after-prune',
      filename: 'retained-after.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:retained-after-prune',
    });
    expect(retainedAttachmentCache.get('retained-after-prune')).toBeTruthy();
    expect(revoke).not.toHaveBeenCalledWith('blob:retained-after-prune');

    oldAttachmentCache.set('after-prune', {
      id: 'after-prune',
      filename: 'after.png',
      mimeType: 'image/png',
      size: 1,
      url: 'blob:after-prune',
    });
    expect(revoke).toHaveBeenCalledWith('blob:after-prune');
    expect(rowUiState.expansionStateFor(retainedItem)).toBe(retainedItemExpansion);
    expect(rowUiState.expansionStateForPayload('payload-retained', 'thread-a')).toBe(retainedPayloadExpansion);
    expect(rowUiState.expansionStateFor(oldItem)).not.toBe(oldItemExpansion);
    expect(rowUiState.expansionStateForPayload('payload-old', 'thread-a')).not.toBe(oldPayloadExpansion);
    revoke.mockRestore();
  });

  it('clears expansion and group state', () => {
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'payload-a',
      threadId: 'thread-a',
    });
    const rowUiState = createThreadRowUiState({
      getItemById(itemId: string): Item | undefined {
        return itemId === item.id ? item : undefined;
      },
    });

    const beforeClear = rowUiState.expansionStateFor(item);
    rowUiState.toggleSubagentGroupExpanded('group-a');
    rowUiState.setDiffCardExpanded(item.id, 'src/foo.ts', false);
    rowUiState.clear();

    const afterClear = rowUiState.expansionStateFor(item);
    expect(Object.is(afterClear, beforeClear)).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
    expect(rowUiState.diffCardExpandedOverride(item.id, 'src/foo.ts')).toBeUndefined();
  });

  // expansionSignature stamps a virtua measured-size snapshot so it only
  // replays into a remount whose rows will render at the same heights — see
  // utils/threadVirtuaSizeCache.ts. The contract: empty for a thread at
  // default expansion (the state clear() restores on every switch), non-empty
  // the moment any row is expanded taller than default, and deterministic so
  // capture and restore compare equal.
  describe('expansionSignature', () => {
    it('is empty when every row is at default expansion', () => {
      const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
      expect(rowUiState.expansionSignature()).toBe('');
    });

    it('reflects an expanded subagent group', () => {
      const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
      rowUiState.toggleSubagentGroupExpanded('group-a');
      expect(rowUiState.expansionSignature()).toContain('g:group-a');
    });

    it('reflects a diff-card override (any value — the user pinned away from default)', () => {
      const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
      rowUiState.setDiffCardExpanded('item-1', 'src/foo.ts', true);
      expect(rowUiState.expansionSignature()).toContain('d:item-1/src/foo.ts=1');
    });

    it('stamps a force-collapsed diff override distinctly from expanded', () => {
      // A `false` override is a real non-default state (the card is pinned away
      // from the collapseDiffPreviews default), so the `=0` arm must stamp AND
      // differ from `=1` — otherwise two different row heights share a
      // signature and the cache replays the wrong one. (The prior coverage only
      // asserted `=1`; "returns to empty after clear" toggled a group too, so
      // its non-empty check never proved the `=0` branch.)
      const collapsed = createThreadRowUiState({ getItemById: () => undefined });
      collapsed.setDiffCardExpanded('item-1', 'src/foo.ts', false);
      expect(collapsed.expansionSignature()).toContain('d:item-1/src/foo.ts=0');

      const expanded = createThreadRowUiState({ getItemById: () => undefined });
      expanded.setDiffCardExpanded('item-1', 'src/foo.ts', true);
      expect(collapsed.expansionSignature()).not.toBe(expanded.expansionSignature());
    });

    it('reflects an expanded payload handle', async () => {
      setBindingMock('GetPayloadData', async () => ({ data: 'x' }));
      const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
      const handle = rowUiState.expansionStateForPayload('payload-a', 'thread-a', 1);
      await handle.expand();
      expect(handle.expanded).toBe(true);
      expect(rowUiState.expansionSignature()).toContain('p:');
    });

    it('is deterministic regardless of expansion order (capture == restore)', () => {
      // Each segment (g:/d:/p:) is sorted independently, so insertion order
      // must not change the string — capture and restore visit the same maps in
      // whatever order they were populated. Exercise both the g: and d: sorts.
      const a = createThreadRowUiState({ getItemById: () => undefined });
      a.toggleSubagentGroupExpanded('group-b');
      a.toggleSubagentGroupExpanded('group-a');
      a.setDiffCardExpanded('item-2', 'src/z.ts', true);
      a.setDiffCardExpanded('item-1', 'src/a.ts', false);
      const b = createThreadRowUiState({ getItemById: () => undefined });
      b.setDiffCardExpanded('item-1', 'src/a.ts', false);
      b.toggleSubagentGroupExpanded('group-a');
      b.setDiffCardExpanded('item-2', 'src/z.ts', true);
      b.toggleSubagentGroupExpanded('group-b');
      expect(a.expansionSignature()).toBe(b.expansionSignature());
    });

    it('returns to empty after clear() (the switch-in reset)', () => {
      const rowUiState = createThreadRowUiState({ getItemById: () => undefined });
      rowUiState.toggleSubagentGroupExpanded('group-a');
      rowUiState.setDiffCardExpanded('item-1', 'src/foo.ts', false);
      expect(rowUiState.expansionSignature()).not.toBe('');
      rowUiState.clear();
      expect(rowUiState.expansionSignature()).toBe('');
    });
  });
});
