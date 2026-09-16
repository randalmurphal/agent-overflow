// End-to-end integration tests for the send-queue feature.
//
// Mounts the full <App> against mocked Wails bindings to validate the
// frontend's reaction to the backend-owned queue across:
//
//  - submit-while-turn-active → pending enqueue via RegisterQueueItem
//  - `provider:queue_state_changed` → reactive pending mirror
//  - `provider:queue_flushed` → flushed-but-unconfirmed pending entries
//  - `provider:item_event` upserts with `provider_item_id` → pending clear
//  - combined pending render above the composer
//  - thread switch sweeps the outgoing thread's queue + flushed markers
//
// Earlier frontend-driven drain tests were removed when the queue
// moved to the backend (Phases G1–G6). The triage-side trigger and
// dispatcher coverage lives in internal/triage/flush_queue_test.go and
// app_flush_queue_test.go. This file owns the cross-piece glue.

import { describe, expect, it, beforeAll, beforeEach } from 'vitest';
import { fireEvent, render, waitFor } from '@testing-library/svelte';
import App from '../../App.svelte';
import type { Thread } from '../../lib/types/models';
import { setBindingMock } from '../mocks/bindings-app';
import { emitWailsEvent } from '../mocks/wailsio-runtime';
import { emitItemEventDelta, emitItemEventUpsert, makeItem } from '../helpers/chat';
import {
  getFlushedForThread,
  getQueueForThread,
  hasQueueItems,
  replaceQueueForThread,
} from '../../lib/stores/sendQueue.svelte';
import { clearThreadStatus } from '../../lib/stores/threadStatuses.svelte';
import {
  flush,
  installAnimateShim,
  installAppDefaults,
  installComposerDefaults,
  installThreadViewDefaults,
  makeThread,
  resetAppState,
  seedSidebarProject,
} from './_helpers';

beforeAll(installAnimateShim);

async function mountWithActiveThread(
  thread: Thread = makeThread({ title: 'Send-Queue Flow' }),
) {
  installAppDefaults();
  setBindingMock('ListThreads', async () => [thread]);
  seedSidebarProject([thread]);
  installThreadViewDefaults();
  installComposerDefaults(thread.id);

  const rendered = render(App);
  await flush();
  const rows = rendered.getAllByText(thread.title);
  await fireEvent.click(rows[0]);
  await flush(15);
  return { ...rendered, thread };
}

// Drive the round through `provider:turn_started` so isTurnActive flips
// on. Mirrors the wire path the backend actually emits — see
// invariant 22 (turn activity is wire-pushed, never derived).
function startActiveTurn(threadId: string, turnId = 'turn-active', turnIndex = 0): void {
  emitWailsEvent('provider:turn_started', {
    threadId,
    turnId,
    turnIndex,
    startedAt: 1,
  });
}

describe('App integration — send-queue flow (Phases G1–G10)', () => {
  beforeEach(() => {
    resetAppState();
    setBindingMock('SendMessageWithOptions', async () => makeThread({ id: 'thread-1' }));
    setBindingMock('InterruptTurn', async () => {});
  });

  // ---- T1 — Submit during turn enqueues to Zone 1 -----------------------

  it('T1: submitting during an active turn calls RegisterQueueItem and clears the composer', async () => {
    const { getByLabelText, getByTestId } = await mountWithActiveThread();
    const sendMock = setBindingMock('SendMessageWithOptions', async () =>
      makeThread({ id: 'thread-1' }),
    );
    const registerMock = setBindingMock('RegisterQueueItem', async (
      threadId: string,
      message: string,
      opts: { attachmentIds?: string[] } = {},
    ) => {
      // Mirror the production round-trip: seed the local pending queue
      // directly without a real event. Tests asserting the event-driven
      // path use T2 below.
      const wire = {
        id: 'q-1',
        threadId,
        message,
        attachmentIds: opts.attachmentIds ?? [],
        sourceProposedPlan: null,
        revisionSourceProposedPlan: null,
        revisionSourceCommentIds: undefined,
        enqueuedAt: 1,
      };
      replaceQueueForThread(threadId, [
        {
          id: wire.id,
          threadId: wire.threadId,
          message: wire.message,
          attachmentIds: wire.attachmentIds,
          sourceProposedPlan: null,
          revisionSourceProposedPlan: null,
          revisionSourceCommentIds: undefined,
          enqueuedAt: wire.enqueuedAt,
        },
      ]);
      return wire;
    });

    startActiveTurn('thread-1');
    await flush();

    const textarea = getByLabelText('Message Input') as HTMLTextAreaElement;
    expect(textarea.disabled).toBe(false);
    await fireEvent.input(textarea, { target: { value: 'queue mid-turn' } });
    await flush();

    await fireEvent.click(getByTestId('composer-send'));
    await waitFor(() => expect(registerMock).toHaveBeenCalled());

    // Mid-turn submit MUST NOT call SendMessageWithOptions — the queue
    // is the routing path while a turn is in flight.
    expect(sendMock).not.toHaveBeenCalled();
    expect(registerMock.mock.calls[0][0]).toBe('thread-1');
    expect(registerMock.mock.calls[0][1]).toBe('queue mid-turn');
    expect(registerMock.mock.calls[0][2]).toEqual(
      expect.objectContaining({ attachmentIds: [] }),
    );
    expect(getQueueForThread('thread-1').map((q) => q.message)).toEqual(['queue mid-turn']);
    // Queueing clears the draft through the send path, which swaps the
    // <textarea> element (recreateInput) — re-query for the live one.
    expect((getByLabelText('Message Input') as HTMLTextAreaElement).value).toBe('');
  });

  // ---- T2 — provider:queue_state_changed mirrors backend snapshot --------

  it('T2: provider:queue_state_changed replaces Zone 1 with the event payload', async () => {
    await mountWithActiveThread();

    // Two items arrive in a single snapshot — the event handler must
    // apply both, not just the last one.
    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [
        {
          id: 'q-alpha',
          threadId: 'thread-1',
          message: 'first wire item',
          attachmentIds: ['att-1'],
          sourceProposedPlan: null,
          revisionSourceProposedPlan: null,
          revisionSourceCommentIds: undefined,
          enqueuedAt: 100,
        },
        {
          id: 'q-beta',
          threadId: 'thread-1',
          message: 'second wire item',
          attachmentIds: [],
          sourceProposedPlan: null,
          revisionSourceProposedPlan: null,
          revisionSourceCommentIds: undefined,
          enqueuedAt: 101,
        },
      ],
    });
    await flush();

    const queue = getQueueForThread('thread-1');
    expect(queue).toHaveLength(2);
    expect(queue[0]).toMatchObject({ id: 'q-alpha', message: 'first wire item' });
    expect(queue[0].attachmentIds).toEqual(['att-1']);
    expect(queue[1]).toMatchObject({ id: 'q-beta', message: 'second wire item' });

    // An empty snapshot then drops the entry entirely.
    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [],
    });
    await flush();
    expect(getQueueForThread('thread-1')).toEqual([]);
  });

  // ---- T3 — provider:queue_flushed moves items to Zone 2 -----------------

  it('T3: provider:queue_flushed populates Zone 2 with userItemIds', async () => {
    await mountWithActiveThread();

    // Seed Zone 1 first via the event channel so we exercise the same
    // path the backend takes.
    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [
        {
          id: 'q-1',
          threadId: 'thread-1',
          message: 'flush me',
          attachmentIds: [],
          sourceProposedPlan: null,
          revisionSourceProposedPlan: null,
          revisionSourceCommentIds: undefined,
          enqueuedAt: 1,
        },
      ],
    });
    await flush();
    expect(getQueueForThread('thread-1')).toHaveLength(1);

    // Backend dispatcher emits flushed BEFORE the snapshot empties —
    // Zone 2 should fill before Zone 1 drops.
    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-1', userItemId: 'user:0:flush:1', message: 'flush me' },
      ],
    });
    await flush();
    expect(getFlushedForThread('thread-1').map((f) => f.userItemId)).toEqual([
      'user:0:flush:1',
    ]);

    // Then the queue snapshot drains.
    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [],
    });
    await flush();
    expect(getQueueForThread('thread-1')).toEqual([]);
    // Zone 2 still holds the in-flight marker — the wire echo hasn't
    // arrived yet.
    expect(getFlushedForThread('thread-1')).toHaveLength(1);
  });

  it('T3b: a requeued dispatch returns its message to Zone 1 and empties Zone 2', async () => {
    const { getByTestId } = await mountWithActiveThread();

    // The eager Claude dispatch emits queue_flushed BEFORE the provider
    // write settles, so the marker exists while the write is still in
    // flight.
    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-1', userItemId: 'user:0:flush:1', message: 'write this' },
      ],
    });
    await flush();
    expect(getFlushedForThread('thread-1')).toHaveLength(1);

    // The write failed: the backend requeued the item under the SAME
    // queue id and published the snapshot. One message, one home.
    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [
        {
          id: 'q-1',
          threadId: 'thread-1',
          message: 'write this',
          attachmentIds: [],
          sourceProposedPlan: null,
          revisionSourceProposedPlan: null,
          revisionSourceCommentIds: undefined,
          enqueuedAt: 1,
        },
      ],
    });
    await flush();
    expect(getFlushedForThread('thread-1')).toEqual([]);
    expect(getQueueForThread('thread-1').map((q) => q.id)).toEqual(['q-1']);
    await waitFor(() => {
      const rows = Array.from(
        getByTestId('send-queue-preview').querySelectorAll('[data-testid="send-queue-preview-row"]'),
      );
      expect(rows.map((row) => row.getAttribute('data-state'))).toEqual(['queued']);
      expect(rows[0].textContent).toContain('write this');
    });
  });

  // ---- T4 — confirmed item_event upsert clears Zone 2 ------------------

  it('T4: provider:item_event upsert for a flushed userItemId clears Zone 2 on any flush user_text', async () => {
    await mountWithActiveThread();

    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-1', userItemId: 'user:0:flush:1', message: 'in-flight' },
        { queueItemId: 'q-2', userItemId: 'user:0:flush:2', message: 'still pending' },
      ],
    });
    await flush();
    expect(getFlushedForThread('thread-1').map((f) => f.userItemId)).toEqual([
      'user:0:flush:1',
      'user:0:flush:2',
    ]);

    // A user_text upsert with a :flush: id clears Zone 2 for that
    // item — even without provider_item_id. This supports the eager
    // persist on interrupt path where the item is persisted into the
    // timeline before the provider echo arrives.
    emitItemEventUpsert({
      id: 'user:0:flush:1',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'in-flight',
      meta: '{}',
      createdAt: 1,
      updatedAt: 1,
    });
    await waitFor(() => {
      expect(getFlushedForThread('thread-1').map((f) => f.userItemId)).toEqual([
        'user:0:flush:2',
      ]);
    });

    // The wire echo lands later with provider_item_id, clearing the
    // remaining item. Both paths (eager persist without id, normal
    // echo with id) use the same confirm gate.
    emitItemEventUpsert({
      id: 'user:0:flush:2',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 2,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'still pending',
      meta: JSON.stringify({ provider_item_id: 'wire-echo-002' }),
      createdAt: 1,
      updatedAt: 2,
    });
    await waitFor(() => {
      expect(getFlushedForThread('thread-1')).toHaveLength(0);
    });
  });

  // ---- T4c — a withheld flush row keeps its Zone 2 entry ----------------

  it('T4c: a flush row the reveal gate withholds stays in Zone 2 until it is revealed', async () => {
    await mountWithActiveThread();
    startActiveTurn('thread-1');
    await flush();

    // An assistant row streaming a long delta owns the reveal frontier, so
    // every row after it is withheld from the timeline until it drains.
    emitItemEventUpsert(makeItem({
      id: 'assistant:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'assistant_text',
      role: 'assistant',
      status: 'streaming',
      summary: '',
    }));
    await flush();
    emitItemEventDelta({
      threadId: 'thread-1',
      itemId: 'assistant:0',
      kind: 'assistant_text',
      delta: 'long prose '.repeat(400),
      updatedAt: 2,
    });
    await flush();

    const { getMainPane } = await import('../../lib/stores/panes.svelte');
    const pane = getMainPane();
    // The item-event channel batches on a frame, so wait for the smoother
    // the delta creates rather than assuming it landed in this microtask.
    await waitFor(() => {
      expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    });

    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-1', userItemId: 'user:0:flush:1', message: 'behind the prose' },
      ],
    });
    await flush();

    // The echo lands at the turn TAIL, after the still-draining row. The
    // window admits it; the gate does not mount it. Clearing Zone 2 here
    // would put the message in neither place for the whole drain.
    emitItemEventUpsert(makeItem({
      id: 'user:0:flush:1',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 1,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'behind the prose',
      meta: JSON.stringify({ provider_item_id: 'wire-echo-001' }),
    }));
    await flush();
    await new Promise<void>((r) => setTimeout(r, 60));
    expect(pane.items.some((item) => item.id === 'user:0:flush:1')).toBe(true);
    expect(pane.revealBoundary).toEqual({ turnIndex: 0, itemIndex: 0 });
    expect(getFlushedForThread('thread-1').map((f) => f.userItemId)).toEqual([
      'user:0:flush:1',
    ]);

    // The prose settles and drains: the gate drops, the row mounts, and the
    // handover happens on that pass — no extra event required.
    emitWailsEvent('provider:item_event', {
      action: 'patch',
      threadId: 'thread-1',
      itemId: 'assistant:0',
      patch: { status: 'completed', updatedAt: 3 },
    });
    await flush();
    pane.__flushItemSmoothersForTest();
    await flush();
    expect(pane.revealBoundary).toBeNull();
    expect(getFlushedForThread('thread-1')).toHaveLength(0);
  });

  // ---- T4d — a flush row refused by a scrolled-back window --------------

  it('T4d: a flush row a scrolled-back window refuses keeps its Zone 2 entry until the window catches up', async () => {
    installAppDefaults();
    const thread = makeThread({ title: 'Scrolled Back' });
    setBindingMock('ListThreads', async () => [thread]);
    seedSidebarProject([thread]);
    installThreadViewDefaults();
    installComposerDefaults(thread.id);
    // A window parked on old history: the pane is scrolled back, so the
    // backend says there is newer history it is not holding.
    setBindingMock('ListThreadSliceAround', async () => ({
      items: [
        makeItem({
          id: 'old-1',
          threadId: thread.id,
          turnIndex: 0,
          itemIndex: 0,
          kind: 'assistant_text',
          role: 'assistant',
          summary: 'old answer',
        }),
      ],
      oldestTurnIndex: 0,
      newestTurnIndex: 0,
      hasMore: true,
      hasMoreOlder: false,
      hasMoreNewer: true,
    }));

    const rendered = render(App);
    await flush();
    await fireEvent.click(rendered.getAllByText(thread.title)[0]);
    await flush(15);

    const { getMainPane } = await import('../../lib/stores/panes.svelte');
    const pane = getMainPane();
    expect(pane.hasMoreNewer).toBe(true);

    emitWailsEvent('provider:queue_flushed', {
      threadId: thread.id,
      items: [
        { queueItemId: 'q-1', userItemId: 'user:9:flush:1', message: 'past the ceiling' },
      ],
    });
    await flush();

    // Above the loaded ceiling with newer history unloaded: the merge drops
    // the row rather than opening a hole in the window.
    emitItemEventUpsert(makeItem({
      id: 'user:9:flush:1',
      threadId: thread.id,
      turnIndex: 9,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'past the ceiling',
      meta: JSON.stringify({ provider_item_id: 'wire-echo-009' }),
    }));
    await flush();
    await new Promise<void>((r) => setTimeout(r, 60));
    expect(pane.items.some((item) => item.id === 'user:9:flush:1')).toBe(false);
    expect(getFlushedForThread(thread.id).map((f) => f.userItemId)).toEqual([
      'user:9:flush:1',
    ]);

    // Paging forward to the live tail admits the row, and the same pane
    // chokepoint hands it over.
    setBindingMock('ListItemsAfterCursor', async () => ({
      items: [
        makeItem({
          id: 'user:9:flush:1',
          threadId: thread.id,
          turnIndex: 9,
          itemIndex: 0,
          kind: 'user_text',
          role: 'user',
          status: 'completed',
          summary: 'past the ceiling',
          meta: JSON.stringify({ provider_item_id: 'wire-echo-009' }),
        }),
      ],
      oldestTurnIndex: 9,
      newestTurnIndex: 9,
      hasMore: false,
      hasMoreOlder: false,
      hasMoreNewer: false,
    }));
    await pane.loadNewer();
    await flush();
    expect(pane.items.some((item) => item.id === 'user:9:flush:1')).toBe(true);
    expect(getFlushedForThread(thread.id)).toHaveLength(0);
  });

  // ---- T4b — non-flush user_text upsert never clears Zone 2 ------------

  it('T4b: a non-flush user_text upsert (no `:flush:` in id) leaves Zone 2 untouched', async () => {
    await mountWithActiveThread();

    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-1', userItemId: 'user:0:flush:1', message: 'queued' },
      ],
    });
    await flush();
    expect(getFlushedForThread('thread-1')).toHaveLength(1);

    // The user-typed `user:0` row arrives via its own send/replay
    // path. Its id has no `:flush:` scope, so the Zone 2 confirm
    // gate must skip it — confirming on this would clear an
    // unrelated queued message's marker.
    emitItemEventUpsert({
      id: 'user:0',
      threadId: 'thread-1',
      turnIndex: 0,
      itemIndex: 0,
      kind: 'user_text',
      role: 'user',
      status: 'completed',
      summary: 'unrelated',
      meta: JSON.stringify({ provider_item_id: 'wire-echo-original' }),
      createdAt: 1,
      updatedAt: 1,
    });
    await new Promise<void>((r) => setTimeout(r, 60));
    expect(getFlushedForThread('thread-1').map((f) => f.userItemId)).toEqual([
      'user:0:flush:1',
    ]);
  });

  // ---- T8 — Pending queue renders queued and flushed rows together ---------

  it('T8: SendQueuePreview keeps pending rows above the composer until provider confirmation', async () => {
    const { getByTestId } = await mountWithActiveThread();

    // Only Zone 1 → no divider.
    replaceQueueForThread('thread-1', [
      {
        id: 'q-only',
        threadId: 'thread-1',
        message: 'queued only',
        attachmentIds: [],
        sourceProposedPlan: null,
        revisionSourceProposedPlan: null,
        revisionSourceCommentIds: undefined,
        enqueuedAt: 1,
      },
    ]);
    await flush();
    let rows = Array.from(getByTestId('send-queue-preview').querySelectorAll('[data-testid="send-queue-preview-row"]'));
    expect(rows.map((row) => row.getAttribute('data-state'))).toEqual(['queued']);

    // Add an in-flight flushed marker. It stays in the same pending stack
    // until the provider echo confirms the user_text row.
    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-prior', userItemId: 'user:0:flush:1', message: 'in flight' },
      ],
    });
    await flush();
    await waitFor(() => {
      rows = Array.from(getByTestId('send-queue-preview').querySelectorAll('[data-testid="send-queue-preview-row"]'));
      expect(rows.map((row) => row.getAttribute('data-state'))).toEqual(['flushed', 'queued']);
    });
  });

  // ---- T9 — Thread switch clears queue state for outgoing thread --------

  it('T9: clearing thread status sweeps both zones for the outgoing thread', async () => {
    await mountWithActiveThread();

    // Seed both zones on thread-1 and a token Zone 1 entry on a peer
    // thread that should NOT be touched.
    emitWailsEvent('provider:queue_state_changed', {
      threadId: 'thread-1',
      items: [
        {
          id: 'q-1',
          threadId: 'thread-1',
          message: 'queued on A',
          attachmentIds: [],
          sourceProposedPlan: null,
          revisionSourceProposedPlan: null,
          revisionSourceCommentIds: undefined,
          enqueuedAt: 1,
        },
      ],
    });
    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-prior', userItemId: 'user:0:flush:1', message: 'flushing on A' },
      ],
    });
    replaceQueueForThread('thread-2', [
      {
        id: 'q-peer',
        threadId: 'thread-2',
        message: 'untouched',
        attachmentIds: [],
        sourceProposedPlan: null,
        revisionSourceProposedPlan: null,
        revisionSourceCommentIds: undefined,
        enqueuedAt: 1,
      },
    ]);
    await flush();

    expect(hasQueueItems('thread-1')).toBe(true);
    expect(getQueueForThread('thread-1')).toHaveLength(1);
    expect(getFlushedForThread('thread-1')).toHaveLength(1);

    // Clearing thread status fans out into clearForThread on the
    // sendQueue store — that's the production thread-archive /
    // delete pathway, and the queue is in-memory state that must not
    // outlive the thread.
    clearThreadStatus('thread-1');
    await flush();

    expect(getQueueForThread('thread-1')).toEqual([]);
    expect(getFlushedForThread('thread-1')).toEqual([]);
    // Peer thread untouched — clearForThread is per-thread.
    expect(getQueueForThread('thread-2')).toHaveLength(1);
  });

  // ---- T10 — Multi-item batch dispatched in order -----------------------

  it('T10: provider:queue_flushed preserves arrival order in Zone 2', async () => {
    await mountWithActiveThread();

    // Three items flush as one batch — the dispatcher hands triage
    // exactly this ordering and Zone 2 must reflect it.
    emitWailsEvent('provider:queue_flushed', {
      threadId: 'thread-1',
      items: [
        { queueItemId: 'q-1', userItemId: 'user:0:flush:1', message: 'one' },
        { queueItemId: 'q-2', userItemId: 'user:0:flush:2', message: 'two' },
        { queueItemId: 'q-3', userItemId: 'user:0:flush:3', message: 'three' },
      ],
    });
    await flush();

    const flushed = getFlushedForThread('thread-1');
    expect(flushed.map((f) => f.userItemId)).toEqual([
      'user:0:flush:1',
      'user:0:flush:2',
      'user:0:flush:3',
    ]);
    expect(flushed.map((f) => f.message)).toEqual(['one', 'two', 'three']);
  });
});
