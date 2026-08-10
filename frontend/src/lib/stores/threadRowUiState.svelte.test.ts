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
import { loadSettings, resetSettingsForTest } from './settings.svelte';
import { makeSettings } from '../../test/helpers/settings';
import { resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';

/**
 * "Nothing is loaded" stated explicitly, which is what most of these cases
 * want: every drop releases the payload it touched. `loadedPayloadRefs` is
 * REQUIRED precisely so this cannot be expressed by omission — the cases
 * that exercise the surviving-rows leg pass their own list.
 */
const NO_ROWS_LOADED = {
  getItemById: () => undefined,
  loadedPayloadRefs: () => [],
};

// Diff-card overrides are read against the live collapseDiffPreviews default
// (see `liveDiffOverride`), so a case about a specific default has to state
// it. Everything else runs on the shipped default: collapseDiffPreviews off,
// i.e. diff cards expanded.
async function seedCollapseDiffPreviews(collapseDiffPreviews: boolean): Promise<void> {
  setBindingMock('GetSettings', async () => makeSettings({ collapseDiffPreviews }));
  await loadSettings();
}

describe('createThreadRowUiState', () => {
  beforeEach(() => {
    resetBindingMocks();
    resetSettingsForTest();
    __resetPayloadCacheForTest();
  });

  it('keeps item-keyed expansion handles stable while reading the latest item reference', () => {
    const items = new Map<string, Item>();
    const rowUiState = createThreadRowUiState({
      loadedPayloadRefs: () => [],
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
        loadedPayloadRefs: () => [],
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
      loadedPayloadRefs: () => [],
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
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

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
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

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
      loadedPayloadRefs: () => [],
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
      loadedPayloadRefs: () => [],
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
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

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
      loadedPayloadRefs: () => [],
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

  it('keeps registry keys distinct across ids carrying JSON metacharacters', () => {
    // The registry keys used to be `JSON.stringify(parts)`, which escaped
    // its way out of any id content; they are NUL joins now, which is
    // injective for every byte an id can actually carry but relies on the
    // separator being absent. Quotes, backslashes, commas and brackets are
    // what the old encoder was protecting against, so they are what a
    // regression would land on first.
    const items = new Map<string, Item>();
    const rowUiState = createThreadRowUiState({
      loadedPayloadRefs: () => [],
      getItemById: (itemId) => items.get(itemId),
    });
    const ids = ['a"b', 'a\\b', 'a,b', '["a"]', 'a]b['];
    const handles = ids.map((id) => {
      const item = makeItem({ id, threadId: 'thread-a', payloadId: `payload-${id}` });
      items.set(id, item);
      return rowUiState.expansionStateFor(item);
    });

    expect(new Set(handles).size).toBe(ids.length);
    expect(rowUiState.debugStats().itemExpansionStates).toBe(ids.length);
    for (const [index, id] of ids.entries()) {
      expect(rowUiState.expansionStateFor(items.get(id)!)).toBe(handles[index]);
    }
  });

  it('does not reuse item-keyed expansion handles across threads with the same item id', () => {
    const items = new Map<string, Item>();
    const rowUiState = createThreadRowUiState({
      loadedPayloadRefs: () => [],
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
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

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
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

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
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
    expect(rowUiState.toggleSubagentGroupExpanded('group-a')).toBe(true);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(true);
    expect(rowUiState.isSubagentGroupExpanded('group-b')).toBe(false);
    expect(rowUiState.toggleSubagentGroupExpanded('group-a')).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
  });

  it('tracks diff card expanded overrides by item id and file path', () => {
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

    // Absent = follow the collapseDiffPreviews setting default, which is
    // expanded here (collapseDiffPreviews defaults off), so a reader's only
    // available deviation is a collapse.
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBeUndefined();

    rowUiState.setDiffCardExpanded('item-a', 'src/foo.ts', false);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(false);

    // Files under the same item stay independent, as do other items.
    rowUiState.setDiffCardExpanded('item-a', 'src/bar.ts', false);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/bar.ts')).toBe(false);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(false);
    expect(rowUiState.diffCardExpandedOverride('item-b', 'src/foo.ts')).toBeUndefined();

    // Putting a card BACK to the default stores nothing: an override is a
    // deviation, so there is no redundant entry to leave behind and no
    // clear call for the caller to forget. Emptying an item drops its entry.
    rowUiState.setDiffCardExpanded('item-a', 'src/bar.ts', true);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/bar.ts')).toBeUndefined();
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(false);
    rowUiState.setDiffCardExpanded('item-a', 'src/foo.ts', true);
    expect(rowUiState.debugStats().diffCardOverrideItems).toBe(0);
  });

  // The derive-the-answer-from-the-default rule (`liveDiffOverride`): the
  // setting flipping retires the overrides it catches up with, with no
  // $effect, no sweep and no generation counter — and flipping back restores
  // the reader's pin, because nothing was destroyed.
  it('retires diff overrides the collapseDiffPreviews default catches up with', async () => {
    await seedCollapseDiffPreviews(true);
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

    // Default collapsed: expanding is the deviation, and it is engagement.
    rowUiState.setDiffCardExpanded('item-a', 'src/foo.ts', true);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(true);
    expect(rowUiState.expansionSignature()).toContain('d:item-a/src/foo.ts=1');
    expect(rowUiState.hasUserExpansionWithin(['item-a'])).toBe(true);

    // Default flips to expanded: the card renders the same, but it is no
    // longer a deviation, so it stops pinning the priors snapshot and stops
    // reading as engagement to the activity-run auto-collapse gate.
    await seedCollapseDiffPreviews(false);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBeUndefined();
    expect(rowUiState.expansionSignature()).toBe('');
    expect(rowUiState.hasUserExpansionWithin(['item-a'])).toBe(false);

    // Flip back and the reader's pin is theirs again.
    await seedCollapseDiffPreviews(true);
    expect(rowUiState.diffCardExpandedOverride('item-a', 'src/foo.ts')).toBe(true);
    expect(rowUiState.hasUserExpansionWithin(['item-a'])).toBe(true);
  });

  it('keeps attachment cache entries isolated by item and clears them with blob revocation', () => {
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
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
    const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
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

  it('disposes per-item expansion and attachment state', () => {
    const item = makeItem({
      id: 'tool:5:0',
      kind: 'tool_call',
      payloadId: 'payload-a',
      threadId: 'thread-a',
    });
    const rowUiState = createThreadRowUiState({
      loadedPayloadRefs: () => [],
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

    rowUiState.setDiffCardExpanded(item.id, 'src/foo.ts', false);
    rowUiState.setDiffCardExpanded('item-other', 'src/keep.ts', false);

    expect(rowUiState.debugStats().itemExpansionStates).toBe(1);
    expect(rowUiState.debugStats().payloadExpansionStates).toBe(1);
    rowUiState.disposeItems([item]);

    expect(rowUiState.debugStats().itemExpansionStates).toBe(0);
    expect(rowUiState.debugStats().payloadExpansionStates).toBe(0);
    expect(rowUiState.debugStats().attachmentItems).toBe(0);
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

  it('releases payload state only once the last loaded row stops referencing it', () => {
    // Two rows share one payload — a tool_call and its completion. The
    // first drop must keep the payload's UI state alive for the survivor;
    // the second must release it. `loadedPayloadRefs` is read once per
    // batch, not once per dropped row: a prune drops hundreds of rows and
    // the per-row form was a full window scan each time.
    const rowA = makeItem({ id: 'tool:5:0', kind: 'tool_call', payloadId: 'payload-a', threadId: 'thread-a' });
    const rowB = makeItem({ id: 'tool:5:1', kind: 'tool_completion', payloadId: 'payload-a', threadId: 'thread-a' });
    const noise = Array.from({ length: 8 }, (_, i) =>
      makeItem({ id: `text:${i}`, threadId: 'thread-a' }));
    let loaded: Item[] = [rowA, rowB, ...noise];
    const loadedPayloadRefs = vi.fn(() => loaded);
    const rowUiState = createThreadRowUiState({
      getItemById: (itemId) => loaded.find((item) => item.id === itemId),
      loadedPayloadRefs,
    });

    rowUiState.expansionStateForPayload('payload-a', 'thread-a');
    expect(rowUiState.debugStats().payloadExpansionStates).toBe(1);

    loaded = [rowB, ...noise];
    rowUiState.disposeItems([rowA, ...noise.slice(0, 3)]);
    expect(loadedPayloadRefs).toHaveBeenCalledTimes(1);
    expect(rowUiState.debugStats().payloadExpansionStates).toBe(1);

    loaded = [];
    rowUiState.disposeItems([rowB]);
    expect(loadedPayloadRefs).toHaveBeenCalledTimes(2);
    expect(rowUiState.debugStats().payloadExpansionStates).toBe(0);
  });

  it('does not consult the loaded window when no dropped row carries a payload', () => {
    const loadedPayloadRefs = vi.fn(() => [] as Item[]);
    const rowUiState = createThreadRowUiState({
      getItemById: () => undefined,
      loadedPayloadRefs,
    });

    rowUiState.disposeItems([makeItem({ id: 'text:0', threadId: 'thread-a' })]);

    expect(loadedPayloadRefs).not.toHaveBeenCalled();
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
      loadedPayloadRefs: () => [],
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
        loadedPayloadRefs: () => [],
        getItemById(itemId: string): Item | undefined {
          return itemId === item.id ? item : undefined;
        },
      });

      const lease = rowUiState.retainExpansionStateFor(item);
      const load = lease.handle.expand();
      await vi.waitFor(() => expect(resolvePreview).toBeDefined());

      rowUiState.pruneRowUiState({
        itemIds: new Set(),
        payloads: [],
        groupKeys: new Set(),
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
      loadedPayloadRefs: () => [],
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
      itemIds: new Set(),
      payloads: [],
      groupKeys: new Set(),
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
      loadedPayloadRefs: () => [],
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
    rowUiState.setDiffCardExpanded(oldItem.id, 'src/old.ts', false);
    rowUiState.setDiffCardExpanded(retainedItem.id, 'src/retained.ts', false);
    rowUiState.setUserMessageExpanded(oldItem.id, true);
    rowUiState.setUserMessageExpanded(retainedItem.id, true);
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
    expect(rowUiState.debugStats()).toMatchObject({
      expansionStates: 4,
      itemExpansionStates: 2,
      payloadExpansionStates: 2,
      subagentGroups: 2,
      expandedUserMessages: 2,
      diffCardOverrideItems: 2,
      attachmentItems: 2,
    });

    rowUiState.pruneRowUiState({
      itemIds: new Set([retainedItem.id]),
      payloads: [{ threadId: 'thread-a', payloadId: 'payload-retained' }],
      groupKeys: new Set(['group-retained']),
    });

    expect(rowUiState.debugStats()).toMatchObject({
      expansionStates: 2,
      itemExpansionStates: 1,
      payloadExpansionStates: 1,
      subagentGroups: 1,
      expandedUserMessages: 1,
      diffCardOverrideItems: 1,
      attachmentItems: 1,
    });
    expect(revoke).toHaveBeenCalledWith('blob:old-attachment');
    expect(rowUiState.isSubagentGroupExpanded('group-old')).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded('group-retained')).toBe(true);
    expect(rowUiState.isUserMessageExpanded(oldItem.id)).toBe(false);
    expect(rowUiState.isUserMessageExpanded(retainedItem.id)).toBe(true);
    expect(rowUiState.diffCardExpandedOverride(oldItem.id, 'src/old.ts')).toBeUndefined();
    expect(rowUiState.diffCardExpandedOverride(retainedItem.id, 'src/retained.ts')).toBe(false);
    expect(retainedAttachmentCache.get('retained-attachment')).toBeTruthy();
    expect(oldAttachmentCache.get('old-attachment')).toBeUndefined();

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
      loadedPayloadRefs: () => [],
      getItemById(itemId: string): Item | undefined {
        return itemId === item.id ? item : undefined;
      },
    });

    const beforeClear = rowUiState.expansionStateFor(item);
    rowUiState.toggleSubagentGroupExpanded('group-a');
    rowUiState.setDiffCardExpanded(item.id, 'src/foo.ts', false);
    rowUiState.setUserMessageExpanded(item.id, true);
    rowUiState.clear();

    const afterClear = rowUiState.expansionStateFor(item);
    expect(Object.is(afterClear, beforeClear)).toBe(false);
    expect(rowUiState.isSubagentGroupExpanded('group-a')).toBe(false);
    expect(rowUiState.isUserMessageExpanded(item.id)).toBe(false);
    expect(rowUiState.diffCardExpandedOverride(item.id, 'src/foo.ts')).toBeUndefined();
  });

  // The registry half of the clamped-user-message feature: the row itself
  // unmounts whenever it leaves the virtualizer's window, so "Show more" has
  // to be remembered here or the message re-clamps under the reader.
  describe('user message clamp expansion', () => {
    it('remembers an expanded message and forgets it on collapse', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

      expect(rowUiState.isUserMessageExpanded('user:1')).toBe(false);
      rowUiState.setUserMessageExpanded('user:1', true);
      expect(rowUiState.isUserMessageExpanded('user:1')).toBe(true);
      // Every message defaults to clamped, so collapsing stores nothing —
      // membership IS the deviation.
      expect(rowUiState.debugStats().expandedUserMessages).toBe(1);
      rowUiState.setUserMessageExpanded('user:1', false);
      expect(rowUiState.isUserMessageExpanded('user:1')).toBe(false);
      expect(rowUiState.debugStats().expandedUserMessages).toBe(0);
    });

    it('keeps messages independent and is idempotent in both directions', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

      rowUiState.setUserMessageExpanded('user:1', true);
      rowUiState.setUserMessageExpanded('user:1', true);
      rowUiState.setUserMessageExpanded('user:2', false);
      rowUiState.setUserMessageExpanded('user:2', false);

      expect(rowUiState.isUserMessageExpanded('user:1')).toBe(true);
      expect(rowUiState.isUserMessageExpanded('user:2')).toBe(false);
      expect(rowUiState.debugStats().expandedUserMessages).toBe(1);
    });

    it('drops the expansion when its item is disposed', () => {
      const item = makeItem({ id: 'user:1', kind: 'user_text', threadId: 'thread-a' });
      const rowUiState = createThreadRowUiState({ getItemById: () => item, loadedPayloadRefs: () => [] });

      rowUiState.setUserMessageExpanded(item.id, true);
      rowUiState.disposeItems([item]);
      expect(rowUiState.isUserMessageExpanded(item.id)).toBe(false);
    });

    it('stamps the priors signature, since an unclamped message is a taller row', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

      expect(rowUiState.expansionSignature()).toBe('');
      rowUiState.setUserMessageExpanded('user:2', true);
      rowUiState.setUserMessageExpanded('user:1', true);
      expect(rowUiState.expansionSignature()).toContain('u:user:1,user:2');
      rowUiState.setUserMessageExpanded('user:1', false);
      rowUiState.setUserMessageExpanded('user:2', false);
      expect(rowUiState.expansionSignature()).toBe('');
    });

    it('counts as a user expansion on the item it belongs to', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

      expect(rowUiState.hasUserExpansionWithin(['user:1'])).toBe(false);
      rowUiState.setUserMessageExpanded('user:1', true);
      expect(rowUiState.hasUserExpansionWithin(['user:1'])).toBe(true);
      expect(rowUiState.hasUserExpansionWithin(['user:2'])).toBe(false);
    });
  });

  // expansionSignature stamps a measured-size priors snapshot so it only
  // replays into a remount whose rows will render at the same heights — see
  // utils/virtual/priors.ts. The contract: empty for a thread at
  // default expansion (the state clear() restores on every switch), non-empty
  // the moment any row is expanded taller than default, and deterministic so
  // capture and restore compare equal.
  describe('expansionSignature', () => {
    it('is empty when every row is at default expansion', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
      expect(rowUiState.expansionSignature()).toBe('');
    });

    it('reflects an expanded subagent group', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
      rowUiState.toggleSubagentGroupExpanded('group-a');
      expect(rowUiState.expansionSignature()).toContain('g:group-a');
    });

    it('reflects a diff-card override in either direction', async () => {
      await seedCollapseDiffPreviews(true);
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
      rowUiState.setDiffCardExpanded('item-1', 'src/foo.ts', true);
      expect(rowUiState.expansionSignature()).toContain('d:item-1/src/foo.ts=1');
    });

    it('stamps a force-collapsed diff override distinctly from expanded', async () => {
      // A `false` override is a real non-default state (the card is pinned away
      // from the collapseDiffPreviews default), so the `=0` arm must stamp AND
      // differ from `=1` — otherwise two different row heights share a
      // signature and the cache replays the wrong one. (The prior coverage only
      // asserted `=1`; "returns to empty after clear" toggled a group too, so
      // its non-empty check never proved the `=0` branch.)
      // The two arms need opposite defaults to exist at all: an override only
      // stores as the negation of the default in force when it was written.
      const collapsed = createThreadRowUiState(NO_ROWS_LOADED);
      collapsed.setDiffCardExpanded('item-1', 'src/foo.ts', false);
      const collapsedSig = collapsed.expansionSignature();
      expect(collapsedSig).toContain('d:item-1/src/foo.ts=0');

      await seedCollapseDiffPreviews(true);
      const expanded = createThreadRowUiState(NO_ROWS_LOADED);
      expanded.setDiffCardExpanded('item-1', 'src/foo.ts', true);
      expect(expanded.expansionSignature()).toContain('d:item-1/src/foo.ts=1');
      expect(collapsedSig).not.toBe(expanded.expansionSignature());
    });

    it('reflects an expanded payload handle', async () => {
      setBindingMock('GetPayloadData', async () => ({ data: 'x' }));
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
      const handle = rowUiState.expansionStateForPayload('payload-a', 'thread-a', 1);
      await handle.expand();
      expect(handle.expanded).toBe(true);
      expect(rowUiState.expansionSignature()).toContain('p:');
    });

    it('is deterministic regardless of expansion order (capture == restore)', () => {
      // Each segment (g:/d:/p:) is sorted independently, so insertion order
      // must not change the string — capture and restore visit the same maps in
      // whatever order they were populated. Exercise both the g: and d: sorts.
      const a = createThreadRowUiState(NO_ROWS_LOADED);
      a.toggleSubagentGroupExpanded('group-b');
      a.toggleSubagentGroupExpanded('group-a');
      a.setDiffCardExpanded('item-2', 'src/z.ts', false);
      a.setDiffCardExpanded('item-1', 'src/a.ts', false);
      const b = createThreadRowUiState(NO_ROWS_LOADED);
      b.setDiffCardExpanded('item-1', 'src/a.ts', false);
      b.toggleSubagentGroupExpanded('group-a');
      b.setDiffCardExpanded('item-2', 'src/z.ts', false);
      b.toggleSubagentGroupExpanded('group-b');
      expect(a.expansionSignature()).toBe(b.expansionSignature());
    });

    it('returns to empty after clear() (the switch-in reset)', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
      rowUiState.toggleSubagentGroupExpanded('group-a');
      rowUiState.setDiffCardExpanded('item-1', 'src/foo.ts', false);
      expect(rowUiState.expansionSignature()).not.toBe('');
      rowUiState.clear();
      expect(rowUiState.expansionSignature()).toBe('');
    });
  });

  describe('hasUserExpansionWithin', () => {
    // The activity-run auto-collapse gate's engagement peek. Same "user
    // deviations from default" contract as expansionSignature: only what the
    // reader explicitly opened counts, so a setting that defaults something
    // open must not pin every run that contains one.

    it('reports nothing for items at their defaults', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

      expect(rowUiState.hasUserExpansionWithin(['item-a', 'item-b'])).toBe(false);
    });

    it('counts a diff card overridden to expanded, not one overridden to collapsed', async () => {
      // An override back to collapsed is an ANSWER about the card, not
      // engagement with the run. Which of the two a reader can even produce
      // follows collapseDiffPreviews, so both arms are exercised under the
      // default that admits them.
      const openByDefault = createThreadRowUiState(NO_ROWS_LOADED);
      openByDefault.setDiffCardExpanded('item-a', 'src/a.ts', false);
      expect(openByDefault.hasUserExpansionWithin(['item-a'])).toBe(false);

      await seedCollapseDiffPreviews(true);
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);
      rowUiState.setDiffCardExpanded('item-a', 'src/b.ts', true);
      expect(rowUiState.hasUserExpansionWithin(['item-a'])).toBe(true);

      // Back to the default: the entry goes, and with it the engagement.
      rowUiState.setDiffCardExpanded('item-a', 'src/b.ts', false);
      expect(rowUiState.hasUserExpansionWithin(['item-a'])).toBe(false);
    });

    it('counts an expanded subagent card under any of its derived group keys', () => {
      const rowUiState = createThreadRowUiState(NO_ROWS_LOADED);

      for (const groupKey of ['item-a', 'wait:item-a', 'reads:item-a']) {
        rowUiState.toggleSubagentGroupExpanded(groupKey);
        expect(rowUiState.hasUserExpansionWithin(['item-a'])).toBe(true);
        rowUiState.toggleSubagentGroupExpanded(groupKey);
        expect(rowUiState.hasUserExpansionWithin(['item-a'])).toBe(false);
      }
    });

    it('tracks an item-keyed payload body across expand and collapse', async () => {
      setBindingMock('GetPayloadPreview', async () => ({
        data: 'x',
        nextOffset: 1,
        totalSize: 1,
        isComplete: true,
      }));
      const items = new Map<string, Item>();
      const item = makeItem({
        id: 'tool:0:0',
        kind: 'tool_call',
        payloadId: 'payload-a',
        threadId: 'thread-a',
      });
      items.set(item.id, item);
      const rowUiState = createThreadRowUiState({
        loadedPayloadRefs: () => [],
        getItemById: (itemId) => items.get(itemId),
      });

      const expansion = rowUiState.expansionStateFor(item);
      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(false);

      await expansion.expand();
      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(true);
      // And scoped: engagement with this item says nothing about others.
      expect(rowUiState.hasUserExpansionWithin(['item-elsewhere'])).toBe(false);

      expansion.collapse();
      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(false);
    });

    it('reaches a payload-keyed body through the item that carries the payload', async () => {
      // Some rows key their expansion by payloadId rather than itemId; the
      // gate only knows member item ids, so the peek resolves the item's
      // payload itself.
      setBindingMock('GetPayloadPreview', async () => ({
        data: 'x',
        nextOffset: 1,
        totalSize: 1,
        isComplete: true,
      }));
      const items = new Map<string, Item>();
      const item = makeItem({
        id: 'tool:0:0',
        kind: 'tool_call',
        payloadId: 'payload-a',
        threadId: 'thread-a',
      });
      items.set(item.id, item);
      const rowUiState = createThreadRowUiState({
        loadedPayloadRefs: () => [],
        getItemById: (itemId) => items.get(itemId),
      });

      const expansion = rowUiState.expansionStateForPayload('payload-a', 'thread-a');
      await expansion.expand();

      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(true);
    });

    it('never counts a loadOnMount entry, whose expanded bit is the setting\'s doing', async () => {
      // DiffFileStack and plan cards create their state with `loadOnMount:
      // true`, which drives expand() with no reader involved — a held run
      // full of auto-opened diff bodies must not be pinned by them. The skip
      // is wholesale (`autoExpands`), so even an expand() a reader triggered
      // on an auto entry stays invisible: on those entries the expanded bit
      // cannot distinguish the setting from the reader, and the accepted
      // reading is the setting.
      setBindingMock('GetPayloadPreview', async () => ({
        data: 'x',
        nextOffset: 1,
        totalSize: 1,
        isComplete: true,
      }));
      const items = new Map<string, Item>();
      const item = makeItem({
        id: 'tool:0:0',
        kind: 'tool_call',
        payloadId: 'payload-a',
        threadId: 'thread-a',
      });
      items.set(item.id, item);
      const rowUiState = createThreadRowUiState({
        loadedPayloadRefs: () => [],
        getItemById: (itemId) => items.get(itemId),
      });

      const payloadKeyed = rowUiState.expansionStateForPayload('payload-a', 'thread-a', {
        loadOnMount: true,
      });
      await payloadKeyed.expand();
      expect(payloadKeyed.expanded).toBe(true);
      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(false);

      const itemKeyed = rowUiState.expansionStateFor(item, { loadOnMount: true });
      await itemKeyed.expand();
      expect(itemKeyed.expanded).toBe(true);
      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(false);
    });

    it('still sees an expansion whose entry was pruned but is leased', async () => {
      // A leased-pruned entry is live user state — the row holding the lease
      // keeps rendering the expansion — so the run containing it is still
      // engaged-with even though the prune moved the entry aside.
      setBindingMock('GetPayloadPreview', async () => ({
        data: 'x',
        nextOffset: 1,
        totalSize: 1,
        isComplete: true,
      }));
      const items = new Map<string, Item>();
      const item = makeItem({
        id: 'tool:0:0',
        kind: 'tool_call',
        payloadId: 'payload-a',
        threadId: 'thread-a',
      });
      items.set(item.id, item);
      const rowUiState = createThreadRowUiState({
        loadedPayloadRefs: () => [],
        getItemById: (itemId) => items.get(itemId),
      });

      const lease = rowUiState.retainExpansionStateFor(item);
      await lease.handle.expand();
      rowUiState.pruneRowUiState({ itemIds: new Set(), payloads: [], groupKeys: new Set() });

      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(true);

      lease.release();
      expect(rowUiState.hasUserExpansionWithin([item.id])).toBe(false);
    });
  });
});
