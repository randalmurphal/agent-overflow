import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import { makeItem } from '../../test/helpers/chat';
import { __resetPayloadCacheForTest, writePayloadCache } from '../utils/payloadDataCache';
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
    rowUiState.clear();

    const afterClear = rowUiState.expansionStateFor(item);
    expect(Object.is(afterClear, beforeClear)).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
  });
});
