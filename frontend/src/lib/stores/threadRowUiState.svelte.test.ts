import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { Item } from '../types/models';
import { makeItem } from '../../test/helpers/chat';
import { __resetPayloadCacheForTest, writePayloadCache } from '../utils/payloadDataCache';
import { createThreadRowUiState } from './threadRowUiState.svelte';

describe('createThreadRowUiState', () => {
  beforeEach(() => {
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
    expect(first.payloadVersion).toBe(2);
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

    expect(rowUiState.expansionStateFor(item)).not.toBe(beforeClear);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
  });
});
