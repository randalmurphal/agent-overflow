// The CI actions over a CI subject (ciActions.svelte.ts,
// ciSubjectActions.svelte.ts): which actions the read's flags show, the
// pending rule (held until the applied CI sequence reaches the answer's,
// released on failure, on a new launch, and a second press refused while
// pending), the artifacts download and where its result shows, and the
// log view following a retried job (ciSubject.svelte.ts#findCIJobRetry).

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { tick } from 'svelte';
import { __resetCIActionsForTest, ciJobActionKinds, ciPipelineActionKinds } from './ciActions.svelte';
import { createCISubjectState, findCIJobRetry, type CISubject } from './ciSubject.svelte';
import { ciArtifactRunIds } from './ciSubjectActions.svelte';
import { __resetPipelineCIForTest, applyPipelineFrame, attachPipelineCI } from './pipelineCI.svelte';
import { getToasts, resetToastsForTest } from './toast.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import type { CIJob, CIPipeline } from '../types/models';
import type { RepoRef } from '../utils/prReference';
import { setBindingMock } from '../../test/mocks/bindings-app';

const REPO: RepoRef = { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'app' };
const REPO_WIRE = { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'app' };
const WIRE_KEY = 'gitlab:group/app:pipeline:77';

function job(id: string, status: string, actions: CIJob['actions'] = {}, extra: Partial<CIJob> = {}): CIJob {
  return { id, name: 'unit', status, logsAvailable: true, actions, ...extra };
}

function pipeline(jobs: CIJob[], actions: CIPipeline['actions'] = {}, status = 'failed'): CIPipeline {
  return { id: '77', status, stages: [{ name: 'test', status, jobs }], actions };
}

const FAILED = pipeline([job('20', 'failed', { retry: 'allowed', artifacts: 'allowed' })], { retryFailed: 'allowed' });

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

function subject(): CISubject {
  return { kind: 'pipeline', backend: HOME_BACKEND, repo: REPO, pipelineId: '77' };
}

function installMocks(first: CIPipeline = FAILED) {
  setBindingMock('SubscribePipeline', async () => ({ id: 'pipe-1', key: WIRE_KEY, pipeline: first, error: '', seq: 3 }));
  setBindingMock('UnsubscribePipeline', async () => undefined);
  setBindingMock('RefreshPipeline', async () => undefined);
  setBindingMock('SetPipelineActive', async () => undefined);
  const follows = setBindingMock('SetPipelineFollows', async (_id: string, jobs: string[]) => ({
    logs: Object.fromEntries(jobs.map((id) => [id, { version: 1, totalLines: 1, totalBytes: 2, stableLines: 0, sections: [], available: true, downloading: false, error: '', seq: 4 }])),
  }));
  return { follows };
}

const states: ReturnType<typeof createCISubjectState>[] = [];

async function attached(first: CIPipeline = FAILED) {
  const mocks = installMocks(first);
  const hold = attachPipelineCI(HOME_BACKEND, REPO, '77');
  const state = createCISubjectState(() => subject());
  states.push(state);
  await flush();
  return { ...mocks, hold, state };
}

function hello(launchId: string): void {
  __setTransportHelloForTest({ protocolVersion: 1, capabilities: [], launchId } as never);
}

beforeEach(() => {
  __resetPipelineCIForTest();
  __resetCIActionsForTest();
  resetToastsForTest();
  hello('launch-1');
});

afterEach(() => {
  for (const state of states.splice(0)) state.dispose();
  __resetPipelineCIForTest();
  __resetCIActionsForTest();
  resetToastsForTest();
  __setTransportHelloForTest(null);
});

describe('which actions show', () => {
  it('shows a job action only for an allowed flag on a job with an id', () => {
    expect(ciJobActionKinds(job('1', 'manual', { play: 'allowed', retry: 'allowed', cancel: 'allowed' }))).toEqual(['play_job', 'retry_job', 'cancel_job']);
    expect(ciJobActionKinds(job('1', 'failed', { retry: 'denied' }))).toEqual([]);
    expect(ciJobActionKinds(job('1', 'failed', { retry: '' }))).toEqual([]);
    expect(ciJobActionKinds(job('1', 'failed', {}))).toEqual([]);
    expect(ciJobActionKinds(job('1', 'failed', undefined))).toEqual([]);
    // An external check carries no id and so no action, whatever it says.
    expect(ciJobActionKinds({ name: 'ext', status: 'failed', logsAvailable: false, actions: { retry: 'allowed' } })).toEqual([]);
  });

  it('shows a pipeline action only for an allowed flag', () => {
    expect(ciPipelineActionKinds(pipeline([], { retryFailed: 'allowed', cancel: 'allowed' }))).toEqual(['retry_failed', 'cancel_pipeline']);
    expect(ciPipelineActionKinds(pipeline([], { retryFailed: 'denied', cancel: '' }))).toEqual([]);
    expect(ciPipelineActionKinds(null)).toEqual([]);
  });

  it('reads artifacts from the job on GitLab and the pipeline on GitHub', async () => {
    const { state, hold } = await attached(pipeline([job('20', 'failed', { artifacts: 'allowed' }), job('21', 'success', { artifacts: 'denied' })], { artifacts: '' }));
    const [first, second] = state.pipeline!.stages[0]!.jobs;
    expect(state.actions.jobHasArtifacts(first!)).toBe(true);
    expect(state.actions.jobHasArtifacts(second!)).toBe(false);
    expect(state.actions.pipelineHasArtifacts).toBe(false);
    hold.release();
  });

  it('names the pipeline run, or every run of a PR\'s checks, for a GitHub download', () => {
    expect(ciArtifactRunIds(pipeline([]))).toEqual(['77']);
    const checks: CIPipeline = { status: 'failed', stages: [
      { name: 'CI', status: 'failed', jobs: [job('1', 'failed', {}, { runId: '9' }), job('2', 'success', {}, { runId: '9' })] },
      { name: 'Lint', status: 'success', jobs: [job('3', 'success', {}, { runId: '10' }), { name: 'ext', status: 'success', logsAvailable: false }] },
    ] };
    expect(ciArtifactRunIds(checks)).toEqual(['9', '10']);
  });
});

describe('the pending rule', () => {
  it('holds an action pending until the applied sequence reaches its answer', async () => {
    let answer!: (value: { seq: number }) => void;
    const run = setBindingMock('RunPipelineAction', () => new Promise((resolve) => { answer = resolve; }));
    const { state, hold } = await attached();
    const failed = state.pipeline!.stages[0]!.jobs[0]!;
    const pressed = state.actions.run('retry_job', failed);
    expect(run).toHaveBeenCalledWith('pipe-1', { kind: 'retry_job', jobId: '20' });
    expect(state.actions.pending).toEqual(expect.objectContaining({ kind: 'retry_job', jobId: '20' }));
    expect(state.actions.isPending('retry_job', failed)).toBe(true);
    answer({ seq: 6 });
    await pressed;
    // The answer alone does not settle it: the frame with seq 6 does.
    expect(state.actions.pending).not.toBeNull();
    applyPipelineFrame({ key: WIRE_KEY, pipeline: FAILED, seq: 5 }, HOME_BACKEND);
    expect(state.actions.pending).not.toBeNull();
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline([job('22', 'pending')], {}, 'pending'), seq: 6 }, HOME_BACKEND);
    expect(state.actions.pending).toBeNull();
    hold.release();
  });

  it('settles at once when the poll found the pipeline unchanged', async () => {
    setBindingMock('RunPipelineAction', async () => ({ seq: 3 }));
    const { state, hold } = await attached();
    expect(await state.actions.run('retry_failed', null)).toBe(true);
    expect(state.actions.pending).toBeNull();
    hold.release();
  });

  it('refuses a second press while one is pending on the pipeline', async () => {
    const run = setBindingMock('RunPipelineAction', async () => ({ seq: 9 }));
    const { state, hold } = await attached();
    await state.actions.run('retry_failed', null);
    expect(await state.actions.run('retry_job', state.pipeline!.stages[0]!.jobs[0]!)).toBe(false);
    expect(run).toHaveBeenCalledTimes(1);
    applyPipelineFrame({ key: WIRE_KEY, pipeline: FAILED, seq: 9 }, HOME_BACKEND);
    expect(await state.actions.run('retry_job', state.pipeline!.stages[0]!.jobs[0]!)).toBe(true);
    expect(run).toHaveBeenCalledTimes(2);
    hold.release();
  });

  it('releases a failed action and toasts its sentence with Details', async () => {
    setBindingMock('RunPipelineAction', async () => {
      throw Object.assign(new Error('This login may not retry unit in group/app.'), { code: 'forge_forbidden' });
    });
    const { state, hold } = await attached();
    expect(await state.actions.run('retry_job', state.pipeline!.stages[0]!.jobs[0]!)).toBe(false);
    expect(state.actions.pending).toBeNull();
    const toast = getToasts().at(-1)!;
    expect(toast.type).toBe('error');
    expect(toast.message).toBe('This login may not retry unit in group/app.');
    expect(toast.captured).toBeTruthy();
    hold.release();
  });

  it('gives the wait up when the backend restarts', async () => {
    setBindingMock('RunPipelineAction', async () => ({ seq: 40 }));
    const { state, hold } = await attached();
    await state.actions.run('cancel_pipeline', null);
    expect(state.actions.pending?.kind).toBe('cancel_pipeline');
    hello('launch-2');
    expect(state.actions.pending).toBeNull();
    hold.release();
  });

  it('drops the pending action with the pipeline', async () => {
    setBindingMock('RunPipelineAction', async () => ({ seq: 40 }));
    const { state, hold } = await attached();
    await state.actions.run('cancel_pipeline', null);
    hold.release();
    await flush();
    expect(state.actions.pending).toBeNull();
  });
});

describe('artifacts', () => {
  it('downloads a job\'s archive, shows the save on its open log, and a toast elsewhere', async () => {
    let answer!: (value: unknown) => void;
    const save = setBindingMock('SaveCIArtifacts', () => new Promise((resolve) => { answer = resolve; }));
    const { state, hold } = await attached();
    const failed = state.pipeline!.stages[0]!.jobs[0]!;
    state.openJobLog('test', failed);
    const saving = state.actions.saveArtifacts(failed);
    expect(save).toHaveBeenCalledWith(REPO_WIRE, { jobId: '20' }, 'unit');
    expect(state.actions.artifactsDownloading(failed)).toBe(true);
    // A second press while it runs starts nothing.
    expect(await state.actions.saveArtifacts(failed)).toBeNull();
    expect(save).toHaveBeenCalledTimes(1);
    answer({ dir: '/data/ci-artifacts', paths: ['/data/ci-artifacts/unit.zip'] });
    await saving;
    expect(state.actions.artifactsDownloading(failed)).toBe(false);
    expect(state.logSavedPath).toBe('/data/ci-artifacts/unit.zip');

    state.closeLogView();
    save.mockImplementation(async () => ({ dir: '/data/ci-artifacts', paths: ['/a.zip', '/b.zip'] }));
    await state.actions.saveArtifacts(null);
    expect(save).toHaveBeenLastCalledWith(REPO_WIRE, { runIds: ['77'] }, '');
    expect(getToasts().at(-1)).toEqual(expect.objectContaining({ type: 'info', message: 'Saved to /data/ci-artifacts' }));
    hold.release();
  });

  it('shows a partial save as the failure it is', async () => {
    setBindingMock('SaveCIArtifacts', async () => {
      throw new Error('Saved 1 artifacts to /data/ci-artifacts before the download stopped. The artifacts of unit have expired on gitlab.com.');
    });
    const { state, hold } = await attached();
    expect(await state.actions.saveArtifacts(state.pipeline!.stages[0]!.jobs[0]!)).toBeNull();
    expect(getToasts().at(-1)).toEqual(expect.objectContaining({ type: 'error', message: expect.stringContaining('before the download stopped') }));
    expect(state.logSavedPath).toBeNull();
    hold.release();
  });
});

describe('following a retried job', () => {
  it('finds the newest attempt of the same job in the same stage and run', () => {
    const old = job('20', 'failed', {}, { runId: '5' });
    const read: CIPipeline = { status: 'running', stages: [
      { name: 'test', status: 'running', jobs: [job('21', 'canceled', {}, { runId: '5' }), job('23', 'pending', {}, { runId: '5' }), job('30', 'pending', {}, { runId: '6' }), { ...job('40', 'pending', {}, { runId: '5' }), name: 'lint' }] },
      { name: 'other', status: 'pending', jobs: [job('50', 'pending', {}, { runId: '5' })] },
    ] };
    expect(findCIJobRetry(read, 'test', old)?.job.id).toBe('23');
    // Another run of the same workflow is not a retry.
    expect(findCIJobRetry({ status: 'pending', stages: [{ name: 'test', status: 'pending', jobs: [job('30', 'pending', {}, { runId: '6' })] }] }, 'test', old)).toBeNull();
    // A lower id is an older attempt.
    expect(findCIJobRetry({ status: 'failed', stages: [{ name: 'test', status: 'failed', jobs: [job('19', 'failed', {}, { runId: '5' })] }] }, 'test', old)).toBeNull();
  });

  it('moves the open log to the retry, follows it, and says so', async () => {
    const { state, hold, follows } = await attached();
    state.openJobLog('test', state.pipeline!.stages[0]!.jobs[0]!);
    await flush();
    expect(follows).toHaveBeenLastCalledWith('pipe-1', ['20']);
    expect(state.logRetriedFrom).toBeNull();
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline([job('21', 'pending', { cancel: 'allowed' })], {}, 'pending'), seq: 7 }, HOME_BACKEND);
    await flush();
    expect(state.logView?.jobId).toBe('21');
    expect(state.logView?.job.status).toBe('pending');
    expect(state.logRetriedFrom).toBe('20');
    expect(follows).toHaveBeenLastCalledWith('pipe-1', ['21']);
    // A later frame for the same attempt keeps it.
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline([job('21', 'success')], {}, 'success'), seq: 8 }, HOME_BACKEND);
    expect(state.logView?.job.status).toBe('success');
    expect(state.logRetriedFrom).toBe('20');
    // Opening a job again starts a fresh log with no retry note.
    state.openJobLog('test', state.pipeline!.stages[0]!.jobs[0]!);
    expect(state.logRetriedFrom).toBeNull();
    hold.release();
  });

  it('leaves a log alone once it is closed, and a job whose name another took', async () => {
    const { state, hold, follows } = await attached();
    state.openJobLog('test', state.pipeline!.stages[0]!.jobs[0]!);
    await flush();
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline([{ ...job('21', 'pending'), name: 'lint' }], {}, 'pending'), seq: 7 }, HOME_BACKEND);
    expect(state.logView?.jobId).toBe('20');
    state.closeLogView();
    await flush();
    const calls = follows.mock.calls.length;
    applyPipelineFrame({ key: WIRE_KEY, pipeline: pipeline([job('22', 'pending')], {}, 'pending'), seq: 8 }, HOME_BACKEND);
    await flush();
    expect(state.logView).toBeNull();
    expect(follows.mock.calls.slice(calls)).toEqual([]);
    hold.release();
  });
});
