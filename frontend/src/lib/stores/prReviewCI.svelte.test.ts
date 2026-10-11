import { beforeEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import { applyPRCILogEvent, applyPRCIUpdatedEvent, applyPRUpdatedEvent, attachPR } from './prReviewStore.svelte';
import { peekPRCI, refreshPRCI, type PRCILogEvent } from './prReviewCI.svelte';
import {
  prCILogFollowPending,
  refreshPRCILogFollows,
  setPRCILogFollow,
} from './prReviewCIFollows.svelte';
import { applyTransportGap } from './eventsTransportGap';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { prKey, type PRRef } from '../utils/prReference';
import type { CIPipeline, PRDetail } from '../types/models';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';
import { DisconnectedError } from '../transport/wsClient';

const REF: PRRef = { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'repo', number: 5 };
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
    url: 'https://gitlab.com/group/repo/-/merge_requests/5',
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

function pipeline(status: string): CIPipeline {
  return {
    status,
    stages: [{ name: 'test', status, jobs: [{ id: '20', name: 'unit', status, logsAvailable: true }] }],
  };
}

interface SubscribeResult {
  id?: string;
  seq?: number;
  ci?: CIPipeline | null;
  ciError?: string;
  headSHA?: string;
}

function subscribeResult(result: SubscribeResult = {}) {
  const headSHA = result.headSHA ?? 'sha-a';
  return {
    id: result.id ?? 'sub-1',
    prKey: KEY,
    detail: detailStub({ headSHA }),
    threads: [],
    headSHA,
    error: '',
    seq: result.seq ?? 1,
    ci: result.ci ?? null,
    ciError: result.ciError ?? '',
  };
}

function installSubscribe(result: SubscribeResult = {}) {
  const subscribe = setBindingMock('SubscribePRUpdates', async () => subscribeResult(result));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  return subscribe;
}

function gatedSubscribe(result: SubscribeResult) {
  let land!: () => void;
  setBindingMock('SubscribePRUpdates', () => new Promise((resolve) => {
    land = () => resolve(subscribeResult(result));
  }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  return () => land();
}

function logFrame(frame: Partial<PRCILogEvent> & { seq: number }): PRCILogEvent {
  return {
    prKey: KEY,
    jobId: '20',
    version: 1,
    totalLines: 1,
    totalBytes: 6,
    stableLines: 0,
    sections: [],
    available: true,
    downloading: false,
    error: '',
    ...frame,
  };
}

/** The wire state of a log of `version` with `lines` lines. */
function wireLog(version: number, lines: number, seq: number, overrides: Record<string, unknown> = {}) {
  return {
    version, totalLines: lines, totalBytes: lines * 6, stableLines: 0, sections: [],
    available: true, downloading: false, error: '', seq, ...overrides,
  };
}

/** Answers every follow call with version 1 of a `lines`-line log at `seq`. */
function installFollows(lines = 1, seq = 5) {
  return setBindingMock('SetPRCILogFollows', async (_id: string, jobs: string[]) => ({
    logs: Object.fromEntries(jobs.map((job) => [job, wireLog(1, lines, seq)])),
  }));
}

/** A follow mock whose calls wait until the returned release runs. */
function gatedFollows(lines = 1, seq = 5) {
  const gates: (() => void)[] = [];
  const follows = setBindingMock('SetPRCILogFollows', (_id: string, jobs: string[]) => new Promise((resolve) => {
    gates.push(() => resolve({ logs: Object.fromEntries(jobs.map((job) => [job, wireLog(1, lines, seq)])) }));
  }));
  return { follows, release: () => gates.shift()?.() };
}

function logLines(jobId = '20'): number | undefined {
  return peekPRCI(KEY).logs.get(jobId)?.totalLines;
}

beforeEach(() => {
  setBindingMock('SetPRUpdatesActive', async () => undefined);
});

describe('prReviewCI: the pipeline rides the subscription', () => {
  it('reads as loading from attach until the first pipeline frame, an empty one included', async () => {
    installSubscribe({ seq: 3, ci: null });
    const a = attachPR(KEY, { ref: REF });
    expect(peekPRCI(KEY).loading).toBe(true);
    await a.ready();
    // The pump's first CI poll has not answered yet.
    expect(peekPRCI(KEY).loading).toBe(true);

    // A PR with no pipeline is an empty pipeline, not "still loading".
    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: { status: '', stages: [] }, seq: 4 });
    expect(peekPRCI(KEY).loading).toBe(false);
    expect(peekPRCI(KEY).pipeline?.stages).toEqual([]);
    a.release();
    await flush();
  });

  it('seeds the pipeline and the active CI failure from the subscribe result', async () => {
    installSubscribe({ seq: 3, ci: pipeline('failed'), ciError: 'failed to list CI jobs (id: abc)' });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();
    expect(peekPRCI(KEY).pipeline?.status).toBe('failed');
    expect(peekPRCI(KEY).error).toBe('failed to list CI jobs (id: abc)');
    expect(peekPRCI(KEY).loading).toBe(false);
    a.release();
    await flush();
  });

  it('keeps the pipeline under an error frame and clears the error on the next pipeline', async () => {
    installSubscribe({ seq: 3, ci: pipeline('running') });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();

    applyPRCIUpdatedEvent({ prKey: KEY, error: 'failed to list CI jobs (id: abc)', seq: 4 });
    expect(peekPRCI(KEY).error).toContain('failed to list CI jobs');
    expect(peekPRCI(KEY).pipeline?.status).toBe('running');

    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('success'), seq: 5 });
    expect(peekPRCI(KEY).error).toBeNull();
    expect(peekPRCI(KEY).pipeline?.status).toBe('success');
    a.release();
    await flush();
  });

  it('ranks CI frames against the watermark pr:updated frames share', async () => {
    installSubscribe({ seq: 7, ci: pipeline('running') });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();

    // Already in the subscribe result.
    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('stale'), seq: 6 });
    expect(peekPRCI(KEY).pipeline?.status).toBe('running');

    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('failed'), seq: 8 });
    expect(peekPRCI(KEY).pipeline?.status).toBe('failed');

    // One pump sequence: a snapshot frame at or below the CI frame's seq is
    // older than what the key already shows.
    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub({ headSHA: 'sha-old' }), threads: [], headSHA: 'sha-old', seq: 8 });
    expect(a.snapshot?.headSHA).toBe('sha-a');
    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b', seq: 9 });
    expect(a.snapshot?.headSHA).toBe('sha-b');
    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('stale'), seq: 9 });
    expect(peekPRCI(KEY).pipeline?.status).toBe('failed');
    a.release();
    await flush();
  });

  it('replays CI and snapshot frames a join missed in sequence order', async () => {
    const land = gatedSubscribe({ seq: 7, ci: null });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('running'), seq: 8 });
    applyPRUpdatedEvent({ prKey: KEY, detail: detailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b', seq: 9 });
    applyPRCIUpdatedEvent({ prKey: KEY, error: 'failed to list CI jobs (id: abc)', seq: 10 });

    land();
    await flush();

    expect(a.snapshot?.headSHA).toBe('sha-b');
    // Neither replayed after the snapshot (the shared watermark would refuse
    // seq 8) nor lost under the error frame that followed it.
    expect(peekPRCI(KEY).pipeline?.status).toBe('running');
    expect(peekPRCI(KEY).error).toContain('failed to list CI jobs');
    a.release();
    await flush();
  });

  it('drops a buffered CI error a later buffered pipeline superseded', async () => {
    const land = gatedSubscribe({ seq: 7, ci: null });
    const a = attachPR(KEY, { ref: REF });
    await flush();

    applyPRCIUpdatedEvent({ prKey: KEY, error: 'failed to list CI jobs (id: abc)', seq: 8 });
    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('success'), seq: 9 });
    land();
    await flush();

    expect(peekPRCI(KEY).pipeline?.status).toBe('success');
    expect(peekPRCI(KEY).error).toBeNull();
    a.release();
    await flush();
  });

  it('re-sources on a pr:ci_updated gap and keeps the held pipeline until the new pump reports', async () => {
    const subscribe = installSubscribe({ seq: 3, ci: pipeline('running') });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();

    const resubscribe = installSubscribe({ id: 'sub-2', seq: 4, ci: null });
    applyTransportGap({ channel: 'pr:ci_updated', seq: 12 });
    await flush();

    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(resubscribe).toHaveBeenCalledTimes(1);
    expect(peekPRCI(KEY).pipeline?.status).toBe('running');
    expect(peekPRCI(KEY).loading).toBe(false);
    a.release();
    await flush();
  });

  it('drops CI state when the last holder leaves, and a late frame resurrects nothing', async () => {
    installSubscribe({ seq: 3, ci: pipeline('running') });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();
    a.release();
    await flush();

    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('failed'), seq: 9 });
    expect(peekPRCI(KEY).pipeline).toBeNull();
    expect(peekPRCI(KEY).loading).toBe(false);
  });
});

describe('prReviewCI: manual refresh', () => {
  it('spins without hiding the pipeline and shows a failure until the next refresh succeeds', async () => {
    installSubscribe({ seq: 3, ci: pipeline('running') });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();

    let fail!: (err: unknown) => void;
    const refresh = setBindingMock('RefreshPRCI', () => new Promise((_resolve, reject) => {
      fail = reject;
    }));
    const first = refreshPRCI(KEY);
    expect(refresh).toHaveBeenCalledWith('sub-1');
    expect(peekPRCI(KEY).refreshing).toBe(true);
    expect(peekPRCI(KEY).loading).toBe(false);
    expect(peekPRCI(KEY).pipeline?.status).toBe('running');

    fail(new Error('failed to list CI jobs (id: abc)'));
    await first;
    expect(peekPRCI(KEY).refreshing).toBe(false);
    expect(peekPRCI(KEY).error).toBe('failed to list CI jobs (id: abc)');

    // The pump emits only on change: an unchanged pipeline sends no frame,
    // so the refresh that succeeded is what clears its own failure.
    setBindingMock('RefreshPRCI', async () => undefined);
    await refreshPRCI(KEY);
    expect(peekPRCI(KEY).error).toBeNull();
    a.release();
    await flush();
  });

  it('clears a refresh failure when a pipeline frame lands', async () => {
    installSubscribe({ seq: 3, ci: pipeline('running') });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();
    setBindingMock('RefreshPRCI', async () => {
      throw new Error('failed to list CI jobs (id: abc)');
    });
    await refreshPRCI(KEY);
    expect(peekPRCI(KEY).error).not.toBeNull();

    applyPRCIUpdatedEvent({ prKey: KEY, pipeline: pipeline('success'), seq: 4 });
    expect(peekPRCI(KEY).error).toBeNull();
    a.release();
    await flush();
  });

  it('lets only the latest of overlapping refreshes settle what is shown', async () => {
    installSubscribe({ seq: 3, ci: pipeline('running') });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();
    const settles: { resolve: () => void; reject: (err: unknown) => void }[] = [];
    setBindingMock('RefreshPRCI', () => new Promise<void>((resolve, reject) => {
      settles.push({ resolve, reject });
    }));
    const first = refreshPRCI(KEY);
    const second = refreshPRCI(KEY);

    settles[0]!.reject(new Error('failed to list CI jobs (id: abc)'));
    await first;
    expect(peekPRCI(KEY).refreshing).toBe(true);
    expect(peekPRCI(KEY).error).toBeNull();

    settles[1]!.resolve();
    await second;
    expect(peekPRCI(KEY).refreshing).toBe(false);
    a.release();
    await flush();
  });
});

describe('prReviewCI: followed logs', () => {
  const paneA = Symbol('pane-a');
  const paneB = Symbol('pane-b');
  const paneC = Symbol('pane-c');

  async function attached(result: SubscribeResult = {}) {
    installSubscribe({ seq: 3, ci: pipeline('running'), ...result });
    const a = attachPR(KEY, { ref: REF });
    await a.ready();
    return a;
  }

  it('sends the union of the panes\' jobs and seeds each log from the reply', async () => {
    const follows = installFollows(3, 5);
    const a = await attached();

    setPRCILogFollow(KEY, paneA, '20');
    expect(prCILogFollowPending(KEY)).toBe(true);
    await flush();
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['20']);
    expect(prCILogFollowPending(KEY)).toBe(false);
    expect(logLines()).toBe(3);

    // A second pane on the same job changes nothing the backend holds.
    setPRCILogFollow(KEY, paneB, '20');
    await flush();
    expect(follows).toHaveBeenCalledTimes(1);

    setPRCILogFollow(KEY, paneB, '21');
    await flush();
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['20', '21']);

    setPRCILogFollow(KEY, paneA, null);
    await flush();
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['21']);
    // Nobody here shows job 20 any more: its state is not kept.
    expect(peekPRCI(KEY).logs.has('20')).toBe(false);

    // The last one closing clears the backend's set, or its pump keeps
    // polling a log nobody reads.
    setPRCILogFollow(KEY, paneB, null);
    await flush();
    expect(follows).toHaveBeenLastCalledWith('sub-1', []);
    expect(follows).toHaveBeenCalledTimes(4);
    expect(peekPRCI(KEY).logs.size).toBe(0);
    a.release();
    await flush();
  });

  it('coalesces changes made while a call is in flight into one trailing call', async () => {
    const { follows, release } = gatedFollows();
    const a = await attached();

    setPRCILogFollow(KEY, paneA, '20');
    setPRCILogFollow(KEY, paneB, '21');
    setPRCILogFollow(KEY, paneC, '22');
    await flush();
    expect(follows).toHaveBeenCalledTimes(1);

    release();
    await flush();
    expect(follows).toHaveBeenCalledTimes(2);
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['20', '21', '22']);
    release();
    await flush();
    expect(prCILogFollowPending(KEY)).toBe(false);
    a.release();
    await flush();
  });

  it('applies each newer frame whole and drops one already reflected', async () => {
    installFollows(1, 5);
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();

    const sections = [{ key: 'job', name: 'unit', status: 'running', start: 0, end: 2, lines: 2, bytes: 12 }];
    applyPRCILogEvent(logFrame({ seq: 6, version: 2, totalLines: 2, totalBytes: 12, stableLines: 1, sections }));
    expect(peekPRCI(KEY).logs.get('20')).toMatchObject({ version: 2, totalLines: 2, totalBytes: 12, stableLines: 1, sections });

    // Out of order: already reflected.
    applyPRCILogEvent(logFrame({ seq: 6, version: 9, totalLines: 99 }));
    expect(logLines()).toBe(2);

    applyPRCILogEvent(logFrame({ seq: 8, version: 2, totalLines: 2, error: 'failed to fetch job log (id: x)' }));
    expect(logLines()).toBe(2);
    expect(peekPRCI(KEY).logs.get('20')?.error).toBe('failed to fetch job log (id: x)');
    expect(peekPRCI(KEY).logs.get('20')?.failure).toBeNull();

    // A rate limit carries its kind and resume time beside the index.
    const resumeAt = '2026-10-10T12:30:00Z';
    applyPRCILogEvent(logFrame({ seq: 9, version: 2, totalLines: 2, error: 'failed (id: y)', errorKind: 'rate_limited', resumeAt }));
    expect(peekPRCI(KEY).logs.get('20')?.failure).toEqual({ kind: 'rate_limited', reserve: false, resumeAt });

    applyPRCILogEvent(logFrame({ seq: 10, version: 2, totalLines: 2, available: false, downloading: true }));
    expect(peekPRCI(KEY).logs.get('20')).toMatchObject({ available: false, downloading: true, error: null, failure: null, totalLines: 2 });
    // A frame without rows reads as none.
    applyPRCILogEvent(logFrame({ seq: 11, sections: null }));
    expect(peekPRCI(KEY).logs.get('20')?.sections).toEqual([]);
    a.release();
    await flush();
  });

  it('never asks the backend again for a frame: each one is whole', async () => {
    const { follows, release } = gatedFollows(1, 5);
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    release();
    await flush();
    applyPRCILogEvent(logFrame({ seq: 8, version: 5, totalLines: 40, stableLines: 0 }));
    applyPRCILogEvent(logFrame({ seq: 9, version: 9, totalLines: 45 }));
    await flush();
    expect(follows).toHaveBeenCalledTimes(1);
    expect(logLines()).toBe(45);
    a.release();
    await flush();
  });

  it('keeps a frame that landed before an older reply', async () => {
    const { release } = gatedFollows(1, 6);
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();

    applyPRCILogEvent(logFrame({ seq: 7, version: 2, totalLines: 7 }));
    expect(logLines()).toBe(7);
    release();
    await flush();
    expect(logLines()).toBe(7);
    a.release();
    await flush();
  });

  it('ignores frames for jobs another client follows', async () => {
    const follows = installFollows();
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();

    applyPRCILogEvent(logFrame({ jobId: '99', seq: 9 }));
    await flush();
    expect(peekPRCI(KEY).logs.has('99')).toBe(false);
    expect(follows).toHaveBeenCalledTimes(1);
    a.release();
    await flush();
  });

  it('re-sends a manual refresh even when the set did not change', async () => {
    const follows = installFollows();
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();
    refreshPRCILogFollows(KEY);
    await flush();
    expect(follows).toHaveBeenCalledTimes(2);
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['20']);
    a.release();
    await flush();
  });

  it('restates the follows to the new handle after a re-source and a reconnect', async () => {
    const follows = installFollows();
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['20']);

    installSubscribe({ id: 'sub-2', seq: 4, ci: pipeline('running') });
    applyTransportGap({ channel: 'pr:updated', seq: 30 });
    await flush();
    expect(follows).toHaveBeenLastCalledWith('sub-2', ['20']);

    installSubscribe({ id: 'sub-3', seq: 5, ci: pipeline('running') });
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    await flush();
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(follows).toHaveBeenLastCalledWith('sub-3', ['20']);
    expect(follows).toHaveBeenCalledTimes(3);
    a.release();
    await flush();
  });

  it('defers a follow made before the subscribe lands to the subscribe', async () => {
    const follows = installFollows();
    const land = gatedSubscribe({ seq: 3, ci: null });
    const a = attachPR(KEY, { ref: REF });
    setPRCILogFollow(KEY, paneA, '20');
    await flush();
    expect(follows).not.toHaveBeenCalled();
    land();
    await flush();
    expect(follows).toHaveBeenCalledTimes(1);
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['20']);
    a.release();
    await flush();
  });

  it('re-sends the follows of the gapped backend on a pr:ci_log gap', async () => {
    const follows = installFollows();
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();
    applyTransportGap({ channel: 'pr:ci_log', seq: 14 });
    await flush();
    expect(follows).toHaveBeenCalledTimes(2);
    expect(follows).toHaveBeenLastCalledWith('sub-1', ['20']);
    a.release();
    await flush();
  });

  it('shows a failed follow call on the log it was for', async () => {
    setBindingMock('SetPRCILogFollows', async () => {
      throw new Error('failed to fetch job log (id: x)');
    });
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();
    expect(peekPRCI(KEY).logs.get('20')?.error).toBe('failed to fetch job log (id: x)');
    expect(prCILogFollowPending(KEY)).toBe(false);
    a.release();
    await flush();
  });

  it('forgets the follows when the last holder leaves', async () => {
    const follows = installFollows();
    const a = await attached();
    setPRCILogFollow(KEY, paneA, '20');
    await flush();
    a.release();
    await flush();
    expect(peekPRCI(KEY).logs.size).toBe(0);

    // A new holder's subscription follows nothing: no pane asked it to.
    const b = await attached({ id: 'sub-2' });
    await flush();
    expect(follows).toHaveBeenCalledTimes(1);
    applyPRCILogEvent(logFrame({ seq: 9 }));
    expect(peekPRCI(KEY).logs.size).toBe(0);
    b.release();
    await flush();
  });

  describe('clearing failures', () => {
    const diagnostics = installDiagnosticsCapture();

    it('reports a failed clear nobody can see, unless the wire is gone', async () => {
      installFollows();
      const a = await attached();
      setPRCILogFollow(KEY, paneA, '20');
      await flush();

      setBindingMock('SetPRCILogFollows', async () => {
        throw new DisconnectedError('closed');
      });
      setPRCILogFollow(KEY, paneA, null);
      await flush();
      expect(await diagnostics.messages()).not.toContain('pr ci: clearing log follows failed');

      setPRCILogFollow(KEY, paneA, '20');
      await flush();
      setBindingMock('SetPRCILogFollows', async () => {
        throw new Error('backend refused');
      });
      setPRCILogFollow(KEY, paneA, null);
      await flush();
      const reported = (await diagnostics.all()).filter((r) => r.message === 'pr ci: clearing log follows failed');
      expect(reported).toHaveLength(1);
      expect(reported[0].detail).toContain('backend refused');
      a.release();
      await flush();
    });
  });
});
