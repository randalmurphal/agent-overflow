// stores/threadTimelineWindow.svelte.test.ts
//
// threadTimelineWindow.svelte.ts through the pane: the bounded live window
// — its floor and ceiling, loadOlder/loadNewer/loadUntilItem paging and
// their generation guards, subagent-child hydration, and the authoritative
// refresh that must fold in the rows SQLite structurally cannot hold.

import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import { createThreadPane } from './thread.svelte';
import { type Item } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { makeItem, makeThread } from '../../test/helpers/chat';
import { flushMicrotasks, installThreadPaneTestEnv } from '../../test/helpers/threadPane';
import {
  ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
  ACTIVE_TIMELINE_WINDOW_MAX_ITEMS,
  ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
  SLICE_AROUND_ITEM_BUDGET,
} from './threadPaneShared';

describe('threadTimelineWindow', () => {
  beforeEach(installThreadPaneTestEnv);

  describe('windowed history', () => {
    it('upsertItem drops new items below the window floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({
          id: 'at-floor',
          threadId: 'thread-windowed',
          turnIndex: 5,
          itemIndex: 0,
        }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 'thread-windowed' }));
      expect(pane.oldestLoadedTurnIndex).toBe(5);

      // Upsert for a turn below the floor (e.g. interrupt-queue replay
      // of an older tool_completion). Must NOT land in the window — the
      // canonical row stays in SQLite and surfaces via loadOlder later.
      pane.upsertItem(
        makeItem({
          id: 'below',
          threadId: 'thread-windowed',
          turnIndex: 2,
          itemIndex: 0,
        }),
      );
      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
    });

    it('upsertItem still accepts replacements for known ids below the floor', async () => {
      const pane = createThreadPane();
      const seed: Item[] = [
        makeItem({
          id: 'known',
          threadId: 't',
          turnIndex: 5,
          itemIndex: 0,
          summary: 'old',
        }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: seed,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      // Known id, turn below floor — cross-turn correction path. Must
      // still replace because the id is clearly in-window already.
      pane.upsertItem(
        makeItem({
          id: 'known',
          threadId: 't',
          turnIndex: 2,
          itemIndex: 0,
          summary: 'new',
        }),
      );
      expect(pane.items.find((it) => it.id === 'known')?.summary).toBe('new');
    });

    it('upsertItem rejects new streaming rows below the floor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'at-floor',
            threadId: 't',
            turnIndex: 5,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));

      pane.upsertItem(
        makeItem({
          id: 'below-streaming',
          threadId: 't',
          turnIndex: 2,
          itemIndex: 0,
          status: 'streaming',
          summary: 'old output',
        }),
      );

      // Window-floor guard rejects the below-floor item; nothing
      // lingers anywhere because the pane no longer carries a parallel
      // streaming overlay.
      expect(pane.items.map((it) => it.id)).toEqual(['at-floor']);
    });

    it('loadOlder prepends older items and updates the floor + hasMore', async () => {
      const pane = createThreadPane();
      const tail: Item[] = [
        makeItem({ id: 't5', threadId: 't', turnIndex: 5, itemIndex: 0 }),
      ];
      setBindingMock('ListThreadSliceAround', async () => ({
        items: tail,
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [
          makeItem({ id: 't3', threadId: 't', turnIndex: 3, itemIndex: 0 }),
          makeItem({ id: 't4', threadId: 't', turnIndex: 4, itemIndex: 0 }),
        ],
        oldestTurnIndex: 3,
        hasMore: true,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      const revisionBeforeLoadOlder = pane.timelineRevision;
      const result = await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['t3', 't4', 't5']);
      expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeLoadOlder);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.loadingOlder).toBe(false);
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: true,
        insertedRows: true,
      });
    });

    it('keeps paging cursors paired with a window committed before bookkeeping fails', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 't5', threadId: 't', turnIndex: 5, itemIndex: 0 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [
          makeItem({ id: 't3', threadId: 't', turnIndex: 3, itemIndex: 0 }),
          makeItem({ id: 't4', threadId: 't', turnIndex: 4, itemIndex: 0 }),
        ],
        oldestTurnIndex: 3,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      vi.spyOn(pane.activityRuns, 'noteWholesaleReplace').mockImplementation(() => {
        throw new Error('activity replacement bookkeeping failed');
      });
      const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {});

      const result = await pane.loadOlder();

      expect(result.status).toBe('error');
      expect(pane.items.map((item) => item.id)).toEqual(['t3', 't4', 't5']);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
      expect(pane.newestLoadedTurnIndex).toBe(5);
      expect(pane.hasMoreHistory).toBe(false);
      consoleError.mockRestore();
    });

    it('loadOlder is a no-op when hasMoreHistory is false', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'a', turnIndex: 0, itemIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      let calls = 0;
      setBindingMock('ListItemsBeforeCursor', async () => {
        calls += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();
      expect(calls).toBe(0);
      expect(result).toEqual({
        status: 'noop',
        insertedBeforeWindow: false,
        insertedRows: false,
      });
    });

    it('loadOlder guards against a thread swap mid-fetch', async () => {
      const pane = createThreadPane();
      let resolveOlder!: (v: {
        items: Item[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }) => void;
      const olderPromise = new Promise<{
        items: Item[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }>((r) => {
        resolveOlder = r;
      });
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', () => olderPromise);

      await pane.switchThread(makeThread({ id: 'thread-a' }));
      const olderPending = pane.loadOlder();
      // Swap before the older fetch resolves.
      await pane.switchThread(makeThread({ id: 'thread-b' }));
      resolveOlder({
        items: [makeItem({ id: 'stale', turnIndex: 3, threadId: 'thread-a' })],
        oldestTurnIndex: 3,
        hasMore: true,
      });
      await olderPending;

      // thread-b has its own fresh window; the stale thread-a older
      // fetch must not leak into it.
      expect(pane.threadId).toBe('thread-b');
      expect(pane.items.some((it) => it.id === 'stale')).toBe(false);
    });

    it('loadUntilItem returns true when the item is already in-window', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'here', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      let fetched = 0;
      setBindingMock('GetThreadItem', async () => {
        fetched += 1;
        return makeItem({ id: 'here', turnIndex: 5 });
      });
      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('here');
      expect(ok).toBe(true);
      expect(fetched).toBe(0);
    });

    it('loadUntilItem replaces the window to cover a below-floor item', async () => {
      const pane = createThreadPane();
      let sliceCalls = 0;
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          sliceCalls += 1;
          if (anchorItemId === 'target') {
            return {
              items: [
                makeItem({ id: 'target', threadId: 't', turnIndex: 1 }),
                makeItem({ id: 't2', threadId: 't', turnIndex: 2 }),
                makeItem({ id: 't3', threadId: 't', turnIndex: 3 }),
              ],
              oldestTurnIndex: 1,
              newestTurnIndex: 3,
              hasMore: false,
              hasMoreOlder: false,
              hasMoreNewer: true,
            };
          }
          return {
            items: [makeItem({ id: 't5', threadId: 't', turnIndex: 5 })],
            oldestTurnIndex: 5,
            newestTurnIndex: 5,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock(
        'GetThreadItem',
        async (_threadId: string, itemId: string) =>
          itemId === 'target'
            ? makeItem({ id: 'target', threadId: 't', turnIndex: 1 })
            : null,
      );
      await pane.switchThread(makeThread({ id: 't' }));
      const revisionBeforeLoadUntil = pane.timelineRevision;
      const ok = await pane.loadUntilItem('target');

      expect(ok).toBe(true);
      expect(pane.timelineRevision).toBeGreaterThan(revisionBeforeLoadUntil);
      expect(pane.oldestLoadedTurnIndex).toBe(1);
      expect(pane.newestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreNewer).toBe(true);
      expect(pane.items.map((it) => it.id)).toEqual(['target', 't2', 't3']);
      expect(sliceCalls).toBe(2);
    });

    it('loadUntilItem returns false when the item is unknown to the backend', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 't5', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () => makeItem({ id: '' }));
      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('ghost');
      expect(ok).toBe(false);
    });

    it('requestScrollToItem bumps the nonce observed by the timeline', () => {
      const pane = createThreadPane();
      const first = pane.scrollToItemRequest.nonce;
      pane.requestScrollToItem('a');
      const second = pane.scrollToItemRequest.nonce;
      expect(second).toBeGreaterThan(first);
      expect(pane.scrollToItemRequest.itemId).toBe('a');
      pane.requestScrollToItem('b');
      expect(pane.scrollToItemRequest.nonce).toBeGreaterThan(second);
      expect(pane.scrollToItemRequest.itemId).toBe('b');
    });

    it('scrollToItemRequest nonce stays monotonic across switchThread', async () => {
      // The timeline tracks `lastHandledScrollNonce` locally. If a pane
      // reset the nonce to 0 on switch, a follow-up intent with nonce=1
      // would compare against the lingering higher handled value and
      // silently not dispatch. Keep the nonce monotonic.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      pane.requestScrollToItem('before-switch');
      const beforeSwitch = pane.scrollToItemRequest.nonce;
      expect(beforeSwitch).toBeGreaterThan(0);

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.scrollToItemRequest.nonce).toBe(beforeSwitch);

      pane.requestScrollToItem('after-switch');
      expect(pane.scrollToItemRequest.nonce).toBeGreaterThan(beforeSwitch);
    });

    it('loadUntilItem loads the target turn when the pane has no floor yet', async () => {
      // An empty-thread pane (or one whose switchThread returned 0 items)
      // has `oldestLoadedTurnIndex = null`. The loader must still be able
      // to pull in history when the user triggers scroll-to-item from
      // search — not skip the fetch and short-circuit to `true`.
      const pane = createThreadPane();
      let sliceAnchor: string | null = null;
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          sliceAnchor = anchorItemId;
          if (anchorItemId === 'deep') {
            return {
              items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
              oldestTurnIndex: 3,
              newestTurnIndex: 3,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: true,
            };
          }
          return {
            items: [],
            oldestTurnIndex: -1,
            newestTurnIndex: -1,
            hasMore: false,
            hasMoreOlder: false,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();

      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(sliceAnchor).toBe('deep');
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
      expect(pane.oldestLoadedTurnIndex).toBe(3);
      expect(pane.newestLoadedTurnIndex).toBe(3);
      expect(pane.hasMoreNewer).toBe(true);
    });

    it('loadUntilItem rejects an item whose threadId does not match the current pane', async () => {
      // Defense-in-depth: a mislayered binding or stale cache that
      // returns a row from a different thread should never cross-pollute
      // a pane. loadUntilItem must treat the mismatch as "not found"
      // rather than trying to page an item that doesn't belong here.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 5 })],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'wrong', threadId: 'other-thread', turnIndex: 1 }),
      );
      let paged = 0;
      setBindingMock('ListItemsBeforeCursor', async () => {
        paged += 1;
        return { items: [], oldestTurnIndex: -1, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const ok = await pane.loadUntilItem('wrong');
      expect(ok).toBe(false);
      expect(paged).toBe(0);
    });

    it('loadUntilItem resolves a subagent child by anchoring on its launch root and hydrating the subtree', async () => {
      // History windows exclude child rows, so a scroll-to-item target
      // inside a subagent transcript must (1) walk the parent chain to
      // the top-level launch root, (2) slice the window around the
      // root, and (3) hydrate the root's descendants so the containing
      // group card can resolve the scroll.
      const pane = createThreadPane();
      const sliceAnchors: string[] = [];
      setBindingMock(
        'GetThreadItem',
        async (_threadId: string, itemId: string) => {
          if (itemId === 'deep-child') {
            return makeItem({
              id: 'deep-child',
              threadId: 't',
              turnIndex: 4,
              itemIndex: 3,
              parentId: 'mid-launch',
            });
          }
          if (itemId === 'mid-launch') {
            return makeItem({
              id: 'mid-launch',
              threadId: 't',
              turnIndex: 4,
              itemIndex: 1,
              parentId: 'root-launch',
              kind: 'tool_call',
              toolName: 'Task',
            });
          }
          if (itemId === 'root-launch') {
            return makeItem({
              id: 'root-launch',
              threadId: 't',
              turnIndex: 4,
              itemIndex: 0,
              kind: 'tool_call',
              toolName: 'Task',
            });
          }
          return makeItem({ id: '' });
        },
      );
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          sliceAnchors.push(anchorItemId);
          if (anchorItemId === 'root-launch') {
            return {
              items: [
                makeItem({
                  id: 'root-launch',
                  threadId: 't',
                  turnIndex: 4,
                  itemIndex: 0,
                  kind: 'tool_call',
                  toolName: 'Task',
                }),
                makeItem({ id: 'after', threadId: 't', turnIndex: 5 }),
              ],
              oldestTurnIndex: 4,
              newestTurnIndex: 5,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: false,
            };
          }
          return {
            items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 9 })],
            oldestTurnIndex: 9,
            newestTurnIndex: 9,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock(
        'ListSubagentDescendants',
        async (_threadId: string, rootItemId: string) =>
          rootItemId === 'root-launch'
            ? [
                makeItem({
                  id: 'mid-launch',
                  threadId: 't',
                  turnIndex: 4,
                  itemIndex: 1,
                  parentId: 'root-launch',
                  kind: 'tool_call',
                  toolName: 'Task',
                }),
                makeItem({
                  id: 'deep-child',
                  threadId: 't',
                  turnIndex: 4,
                  itemIndex: 3,
                  parentId: 'mid-launch',
                }),
              ]
            : [],
      );
      await pane.switchThread(makeThread({ id: 't' }));

      const ok = await pane.loadUntilItem('deep-child');

      expect(ok).toBe(true);
      expect(sliceAnchors.at(-1)).toBe('root-launch');
      expect(pane.items.map((it) => it.id)).toEqual([
        'root-launch',
        'mid-launch',
        'deep-child',
        'after',
      ]);
    });

    it('ensureSubagentChildren merges descendants additively and dedupes repeat calls', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 0,
            kind: 'tool_call',
            toolName: 'Task',
          }),
          makeItem({ id: 'tail', threadId: 't', turnIndex: 2 }),
        ],
        oldestTurnIndex: 1,
        newestTurnIndex: 2,
        hasMore: false,
      }));
      let listCalls = 0;
      setBindingMock('ListSubagentDescendants', async () => {
        listCalls += 1;
        return [
          makeItem({
            id: 'child-1',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 1,
            parentId: 'anchor',
          }),
          makeItem({
            id: 'child-2',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 2,
            parentId: 'anchor',
          }),
        ];
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const first = await pane.ensureSubagentChildren('anchor');
      expect(first).toBe(true);
      expect(pane.items.map((it) => it.id)).toEqual([
        'anchor',
        'child-1',
        'child-2',
        'tail',
      ]);

      // A repeat call re-fetches once (children might have grown), adds
      // nothing, and marks the anchor exhausted.
      const second = await pane.ensureSubagentChildren('anchor');
      expect(second).toBe(false);
      expect(listCalls).toBe(2);

      // Exhausted anchors skip the backend entirely so a stale
      // decorated count can't loop the expansion effect.
      const third = await pane.ensureSubagentChildren('anchor');
      expect(third).toBe(false);
      expect(listCalls).toBe(2);
    });

    it('ensureSubagentChildren dedupes concurrent calls for the same anchor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 't',
            turnIndex: 1,
            kind: 'tool_call',
            toolName: 'Task',
          }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      let resolveList: (items: Item[]) => void = () => {};
      const listMock = setBindingMock(
        'ListSubagentDescendants',
        () =>
          new Promise((resolve) => {
            resolveList = resolve as (items: Item[]) => void;
          }),
      );
      await pane.switchThread(makeThread({ id: 't' }));

      const firstPromise = pane.ensureSubagentChildren('anchor');
      const duplicate = await pane.ensureSubagentChildren('anchor');
      expect(duplicate).toBe(false);

      resolveList([
        makeItem({
          id: 'child-1',
          threadId: 't',
          turnIndex: 1,
          itemIndex: 1,
          parentId: 'anchor',
        }),
      ]);
      expect(await firstPromise).toBe(true);
      expect(listMock.mock.calls).toHaveLength(1);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('ensureSubagentChildren discards a fetch that resolves after a thread switch', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 'thread-a',
            turnIndex: 1,
            kind: 'tool_call',
            toolName: 'Task',
          }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      let resolveList: (items: Item[]) => void = () => {};
      setBindingMock(
        'ListSubagentDescendants',
        () =>
          new Promise((resolve) => {
            resolveList = resolve as (items: Item[]) => void;
          }),
      );
      await pane.switchThread(makeThread({ id: 'thread-a' }));
      const pending = pane.ensureSubagentChildren('anchor');

      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'b-item', threadId: 'thread-b', turnIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 'thread-b' }));

      resolveList([
        makeItem({
          id: 'stale-child',
          threadId: 'thread-a',
          turnIndex: 1,
          itemIndex: 1,
          parentId: 'anchor',
        }),
      ]);
      expect(await pending).toBe(false);
      expect(pane.items.some((it) => it.id === 'stale-child')).toBe(false);
    });

    it('ensureSubagentChildren recovers after a failed fetch', async () => {
      // A transient backend failure must not wedge the anchor: the
      // in-flight marker clears in finally and the anchor is NOT marked
      // exhausted, so the next call (the user re-expanding the card)
      // re-fetches instead of being suppressed.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'anchor',
            threadId: 't',
            turnIndex: 1,
            kind: 'tool_call',
            toolName: 'Task',
          }),
        ],
        oldestTurnIndex: 1,
        hasMore: false,
      }));
      let listCalls = 0;
      setBindingMock('ListSubagentDescendants', async () => {
        listCalls += 1;
        if (listCalls === 1) throw new Error('mock backend down');
        return [
          makeItem({
            id: 'child-1',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 1,
            parentId: 'anchor',
          }),
        ];
      });
      await pane.switchThread(makeThread({ id: 't' }));

      const failed = await pane.ensureSubagentChildren('anchor');
      expect(failed).toBe(false);
      expect(pane.items.map((it) => it.id)).toEqual(['anchor']);

      const retried = await pane.ensureSubagentChildren('anchor');
      expect(retried).toBe(true);
      expect(listCalls).toBe(2);
      expect(pane.items.some((it) => it.id === 'child-1')).toBe(true);
    });

    it('loadOlder disables hasMoreHistory when the backend cannot advance the floor', async () => {
      // Pathological scenario: turns table claims more history exists
      // but the item range before the current cursor is empty (a sparse
      // turn row with no items). Without a progress guard the Load
      // Older button would keep firing the same query. The store must
      // break the loop by forcing hasMoreHistory=false when no rows
      // were returned AND the floor did not decrease.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 10 })],
        oldestTurnIndex: 10,
        hasMore: true,
      }));
      let calls = 0;
      setBindingMock('ListItemsBeforeCursor', async () => {
        calls += 1;
        // Backend cooperates: no items, floor unchanged, but still
        // claims more exists. Common when a turn row has zero items.
        return { items: [], oldestTurnIndex: 10, hasMore: true };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.hasMoreHistory).toBe(true);
      await pane.loadOlder();
      expect(calls).toBe(1);
      expect(pane.hasMoreHistory).toBe(false);
      // Second invocation should short-circuit; no network call.
      await pane.loadOlder();
      expect(calls).toBe(1);
    });

    it('loadOlder clears loadingOlder even when a concurrent loadUntilItem bumps the paging generation', async () => {
      // Regression pin: `loadingOlder` is a UI-only flag. If a
      // concurrent `loadUntilItem` increments `pagingGeneration`
      // while `loadOlder` is mid-fetch, the generation-guarded
      // finally block used to skip clearing the flag, greying out
      // the Load Older button forever. The fix resets the flag
      // unconditionally.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'tail', threadId: 't', turnIndex: 10 })],
        oldestTurnIndex: 10,
        hasMore: true,
      }));
      let releaseOlder!: (v: {
        items: ReturnType<typeof makeItem>[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }) => void;
      const olderPending = new Promise<{
        items: ReturnType<typeof makeItem>[];
        oldestTurnIndex: number;
        hasMore: boolean;
      }>((r) => {
        releaseOlder = r;
      });
      setBindingMock('ListItemsBeforeCursor', () => olderPending);

      await pane.switchThread(makeThread({ id: 't' }));
      const olderPromise = pane.loadOlder();
      expect(pane.loadingOlder).toBe(true);

      // Kick off loadUntilItem, which increments pagingGeneration and
      // takes its own path. It must not deadlock loadOlder's cleanup.
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));
      await pane.loadUntilItem('deep');

      releaseOlder({ items: [], oldestTurnIndex: 10, hasMore: false });
      await olderPromise;

      expect(pane.loadingOlder).toBe(false);
    });

    it('loadUntilItem uses a bounded centered slice when the pane floor is null', async () => {
      // Regression pin for the MAX_SAFE_INTEGER itemBudget bug: search
      // jumps must request a bounded centered slice, not an unbounded
      // page from the target to the tail.
      const pane = createThreadPane();
      let capturedAnchor = '';
      let capturedTargetCount = 0;
      setBindingMock(
        'ListThreadSliceAround',
        async (_id, anchor, targetCount) => {
          capturedAnchor = anchor as string;
          capturedTargetCount = targetCount as number;
          if (anchor === 'deep') {
            return {
              items: [makeItem({ id: 'deep', threadId: 't', turnIndex: 3 })],
              oldestTurnIndex: 3,
              newestTurnIndex: 3,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: true,
            };
          }
          return {
            items: [],
            oldestTurnIndex: -1,
            newestTurnIndex: -1,
            hasMore: false,
            hasMoreOlder: false,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 3 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(capturedAnchor).toBe('deep');
      expect(capturedTargetCount).toBeLessThanOrEqual(500);
      expect(capturedTargetCount).toBeGreaterThan(0);
    });

    it('pagingGeneration stays monotonic across switchThread', async () => {
      // Regression: earlier the reset to 0 on swap meant a stale
      // in-flight paging fetch could see its captured generation
      // match the freshly-reset counter and proceed to clobber
      // state. The switchGeneration guard catches the common case
      // but pinning the monotonicity invariant here prevents a
      // future refactor from reintroducing the reset.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'a', threadId: 't', turnIndex: 0 })],
        oldestTurnIndex: 0,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'x', threadId: 't', turnIndex: 0 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      // Trigger a paging call so pagingGeneration advances to 1.
      await pane.loadOlder();
      await pane.switchThread(makeThread({ id: 't2' }));
      // After switch, another paging call should advance the counter
      // further — never regress to a prior value. We observe by
      // chaining two calls and ensuring the second still makes a
      // network call (i.e. the guards remain accurate).
      let postSwitchCalls = 0;
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'b', threadId: 't2', turnIndex: 3 })],
        oldestTurnIndex: 3,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => {
        postSwitchCalls += 1;
        return { items: [], oldestTurnIndex: 2, hasMore: false };
      });
      await pane.switchThread(makeThread({ id: 't3' }));
      await pane.loadOlder();
      expect(postSwitchCalls).toBe(1);
    });

    it('loadOlder dedupes by id when the backend re-returns a loaded row', async () => {
      // Defensive contract: a paging response can re-return a row the
      // window already holds (overlapping ranges after a prune, or a
      // row that arrived via a streamed upsert mid-fetch). The store
      // must not duplicate it in `items` — the dedup happens via
      // `mergeItemsById`.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        // Backend legitimately returns the ancestor again (it sits
        // below the new paging floor and the recursive CTE pulls it
        // in for any child-in-range query).
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'between', threadId: 't', turnIndex: 3 }),
        ],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items.map((it) => it.id)).toEqual(['ancestor', 'child']);

      await pane.loadOlder();
      // 'ancestor' appears once — duplicates are filtered out.
      const ancestors = pane.items.filter((it) => it.id === 'ancestor');
      expect(ancestors.length).toBe(1);
      // Ordering: the newly prepended 'between' sits before the
      // existing tail. The duplicate ancestor row was dropped so the
      // original position is preserved.
      expect(pane.items.map((it) => it.id)).toEqual([
        'ancestor',
        'between',
        'child',
      ]);
    });

    it('loadOlder replaces duplicate rows with enriched backend copies', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'ancestor',
            threadId: 't',
            turnIndex: 0,
            summary: 'summary-only',
          }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [
          makeItem({
            id: 'ancestor',
            threadId: 't',
            turnIndex: 0,
            summary: 'enriched',
            payloadId: 'payload-ancestor',
          }),
          makeItem({ id: 'between', threadId: 't', turnIndex: 3 }),
        ],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadOlder();

      const ancestor = pane.items.find((it) => it.id === 'ancestor');
      expect(ancestor?.summary).toBe('enriched');
      expect(ancestor?.payloadId).toBe('payload-ancestor');
      expect(pane.items.filter((it) => it.id === 'ancestor')).toHaveLength(1);
    });

    it('loadOlder reports rows inserted after an ancestor above the floor', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
          makeItem({ id: 'child', threadId: 't', turnIndex: 5 }),
        ],
        oldestTurnIndex: 5,
        hasMore: true,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [makeItem({ id: 'between', threadId: 't', turnIndex: 3 })],
        oldestTurnIndex: 3,
        hasMore: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual([
        'ancestor',
        'between',
        'child',
      ]);
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: false,
        insertedRows: true,
      });
    });

    it('loadUntilItem dedupes by id when pulling in a below-floor target', async () => {
      // Same contract as loadOlder's dedup, but via the scroll-to-item
      // entry point. The centered replacement can include rows that were
      // already present by id; no duplicate should land in the window.
      const pane = createThreadPane();
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId: string, anchorItemId: string) => {
          if (anchorItemId === 'deep') {
            return {
              items: [
                makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
                makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
              ],
              oldestTurnIndex: 2,
              newestTurnIndex: 2,
              hasMore: false,
              hasMoreOlder: false,
              hasMoreNewer: true,
            };
          }
          return {
            items: [
              makeItem({ id: 'ancestor', threadId: 't', turnIndex: 0 }),
              makeItem({ id: 'tail', threadId: 't', turnIndex: 5 }),
            ],
            oldestTurnIndex: 5,
            newestTurnIndex: 5,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: false,
          };
        },
      );
      setBindingMock('GetThreadItem', async () =>
        makeItem({ id: 'deep', threadId: 't', turnIndex: 2 }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      const ok = await pane.loadUntilItem('deep');
      expect(ok).toBe(true);
      expect(pane.items.filter((it) => it.id === 'ancestor').length).toBe(1);
      expect(pane.items.some((it) => it.id === 'deep')).toBe(true);
      expect(pane.hasMoreNewer).toBe(true);
    });

    it('upsertItem accepts new items when the pane floor is null (empty thread)', async () => {
      // Regression: the floor guard short-circuits when
      // `oldestLoadedTurnIndex` is null so streamed upserts on a
      // fresh pane still land. Without the null check, every first
      // item on a brand-new thread would be dropped.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [],
        oldestTurnIndex: -1,
        hasMore: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.oldestLoadedTurnIndex).toBeNull();
      pane.upsertItem(
        makeItem({ id: 'first', threadId: 't', turnIndex: 0, itemIndex: 0 }),
      );
      expect(pane.items.map((it) => it.id)).toEqual(['first']);
    });

    it('holds newer upserts behind the newer-history gap when reading an old window', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({
            id: 'old-window',
            threadId: 't',
            turnIndex: 3,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const changed = pane.upsertItem(
        makeItem({ id: 'newer', threadId: 't', turnIndex: 9, itemIndex: 0 }),
      );

      expect(changed).toBe(true);
      expect(pane.items.map((it) => it.id)).toEqual(['old-window']);
      expect(pane.hasMoreNewer).toBe(true);
      expect(pane.newestLoadedTurnIndex).toBe(3);
    });

    it('loadNewer pages forward from the current ceiling', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 't3', threadId: 't', turnIndex: 3, itemIndex: 0 }),
        ],
        oldestTurnIndex: 3,
        newestTurnIndex: 3,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));
      let afterCursor: unknown = null;
      setBindingMock('ListItemsAfterCursor', async (_threadId, after) => {
        afterCursor = after;
        return {
          items: [
            makeItem({ id: 't4', threadId: 't', turnIndex: 4, itemIndex: 0 }),
            makeItem({ id: 't5', threadId: 't', turnIndex: 5, itemIndex: 0 }),
          ],
          oldestTurnIndex: 4,
          newestTurnIndex: 5,
          hasMore: true,
          hasMoreOlder: true,
          hasMoreNewer: false,
        };
      });

      await pane.switchThread(makeThread({ id: 't' }));
      const result = await pane.loadNewer();

      expect(afterCursor).toEqual({ turnIndex: 3, itemIndex: 0, itemId: 't3' });
      expect(result).toEqual({
        status: 'loaded',
        insertedBeforeWindow: true,
        insertedRows: true,
      });
      expect(pane.items.map((it) => it.id)).toEqual(['t3', 't4', 't5']);
      expect(pane.newestLoadedTurnIndex).toBe(5);
      expect(pane.hasMoreNewer).toBe(false);
    });

    it('loadNewer preserves the older-history flag when the merged window still starts at the thread head', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 't0', threadId: 't', turnIndex: 0, itemIndex: 0 }),
        ],
        oldestTurnIndex: 0,
        newestTurnIndex: 0,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: true,
      }));
      setBindingMock('ListItemsAfterCursor', async () => ({
        items: [
          makeItem({ id: 't1', threadId: 't', turnIndex: 1, itemIndex: 0 }),
        ],
        oldestTurnIndex: 1,
        newestTurnIndex: 1,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadNewer();

      expect(pane.items.map((it) => it.id)).toEqual(['t0', 't1']);
      expect(pane.oldestLoadedTurnIndex).toBe(0);
      expect(pane.hasMoreHistory).toBe(false);
      expect(pane.hasMoreNewer).toBe(false);
    });

    it('loadNewer preserves the newer-history gap when pruning older head rows', async () => {
      const pane = createThreadPane();
      const initial = Array.from(
        { length: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS },
        (_, index) =>
          makeItem({
            id: `t${index}`,
            threadId: 't',
            turnIndex: index,
            itemIndex: 0,
          }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: true,
      }));
      setBindingMock('ListItemsAfterCursor', async () => ({
        items: [
          makeItem({
            id: `t${ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS}`,
            threadId: 't',
            turnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadNewer();

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasMoreNewer).toBe(true);
    });

    // === keyed combined paging mutations ===
    // The virtualizer derives structural changes from row keys, so paging can
    // commit a prepend + opposite-edge prune in one render flush. No caller
    // direction flag or transient over-cap render exists.
    it('loadOlder prepends and tail-prunes in one render flush', async () => {
      const pane = createThreadPane();
      const initial = Array.from(
        { length: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS },
        (_, index) =>
          makeItem({
            id: `t${index}`,
            threadId: 't',
            turnIndex: 1000 + index,
            itemIndex: 0,
          }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 1000,
        newestTurnIndex: 1000 + ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS - 1,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: false,
      }));
      let releaseOlder!: (value: unknown) => void;
      setBindingMock(
        'ListItemsBeforeCursor',
        () =>
          new Promise((resolve) => {
            releaseOlder = resolve;
          }),
      );

      await pane.switchThread(makeThread({ id: 't' }));
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS);

      const pending = pane.loadOlder();
      releaseOlder({
        items: [
          makeItem({ id: 'older', threadId: 't', turnIndex: 999, itemIndex: 0 }),
        ],
        oldestTurnIndex: 999,
        hasMore: true,
        hasMoreOlder: true,
      });
      // Resume past the fetch. The prepend and prune both commit before the
      // one render tick the method awaits.
      await Promise.resolve();

      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0]?.id).toBe('older');

      await pending;
      await tick();

      // The freshly prepended head survives and the dropped tail becomes a
      // newer-history gap.
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items[0]?.id).toBe('older');
      expect(pane.hasMoreNewer).toBe(true);
    });

    it('loadNewer appends and head-prunes in one render flush', async () => {
      const pane = createThreadPane();
      const initial = Array.from(
        { length: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS },
        (_, index) =>
          makeItem({
            id: `t${index}`,
            threadId: 't',
            turnIndex: index,
            itemIndex: 0,
          }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS - 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: true,
      }));
      setBindingMock('ListItemsAfterCursor', async () => ({
        items: [
          makeItem({
            id: `t${ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS}`,
            threadId: 't',
            turnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
            itemIndex: 0,
          }),
        ],
        oldestTurnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
        newestTurnIndex: ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));

      const snapshots: number[] = [];
      const stop = $effect.root(() => {
        $effect(() => {
          snapshots.push(pane.items.length);
        });
      });
      try {
        flushSync();
        snapshots.length = 0;

        await pane.loadNewer();
        await tick();
        flushSync();
      } finally {
        stop();
      }

      expect(snapshots).not.toContain(ACTIVE_TIMELINE_WINDOW_HARD_CEILING_ITEMS + 1);
      expect(snapshots).toContain(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.items).toHaveLength(ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS);
      expect(pane.hasMoreHistory).toBe(true);
    });

    // Incident 2026-08-25: one giant activity run (~700 tool rows rendering
    // as a single collapsed node) held most of the item budget, so paging
    // older past MAX_ITEMS evicted the on-screen conversation tail and
    // flipped the pane into windowed mid-history. The paged prunes now
    // tolerate up to the hard ceiling; only the streaming prune keeps the
    // tight MAX bound.
    it('loadOlder keeps the conversation tail while paging through a giant activity run', async () => {
      const pane = createThreadPane();
      const runChildren = Array.from(
        { length: ACTIVE_TIMELINE_WINDOW_MAX_ITEMS + 90 },
        (_, index) =>
          makeItem({
            id: `run${index}`,
            threadId: 't',
            turnIndex: 1000,
            itemIndex: index,
            kind: 'tool_call',
            role: 'assistant',
          }),
      );
      const tail = Array.from({ length: 10 }, (_, index) =>
        makeItem({
          id: `tail${index}`,
          threadId: 't',
          turnIndex: 1001 + index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [...runChildren, ...tail],
        oldestTurnIndex: 1000,
        newestTurnIndex: 1010,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: false,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: Array.from({ length: 200 }, (_, index) =>
          makeItem({
            id: `older${index}`,
            threadId: 't',
            turnIndex: 999,
            itemIndex: index,
            kind: 'tool_call',
            role: 'assistant',
          }),
        ),
        oldestTurnIndex: 999,
        hasMore: true,
        hasMoreOlder: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      const beforeCount = pane.items.length;
      await pane.loadOlder();

      // Every prior item survives — no tail eviction, no mid-history gap.
      expect(pane.items).toHaveLength(beforeCount + 200);
      expect(pane.items[0]?.id).toBe('older0');
      expect(pane.items.at(-1)?.id).toBe('tail9');
      expect(pane.hasMoreNewer).toBe(false);
    });

    it('loadOlder does not invent a newer-history gap from the older page response', async () => {
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [
          makeItem({ id: 'tail', threadId: 't', turnIndex: 5, itemIndex: 0 }),
        ],
        oldestTurnIndex: 5,
        newestTurnIndex: 5,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: false,
      }));
      setBindingMock('ListItemsBeforeCursor', async () => ({
        items: [
          makeItem({ id: 'older', threadId: 't', turnIndex: 4, itemIndex: 0 }),
        ],
        oldestTurnIndex: 4,
        newestTurnIndex: 4,
        hasMore: true,
        hasMoreOlder: true,
        hasMoreNewer: true,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.loadOlder();

      expect(pane.items.map((it) => it.id)).toEqual(['older', 'tail']);
      expect(pane.hasMoreNewer).toBe(false);
    });

    it('refreshFromBackend reloads through the bounded slice API instead of the broad recent loader', async () => {
      const pane = createThreadPane();
      const sliceCalls: Array<{ anchor: unknown; budget: unknown }> = [];
      setBindingMock('AutoResumeThread', async () => {});
      setBindingMock(
        'ListThreadSliceAround',
        async (_threadId, anchor, budget) => {
          sliceCalls.push({ anchor, budget });
          if (sliceCalls.length === 1) {
            return {
              items: [
                makeItem({
                  id: 'window-ceiling',
                  threadId: 't',
                  turnIndex: 3,
                  itemIndex: 0,
                }),
              ],
              oldestTurnIndex: 3,
              newestTurnIndex: 3,
              hasMore: true,
              hasMoreOlder: true,
              hasMoreNewer: true,
            };
          }
          return {
            items: [
              makeItem({
                id: 'refreshed',
                threadId: 't',
                turnIndex: 4,
                itemIndex: 0,
              }),
            ],
            oldestTurnIndex: 4,
            newestTurnIndex: 4,
            hasMore: true,
            hasMoreOlder: true,
            hasMoreNewer: true,
          };
        },
      );
      setBindingMock('ListRecentThreadItems', async () => {
        throw new Error(
          'refreshFromBackend should not use the broad recent loader',
        );
      });

      await pane.switchThread(makeThread({ id: 't' }));
      await pane.refreshFromBackend();

      expect(sliceCalls).toEqual([
        { anchor: '', budget: SLICE_AROUND_ITEM_BUDGET },
        {
          anchor: 'window-ceiling',
          budget: ACTIVE_TIMELINE_WINDOW_TARGET_ITEMS,
        },
      ]);
      expect(pane.items.map((it) => it.id)).toEqual(['refreshed']);
    });

    it('keeps live item mutations that land while a gap snapshot is in flight', async () => {
      const pane = createThreadPane();
      let sliceCall = 0;
      let releaseRefresh!: (value: unknown) => void;
      setBindingMock('ListThreadSliceAround', async () => {
        sliceCall += 1;
        if (sliceCall === 1) {
          return {
            items: [
              makeItem({
                id: 'streaming',
                threadId: 't',
                turnIndex: 1,
                itemIndex: 0,
                kind: 'assistant_text',
                role: 'assistant',
                status: 'streaming',
                summary: 'persisted before stream',
              }),
              makeItem({
                id: 'obsolete',
                threadId: 't',
                turnIndex: 1,
                itemIndex: 2,
              }),
              makeItem({
                id: 'removed-live',
                threadId: 't',
                turnIndex: 1,
                itemIndex: 3,
              }),
            ],
            oldestTurnIndex: 1,
            newestTurnIndex: 1,
            hasMore: false,
            hasMoreOlder: false,
            hasMoreNewer: false,
          };
        }
        return new Promise((resolve) => {
          releaseRefresh = resolve;
        });
      });

      await pane.switchThread(makeThread({ id: 't' }));
      const refreshing = pane.refreshFromBackend();
      await flushMicrotasks();

      pane.applyItemPatch({
        threadId: 't',
        itemId: 'streaming',
        kind: 'assistant_text',
        patch: { status: 'completed', updatedAt: 2 },
      });
      pane.upsertItem(makeItem({
        id: 'live-only',
        threadId: 't',
        turnIndex: 2,
        itemIndex: 0,
        summary: 'not persisted yet',
      }));
      expect(pane.removeItemById('removed-live', 't')?.id).toBe('removed-live');

      releaseRefresh({
        items: [
          makeItem({
            id: 'streaming',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 0,
            kind: 'assistant_text',
            role: 'assistant',
            status: 'streaming',
            summary: 'persisted before stream',
          }),
          makeItem({
            id: 'missed-event',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 1,
            summary: 'recovered from snapshot',
          }),
          makeItem({
            id: 'removed-live',
            threadId: 't',
            turnIndex: 1,
            itemIndex: 3,
          }),
        ],
        oldestTurnIndex: 1,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      });
      await refreshing;

      expect(pane.items.map((item) => [item.id, item.summary, item.status])).toEqual([
        ['streaming', 'persisted before stream', 'completed'],
        ['missed-event', 'recovered from snapshot', 'completed'],
        ['live-only', 'not persisted yet', 'completed'],
      ]);
      expect(pane.newestLoadedTurnIndex).toBe(2);
    });

    it('answers a refresh requested mid-refresh with one trailing run that converges the window', async () => {
      // Single-flight: the overlapping request opens no second concurrent
      // fetch (the stale-overwrites-newer interleaving is structurally
      // impossible), and is answered by exactly one trailing run whose
      // page the window converges to.
      const pane = createThreadPane();
      const releases: Array<(value: unknown) => void> = [];
      let sliceCall = 0;
      setBindingMock('ListThreadSliceAround', async () => {
        sliceCall += 1;
        if (sliceCall === 1) {
          return {
            items: [makeItem({ id: 'initial', threadId: 't' })],
            oldestTurnIndex: 0,
            newestTurnIndex: 0,
            hasMore: false,
            hasMoreOlder: false,
            hasMoreNewer: false,
          };
        }
        return new Promise((resolve) => releases.push(resolve));
      });

      await pane.switchThread(makeThread({ id: 't' }));
      const older = pane.refreshFromBackend();
      await flushMicrotasks();
      const newer = pane.refreshFromBackend();
      await flushMicrotasks();
      expect(releases).toHaveLength(1);

      releases[0]({
        items: [makeItem({ id: 'older', threadId: 't', turnIndex: 1 })],
        oldestTurnIndex: 1,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      });
      await older;
      expect(pane.items.map((item) => item.id)).toEqual(['older']);

      await vi.waitFor(() => expect(releases).toHaveLength(2), {
        timeout: 2000,
      });
      releases[1]({
        items: [makeItem({ id: 'newer', threadId: 't', turnIndex: 2 })],
        oldestTurnIndex: 2,
        newestTurnIndex: 2,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      });
      await newer;

      expect(pane.items.map((item) => item.id)).toEqual(['newer']);
      expect(pane.newestLoadedTurnIndex).toBe(2);
    });

    it('merges deferred pending-send rows into a refresh page', async () => {
      // A pending send has no SQLite row until its wire echo, so the
      // refresh page is structurally blind to it. The live-state
      // snapshot carries those rows (deferredItems) and the refresh
      // merges them in — without this, a transport-gap refresh mid-send
      // dropped the user's own message from the timeline.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'persisted', threadId: 't', turnIndex: 1 })],
        oldestTurnIndex: 1,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      await pane.switchThread(makeThread({ id: 't' }));
      setBindingMock('GetThreadLiveState', async () => ({
        threadId: 't',
        activeTurn: null,
        queueItems: [],
        flushedItems: [],
        deferredItems: [
          makeItem({
            id: 'pending-send',
            threadId: 't',
            kind: 'user_text',
            turnIndex: 2,
            itemIndex: 0,
            summary: 'not yet echoed',
          }),
        ],
        interactive: { approvals: [], userInputs: [] },
        todo: null,
      }));

      await pane.refreshFromBackend();

      expect(pane.items.map((item) => item.id)).toEqual([
        'persisted',
        'pending-send',
      ]);
      // Merged deferred rows join the optimistic-id ledger so the
      // stamped tiers (L1 / replica write-back) keep stripping them —
      // no SQLite row exists until the wire echo.
      expect(pane.isOptimisticItem('pending-send')).toBe(true);
      expect(pane.isOptimisticItem('persisted')).toBe(false);
    });

    it('merges deferred pending-send rows into the cold-open window', async () => {
      // Same blindness on the cold-open leg: the stamped tiers strip
      // optimistic rows and SQLite has no row until the echo, so after
      // a reload the ONLY source for a pending send is the live-state
      // snapshot. Without the merge the user's queued message vanished
      // across an app restart until its echo landed.
      const pane = createThreadPane();
      setBindingMock('ListThreadSliceAround', async () => ({
        items: [makeItem({ id: 'persisted', threadId: 't', turnIndex: 1 })],
        oldestTurnIndex: 1,
        newestTurnIndex: 1,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));
      setBindingMock('GetThreadLiveState', async () => ({
        threadId: 't',
        activeTurn: null,
        queueItems: [],
        flushedItems: [],
        deferredItems: [
          makeItem({
            id: 'pending-send',
            threadId: 't',
            kind: 'user_text',
            turnIndex: 2,
            itemIndex: 0,
            summary: 'not yet echoed',
          }),
        ],
        interactive: { approvals: [], userInputs: [] },
        todo: null,
      }));

      await pane.switchThread(makeThread({ id: 't' }));

      expect(pane.items.map((item) => item.id)).toEqual([
        'persisted',
        'pending-send',
      ]);
      expect(pane.isOptimisticItem('pending-send')).toBe(true);
      expect(pane.isOptimisticItem('persisted')).toBe(false);
    });

    it('folds deferred pending-send rows into a page-less sync answer', async () => {
      // A `fresh` answer keeps the painted rows as-is, but no stamped
      // tier can carry a pending send, so the deferred fold is the only
      // way the row reaches the window.
      const pane = createThreadPane();
      setBindingMock('SyncThreadWindow', async () => ({
        status: 'fresh',
        epoch: 1,
        rev: 1,
        generation: 'test-generation',
        page: null,
      }));
      setBindingMock('GetThreadLiveState', async () => ({
        threadId: 't',
        activeTurn: null,
        queueItems: [],
        flushedItems: [],
        deferredItems: [
          makeItem({
            id: 'pending-send',
            threadId: 't',
            kind: 'user_text',
            turnIndex: 2,
            itemIndex: 0,
            summary: 'not yet echoed',
          }),
        ],
        interactive: { approvals: [], userInputs: [] },
        todo: null,
      }));

      await pane.switchThread(makeThread({ id: 't' }));

      expect(pane.items.map((item) => item.id)).toEqual(['pending-send']);
      expect(pane.isOptimisticItem('pending-send')).toBe(true);
    });

    it('prunes older rows when live tail growth exceeds the active window cap', async () => {
      const pane = createThreadPane();
      const initial = Array.from({ length: 800 }, (_, index) =>
        makeItem({
          id: `t${index}`,
          threadId: 't',
          turnIndex: index,
          itemIndex: 0,
        }),
      );
      setBindingMock('ListThreadSliceAround', async () => ({
        items: initial,
        oldestTurnIndex: 0,
        newestTurnIndex: 799,
        hasMore: false,
        hasMoreOlder: false,
        hasMoreNewer: false,
      }));

      await pane.switchThread(makeThread({ id: 't' }));
      pane.upsertItem(
        makeItem({ id: 't800', threadId: 't', turnIndex: 800, itemIndex: 0 }),
      );

      expect(pane.items).toHaveLength(500);
      expect(pane.items[0].id).toBe('t301');
      expect(pane.items.at(-1)?.id).toBe('t800');
      expect(pane.oldestLoadedTurnIndex).toBe(301);
      expect(pane.hasMoreHistory).toBe(true);
      expect(pane.hasMoreNewer).toBe(false);
    });
  });
});
