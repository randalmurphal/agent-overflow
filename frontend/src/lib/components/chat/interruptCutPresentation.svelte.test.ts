// What the timeline presents while an early Stop's un-send settles.
//
// The Stop restores the message into the composer at once and installs a
// temporary cut: rows on the cut turn or later, and the user row of the
// send being taken back on any turn. The item stream refuses such a pushed
// row. A history read is the backend's state and installs whole, so the
// window can hold these rows while the cut stands; the row projection is
// what keeps them off the screen, and shows them the moment a refusal
// clears the cut.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { loadSettingsFixture as loadSettings } from '../../../test/helpers/settingsFixture';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { buildPane, makeItem, makeThread } from '../../../test/helpers/chat';
import type { ThreadPane } from '../../stores/thread.svelte';
import type { Item } from '../../types/models';
import type { TimelineNode } from '../../utils/subagentGrouping';
import type { ComposerDraftSnapshot } from '../../stores/composerDraftSnapshots';
import { beginUndoableSend, retireUndoableSend } from '../../stores/composerSendUndo';
import { resetItemEventQueue } from '../../stores/eventsItemStream';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { runInterruptOrRevert } from '../../stores/revertOnInterrupt.svelte';
import {
  beginThreadInterrupt,
  finishThreadInterrupt,
  isThreadInterruptPending,
  presentThreadInterruptAsRestored,
  resetThreadInterruptStateForTest,
} from '../../stores/threadInterruptState.svelte';
import { createTimelineRowProjection } from './timelineRowProjection.svelte';

const SENT = JSON.stringify({ sendId: 'send' });
const SNAPSHOT: ComposerDraftSnapshot = { content: 'sent', attachments: [], terminalChips: [], sourceProposedPlan: null };

function row(threadId: string, id: string, turnIndex: number, itemIndex: number, meta = '', kind: Item['kind'] = 'user_text'): Item {
  return makeItem({ id, threadId, turnIndex, itemIndex, kind, role: kind === 'user_text' ? 'user' : 'assistant',
    status: 'completed', summary: id, meta });
}

/** A live projection over a real pane, as a mounted timeline reads it. */
function mountProjection(pane: ThreadPane) {
  let evaluations = 0;
  let nodes: TimelineNode[] = [];
  const dispose = $effect.root(() => {
    const projection = createTimelineRowProjection({ getPane: () => pane });
    $effect(() => {
      nodes = projection.revealedNodes;
      evaluations += 1;
    });
  });
  flushSync();
  const ids = () => {
    flushSync();
    return nodes.map((node) => node.kind === 'leaf' ? node.item.id : node.kind);
  };
  return { ids, get evaluations() { flushSync(); return evaluations; }, dispose };
}

/** Answer the pane's next history read with `items` and an idle live state. */
function answerHistoryRead(threadId: string, items: Item[]): void {
  const cursor = (item: Item) => ({ turnIndex: item.turnIndex, itemIndex: item.itemIndex, itemId: item.id });
  setBindingMock('ListThreadSliceAround', async () => ({ items, oldestTurnIndex: items[0].turnIndex,
    newestTurnIndex: items[items.length - 1].turnIndex, hasMore: false, hasMoreOlder: false, hasMoreNewer: false,
    oldestCursor: cursor(items[0]), newestCursor: cursor(items[items.length - 1]) }));
  setBindingMock('GetThreadLiveState', async () => ({ threadId, activeTurn: null, queueItems: [], flushedItems: [] }));
}

function draftProbe() {
  return {
    content: '', attachments: [] as { length: number }, terminalChips: [] as { length: number },
    applyOptimisticRestoredDraft(): void {},
    clearOptimisticRestoredDraft(): void {},
  };
}

/** An early Stop on a send whose row has not reached the pane, its un-send RPC held open. */
async function stopBeforeTheRow(threadId: string, loaded: Item[], predictedTurn: number) {
  const pane = await buildPane(makeThread({ id: threadId }), loaded, `pane-${threadId}`);
  setBindingMock('CountRunningBackgroundTasks', async () => 0);
  const view = mountProjection(pane);
  const pending = beginUndoableSend(threadId, 'send', SNAPSHOT, predictedTurn);
  let answer!: (result: unknown) => void;
  const revert = setBindingMock('InterruptAndRevertIfClean', () => new Promise((resolve) => { answer = resolve; }));
  expect(runInterruptOrRevert(pane, draftProbe())).toBe(true);
  pending.finish('accepted');
  await vi.waitFor(() => expect(revert).toHaveBeenCalledTimes(1));
  return { pane, view, answer: (result: unknown) => answer(result) };
}

const windowIds = (pane: ThreadPane) => pane.items.map((item) => item.id);

describe('rows an early Stop took off the screen', () => {
  beforeEach(async () => {
    resetBindingMocks();
    setBindingMock('GetSettings', async () => null);
    await loadSettings();
  });
  afterEach(() => {
    resetThreadInterruptStateForTest();
    resetItemEventQueue();
    resetPanesForTest();
  });

  it('are not painted when a history read installs them on the cut turn, and leave with a successful un-send', async () => {
    const threadId = 'cut-same-turn';
    const earlier = [row(threadId, 'u:0', 0, 0), row(threadId, 'a:0', 0, 1, '', 'assistant_text')];
    const { pane, view, answer } = await stopBeforeTheRow(threadId, earlier, 1);
    try {
      expect(view.ids()).toEqual(['u:0', 'a:0']);
      // Gap recovery reads the row the rollback has not removed yet, and the
      // reasoning that followed it.
      answerHistoryRead(threadId, [...earlier, row(threadId, 'user:send', 1, 0, SENT), row(threadId, 'think:1', 1, 1, '', 'thinking')]);
      await pane.refreshFromBackend(true);
      expect(windowIds(pane)).toEqual(['u:0', 'a:0', 'user:send', 'think:1']);
      expect(view.ids()).toEqual(['u:0', 'a:0']);

      answer({ reverted: true, userItemId: 'user:send', turnIndex: 1, historyEpoch: 1, historyRev: 1 });
      await vi.waitFor(() => expect(isThreadInterruptPending(threadId)).toBe(false));
      expect(windowIds(pane)).toEqual(['u:0', 'a:0']);
      expect(view.ids()).toEqual(['u:0', 'a:0']);
    } finally {
      view.dispose();
    }
  });

  it('are not painted when a history read installs the message below the cut turn, and show when the un-send is refused', async () => {
    const threadId = 'cut-lower-turn';
    // The pane still shows turn 1, which another client took back: the send
    // predicts turn 2 and the backend places it on turn 1.
    const loaded = [row(threadId, 'u:0', 0, 0), row(threadId, 'stale:1', 1, 0)];
    const { pane, view, answer } = await stopBeforeTheRow(threadId, loaded, 2);
    try {
      answerHistoryRead(threadId, [row(threadId, 'u:0', 0, 0), row(threadId, 'user:send', 1, 0, SENT),
        row(threadId, 'user:other', 1, 1, JSON.stringify({ sendId: 'other' }))]);
      await pane.refreshFromBackend(true);
      expect(windowIds(pane)).toEqual(['u:0', 'user:send', 'user:other']);
      // Only the message being taken back is withheld below the cut.
      expect(view.ids()).toEqual(['u:0', 'user:other']);

      const before = view.evaluations;
      answer({ reverted: false, reason: 'turn holds assistant_text' });
      await vi.waitFor(() => expect(isThreadInterruptPending(threadId)).toBe(false));
      // No row was written: clearing the cut alone presents the message.
      expect(view.ids()).toEqual(['u:0', 'user:send', 'user:other']);
      expect(view.evaluations).toBeGreaterThan(before);
    } finally {
      view.dispose();
      retireUndoableSend(threadId);
    }
  });

  it('leave the screen the moment the cut is installed and return the moment it clears', async () => {
    const threadId = 'cut-reactive';
    const pane = await buildPane(makeThread({ id: threadId }), [
      row(threadId, 'u:0', 0, 0), row(threadId, 'user:send', 1, 0, SENT), row(threadId, 'a:1', 1, 1, '', 'assistant_text'),
      row(threadId, 'u:2', 2, 0),
    ], `pane-${threadId}`);
    const view = mountProjection(pane);
    try {
      expect(view.ids()).toEqual(['u:0', 'user:send', 'a:1', 'u:2']);
      // Another thread's cut is not this pane's.
      const other = beginThreadInterrupt('cut-elsewhere')!;
      presentThreadInterruptAsRestored('cut-elsewhere', other, 0, 'send');
      expect(view.ids()).toEqual(['u:0', 'user:send', 'a:1', 'u:2']);

      const token = beginThreadInterrupt(threadId)!;
      presentThreadInterruptAsRestored(threadId, token, 2, 'send');
      // Turn 2 by position, the message on turn 1 by its send id.
      expect(view.ids()).toEqual(['u:0', 'a:1']);
      expect(windowIds(pane)).toEqual(['u:0', 'user:send', 'a:1', 'u:2']);
      finishThreadInterrupt(threadId, token);
      expect(view.ids()).toEqual(['u:0', 'user:send', 'a:1', 'u:2']);
    } finally {
      view.dispose();
    }
  });
});
