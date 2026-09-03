import { beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import {
  applyPRThreads,
  applyPRUpdatedEvent,
  attachPR,
  clearPRThreadResolveOverride,
  handlePRVisibilityChange,
  overriddenPRThreads,
  peekPRError,
  peekPRSnapshot,
  prReviewKeys,
  setPRThreadResolveOverride,
} from './prReviewStore.svelte';
import { loadPRCIJobs, peekPRCI } from './prReviewCI.svelte';
import {
  openPRConflicts,
  peekPRConflicts,
  permitPRConflictReconcile,
} from './prReviewConflicts.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { prKey, type PRRef } from '../utils/prReference';
import type { PRDetail, ReviewThread } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import type { WorkspaceRef } from '../types/git';

// The checkout merge-tree runs in. A pr-anchor thread would pass the zero
// ref instead and the backend would refuse — see reviewPane's pr scope.
const CONFLICT_WS: WorkspaceRef = { projectId: 'project-1', workspacePath: '/workspace' };
import { applyTransportGap } from './eventsTransportGap';

const REF: PRRef = { forge: 'github', namespace: 'owner', repo: 'repo', number: 5 };
const KEY = prKey(REF);

async function flush(n = 8): Promise<void> {
  for (let i = 0; i < n; i += 1) await tick();
}

function detailStub(overrides: Partial<PRDetail> = {}): PRDetail {
  return {
    number: 5,
    title: 'PR',
    body: '',
    authorLogin: 'alice',
    state: 'open',
    draft: false,
    baseRefName: 'main',
    headRefName: 'feature',
    headSHA: 'sha-a',
    url: 'https://github.com/owner/repo/pull/5',
    additions: 1,
    deletions: 0,
    changedFiles: 1,
    viewerIsAuthor: false,
    reviewDecision: '',
    latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean',
    ...overrides,
  };
}

function threadStub(id: string): ReviewThread {
  return {
    id,
    path: 'main.go',
    line: 1,
    side: 'right',
    isResolvable: true,
    isResolved: false,
    isOutdated: false,
    comments: [],
  };
}

function installSubscribeMock(id = 'sub-1', detail = detailStub()) {
  const subscribe = setBindingMock('SubscribePRUpdates', async () => ({
    id,
    prKey: KEY,
    detail,
    threads: [],
    headSHA: detail.headSHA,
  }));
  const unsubscribe = setBindingMock('UnsubscribePRUpdates', async () => undefined);
  return { subscribe, unsubscribe };
}

function installConflictMocks(paths = ['main.go'], treeOID = 'tree-1') {
  const conflicts = setBindingMock('GetPRMergeConflicts', async () => ({
    conflicted: true,
    treeOID,
    baseLabel: 'origin/main',
    headLabel: 'feature',
    paths,
    notes: {},
    messages: [],
  }));
  const file = setBindingMock('GetMergeConflictFile', async () => 'merged content');
  return { conflicts, file };
}

beforeEach(() => {
  setBindingMock('SetPRUpdatesActive', async () => undefined);
  setBindingMock('GetPRCIJobs', async () => ({ status: 'success', stages: [] }));
});

describe('prReviewStore — one entity, many holders', () => {
  it('shares ONE subscription between holders and releases it on the last one', async () => {
    const { subscribe, unsubscribe } = installSubscribeMock();

    const a = attachPR(KEY, { ref: REF });
    const b = attachPR(KEY, { ref: REF });
    await flush();

    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(a.snapshot?.detail?.number).toBe(5);
    // The point of the entity store: the second holder observes the same
    // object, not a private copy that has to catch up.
    expect(b.snapshot).toBe(a.snapshot);

    a.release();
    await flush();
    expect(unsubscribe).not.toHaveBeenCalled();

    b.release();
    await flush();
    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
    expect(prReviewKeys()).toEqual([]);
  });

  it('is idempotent on a double release and re-subscribes on re-attach', async () => {
    const { subscribe, unsubscribe } = installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();

    a.release();
    a.release();
    await flush();
    // A holder that releases twice must not release someone else's vote.
    expect(unsubscribe).toHaveBeenCalledTimes(1);

    const b = attachPR(KEY, { ref: REF });
    await flush();
    expect(subscribe).toHaveBeenCalledTimes(2);
    expect(b.snapshot?.detail?.number).toBe(5);
    b.release();
  });

  it('resolves ready() from the first observation and again immediately after', async () => {
    installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    const first = await a.ready();
    expect(first.headSHA).toBe('sha-a');
    // Every waiter gets the object the store is holding, first one included,
    // so a snapshot captured from ready() is never a frozen copy.
    expect(first).toBe(a.snapshot);
    // A later holder must not park on a deferred nobody will settle again.
    const b = attachPR(KEY, { ref: REF });
    expect(await b.ready()).toBe(first);
    a.release();
    b.release();
  });

  it('rejects ready() when the subscribe fails, instead of hanging the load', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('SubscribePRUpdates', async () => {
      throw new Error('gh api boom');
    });
    const a = attachPR(KEY, { ref: REF });

    await expect(a.ready()).rejects.toThrow('gh api boom');
    expect(peekPRError(KEY)).toContain('gh api boom');
    a.release();
  });

  it('rejects ready() when the last holder leaves while the subscribe is in flight', async () => {
    let resolveSubscribe!: (value: unknown) => void;
    setBindingMock('SubscribePRUpdates', () => new Promise((resolve) => {
      resolveSubscribe = resolve;
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);

    const a = attachPR(KEY, { ref: REF });
    const ready = a.ready();
    a.release();

    await expect(ready).rejects.toThrow('PR updates released');
    // Settle the abandoned source so it tears its own subscription down.
    resolveSubscribe({ id: 'sub-late', prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a' });
    await flush();
  });
});

describe('prReviewStore — superseded source runs', () => {
  it('does not publish a superseded run\'s subscription id, and releases it', async () => {
    const pending: Array<(value: unknown) => void> = [];
    const subscribe = setBindingMock('SubscribePRUpdates', () => new Promise((resolve) => {
      pending.push(resolve);
    }));
    const unsubscribe = setBindingMock('UnsubscribePRUpdates', async () => undefined);
    const setActive = setBindingMock('SetPRUpdatesActive', async () => undefined);

    const a = attachPR(KEY, { ref: REF });
    await flush();
    // Supersede the in-flight run (what a reconnect or a retry does).
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(subscribe).toHaveBeenCalledTimes(2);

    // The loser resolves last, as a slow first attempt does.
    pending[1]({ id: 'sub-live', prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a' });
    await flush();
    pending[0]({ id: 'sub-stale', prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a' });
    await flush();

    // The stale run's id is released immediately and never becomes the
    // key's live subscription — a visibility flip addressing it would pause
    // a stream nobody is reading while the live one keeps polling.
    expect(unsubscribe).toHaveBeenCalledWith('sub-stale');
    setActive.mockClear();
    handlePRVisibilityChange();
    expect(setActive.mock.calls.map((call) => call[0])).toEqual(['sub-live']);

    a.release();
    await flush();
  });

  it('does not reject a live waiter from a superseded run\'s failure', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    let failFirst!: (err: unknown) => void;
    let calls = 0;
    setBindingMock('SubscribePRUpdates', () => {
      calls += 1;
      if (calls === 1) {
        return new Promise((_resolve, reject) => {
          failFirst = reject;
        });
      }
      return Promise.resolve({
        id: 'sub-live',
        prKey: KEY,
        detail: detailStub(),
        threads: [],
        headSHA: 'sha-a',
      });
    });
    setBindingMock('UnsubscribePRUpdates', async () => undefined);

    const a = attachPR(KEY, { ref: REF });
    const ready = a.ready();
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();

    // The first attempt fails AFTER a newer one has already succeeded. Its
    // failure belongs to a world that is gone; letting it reject would fail
    // a load the live subscription has already answered.
    failFirst(new Error('gh api boom'));
    await flush();

    // The disconnect settled the original waiter (see the transport test
    // below), so a fresh one is what a reload would park on.
    await expect(ready).rejects.toThrow('Disconnected');
    expect((await a.ready()).headSHA).toBe('sha-a');
    expect(peekPRError(KEY)).toBeNull();
    a.release();
  });

  it('routes pushes by the BACKEND key, aliased to the local one', async () => {
    // The two formatters agree for every real PR, but a namespace-less
    // project is already a case where they do not: Go joins through
    // PRReference.Project(), TypeScript through `${namespace}/${repo}`.
    const bare: PRRef = { forge: 'github', namespace: '', repo: 'repo', number: 7 };
    const localKey = prKey(bare);
    const wireKey = 'github:repo:7';
    expect(localKey).not.toBe(wireKey);

    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-bare',
      prKey: wireKey,
      detail: detailStub({ number: 7 }),
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);

    const a = attachPR(localKey, { ref: bare });
    await flush();

    applyPRUpdatedEvent({
      prKey: wireKey,
      detail: detailStub({ number: 7, title: 'retitled' }),
      threads: [],
      headSHA: 'sha-b',
    });
    expect(a.snapshot?.detail?.title).toBe('retitled');
    expect(a.snapshot?.headSHA).toBe('sha-b');

    a.release();
    await flush();
    // The alias goes with the subscription: a push arriving after the last
    // holder left routes nowhere rather than resurrecting the key.
    applyPRUpdatedEvent({ prKey: wireKey, detail: detailStub({ number: 7 }), threads: [], headSHA: 'sha-c' });
    expect(peekPRSnapshot(localKey)).toBeNull();
  });
});

// A subscribe reads the pump's state under the backend's mutex, but the
// alias that routes `pr:updated` is installed only once the response gets
// home. A frame emitted in that window routes to no key — and the pump only
// emits on CHANGE, so nothing re-states it.
describe('prReviewStore — the join/push handoff', () => {
  function gatedSubscribe(result: Record<string, unknown>) {
    let land!: () => void;
    setBindingMock('SubscribePRUpdates', () => new Promise((resolve) => {
      land = () => resolve(result);
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    return () => land();
  }

  it('replays a frame emitted while the subscribe was in flight', async () => {
    const land = gatedSubscribe({
      id: 'sub-1',
      prKey: KEY,
      detail: detailStub(),
      threads: [],
      headSHA: 'sha-a',
      error: '',
      seq: 7,
    });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ title: 'retitled', headSHA: 'sha-b' }),
      threads: [threadStub('t-1')],
      headSHA: 'sha-b',
      seq: 8,
    });
    // Nothing routes it yet — that is the whole window.
    expect(peekPRSnapshot(KEY)).toBeNull();

    land();
    await flush();

    expect(a.snapshot?.headSHA).toBe('sha-b');
    expect(a.snapshot?.detail?.title).toBe('retitled');
    expect(a.snapshot?.threads.map((t) => t.id)).toEqual(['t-1']);
    // …and the load resolves on what the replay left, not on the snapshot
    // the join returned.
    expect((await a.ready()).headSHA).toBe('sha-b');
    a.release();
    await flush();
  });

  // The guard is what keeps the replay honest: the backend stamps a frame in
  // the same critical section that stored the state it carries, so a frame
  // at or below the join's seq is already IN the snapshot it returned.
  it('does not replay a frame the subscribe result already accounts for', async () => {
    const land = gatedSubscribe({
      id: 'sub-1',
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-joined' }),
      threads: [],
      headSHA: 'sha-joined',
      error: '',
      seq: 7,
    });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-already-folded-in' }),
      threads: [],
      headSHA: 'sha-already-folded-in',
      seq: 7,
    });

    land();
    await flush();
    expect(a.snapshot?.headSHA).toBe('sha-joined');
    a.release();
    await flush();
  });

  // A second key on the SAME wireKey has the same window — the frame reaches
  // key 1 only, because key 2's alias is not installed yet.
  it('replays into a second key joining a wireKey the first one already routes', async () => {
    const bare: PRRef = { forge: 'github', namespace: '', repo: 'repo', number: 7 };
    const otherLocalKey = `${prKey(bare)}:mirror`;
    const wireKey = 'github:repo:7';

    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-first',
      prKey: wireKey,
      detail: detailStub({ number: 7 }),
      threads: [],
      headSHA: 'sha-a',
      error: '',
      seq: 3,
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    const first = attachPR(prKey(bare), { ref: bare });
    await flush();

    const land = gatedSubscribe({
      id: 'sub-second',
      prKey: wireKey,
      detail: detailStub({ number: 7 }),
      threads: [],
      headSHA: 'sha-a',
      error: '',
      seq: 3,
    });
    const second = attachPR(otherLocalKey, { ref: bare });
    await flush();

    applyPRUpdatedEvent({
      prKey: wireKey,
      detail: detailStub({ number: 7, headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
      seq: 4,
    });
    expect(first.snapshot?.headSHA).toBe('sha-b');
    expect(peekPRSnapshot(otherLocalKey)).toBeNull();

    land();
    await flush();
    expect(second.snapshot?.headSHA).toBe('sha-b');

    first.release();
    second.release();
    await flush();
  });

  it('drops buffered frames on a disconnect so they cannot replay over a fresh subscribe', async () => {
    const land = gatedSubscribe({
      id: 'sub-dead',
      prKey: KEY,
      detail: detailStub(),
      threads: [],
      headSHA: 'sha-a',
      error: '',
      seq: 1,
    });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-from-a-dead-connection' }),
      threads: [],
      headSHA: 'sha-from-a-dead-connection',
      seq: 99,
    });
    // The dead connection's subscribe is still parked, so the buffer only
    // empties if the disconnect empties it.
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    await flush();

    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-fresh',
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-fresh' }),
      threads: [],
      headSHA: 'sha-fresh',
      error: '',
      seq: 2,
    }));
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();

    // seq 99 outranks the fresh result's 2, but it was stamped by a pump
    // that is gone — sequences do not survive the connection that minted
    // them.
    expect(a.snapshot?.headSHA).toBe('sha-fresh');

    // Settle the abandoned subscribe so its run tears its own handle down.
    land();
    await flush();
    a.release();
    await flush();
  });

  // The pump dedups identical failures, so an outage produces no frame for a
  // subscriber that arrives during it: without the error on the join result
  // the pane showed stale data with no banner until the forge changed its
  // answer.
  it('shows the pump\'s active failure to a subscriber that joined during it', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: KEY,
      detail: detailStub(),
      threads: [],
      headSHA: 'sha-a',
      error: 'failed to refresh pull request (id: abc)',
      seq: 4,
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);

    const a = attachPR(KEY, { ref: REF });
    // A pump error does not fail the load: stale data plus a banner is what
    // every holder already on the key sees.
    expect((await a.ready()).headSHA).toBe('sha-a');
    expect(a.error).toContain('failed to refresh');
    expect(a.snapshot?.detail?.number).toBe(5);

    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a', seq: 5 });
    expect(a.error).toBeNull();
    a.release();
    await flush();
  });

  // The two frame kinds carry different things: a snapshot frame states the
  // PR, an error-only frame states the pump. Keeping just the newest meant
  // the error replayed over the join's stale snapshot and the observation
  // underneath it was never stated — and the pump emits only on CHANGE, so
  // nothing would restate it until the PR moved.
  it('replays both a buffered snapshot and the error frame that followed it', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const land = gatedSubscribe({
      id: 'sub-1',
      prKey: KEY,
      detail: detailStub(),
      threads: [],
      headSHA: 'sha-a',
      error: '',
      seq: 7,
    });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ title: 'retitled', headSHA: 'sha-b' }),
      threads: [threadStub('t-1')],
      headSHA: 'sha-b',
      seq: 8,
    });
    applyPRUpdatedEvent({ prKey: KEY, error: 'failed to refresh pull request (id: abc)', seq: 9 });

    land();
    await flush();

    expect(a.snapshot?.headSHA).toBe('sha-b');
    expect(a.snapshot?.detail?.title).toBe('retitled');
    expect(a.snapshot?.threads.map((t) => t.id)).toEqual(['t-1']);
    expect(a.error).toContain('failed to refresh');
    a.release();
    await flush();
  });

  // The other direction: the pump clears its stored failure before emitting a
  // successful poll, so a later snapshot supersedes the error — replaying
  // both would leave a banner the pump has already recovered from.
  it('drops a buffered error the snapshot after it superseded', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const land = gatedSubscribe({
      id: 'sub-1',
      prKey: KEY,
      detail: detailStub(),
      threads: [],
      headSHA: 'sha-a',
      error: '',
      seq: 7,
    });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({ prKey: KEY, error: 'failed to refresh pull request (id: abc)', seq: 8 });
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
      seq: 9,
    });

    land();
    await flush();

    expect(a.snapshot?.headSHA).toBe('sha-b');
    expect(a.error).toBeNull();
    a.release();
    await flush();
  });

  // Normal routing has no ordering of its own — this frame lost a race with
  // the subscribe that already accounts for it, and the buffer is not
  // involved. Applying it regresses the entity to a superseded head, and the
  // backend's change detection is unaffected, so no later tick heals it.
  it('ignores a live frame the installed subscribe result already outranks', async () => {
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-joined' }),
      threads: [],
      headSHA: 'sha-joined',
      error: '',
      seq: 7,
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-stale' }),
      threads: [],
      headSHA: 'sha-stale',
      seq: 6,
    });

    expect(a.snapshot?.headSHA).toBe('sha-joined');
    a.release();
    await flush();
  });

  it('applies a higher-seq frame and then refuses a repeat of it', async () => {
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-joined' }),
      threads: [],
      headSHA: 'sha-joined',
      error: '',
      seq: 7,
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    const a = attachPR(KEY, { ref: REF });
    await flush();

    const frame8 = {
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
      seq: 8,
    };
    applyPRUpdatedEvent(frame8);
    expect(a.snapshot?.headSHA).toBe('sha-b');

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-c' }),
      threads: [],
      headSHA: 'sha-c',
      seq: 9,
    });
    // A second delivery of a frame already applied — a replay racing the live
    // route — must not put it back over what followed it.
    applyPRUpdatedEvent(frame8);

    expect(a.snapshot?.headSHA).toBe('sha-c');
    a.release();
    await flush();
  });
});

describe('prReviewStore — applying pushes', () => {
  it('applies a pr:updated to every holder at once', async () => {
    installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    const b = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ title: 'retitled', headSHA: 'sha-b' }),
      threads: [threadStub('t-1')],
      headSHA: 'sha-b',
    });

    expect(a.snapshot?.detail?.title).toBe('retitled');
    expect(b.snapshot).toBe(a.snapshot);
    expect(a.snapshot?.headSHA).toBe('sha-b');
    expect(a.snapshot?.threads.map((t) => t.id)).toEqual(['t-1']);
    a.release();
    b.release();
  });

  it('drops a push for a PR nobody holds', async () => {
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub(),
      threads: [],
      headSHA: 'sha-a',
    });
    // A late frame for a closed pane must not resurrect an entry.
    expect(peekPRSnapshot(KEY)).toBeNull();
    expect(prReviewKeys()).toEqual([]);
  });

  it('surfaces a pump fetch failure without blanking the PR, and clears it on recovery', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const before = a.snapshot;

    applyPRUpdatedEvent({ prKey: KEY, error: 'gh api rate limit exceeded' });
    expect(a.error).toContain('rate limit');
    // Principle 5: a failed poll is user-facing state — and the PR the user
    // is reading stays on screen while it is failing.
    expect(a.snapshot).toBe(before);

    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a' });
    expect(a.error).toBeNull();
    a.release();
  });

  // A thread-only re-list (submit / reply) observes the FORGE's review
  // threads, which says nothing about the poll pump. Clearing the error
  // there dismissed a "PR updates stopped" banner that was still true — and
  // the backend dedups identical failures, so no later event restores it.
  it('a thread-only re-list keeps a still-failing pump\'s error visible', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({ prKey: KEY, error: 'failed to refresh pull request (id: abc)' });
    expect(a.error).toContain('failed to refresh');

    applyPRThreads(KEY, [threadStub('t-1')]);
    expect(a.snapshot?.threads.map((t) => t.id)).toEqual(['t-1']);
    expect(a.error).toContain('failed to refresh');

    // A healthy pump observation is what clears it.
    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a' });
    expect(a.error).toBeNull();
    a.release();
  });

  it('refreshes CI on a push only for a PR whose CI was loaded', async () => {
    installSubscribeMock();
    const ci = setBindingMock('GetPRCIJobs', async () => ({ status: 'success', stages: [] }));
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a' });
    await flush();
    // Nothing asked for CI yet, so the poll must not invent the work.
    expect(ci).not.toHaveBeenCalled();

    await loadPRCIJobs(KEY, REF);
    expect(peekPRCI(KEY).pipeline?.status).toBe('success');
    expect(ci).toHaveBeenCalledTimes(1);

    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b' });
    await flush();
    expect(ci).toHaveBeenCalledTimes(2);
    a.release();
  });

  it('drops CI state when the last holder leaves', async () => {
    installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    await loadPRCIJobs(KEY, REF);
    expect(peekPRCI(KEY).pipeline).not.toBeNull();

    a.release();
    await flush();
    expect(peekPRCI(KEY).pipeline).toBeNull();
  });

  it('does not let a CI fetch in flight when the holder left resurrect the entry', async () => {
    installSubscribeMock();
    let resolveCI!: (value: unknown) => void;
    setBindingMock('GetPRCIJobs', () => new Promise((resolve) => {
      resolveCI = resolve;
    }));
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const loading = loadPRCIJobs(KEY, REF);

    a.release();
    await flush();
    // The fetch still holds the entry object; without the token bump its
    // result would land in an entry that is out of the map, where nothing
    // can clear it — the same trap dropConflicts has always guarded.
    resolveCI({ status: 'success', stages: [] });
    await loading;
    expect(peekPRCI(KEY).pipeline).toBeNull();
    expect(peekPRCI(KEY).loading).toBe(false);
  });
});

describe('prReviewStore — merge conflicts follow the PR', () => {
  it('computes the merged tree once and reuses it for the same base/head', async () => {
    installSubscribeMock();
    const { conflicts, file } = installConflictMocks();
    const a = attachPR(KEY, { ref: REF });
    await flush();

    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    expect(conflicts).toHaveBeenCalledTimes(1);
    expect(file).toHaveBeenCalledTimes(1);
    expect(peekPRConflicts(KEY).state?.treeOID).toBe('tree-1');
    expect(peekPRConflicts(KEY).contentByPath.get('main.go')).toBe('merged content');

    // A second pane opening the same view pays nothing.
    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    expect(conflicts).toHaveBeenCalledTimes(1);
    a.release();
  });

  it('recomputes the tree when the head moves under it', async () => {
    installSubscribeMock();
    const { conflicts } = installConflictMocks();
    setBindingMock('GetMergeConflictFile', async () => 'content for sha-a');
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const viewing = permitPRConflictReconcile(KEY);
    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    expect(peekPRConflicts(KEY).contentByPath.get('main.go')).toBe('content for sha-a');

    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-2',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      notes: {},
      messages: [],
    }));
    setBindingMock('GetMergeConflictFile', async () => 'content for sha-b');
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
    });
    await flush();

    // The old tree OID names an object that answers for a merge nobody is
    // looking at any more; leaving it in place rendered the previous head's
    // content forever.
    expect(conflicts).toHaveBeenCalledTimes(1); // the first mock, superseded
    expect(peekPRConflicts(KEY).state?.treeOID).toBe('tree-2');
    expect(peekPRConflicts(KEY).state?.headSHA).toBe('sha-b');
    expect(peekPRConflicts(KEY).contentByPath.get('main.go')).toBe('content for sha-b');
    viewing();
    a.release();
  });

  // A head move that lands WHILE the tree (and its per-file reads) are
  // running used to be dropped on the `loading` early return. Later polls
  // dedup to nothing, so the open view stayed pinned to the superseded
  // merge for as long as it was open.
  it('converges on a head that moved while the first load was still running', async () => {
    installSubscribeMock();
    let releaseFirstTree!: () => void;
    const firstTree = new Promise<void>((resolve) => {
      releaseFirstTree = resolve;
    });
    setBindingMock('GetPRMergeConflicts', async () => {
      await firstTree;
      return {
        conflicted: true,
        treeOID: 'tree-1',
        baseLabel: 'origin/main',
        headLabel: 'feature',
        paths: ['main.go'],
        notes: {},
        messages: [],
      };
    });
    setBindingMock('GetMergeConflictFile', async () => 'content for sha-a');
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const viewing = permitPRConflictReconcile(KEY);

    const opening = openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    await flush();
    expect(peekPRConflicts(KEY).loading).toBe(true);

    // The PR moves under the in-flight load.
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
    });
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-2',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      notes: {},
      messages: [],
    }));
    setBindingMock('GetMergeConflictFile', async () => 'content for sha-b');

    releaseFirstTree();
    await opening;
    await flush();

    expect(peekPRConflicts(KEY).state?.treeOID).toBe('tree-2');
    expect(peekPRConflicts(KEY).state?.headSHA).toBe('sha-b');
    expect(peekPRConflicts(KEY).contentByPath.get('main.go')).toBe('content for sha-b');
    expect(peekPRConflicts(KEY).loading).toBe(false);
    viewing();
    a.release();
  });

  // …and a load that FAILED is not a settled answer about the pair that
  // moved under it. The parked pair used to be discarded on the `!state`
  // guard, so the view stayed terminal on the failed head's error — later
  // polls dedup to nothing, so nothing ever re-stated the new pair.
  it('runs the pair that moved under a FAILED tree load', async () => {
    installSubscribeMock();
    let failFirstTree!: (err: unknown) => void;
    const firstTree = new Promise<void>((_resolve, reject) => {
      failFirstTree = reject;
    });
    setBindingMock('GetPRMergeConflicts', async () => {
      await firstTree;
      throw new Error('unreachable');
    });
    setBindingMock('GetMergeConflictFile', async () => 'content for sha-a');
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const viewing = permitPRConflictReconcile(KEY);

    const opening = openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    await flush();
    expect(peekPRConflicts(KEY).loading).toBe(true);

    // The PR moves while the merge-tree run is still going, so the new pair
    // parks on the entry.
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
    });
    const recompute = setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-2',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      notes: {},
      messages: [],
    }));
    setBindingMock('GetMergeConflictFile', async () => 'content for sha-b');

    failFirstTree(new Error('merge-tree: fatal: bad object'));
    await opening;
    await flush();

    expect(recompute).toHaveBeenCalledTimes(1);
    expect(peekPRConflicts(KEY).state?.treeOID).toBe('tree-2');
    expect(peekPRConflicts(KEY).state?.headSHA).toBe('sha-b');
    expect(peekPRConflicts(KEY).contentByPath.get('main.go')).toBe('content for sha-b');
    expect(peekPRConflicts(KEY).error).toBeNull();
    viewing();
    a.release();
  });

  // The per-file reads run in PARALLEL. One shared error slot let a later
  // file's success clear an earlier file's failure while that file was
  // still contentless — a healthy-looking view with a hole in it.
  it('keeps a failed conflict file visible when a sibling read succeeds', async () => {
    installSubscribeMock();
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-1',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['broken.go', 'fine.go'],
      notes: {},
      messages: [],
    }));
    setBindingMock('GetMergeConflictFile', async (_thread: unknown, _tree: unknown, path: unknown) => {
      if (path === 'broken.go') throw new Error('object not found');
      return 'merged content';
    });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    await flush();

    const conflicts = peekPRConflicts(KEY);
    expect(conflicts.contentByPath.get('fine.go')).toBe('merged content');
    expect(conflicts.contentByPath.has('broken.go')).toBe(false);
    expect(conflicts.error).toContain('broken.go');
    expect(conflicts.error).toContain('object not found');
    a.release();
  });

  it('recomputes the tree when the base ref changes', async () => {
    installSubscribeMock();
    installConflictMocks();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const viewing = permitPRConflictReconcile(KEY);
    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());

    const recompute = setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: false,
      treeOID: 'tree-3',
      baseLabel: 'origin/develop',
      headLabel: 'feature',
      paths: [],
      notes: {},
      messages: [],
    }));
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ baseRefName: 'develop' }),
      threads: [],
      headSHA: 'sha-a',
    });
    await flush();

    expect(recompute).toHaveBeenCalledTimes(1);
    expect(peekPRConflicts(KEY).state?.baseRefName).toBe('develop');
    expect(peekPRConflicts(KEY).state?.paths).toEqual([]);
    viewing();
    a.release();
  });

  it('leaves the tree alone when the push moves nothing', async () => {
    installSubscribeMock();
    installConflictMocks();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const viewing = permitPRConflictReconcile(KEY);
    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());

    const recompute = setBindingMock('GetPRMergeConflicts', async () => {
      throw new Error('must not recompute');
    });
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ title: 'retitled' }),
      threads: [],
      headSHA: 'sha-a',
    });
    await flush();

    expect(recompute).not.toHaveBeenCalled();
    expect(peekPRConflicts(KEY).state?.treeOID).toBe('tree-1');
    viewing();
    a.release();
  });

  it('does not compute a tree for a PR whose conflict view was never opened', async () => {
    installSubscribeMock();
    const recompute = setBindingMock('GetPRMergeConflicts', async () => {
      throw new Error('must not compute');
    });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
    });
    await flush();

    expect(recompute).not.toHaveBeenCalled();
    expect(peekPRConflicts(KEY).state).toBeNull();
    a.release();
  });

  it('does not reconcile a head move while no pane is rendering the conflict view', async () => {
    installSubscribeMock();
    installConflictMocks();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    const viewing = permitPRConflictReconcile(KEY);
    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    // The user closes the conflict view and goes back to the diff.
    viewing();

    const recompute = setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-2',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      notes: {},
      messages: [],
    }));
    const files = setBindingMock('GetMergeConflictFile', async () => 'content for sha-b');
    applyPRUpdatedEvent({
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
    });
    await flush();

    // A merge-tree run plus one read per conflicted file is not something a
    // background poll may spend on a surface nobody is looking at.
    expect(recompute).not.toHaveBeenCalled();
    expect(files).not.toHaveBeenCalled();

    // Reopening is what recomputes it — lazily, against the head that is
    // live by then, so the closed view is never a stale-render hole.
    const reopened = permitPRConflictReconcile(KEY);
    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub({ headSHA: 'sha-b' }));
    expect(recompute).toHaveBeenCalledTimes(1);
    expect(peekPRConflicts(KEY).state?.treeOID).toBe('tree-2');
    reopened();
    a.release();
  });

  it('drops conflict state when the last holder leaves', async () => {
    installSubscribeMock();
    installConflictMocks();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    await openPRConflicts(KEY, CONFLICT_WS, REF, detailStub());
    expect(peekPRConflicts(KEY).state).not.toBeNull();

    a.release();
    await flush();
    expect(peekPRConflicts(KEY).state).toBeNull();
    expect(peekPRConflicts(KEY).contentByPath.size).toBe(0);
  });
});

describe('prReviewStore — visibility and transport transitions', () => {
  function setDocumentVisibility(value: DocumentVisibilityState): void {
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => value,
    });
    document.dispatchEvent(new Event('visibilitychange'));
  }

  it('votes the shared pump down while hidden and back up on visible, once per PR', async () => {
    installSubscribeMock();
    const setActive = setBindingMock('SetPRUpdatesActive', async () => undefined);
    const a = attachPR(KEY, { ref: REF });
    const b = attachPR(KEY, { ref: REF });
    await flush();

    setDocumentVisibility('hidden');
    // One subscription serves both holders, so one vote — not one per pane.
    expect(setActive).toHaveBeenCalledTimes(1);
    expect(setActive).toHaveBeenCalledWith('sub-1', false);
    // Votes are serialized per subscription, so the next one leaves only
    // once this one has answered (see the out-of-order test below).
    await flush();

    setDocumentVisibility('visible');
    await flush();
    expect(setActive).toHaveBeenLastCalledWith('sub-1', true);
    expect(setActive).toHaveBeenCalledTimes(2);

    a.release();
    b.release();
    await flush();
    setActive.mockClear();
    // A released subscription is gone from the backend; flipping it would
    // be a call against a dead id.
    setDocumentVisibility('hidden');
    expect(setActive).not.toHaveBeenCalled();
    setDocumentVisibility('visible');
    delete (document as unknown as Record<string, unknown>).visibilityState;
  });

  // The votes are fire-and-forget RPCs and the transport dispatches them
  // concurrently, so a hidden→visible flurry could complete out of order and
  // leave `false` as the pump's last word for a client that is on screen —
  // a review pane that silently stops updating until the next flip.
  it('serializes visibility votes, so a flurry cannot pause a visible client', async () => {
    installSubscribeMock();
    const inFlight: Array<{ active: boolean; resolve: () => void }> = [];
    const setActive = setBindingMock('SetPRUpdatesActive', (_id: unknown, active: unknown) =>
      new Promise<void>((resolve) => {
        inFlight.push({ active: Boolean(active), resolve });
      }),
    );
    const a = attachPR(KEY, { ref: REF });
    await flush();
    expect(inFlight).toHaveLength(0);

    setDocumentVisibility('hidden');
    expect(inFlight.map((call) => call.active)).toEqual([false]);

    // The flurry lands while the first vote is still on the wire.
    setDocumentVisibility('visible');
    setDocumentVisibility('hidden');
    setDocumentVisibility('visible');
    expect(inFlight).toHaveLength(1);

    inFlight[0].resolve();
    await flush();
    // One trailing send carrying the LATEST state — not one per flip, and
    // never an older value landing last.
    expect(inFlight.map((call) => call.active)).toEqual([false, true]);
    inFlight[1].resolve();
    await flush();
    expect(setActive).toHaveBeenCalledTimes(2);

    a.release();
    await flush();
    delete (document as unknown as Record<string, unknown>).visibilityState;
  });

  it('fails a parked ready() on disconnect instead of hanging it for the outage', async () => {
    let resolveSubscribe!: (value: unknown) => void;
    setBindingMock('SubscribePRUpdates', () => new Promise((resolve) => {
      resolveSubscribe = resolve;
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);

    const a = attachPR(KEY, { ref: REF });
    const parked = a.ready();
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    await flush();
    // Nothing will source over a dead socket, so a waiter left parked is a
    // spinner for the whole outage next to a banner already explaining it.
    await expect(parked).rejects.toThrow('Disconnected');

    // Settle the abandoned subscribe, then reconnect: the load a pane
    // retries must succeed, so the failure cannot be sticky.
    resolveSubscribe({ id: 'sub-dead', prKey: KEY, detail: detailStub(), threads: [], headSHA: 'sha-a' });
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-fresh',
      prKey: KEY,
      detail: detailStub({ headSHA: 'sha-z' }),
      threads: [],
      headSHA: 'sha-z',
    }));
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect((await a.ready()).headSHA).toBe('sha-z');
    a.release();
    await flush();
  });

  it('re-subscribes a held PR on reconnect and stops retrying while disconnected', async () => {
    const { subscribe, unsubscribe } = installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    expect(subscribe).toHaveBeenCalledTimes(1);

    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    await flush();

    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    // The dropped connection's cleanup released every subscription
    // server-side, so a held key MUST re-acquire rather than sit on a dead
    // id waiting for a push that can never come.
    expect(subscribe).toHaveBeenCalledTimes(2);
    expect(a.snapshot?.detail?.number).toBe(5);

    a.release();
    await flush();
    expect(unsubscribe).toHaveBeenCalled();
  });
});

// `pr:updated` only fires when the pump observes a CHANGED snapshot, so a
// frame dropped mid-connection (wsClient's forward-seq-skip detection) is
// never followed by a corrective one. Recovery is blanket because the gap
// carries no PR key — and it must not blank the pane on the way.
describe('prReviewStore — transport gap', () => {
  const OTHER_REF: PRRef = { forge: 'github', namespace: 'owner', repo: 'repo', number: 9 };
  const OTHER_KEY = prKey(OTHER_REF);
  const keyForNumber = (n: number): string =>
    prKey(n === OTHER_REF.number ? OTHER_REF : REF);

  it('re-sources every live PR and keeps the last snapshot while it reloads', async () => {
    const subscribe = setBindingMock('SubscribePRUpdates', async (wire: { Number: number }) => ({
      id: `sub-${wire.Number}`,
      prKey: keyForNumber(wire.Number),
      detail: detailStub({ number: wire.Number }),
      threads: [],
      headSHA: 'sha-a',
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);

    const a = attachPR(KEY, { ref: REF });
    const b = attachPR(OTHER_KEY, { ref: OTHER_REF });
    await flush();
    expect(subscribe).toHaveBeenCalledTimes(2);
    expect(prReviewKeys().sort()).toEqual([KEY, OTHER_KEY].sort());

    // Gate the re-subscribe so the assertions below run while the fresh
    // snapshot is still in flight — the window a blanking recovery would
    // show as an empty review pane.
    let openGate = (): void => {};
    const gate = new Promise<void>((resolve) => {
      openGate = resolve;
    });
    const resubscribe = setBindingMock('SubscribePRUpdates', async (wire: { Number: number }) => {
      await gate;
      return {
        id: `re-${wire.Number}`,
        prKey: keyForNumber(wire.Number),
        detail: detailStub({ number: wire.Number, headSHA: 'sha-fresh' }),
        threads: [],
        headSHA: 'sha-fresh',
      };
    });

    applyTransportGap({ channel: 'pr:updated', seq: 31 });
    await flush();

    expect(resubscribe).toHaveBeenCalledTimes(2);
    expect(a.snapshot?.headSHA).toBe('sha-a');
    expect(b.snapshot?.headSHA).toBe('sha-a');

    openGate();
    await flush();
    expect(a.snapshot?.headSHA).toBe('sha-fresh');
    expect(b.snapshot?.headSHA).toBe('sha-fresh');

    a.release();
    b.release();
    await flush();
  });

  it('ignores a gap on a channel this store does not own', async () => {
    const { subscribe } = installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    expect(subscribe).toHaveBeenCalledTimes(1);

    applyTransportGap({ channel: 'system:stats', seq: 4 });
    await flush();
    expect(subscribe).toHaveBeenCalledTimes(1);

    a.release();
    await flush();
  });
});

describe('prReviewStore — resolve overrides', () => {
  function resolved(id: string): ReviewThread {
    return { ...threadStub(id), isResolved: true };
  }

  it('projects the optimistic state over a stale snapshot until one agrees', async () => {
    installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    applyPRThreads(KEY, [threadStub('t-1'), threadStub('t-2')]);

    // No overrides: the projection is identity-stable (re-anchoring keys
    // off prThreads identity, so a fresh array here would thrash it).
    const before = a.snapshot!.threads;
    expect(overriddenPRThreads(KEY, before)).toBe(before);

    setPRThreadResolveOverride(KEY, 't-1', true);
    const projected = overriddenPRThreads(KEY, a.snapshot!.threads);
    expect(projected.find((t) => t.id === 't-1')?.isResolved).toBe(true);
    expect(projected.find((t) => t.id === 't-2')?.isResolved).toBe(false);

    // A snapshot fetched BEFORE the mutation landed still says false —
    // the override wins (no flap) and survives the apply.
    applyPRThreads(KEY, [threadStub('t-1'), threadStub('t-2')]);
    const afterStale = overriddenPRThreads(KEY, a.snapshot!.threads);
    expect(afterStale.find((t) => t.id === 't-1')?.isResolved).toBe(true);

    // A snapshot that AGREES retires the override: a later genuine
    // unresolve (someone reopened it on the forge) must flow through.
    applyPRThreads(KEY, [resolved('t-1'), threadStub('t-2')]);
    applyPRThreads(KEY, [threadStub('t-1'), threadStub('t-2')]);
    const afterReopen = overriddenPRThreads(KEY, a.snapshot!.threads);
    expect(afterReopen.find((t) => t.id === 't-1')?.isResolved).toBe(false);

    a.release();
    await flush();
  });

  it('clears on RPC failure and for threads that left the snapshot', async () => {
    installSubscribeMock();
    const a = attachPR(KEY, { ref: REF });
    await flush();
    applyPRThreads(KEY, [threadStub('t-1'), threadStub('t-2')]);

    // Failure revert: the forge state stands.
    setPRThreadResolveOverride(KEY, 't-1', true);
    clearPRThreadResolveOverride(KEY, 't-1');
    const reverted = overriddenPRThreads(KEY, a.snapshot!.threads);
    expect(reverted.find((t) => t.id === 't-1')?.isResolved).toBe(false);

    // A thread deleted on the forge has nothing left to override; its
    // entry must not linger for the PR's lifetime.
    setPRThreadResolveOverride(KEY, 't-2', true);
    applyPRThreads(KEY, [threadStub('t-1')]);
    applyPRThreads(KEY, [threadStub('t-1'), threadStub('t-2')]);
    const afterReturn = overriddenPRThreads(KEY, a.snapshot!.threads);
    expect(afterReturn.find((t) => t.id === 't-2')?.isResolved).toBe(false);

    a.release();
    await flush();
  });
});
