// Transport-gap stamp discipline (docs/specs/thread-replica-sync.md
// §3.4). A gap carries no entity key, so the only safe answer is to
// forget what we claimed to know about the backend's counters — in every
// tier that holds a stamp, not just the registry.
import { beforeEach, describe, expect, it } from 'vitest';
import { applyTransportGap } from './eventsTransportGap';
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
