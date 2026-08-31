// Transport-gap stamp discipline (docs/architecture/thread-replica-sync.md
// §3.4). A gap carries no entity key, so the only safe answer is to
// forget what we claimed to know about the backend's counters — in every
// tier that holds a stamp, not just the registry.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { applyTransportGap } from './eventsTransportGap';
import { resetPanesForTest } from './panes.svelte';
import {
  registerComposerDraft,
  resetComposerDraftRegistryForTest,
} from './composerDraftRegistry.svelte';
import {
  rememberDraftSnapshot,
  resetComposerDraftSnapshotStateForTest,
} from './composerDraftSnapshots';
import type { ComposerDraftStore } from './composerDraft.svelte';
import { buildPane, makeThread } from '../../test/helpers/chat';
import {
  getQueueForThread,
  resetForTest as resetSendQueueForTest,
} from './sendQueue.svelte';
import { applyQueueStateChanged } from './eventsQueue';
import {
  __resetThreadHistoryStampsForTest,
  getThreadHistoryStamp,
  recordAttestedStamp,
} from './threadHistoryStamps';
import { threadItemCache } from './threadItemCache';
import { getBindingMock, setBindingMock } from '../../test/mocks/bindings-app';
import {
  isWorkflowEnginePaused,
  loadWorkflowsOverlayData,
  resetWorkflowRunsForTest,
} from './workflowRuns.svelte';
import {
  attachWorkflowRunMap,
  __resetWorkflowRunMapStoreForTest,
} from './workflowRunMap.svelte';
import { mapRun, mapView } from '../../test/fixtures/runMap';
import { makeItem } from '../../test/helpers/chat';
import {
  regenerateThreadTitle,
  resetThreadTitleGenerationForTest,
  titleGenerationPending,
} from './threadTitleGeneration.svelte';
import type { ThreadItemSnapshot } from './threadItemCache';
import type { ThreadHistoryStamp } from './threadHistoryStamps';

function snapshot(threadId: string, stamp: ThreadHistoryStamp): ThreadItemSnapshot {
  return {
    items: [makeItem({ id: `${threadId}-i0`, threadId })],
    oldestLoadedCursor: null,
    newestLoadedCursor: null,
    oldestLoadedTurnIndex: 0,
    newestLoadedTurnIndex: 0,
    hasMoreHistory: false,
    hasMoreNewer: false,
    latestSettledTurn: null,
    subagentFolds: null,
    historyStamp: stamp,
  };
}

describe('transport gap', () => {
  beforeEach(() => {
    __resetThreadHistoryStampsForTest();
    threadItemCache.clear();
    resetThreadTitleGenerationForTest();
    // The gap handler resyncs the sidebar; both legs swallow their own
    // errors, but an unmocked binding would still log and toast.
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
  });

  it('strips unattested stamps from cached snapshots and keeps attested ones', () => {
    threadItemCache.set('t-event', snapshot('t-event', { epoch: 1, rev: 30, attested: false }));
    threadItemCache.set('t-sync', snapshot('t-sync', { epoch: 1, rev: 12, attested: true }));
    recordAttestedStamp('t-sync', 1, 12);

    applyTransportGap({ channel: 'provider:item_event', seq: 7 });

    // The registry is dropped wholesale — it is one entry per thread and
    // re-earning it costs one window fetch.
    expect(getThreadHistoryStamp('t-sync')).toBeNull();
    // The unattested COPY paired with L1 rows would otherwise outlive the
    // drop and name a rev whose frames this gap ate.
    expect(threadItemCache.get('t-event')?.historyStamp).toBeNull();
    expect(threadItemCache.get('t-event')?.items).toHaveLength(1);
    // An attested copy describes rows a sync returned; any mutation since
    // has advanced the backend's rev past it, so `fresh` cannot lie.
    expect(threadItemCache.get('t-sync')?.historyStamp).toEqual({
      epoch: 1,
      rev: 12,
      attested: true,
    });
  });

  it('strips them on an unknown channel too', () => {
    threadItemCache.set('t-event', snapshot('t-event', { epoch: 1, rev: 30, attested: false }));

    applyTransportGap({ channel: 'future:channel', seq: 1 });

    expect(threadItemCache.get('t-event')?.historyStamp).toBeNull();
  });

  it('thread:title_generation releases pending flags and leaves stamps alone', async () => {
    threadItemCache.set('t-event', snapshot('t-event', { epoch: 1, rev: 30, attested: false }));
    setBindingMock('RegenerateThreadTitle', async () => undefined);
    await regenerateThreadTitle('t-title');
    expect(titleGenerationPending('t-title')).toBe(true);

    // A gap on this channel means a completion frame may be lost — the flag
    // must release or the affordance spins forever. No window data rides the
    // channel, so the stamp discipline does not apply to it.
    applyTransportGap({ channel: 'thread:title_generation', seq: 2 });

    expect(titleGenerationPending('t-title')).toBe(false);
    expect(threadItemCache.get('t-event')?.historyStamp).toEqual({
      epoch: 1,
      rev: 30,
      attested: false,
    });
  });

  it('leaves stamps alone on the self-repairing channels', () => {
    threadItemCache.set('t-event', snapshot('t-event', { epoch: 1, rev: 30, attested: false }));
    recordAttestedStamp('t-sync', 1, 12);

    applyTransportGap({ channel: 'system:stats', seq: 3 });

    expect(threadItemCache.get('t-event')?.historyStamp).toEqual({
      epoch: 1,
      rev: 30,
      attested: false,
    });
    expect(getThreadHistoryStamp('t-sync')).not.toBeNull();
  });
});

/**
 * The `workflow:` prefix branch. It exists so a channel added later cannot fall
 * through to the pane-refresh default, but the channels behind it do not all
 * carry run records: answering every one of them with the run-record resync
 * left the pause banner and the definition catalogs stale indefinitely, with
 * nothing on any surface to say so.
 */
describe('transport gap — workflow channels', () => {
  beforeEach(() => {
    resetWorkflowRunsForTest();
    __resetWorkflowRunMapStoreForTest();
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
    setBindingMock('WorkflowListItems', async () => []);
    setBindingMock('WorkflowGetEngineState', async () => ({ paused: false }));
    setBindingMock('WorkflowListDefinitions', async () => ({ projectId: 'p1', workflows: [] }));
    setBindingMock('WorkflowListAutomations', async () => []);
    setBindingMock('WorkflowListItemCosts', async () => ({}));
  });

  async function settle(): Promise<void> {
    for (let index = 0; index < 8; index += 1) await Promise.resolve();
  }

  const RECORD_CHANNELS = ['workflow:item-state', 'workflow:phase-state', 'workflow:soft-stop'];

  it.each(RECORD_CHANNELS)('%s re-sources the run maps', async (channel) => {
    setBindingMock('WorkflowGetRunMap', async () => mapView([mapRun('root')]));
    const attachment = attachWorkflowRunMap('root');
    await settle();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(1);

    applyTransportGap({ channel, seq: 1 });
    await settle();
    expect(getBindingMock('WorkflowGetRunMap')).toHaveBeenCalledTimes(2);
    // Not the engine state: nothing on this channel says anything about it.
    expect(getBindingMock('WorkflowGetEngineState')).not.toHaveBeenCalled();
    attachment.release();
  });

  it('workflow:engine-state re-reads the paused flag it could not have healed', async () => {
    setBindingMock('WorkflowGetEngineState', async () => ({ paused: true }));
    expect(isWorkflowEnginePaused()).toBe(false);

    applyTransportGap({ channel: 'workflow:engine-state', seq: 2 });
    await settle();

    expect(getBindingMock('WorkflowGetEngineState')).toHaveBeenCalledTimes(1);
    expect(isWorkflowEnginePaused()).toBe(true);
  });

  it('workflow:definitions-changed re-lists the catalogs', async () => {
    await loadWorkflowsOverlayData(['p1']);
    const before = getBindingMock('WorkflowListDefinitions')?.mock.calls.length ?? 0;

    applyTransportGap({ channel: 'workflow:definitions-changed', seq: 3 });
    await settle();

    expect(getBindingMock('WorkflowListDefinitions')?.mock.calls.length).toBe(before + 1);
  });

  it('workflow:error recovers nothing — its frames are toasts, not state', async () => {
    applyTransportGap({ channel: 'workflow:error', seq: 4 });
    await settle();
    expect(getBindingMock('WorkflowGetEngineState')).not.toHaveBeenCalled();
    expect(getBindingMock('WorkflowListItems')).not.toHaveBeenCalled();
  });

  it('a workflow channel this build does not know recovers ALL of it', async () => {
    await loadWorkflowsOverlayData(['p1']);
    const definitions = getBindingMock('WorkflowListDefinitions')?.mock.calls.length ?? 0;

    applyTransportGap({ channel: 'workflow:something-new', seq: 5 });
    await settle();

    expect(getBindingMock('WorkflowGetEngineState')?.mock.calls.length).toBeGreaterThan(1);
    expect(getBindingMock('WorkflowListDefinitions')?.mock.calls.length).toBe(definitions + 1);
  });
});

// The send-queue family, which splits three ways: one channel whose whole
// state is one RPC away, one that took the timeline with it, and two that
// carry delivery badges nothing can re-derive.
describe('transport gap — the send-queue channels', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetSendQueueForTest();
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
  });

  async function settle(): Promise<void> {
    for (let index = 0; index < 8; index += 1) await Promise.resolve();
  }

  it('queue_state_changed re-reads the queue and nothing else', async () => {
    setBindingMock('GetQueueState', async () => [
      { threadId: 'thread-a', id: 'q1', message: 'queued while offline', enqueuedAt: 1 },
    ]);
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [], 'main');
    const refresh = vi.spyOn(pane, 'refreshFromBackend');

    applyTransportGap({ channel: 'provider:queue_state_changed', seq: 3 });
    await settle();

    expect(getQueueForThread('thread-a').map((item) => item.message))
      .toEqual(['queued while offline']);
    // A page of timeline items is not the way to repair a handful of queue
    // rows; the targeted read exists precisely to avoid it.
    expect(refresh).not.toHaveBeenCalled();
  });

  // The queue is per thread, not per pane — unlike the item windows two panes
  // on one thread can hold different slices of.
  it('reads once for two panes on the same thread', async () => {
    setBindingMock('GetQueueState', async () => []);
    await buildPane(makeThread({ id: 'thread-a' }), [], 'main');
    await buildPane(makeThread({ id: 'thread-a' }), [], 'second');

    applyTransportGap({ channel: 'provider:queue_state_changed', seq: 3 });
    await settle();

    expect(getBindingMock('GetQueueState')?.mock.calls.length).toBe(1);
  });

  // A live frame that lands while the snapshot is in flight is newer than the
  // state we asked for, so it wins.
  it('discards a snapshot a live frame overtook', async () => {
    const inFlight: { release: () => void } = { release: () => {} };
    setBindingMock('GetQueueState', async () => {
      await new Promise<void>((resolve) => { inFlight.release = resolve; });
      return [{ threadId: 'thread-a', id: 'stale', message: 'from before', enqueuedAt: 1 }];
    });
    await buildPane(makeThread({ id: 'thread-a' }), [], 'main');

    applyTransportGap({ channel: 'provider:queue_state_changed', seq: 3 });
    await settle();
    applyQueueStateChanged({
      threadId: 'thread-a',
      items: [{ threadId: 'thread-a', id: 'live', message: 'from after', enqueuedAt: 2 }],
    });
    inFlight.release();
    await settle();

    expect(getQueueForThread('thread-a').map((item) => item.message)).toEqual(['from after']);
  });

  // A restore deletes timeline rows and hands their text back to the composer,
  // so the queue is not the only casualty of a missed frame.
  it('queue_restored refreshes the pane rather than just the queue', async () => {
    setBindingMock('GetQueueState', async () => []);
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [], 'main');
    const refresh = vi.spyOn(pane, 'refreshFromBackend');

    applyTransportGap({ channel: 'provider:queue_restored', seq: 3 });
    await settle();

    expect(refresh).toHaveBeenCalled();
    expect(getBindingMock('GetQueueState')).not.toHaveBeenCalled();
  });

  // Delivery badges are transient state no RPC returns. Refetching a pane's
  // window would not repair them, so the honest answer is to do nothing.
  it('the delivery channels recover nothing, and cost nothing', async () => {
    setBindingMock('GetQueueState', async () => []);
    const pane = await buildPane(makeThread({ id: 'thread-a' }), [], 'main');
    const refresh = vi.spyOn(pane, 'refreshFromBackend');

    applyTransportGap({ channel: 'provider:queue_flushed', seq: 3 });
    applyTransportGap({ channel: 'provider:command_lifecycle', seq: 4 });
    await settle();

    expect(refresh).not.toHaveBeenCalled();
    expect(getBindingMock('GetQueueState')).not.toHaveBeenCalled();
  });
});

// A gap on draft:updated means this client missed a write another screen
// made. Recovery is the applier's re-read minus the identity checks: after a
// gap there is no frame to read an identity from, and re-reading a draft this
// client wrote itself costs a round trip and changes nothing.
describe('transport gap — draft:updated', () => {
  beforeEach(() => {
    resetPanesForTest();
    resetComposerDraftRegistryForTest();
    resetComposerDraftSnapshotStateForTest();
    setBindingMock('ListThreads', async () => []);
    setBindingMock('ListProjects', async () => []);
  });

  async function paneWithDraft(threadId: string, paneKey: string) {
    const pane = await buildPane(makeThread({ id: threadId }), [], paneKey);
    const reloadFromBackend = vi.fn(async () => {});
    registerComposerDraft(pane.paneId, { reloadFromBackend } as unknown as ComposerDraftStore);
    return reloadFromBackend;
  }

  it('re-reads every mounted composer', async () => {
    const first = await paneWithDraft('thread-a', 'main');
    const second = await paneWithDraft('thread-b', 'second');

    applyTransportGap({ channel: 'draft:updated', seq: 9 });

    expect(first).toHaveBeenCalledWith('thread-a');
    expect(second).toHaveBeenCalledWith('thread-b');
  });

  // The guard the applier uses holds here too: reloadFromBackend discards
  // unsaved local text, and a gap is not a reason to do that to someone who
  // is mid-sentence.
  it('leaves a composer holding unsaved work alone', async () => {
    const reload = await paneWithDraft('thread-a', 'main');
    rememberDraftSnapshot('thread-a', {
      content: 'half a sentence',
      attachments: [],
      terminalChips: [],
      sourceProposedPlan: null,
    });

    applyTransportGap({ channel: 'draft:updated', seq: 9 });

    expect(reload).not.toHaveBeenCalled();
  });
});
