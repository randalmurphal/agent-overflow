// The CI subject state over a repository pipeline (pipelineCI.svelte.ts)
// and over a PR's head pipeline: the subscription's lifecycle and routing,
// the sequence watermark, follows, log windows, refresh, save, and
// dispose. The review pane's own CI tests cover the PR feed end to end.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { ciKeyFor, createCISubjectState, type CISubject } from './ciSubject.svelte';
import {
  __resetPipelineCIForTest,
  applyPipelineFrame,
  applyPipelineLogFrame,
  attachPipelineCI,
  pipelineCIKeys,
  resyncPipelinesAfterGap,
} from './pipelineCI.svelte';
import { __resetPRReviewStoreForTest } from './prReviewStore.svelte';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import type { CIPipeline } from '../types/models';
import type { RepoRef } from '../utils/prReference';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

const REPO: RepoRef = { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'app' };
const REPO_WIRE = { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'app' };
const WIRE_KEY = 'gitlab:group/app:pipeline:77';

function pipeline(status: string, jobStatus = status): CIPipeline {
  return {
    id: '77',
    status,
    ref: 'main',
    sha: 'abc',
    stages: [{ name: 'test', status, jobs: [{ id: '20', name: 'unit', status: jobStatus, logsAvailable: true }] }],
  } as CIPipeline;
}

function wireLog(version: number, lines: number, seq: number) {
  return { version, totalLines: lines, totalBytes: lines * 7, stableLines: 0, sections: [], available: true, downloading: false, error: '', seq };
}

interface Mocks {
  subscribe: ReturnType<typeof vi.fn>;
  unsubscribe: ReturnType<typeof vi.fn>;
  follows: ReturnType<typeof vi.fn>;
  refresh: ReturnType<typeof vi.fn>;
  active: ReturnType<typeof vi.fn>;
  routed: Record<string, string[]>;
}

function installMocks(first: { pipeline: CIPipeline | null; seq: number; error?: string; errorKind?: string } = { pipeline: pipeline('running'), seq: 3 }): Mocks {
  const routed: Record<string, string[]> = {};
  const note = (name: string) => (routed[name] ??= []).push(String(takePinnedBackend() ?? ''));
  let n = 0;
  const subscribe = setBindingMock('SubscribePipeline', async () => {
    note('SubscribePipeline');
    n += 1;
    return { id: `pipe-${n}`, key: WIRE_KEY, pipeline: first.pipeline, error: first.error ?? '', errorKind: first.errorKind ?? '', reserve: false, resumeAt: '', seq: first.seq };
  });
  const unsubscribe = setBindingMock('UnsubscribePipeline', async () => note('UnsubscribePipeline'));
  const follows = setBindingMock('SetPipelineFollows', async (_id: string, jobs: string[]) => {
    note('SetPipelineFollows');
    return { logs: Object.fromEntries(jobs.map((job) => [job, wireLog(1, 3, 4)])) };
  });
  const refresh = setBindingMock('RefreshPipeline', async () => note('RefreshPipeline'));
  const active = setBindingMock('SetPipelineActive', async () => note('SetPipelineActive'));
  return { subscribe, unsubscribe, follows, refresh, active, routed };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

function pipelineSubject(backend = HOME_BACKEND): CISubject {
  return { kind: 'pipeline', backend, repo: REPO, pipelineId: '77' };
}

beforeEach(() => {
  __resetPipelineCIForTest();
});

afterEach(() => {
  __resetPipelineCIForTest();
  __resetPRReviewStoreForTest();
  resetStagedBackends();
  delete (document as unknown as Record<string, unknown>).visibilityState;
});

describe('ciKeyFor', () => {
  it('keys a PR by its computer PR key and a pipeline by its repository and id', () => {
    expect(ciKeyFor({ kind: 'pr', backend: HOME_BACKEND, pr: { ...REPO, number: 4 } })).toBe(' gitlab:group/app:4');
    expect(ciKeyFor(pipelineSubject('laptop'))).toBe('laptop pipeline:gitlab:group/app:77');
  });
});

describe('a pipeline subject', () => {
  it('subscribes once however many hold it, reads its stages, and unsubscribes with the last release', async () => {
    const mocks = installMocks();
    const a = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const b = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    expect(state.loading).toBe(true);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    expect(mocks.subscribe).toHaveBeenCalledWith(REPO_WIRE, '77');
    expect(state.loading).toBe(false);
    expect(state.pipeline?.status).toBe('running');
    a.release();
    await flush();
    expect(mocks.unsubscribe).not.toHaveBeenCalled();
    b.release();
    await flush();
    expect(mocks.unsubscribe).toHaveBeenCalledWith('pipe-1');
    expect(pipelineCIKeys()).toEqual([]);
    expect(state.pipeline).toBeNull();
  });

  it('pins every call to the computer that answered the subscribe', async () => {
    stageBackend();
    const mocks = installMocks();
    const held = attachPipelineCI('laptop', REPO, '77');
    const state = createCISubjectState(() => pipelineSubject('laptop'));
    await flush();
    state.openJobLog('test', pipeline('running').stages[0]!.jobs[0]!);
    await flush();
    await state.refresh();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    state.dispose();
    held.release();
    await flush();
    for (const name of ['SubscribePipeline', 'SetPipelineFollows', 'RefreshPipeline', 'SetPipelineActive', 'UnsubscribePipeline']) {
      expect(mocks.routed[name], name).toEqual(expect.arrayContaining(['laptop']));
      expect(new Set(mocks.routed[name]), name).toEqual(new Set(['laptop']));
    }
  });

  it('applies a newer frame, drops an older one, and shows an error frame over the pipeline', async () => {
    installMocks();
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    await flush();
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('failed'), seq: 2 }, HOME_BACKEND);
    expect(state.pipeline?.status).toBe('running');
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('success'), seq: 5 }, HOME_BACKEND);
    expect(state.pipeline?.status).toBe('success');
    applyPipelineFrame({ key: WIRE_KEY, error: 'GitLab rate limit', errorKind: 'rate_limited', resumeAt: '2026-10-20T00:00:00Z', seq: 6 }, HOME_BACKEND);
    expect(state.pipeline?.status).toBe('success');
    expect(state.error).toBe('GitLab rate limit');
    expect(state.failure?.kind).toBe('rate_limited');
    // Another computer's pipeline of the same key is not this one.
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('canceled'), seq: 9 }, 'laptop');
    expect(state.pipeline?.status).toBe('success');
    held.release();
  });

  it('keeps a frame that landed during the subscribe over its older answer', async () => {
    let answer!: (value: unknown) => void;
    setBindingMock('SubscribePipeline', () => new Promise((resolve) => {
      answer = resolve;
    }));
    setBindingMock('UnsubscribePipeline', async () => undefined);
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    await flush();
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('success'), seq: 8 }, HOME_BACKEND);
    answer({ id: 'pipe-1', key: WIRE_KEY, pipeline: pipeline('running'), error: '', errorKind: '', reserve: false, resumeAt: '', seq: 7 });
    await flush();
    expect(state.pipeline?.status).toBe('success');
    held.release();
  });

  it('follows the opened job, applies its log frames, reads its lines from the repository, and stops following on close', async () => {
    const mocks = installMocks();
    const reads = setBindingMock('GetPRCILogLines', async () => ({ from: 1, lines: ['b', 'c'], totalLines: 3, version: 1 }));
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    await flush();
    state.openJobLog('test', pipeline('running').stages[0]!.jobs[0]!);
    await flush();
    expect(mocks.follows).toHaveBeenLastCalledWith('pipe-1', ['20']);
    expect(state.logView).toMatchObject({ stageName: 'test', jobId: '20' });
    expect(state.log).toMatchObject({ version: 1, totalLines: 3 });

    applyPipelineLogFrame({ prKey: '', pipelineKey: WIRE_KEY, jobId: '20', ...wireLog(2, 5, 9) }, HOME_BACKEND);
    expect(state.log).toMatchObject({ version: 2, totalLines: 5 });
    // The job's row follows the pipeline while the log is open.
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('failed'), seq: 10 }, HOME_BACKEND);
    expect(state.logView?.job.status).toBe('failed');

    expect((await state.readLines(1, 2)).lines).toEqual(['b', 'c']);
    expect(reads).toHaveBeenLastCalledWith(REPO_WIRE, '20', 1, 2);

    state.refreshLog();
    await flush();
    expect(mocks.follows).toHaveBeenCalledTimes(2);

    state.closeLogView();
    await flush();
    expect(mocks.follows).toHaveBeenLastCalledWith('pipe-1', []);
    expect(state.logView).toBeNull();
    held.release();
  });

  it('refreshes through its handle and shows a refresh failure until the next frame', async () => {
    const mocks = installMocks();
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    await flush();
    mocks.refresh.mockImplementationOnce(async () => {
      throw new Error('pipeline gone');
    });
    await state.refresh();
    expect(mocks.refresh).toHaveBeenCalledWith('pipe-1');
    expect(state.error).toBe('pipeline gone');
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('success'), seq: 4 }, HOME_BACKEND);
    expect(state.error).toBeNull();
    held.release();
  });

  it('saves the job log and a section by the repository', async () => {
    installMocks();
    const save = setBindingMock('SavePRCIJobLog', async () => '/ci-logs/unit.log');
    const saveSection = setBindingMock('SavePRCIJobLogSection', async () => '/ci-logs/unit-step.log');
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    await flush();
    state.openJobLog('test', pipeline('running').stages[0]!.jobs[0]!);
    expect(await state.saveLog()).toBe('/ci-logs/unit.log');
    expect(save).toHaveBeenCalledWith(REPO_WIRE, '20', 'unit');
    expect(state.logSavedPath).toBe('/ci-logs/unit.log');
    const section = { key: 'section:a', name: 'a', status: 'done', start: 0, end: 1, lines: 1, bytes: 10 };
    expect(await state.saveSection(section)).toBe('/ci-logs/unit-step.log');
    expect(saveSection).toHaveBeenCalledWith(REPO_WIRE, '20', 'unit', 'section:a');
    save.mockImplementationOnce(async () => {
      throw new Error('disk full');
    });
    expect(await state.saveLog()).toBeNull();
    expect(state.logError).toBe('disk full');
    state.dispose();
    held.release();
  });

  it('subscribes again after a reconnect and after a gap, restating its follows', async () => {
    const mocks = installMocks();
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    await flush();
    state.openJobLog('test', pipeline('running').stages[0]!.jobs[0]!);
    await flush();
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('failed'), seq: 50 }, HOME_BACKEND);
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(2);
    expect(mocks.follows).toHaveBeenLastCalledWith('pipe-2', ['20']);
    resyncPipelinesAfterGap(HOME_BACKEND);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(3);
    expect(mocks.follows).toHaveBeenLastCalledWith('pipe-3', ['20']);
    // A restarted backend's pump numbers its frames afresh.
    expect(state.pipeline?.status).toBe('running');
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline('success'), seq: 4 }, HOME_BACKEND);
    expect(state.pipeline?.status).toBe('success');
    state.dispose();
    held.release();
  });

  it('votes its pump down while the document is hidden', async () => {
    const mocks = installMocks();
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    await flush();
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    document.dispatchEvent(new Event('visibilitychange'));
    await flush();
    expect(mocks.active).toHaveBeenLastCalledWith('pipe-1', false);
    held.release();
  });

  it('shows a failed first read with its kind', async () => {
    installMocks({ pipeline: null, seq: 2, error: 'No access to group/app with this login.', errorKind: 'forbidden' });
    const held = attachPipelineCI(HOME_BACKEND, REPO, '77');
    const state = createCISubjectState(() => pipelineSubject());
    await flush();
    expect(state.pipeline).toBeNull();
    expect(state.error).toBe('No access to group/app with this login.');
    expect(state.failure?.kind).toBe('forbidden');
    held.release();
  });
});
