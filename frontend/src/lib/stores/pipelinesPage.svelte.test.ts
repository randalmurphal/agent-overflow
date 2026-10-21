// The Pipelines tab store: list reads on open, refresh and the interval
// (paused while hidden), one read per repository, the filters, the stage
// summaries for rows on screen, their batching cap, failures and
// retention, per-project failures, the owning computer's connection,
// removal and grant, and the selected pipeline's subscription.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import {
  __pipelineStageKeysForTest,
  __resetPipelinesPageForTest,
  attachPipelinesPage,
  clearPipelineSelection,
  detachPipelinesPage,
  PIPELINE_STAGE_READS_MAX,
  PIPELINES_REFRESH_MS,
  pipelineDetailOpen,
  pipelinesError,
  pipelinesLoading,
  pipelinesProjectErrors,
  pipelinesReading,
  pipelinesRows,
  refreshPipelinesPage,
  selectedPipeline,
  selectPipeline,
  setPipelinesFilters,
  setVisiblePipelines,
} from './pipelinesPage.svelte';
import { DEFAULT_RETRY } from './entityStore.svelte';
import { __resetPipelineCIForTest } from './pipelineCI.svelte';
import { PIPELINE_FILTERS_KEY, resetPullsFiltersForTest } from './pullsPageFilters.svelte';
import { appStorageGet, resetAppStorageForTest } from './appStorage';
import { HOME_BACKEND } from '../transport/backendKey';
import { detachBackend, takePinnedBackend } from '../transport/backends';
import { presentScope } from '../transport/scopeRefusal';
import { TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { grantBackendScopes, revokeBackendScopes } from '../../test/helpers/scopes';

const NOT_GRANTED = `${presentScope('git:operate').title} ${presentScope('git:operate').hint}`;

const PANE = 'tools-pulls';

function forgeProject(id: string, repo: string) {
  return { projectId: id, name: `Project ${id}`, path: `/src/${repo}`, repo: { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: repo } };
}

function summary(id: number, overrides: Record<string, unknown> = {}) {
  return {
    id: String(id),
    status: 'success',
    ref: 'main',
    refKind: 'branch',
    sha: `${id}`.padStart(40, 'a'),
    source: 'push',
    createdAt: new Date(Date.UTC(2026, 9, 1, 0, id)).toISOString(),
    updatedAt: new Date(Date.UTC(2026, 9, 1, 0, id)).toISOString(),
    url: `https://gitlab.com/group/app/-/pipelines/${id}`,
    ...overrides,
  };
}

interface Mocks {
  projects: ReturnType<typeof vi.fn>;
  list: ReturnType<typeof vi.fn>;
  stages: ReturnType<typeof vi.fn>;
  routed: string[];
}

function installMocks(rowsByProject: Record<string, ReturnType<typeof summary>[]> = { p1: [summary(1), summary(3, { refKind: 'tag', ref: 'v1' })], p2: [summary(2)] }): Mocks {
  const routed: string[] = [];
  const projects = setBindingMock('ListForgeProjects', async () => {
    routed.push(String(takePinnedBackend() ?? ''));
    return [forgeProject('p1', 'app'), forgeProject('p2', 'api')];
  });
  const list = setBindingMock('ListProjectPipelines', async (projectId: string) => {
    routed.push(String(takePinnedBackend() ?? ''));
    return { rows: rowsByProject[projectId] ?? [], fetchedAt: '' };
  });
  const stages = setBindingMock('ReadPipelineStages', async (_repo: unknown, ids: string[]) => {
    routed.push(String(takePinnedBackend() ?? ''));
    return Object.fromEntries(ids.map((id) => [id, { stages: [{ name: 'build', status: 'success', jobs: [] }, { name: 'test', status: 'failed', jobs: [] }] }]));
  });
  return { projects, list, stages, routed };
}

async function settle(): Promise<void> {
  await vi.advanceTimersByTimeAsync(0);
}

function setDocumentVisibility(value: DocumentVisibilityState): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value });
  document.dispatchEvent(new Event('visibilitychange'));
}

beforeEach(() => {
  vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
  resetAppStorageForTest();
  resetPullsFiltersForTest();
});

afterEach(() => {
  __resetPipelinesPageForTest();
  __resetPipelineCIForTest();
  revokeBackendScopes('laptop');
  resetStagedBackends();
  delete (document as unknown as Record<string, unknown>).visibilityState;
  vi.useRealTimers();
});

describe('list reads', () => {
  it('lists every project on open, newest first, pinned to the computer the page lists', async () => {
    stageBackend();
    await grantBackendScopes('laptop', ['git:operate']);
    const mocks = installMocks();
    attachPipelinesPage(PANE, 'laptop');
    await settle();
    expect(mocks.list.mock.calls.map((call) => call[0])).toEqual(['p1', 'p2']);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['3', '2', '1']);
    expect(pipelinesRows(PANE)[1]).toMatchObject({ key: 'laptop pipeline:gitlab:group/api:2', projectName: 'Project p2', forge: 'gitlab', shortSHA: 'aaaaaaa', stages: null });
    expect(new Set(mocks.routed)).toEqual(new Set(['laptop']));
  });

  it('attaches and reports its rows from a mount effect without the effect depending on the tab', async () => {
    installMocks();
    setBindingMock('SubscribePipeline', async () => ({ id: 'pipe-1', key: 'gitlab:group/app:pipeline:1', pipeline: null, error: '', errorKind: '', reserve: false, resumeAt: '', seq: 1 }));
    setBindingMock('UnsubscribePipeline', async () => undefined);
    const probe = probeReactivity(() => {
      attachPipelinesPage(PANE, HOME_BACKEND);
      setVisiblePipelines(PANE, []);
      return 0;
    });
    try {
      await settle();
      attachPipelinesPage('tools-other', HOME_BACKEND);
      selectPipeline(PANE, pipelinesRows(PANE)[0]!.key);
      detachPipelinesPage('tools-other');
      flushSync();
      expect(probe.evaluations).toBe(1);
    } finally {
      probe.dispose();
    }
  });

  it('reads again on refresh and every interval while visible, and not while hidden', async () => {
    const mocks = installMocks();
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    expect(mocks.list).toHaveBeenCalledTimes(2);
    refreshPipelinesPage(PANE);
    await settle();
    expect(mocks.list).toHaveBeenCalledTimes(4);
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS);
    expect(mocks.list).toHaveBeenCalledTimes(6);

    setDocumentVisibility('hidden');
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 3);
    expect(mocks.list).toHaveBeenCalledTimes(6);
    // Due while hidden: it reads as the document shows.
    setDocumentVisibility('visible');
    await settle();
    expect(mocks.list).toHaveBeenCalledTimes(8);

    // A refresh asked for while hidden reads once and arms nothing.
    setDocumentVisibility('hidden');
    refreshPipelinesPage(PANE);
    await settle();
    expect(mocks.list).toHaveBeenCalledTimes(10);
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 3);
    expect(mocks.list).toHaveBeenCalledTimes(10);
    setDocumentVisibility('visible');
    await settle();

    detachPipelinesPage(PANE);
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 2);
    expect(mocks.list).toHaveBeenCalledTimes(12);
  });

  it('keeps a failed project apart with its kind while the others list', async () => {
    installMocks();
    setBindingMock('ListProjectPipelines', async (projectId: string) => {
      if (projectId === 'p2') throw new TransportError('forge_forbidden', 'No access to group/api with this login.');
      return { rows: [summary(1)], fetchedAt: '' };
    });
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['1']);
    expect(pipelinesProjectErrors(PANE)).toEqual([{
      projectId: 'p2', name: 'Project p2', message: 'No access to group/api with this login.', failure: { kind: 'forbidden', reserve: false, resumeAt: '' }, forge: 'gitlab',
      cause: expect.objectContaining({ code: 'forge_forbidden' }),
    }]);
  });

  it('reads a repository two projects clone once, lists each pipeline once, and lists it when either project is picked', async () => {
    const mocks = installMocks({ p1: [summary(1), summary(2)], p2: [summary(1), summary(2)] });
    mocks.projects.mockImplementation(async () => [forgeProject('p1', 'app'), forgeProject('p2', 'app')]);
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    expect(mocks.list.mock.calls.map((call) => call[0])).toEqual(['p1']);
    const keys = pipelinesRows(PANE).map((row) => row.key);
    expect(keys).toHaveLength(2);
    expect(new Set(keys).size).toBe(2);
    expect(pipelinesRows(PANE).map((row) => row.projectName)).toEqual(['Project p1', 'Project p1']);

    setPipelinesFilters(PANE, { projects: ['p2'] });
    await settle();
    expect(mocks.list).toHaveBeenLastCalledWith('p2', { status: undefined }, true);
    expect(pipelinesRows(PANE).map((row) => [row.id, row.projectName])).toEqual([['2', 'Project p2'], ['1', 'Project p2']]);

    // A failed read of the repository shows on every picked clone, and
    // the rows read before stay.
    setPipelinesFilters(PANE, { projects: [] });
    await settle();
    mocks.list.mockImplementation(async () => {
      throw new TransportError('forge_forbidden', 'No access to group/app with this login.');
    });
    refreshPipelinesPage(PANE);
    await settle();
    expect(pipelinesProjectErrors(PANE).map((failed) => failed.projectId)).toEqual(['p1', 'p2']);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['2', '1']);
  });

  it('shows a project whose origin could not be read beside the projects that list', async () => {
    const mocks = installMocks();
    const unreadable = { ...forgeProject('p2', 'api'), repo: { Forge: '', Host: '', Namespace: '', Repo: '' }, error: 'The origin of Project p2 could not be read.' };
    mocks.projects.mockImplementation(async () => [forgeProject('p1', 'app'), unreadable]);
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    expect(mocks.list.mock.calls.map((call) => call[0])).toEqual(['p1']);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['3', '1']);
    expect(pipelinesProjectErrors(PANE)).toEqual([{
      projectId: 'p2', name: 'Project p2', message: 'The origin of Project p2 could not be read.', failure: { kind: 'forge', reserve: false, resumeAt: '' }, forge: '', cause: null,
    }]);
    // Picking only the other project drops its header.
    setPipelinesFilters(PANE, { projects: ['p1'] });
    await settle();
    expect(pipelinesProjectErrors(PANE)).toEqual([]);
  });
});

describe('the owning computer', () => {
  it('stops reading on a disconnect, discards the read in flight, and reads at once on the reconnect', async () => {
    const laptop = stageBackend();
    await grantBackendScopes('laptop', ['git:operate']);
    const mocks = installMocks();
    attachPipelinesPage(PANE, 'laptop');
    await settle();
    expect(mocks.projects).toHaveBeenCalledTimes(1);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['3', '2', '1']);

    let answer!: (value: unknown) => void;
    mocks.projects.mockImplementationOnce(() => new Promise((resolve) => {
      answer = resolve;
    }));
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS);
    expect(mocks.projects).toHaveBeenCalledTimes(2);
    expect(pipelinesReading(PANE)).toBe(true);
    laptop.setStatus('disconnected');
    await settle();
    expect(pipelinesReading(PANE)).toBe(false);
    // The read the disconnect cut applies nothing; the rows listed stay.
    answer([forgeProject('p9', 'other')]);
    await settle();
    expect(mocks.list.mock.calls.map((call) => call[0])).toEqual(['p1', 'p2']);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['3', '2', '1']);

    // Nothing reads while it is down: no interval, no stages.
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 3);
    setVisiblePipelines(PANE, pipelinesRows(PANE).map((row) => row.key));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.projects).toHaveBeenCalledTimes(2);
    expect(mocks.stages).not.toHaveBeenCalled();

    laptop.setStatus('connected');
    await settle();
    expect(mocks.projects).toHaveBeenCalledTimes(3);
    expect(mocks.stages).toHaveBeenCalledTimes(2);
  });

  it('waits out a hidden document on a reconnect and a regrant, then reads once as it shows', async () => {
    const laptop = stageBackend();
    await grantBackendScopes('laptop', ['git:operate']);
    const mocks = installMocks();
    attachPipelinesPage(PANE, 'laptop');
    await settle();
    setVisiblePipelines(PANE, pipelinesRows(PANE).map((row) => row.key));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.projects).toHaveBeenCalledTimes(1);
    expect(mocks.stages).toHaveBeenCalledTimes(2);

    setDocumentVisibility('hidden');
    laptop.setStatus('disconnected');
    await settle();
    laptop.setStatus('connected');
    await settle();
    revokeBackendScopes('laptop');
    await settle();
    await grantBackendScopes('laptop', ['git:operate']);
    await settle();
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 3);
    expect(mocks.projects).toHaveBeenCalledTimes(1);
    expect(mocks.list).toHaveBeenCalledTimes(2);
    expect(mocks.stages).toHaveBeenCalledTimes(2);

    setDocumentVisibility('visible');
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.projects).toHaveBeenCalledTimes(2);
    expect(mocks.list).toHaveBeenCalledTimes(4);
    expect(mocks.stages).toHaveBeenCalledTimes(4);

    // Rows reported on screen while hidden read their stages as it shows.
    setDocumentVisibility('hidden');
    setVisiblePipelines(PANE, pipelinesRows(PANE).slice(0, 1).map((row) => row.key));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.stages).toHaveBeenCalledTimes(4);
    setDocumentVisibility('visible');
    await settle();
    expect(mocks.stages).toHaveBeenCalledTimes(5);
    expect(mocks.projects).toHaveBeenCalledTimes(2);
  });

  it('forgets a removed computer while the tab is open, reads nothing for it, and reads again once it is back', async () => {
    stageBackend();
    await grantBackendScopes('laptop', ['git:operate']);
    const mocks = installMocks();
    setBindingMock('SubscribePipeline', async (_repo: unknown, id: string) => ({
      id: `pipe-${id}`, key: `gitlab:group/app:pipeline:${id}`, pipeline: { id, status: 'running', stages: [] }, error: '', errorKind: '', reserve: false, resumeAt: '', seq: 1,
    }));
    const unsubscribe = setBindingMock('UnsubscribePipeline', async () => undefined);
    attachPipelinesPage(PANE, 'laptop');
    await settle();
    selectPipeline(PANE, pipelinesRows(PANE)[0]!.key);
    await settle();

    detachBackend('laptop');
    await settle();
    expect(pipelinesRows(PANE)).toEqual([]);
    expect(selectedPipeline(PANE)).toBeNull();
    expect(unsubscribe).toHaveBeenCalledWith('pipe-3');
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 3);
    expect(mocks.projects).toHaveBeenCalledTimes(1);

    stageBackend();
    await grantBackendScopes('laptop', ['git:operate']);
    await settle();
    expect(mocks.projects).toHaveBeenCalledTimes(2);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['3', '2', '1']);
  });

  it('reads nothing until the computer grants git, stops when it is revoked, and the refresh still shows the refusal', async () => {
    stageBackend();
    const mocks = installMocks();
    const job = { id: '20', name: 'unit', status: 'running', logsAvailable: true };
    const subscribe = setBindingMock('SubscribePipeline', async (_repo: unknown, id: string) => ({
      id: `pipe-${id}`, key: `gitlab:group/app:pipeline:${id}`, pipeline: { id, status: 'running', stages: [{ name: 'test', status: 'running', jobs: [job] }] }, error: '', errorKind: '', reserve: false, resumeAt: '', seq: 1,
    }));
    const unsubscribe = setBindingMock('UnsubscribePipeline', async () => undefined);
    setBindingMock('SetPipelineFollows', async (_id: string, jobs: string[]) => ({
      logs: Object.fromEntries(jobs.map((jobId) => [jobId, { version: 1, totalLines: 1, totalBytes: 8, stableLines: 0, sections: [], available: true, downloading: false, error: '', seq: 2 }])),
    }));
    attachPipelinesPage(PANE, 'laptop');
    await settle();
    expect(mocks.projects).not.toHaveBeenCalled();
    expect(pipelinesError(PANE)).toBe(NOT_GRANTED);
    expect(pipelinesLoading(PANE)).toBe(false);

    // The refresh button asks anyway, shows the refusal, and arms nothing.
    mocks.projects.mockImplementationOnce(async () => {
      throw new TransportError('scope_required', 'refused', { scope: 'git:operate' });
    });
    refreshPipelinesPage(PANE);
    await settle();
    expect(mocks.projects).toHaveBeenCalledTimes(1);
    expect(pipelinesError(PANE)).toBe(NOT_GRANTED);
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 3);
    expect(mocks.projects).toHaveBeenCalledTimes(1);

    await grantBackendScopes('laptop', ['git:operate']);
    await settle();
    expect(mocks.projects).toHaveBeenCalledTimes(2);
    expect(pipelinesError(PANE)).toBeNull();
    selectPipeline(PANE, pipelinesRows(PANE)[0]!.key);
    await settle();
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(selectedPipeline(PANE)?.ci.pipeline?.status).toBe('running');
    selectedPipeline(PANE)!.ci.openJobLog('test', job);
    await settle();
    expect(selectedPipeline(PANE)?.ci.log).toMatchObject({ version: 1 });

    // Revoked: the reads stop, the subscription goes with the open log,
    // and both say why.
    revokeBackendScopes('laptop');
    await settle();
    expect(unsubscribe).toHaveBeenCalledWith('pipe-3');
    expect(selectedPipeline(PANE)?.ci.logView).toBeNull();
    expect(selectedPipeline(PANE)?.ci.error).toBe(NOT_GRANTED);
    expect(pipelinesError(PANE)).toBe(NOT_GRANTED);
    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS * 3);
    expect(mocks.projects).toHaveBeenCalledTimes(2);

    await grantBackendScopes('laptop', ['git:operate']);
    await settle();
    expect(mocks.projects).toHaveBeenCalledTimes(3);
    expect(subscribe).toHaveBeenCalledTimes(2);
    expect(selectedPipeline(PANE)?.ci.error).toBeNull();
    expect(selectedPipeline(PANE)?.ci.pipeline?.status).toBe('running');
  });
});

describe('rate limits', () => {
  it('keeps a background read refused under the reserve as a reserve pause with its release', async () => {
    installMocks();
    setBindingMock('ListProjectPipelines', async (projectId: string) => {
      if (projectId === 'p2') {
        throw new TransportError('forge_rate_limited', 'paused', { reserve: true, resumeAt: '2026-10-21T08:30:00Z' });
      }
      return { rows: [summary(1)], fetchedAt: '' };
    });
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    expect(pipelinesProjectErrors(PANE).map((failed) => failed.failure)).toEqual([
      { kind: 'rate_limited', reserve: true, resumeAt: '2026-10-21T08:30:00Z' },
    ]);
  });
});

describe('filters', () => {
  it('reads again for a status or projects, filters the ref kind in place, and remembers them', async () => {
    const mocks = installMocks();
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    setPipelinesFilters(PANE, { status: 'failed' });
    await settle();
    expect(mocks.list).toHaveBeenLastCalledWith('p2', { status: 'failed' }, true);
    setPipelinesFilters(PANE, { projects: ['p1'] });
    await settle();
    expect(mocks.list).toHaveBeenLastCalledWith('p1', { status: 'failed' }, true);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['3', '1']);
    const calls = mocks.list.mock.calls.length;
    setPipelinesFilters(PANE, { refKind: 'tag' });
    await settle();
    expect(mocks.list.mock.calls.length).toBe(calls);
    expect(pipelinesRows(PANE).map((row) => row.id)).toEqual(['3']);
    expect(JSON.parse(appStorageGet(PIPELINE_FILTERS_KEY, HOME_BACKEND) ?? '{}')).toEqual({ projects: ['p1'], refKind: 'tag', status: 'failed' });
  });
});

describe('stage summaries', () => {
  it('reads the stages of the rows on screen, at most the cap per call, and again with the list', async () => {
    const many = Array.from({ length: PIPELINE_STAGE_READS_MAX + 5 }, (_, index) => summary(index + 1));
    const mocks = installMocks({ p1: many });
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    expect(mocks.stages).not.toHaveBeenCalled();
    setVisiblePipelines(PANE, pipelinesRows(PANE).map((row) => row.key));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.stages).toHaveBeenCalledTimes(1);
    const [repo, ids] = mocks.stages.mock.calls[0]!;
    expect(repo).toEqual({ Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'app' });
    expect(ids).toHaveLength(PIPELINE_STAGE_READS_MAX);
    const rows = pipelinesRows(PANE);
    expect(rows[0]!.stages).toEqual([{ name: 'build', status: 'success' }, { name: 'test', status: 'failed' }]);
    expect(rows.at(-1)!.stages).toBeNull();

    refreshPipelinesPage(PANE);
    await settle();
    expect(mocks.stages).toHaveBeenCalledTimes(2);
  });

  it('shows a failed stage read on its rows in place of their dots, and the dots again once a read answers', async () => {
    const mocks = installMocks({ p1: [summary(1), summary(2)] });
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    const keys = pipelinesRows(PANE).map((row) => row.key);
    const shown = () => pipelinesRows(PANE).map((row) => [row.id, row.stages?.map((stage) => stage.status) ?? null, row.stagesError]);
    setVisiblePipelines(PANE, keys);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(shown()).toEqual([['2', ['success', 'failed'], null], ['1', ['success', 'failed'], null]]);

    // The call fails: every row it read shows the failure, not the old dots.
    mocks.stages.mockImplementationOnce(async () => {
      throw new TransportError('forge_transient', 'GitLab did not answer.');
    });
    setVisiblePipelines(PANE, keys);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(shown()).toEqual([['2', null, 'GitLab did not answer.'], ['1', null, 'GitLab did not answer.']]);

    // One pipeline's own failure is that row's alone.
    mocks.stages.mockImplementationOnce(async () => ({
      2: { stages: [], error: 'Pipeline 2 could not be read.' },
      1: { stages: [{ name: 'build', status: 'running', jobs: [] }] },
    }));
    setVisiblePipelines(PANE, keys);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(shown()).toEqual([['2', null, 'Pipeline 2 could not be read.'], ['1', ['running'], null]]);

    setVisiblePipelines(PANE, keys);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(shown()).toEqual([['2', ['success', 'failed'], null], ['1', ['success', 'failed'], null]]);
  });

  it('holds stages for listed rows only, and drops a stage reply for a list that has moved on', async () => {
    const mocks = installMocks({ p1: [summary(1), summary(2), summary(3)] });
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    setVisiblePipelines(PANE, pipelinesRows(PANE).map((row) => row.key));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(__pipelineStageKeysForTest(PANE)).toHaveLength(3);

    // Every refresh lists other pipelines: what is held is what is listed.
    for (let id = 4; id < 10; id += 1) {
      mocks.list.mockImplementation(async () => ({ rows: [summary(id)], fetchedAt: '' }));
      refreshPipelinesPage(PANE);
      await settle();
      setVisiblePipelines(PANE, pipelinesRows(PANE).map((row) => row.key));
      await vi.advanceTimersByTimeAsync(2_000);
      const listedKeys = pipelinesRows(PANE).map((row) => row.key);
      expect(listedKeys).toHaveLength(2);
      expect(__pipelineStageKeysForTest(PANE).sort()).toEqual(listedKeys.sort());
    }

    // A stage read in flight when the list moves on applies nothing.
    const stale = pipelinesRows(PANE)[0]!.key;
    let answer!: (value: unknown) => void;
    mocks.stages.mockImplementationOnce(() => new Promise((resolve) => {
      answer = resolve;
    }));
    setVisiblePipelines(PANE, [stale]);
    await vi.advanceTimersByTimeAsync(400);
    mocks.list.mockImplementation(async () => ({ rows: [summary(10)], fetchedAt: '' }));
    refreshPipelinesPage(PANE);
    await settle();
    expect(__pipelineStageKeysForTest(PANE)).toEqual([]);
    answer({ 9: { stages: [{ name: 'late', status: 'success', jobs: [] }] } });
    await settle();
    expect(__pipelineStageKeysForTest(PANE)).toEqual([]);
  });
});

describe('read intent', () => {
  it('reads as the user on open, refresh and a filter change, and in the background on the interval and on scroll', async () => {
    const mocks = installMocks();
    const lastList = () => mocks.list.mock.calls.at(-1)![2];
    const lastStages = () => mocks.stages.mock.calls.at(-1)![2];
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    expect(mocks.list.mock.calls.map((call) => call[2])).toEqual([true, true]);
    // The first rows on screen after the open are the open's.
    const keys = pipelinesRows(PANE).map((row) => row.key);
    setVisiblePipelines(PANE, keys.slice(0, 1));
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.stages).toHaveBeenCalledTimes(1);
    expect(lastStages()).toBe(true);
    // Scrolling is not.
    setVisiblePipelines(PANE, keys);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.stages).toHaveBeenCalledTimes(3);
    expect(lastStages()).toBe(false);

    await vi.advanceTimersByTimeAsync(PIPELINES_REFRESH_MS);
    expect(mocks.list).toHaveBeenCalledTimes(4);
    expect(lastList()).toBe(false);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.stages.mock.calls.slice(3).map((call) => call[2])).toEqual([false, false]);

    refreshPipelinesPage(PANE);
    await settle();
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.list.mock.calls.slice(4).map((call) => call[2])).toEqual([true, true]);
    expect(mocks.stages.mock.calls.slice(5).map((call) => call[2])).toEqual([true, true]);

    setPipelinesFilters(PANE, { status: 'failed' });
    await settle();
    expect(lastList()).toBe(true);
  });
});

describe('selection', () => {
  it('shows a failing subscribe on the detail, and clears it once the subscribe answers', async () => {
    installMocks();
    let refuse = true;
    const subscribe = setBindingMock('SubscribePipeline', async (_repo: unknown, id: string) => {
      if (refuse) throw new TransportError('pipeline_subscriptions_full', 'Too many pipelines are followed.');
      return { id: `pipe-${id}`, key: `gitlab:group/app:pipeline:${id}`, pipeline: { id, status: 'running', stages: [] }, error: '', errorKind: '', reserve: false, resumeAt: '', seq: 1 };
    });
    setBindingMock('UnsubscribePipeline', async () => undefined);
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    selectPipeline(PANE, pipelinesRows(PANE).find((row) => row.id === '1')!.key);
    await settle();
    const ci = selectedPipeline(PANE)!.ci;
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(ci.error).toBe('Too many pipelines are followed.');
    expect(ci.loading).toBe(false);

    refuse = false;
    await vi.advanceTimersByTimeAsync(DEFAULT_RETRY.initialMs);
    await settle();
    expect(subscribe).toHaveBeenCalledTimes(2);
    expect(ci.error).toBeNull();
    expect(ci.pipeline?.status).toBe('running');
  });

  it('follows the selected pipeline and releases it on deselect and detach', async () => {
    installMocks();
    const subscribe = setBindingMock('SubscribePipeline', async (_repo: unknown, id: string) => ({
      id: `pipe-${id}`, key: `gitlab:group/app:pipeline:${id}`, pipeline: { id, status: 'running', stages: [] }, error: '', errorKind: '', reserve: false, resumeAt: '', seq: 1,
    }));
    const unsubscribe = setBindingMock('UnsubscribePipeline', async () => undefined);
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    const key = pipelinesRows(PANE).find((row) => row.id === '1')!.key;
    expect(selectPipeline(PANE, 'nope')).toBe(false);
    expect(selectPipeline(PANE, key)).toBe(true);
    await settle();
    expect(subscribe).toHaveBeenCalledWith({ Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'app' }, '1');
    expect(pipelineDetailOpen(PANE)).toBe(true);
    expect(selectedPipeline(PANE)?.ci.pipeline?.status).toBe('running');
    expect(selectedPipeline(PANE)?.row.id).toBe('1');

    clearPipelineSelection(PANE);
    await settle();
    expect(unsubscribe).toHaveBeenCalledWith('pipe-1');
    expect(pipelineDetailOpen(PANE)).toBe(false);

    selectPipeline(PANE, key);
    await settle();
    detachPipelinesPage(PANE);
    await settle();
    expect(unsubscribe).toHaveBeenCalledTimes(2);
  });
});
