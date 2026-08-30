// stores/threadPaneRowUiHandles.test.ts
//
// threadRowUiState.svelte.ts through the pane: the per-row, per-payload and
// per-group handles the pane hands out have to be STABLE across lookups so
// a remounted row keeps its expansion and attachment state. The store's own
// eviction and retention rules are threadRowUiState.svelte.test.ts.

import { beforeEach, describe, expect, it } from 'vitest';
import { createThreadPane } from './thread.svelte';
import { type Item } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem } from '../../test/helpers/chat';
import { installThreadPaneTestEnv } from '../../test/helpers/threadPane';

describe('threadRowUiState (pane handles)', () => {
  beforeEach(installThreadPaneTestEnv);

  it('expansionStateFor returns the same handle across calls (survives row remount)', () => {
    // Why: the window's overscan eviction unmounts a row component when it
    // scrolls past the buffer; remounting reconstructs the snippet's
    // closure-scoped $state from scratch. The pane registry returns
    // the SAME handle reference for the same itemId, so toggle state
    // and loaded chunks survive the round-trip.
    const pane = createThreadPane();
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'p-foo',
    });
    pane.upsertItem(item);

    const h1 = pane.expansionStateFor(item);
    const h2 = pane.expansionStateFor(item);
    expect(h2).toBe(h1);

    // Even when the Item reference is replaced (e.g. enrichment), the
    // handle stays stable because the cache key is item.id.
    const itemRefBumped = { ...pane.items[0], updatedAt: 999 } as Item;
    const h3 = pane.expansionStateFor(itemRefBumped);
    expect(h3).toBe(h1);
  });

  it('expansionStateForPayload returns the same handle for the same payloadId', () => {
    const pane = createThreadPane();
    const h1 = pane.expansionStateForPayload('p-foo', 'thread-1');
    const h2 = pane.expansionStateForPayload('p-foo', 'thread-1');
    expect(h2).toBe(h1);
  });

  it('payload-keyed expansion handles reload when their version changes', async () => {
    let version = 1;
    const preview = setBindingMock('GetPayloadPreview', async () => ({
      data: version === 1 ? 'payload v1' : 'payload v2',
      nextOffset: 10,
      totalSize: 10,
      isComplete: true,
    }));

    const pane = createThreadPane();
    const first = pane.expansionStateForPayload(
      'p-versioned',
      'thread-1',
      version,
    );
    await first.expand();
    expect(first.displayData).toBe('payload v1');

    version = 2;
    const second = pane.expansionStateForPayload(
      'p-versioned',
      'thread-1',
      version,
    );
    expect(second).toBe(first);

    await second.ensureLoaded();
    expect(second.displayData).toBe('payload v2');
    expect(preview).toHaveBeenCalledTimes(2);
  });

  it('subagent group expansion state is keyed by groupKey and survives lookup', () => {
    const pane = createThreadPane();
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(false);
    pane.toggleSubagentGroupExpanded('group-1');
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(true);
    expect(pane.isSubagentGroupExpanded('group-2')).toBe(false);
    pane.toggleSubagentGroupExpanded('group-1');
    expect(pane.isSubagentGroupExpanded('group-1')).toBe(false);
  });

  it('attachmentCacheFor returns a stable view per itemId; survives lookup', () => {
    // Why: pre-rebuild, UserMessage.svelte allocated blob URLs in its
    // own onDestroy-revoking factory. The window's overscan eviction would
    // unmount + remount the row on a back-scroll, refetching every
    // attachment from Go. The pane-owned cache survives remount; the
    // factory seeds from it and writes loaded previews back.
    const pane = createThreadPane();
    const cacheA = pane.attachmentCacheFor('item-1');
    cacheA.set('att-1', {
      id: 'att-1',
      filename: 'a.png',
      mimeType: 'image/png',
      size: 1,
      url: 'data:img',
    });
    const cacheA2 = pane.attachmentCacheFor('item-1');
    expect(cacheA2.get('att-1')).toBeTruthy();
    expect(cacheA2.get('att-1')?.url).toBe('data:img');
    // Different itemId = isolated cache.
    const cacheB = pane.attachmentCacheFor('item-2');
    expect(cacheB.get('att-1')).toBeUndefined();
  });
});
