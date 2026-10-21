// The Pipelines tab store: list reads on open, refresh and the interval
// (paused while hidden), the filters, the stage summaries for rows on
// screen and their batching cap, per-project failures, and the selected
// pipeline's subscription.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import {
  __resetPipelinesPageForTest,
  attachPipelinesPage,
  clearPipelineSelection,
  detachPipelinesPage,
  PIPELINE_STAGE_READS_MAX,
  PIPELINES_REFRESH_MS,
  pipelineDetailOpen,
  pipelinesProjectErrors,
  pipelinesRows,
  refreshPipelinesPage,
  selectedPipeline,
  selectPipeline,
  setPipelinesFilters,
  setVisiblePipelines,
} from './pipelinesPage.svelte';
import { __resetPipelineCIForTest } from './pipelineCI.svelte';
import { PIPELINE_FILTERS_KEY, resetPullsFiltersForTest } from './pullsPageFilters.svelte';
import { appStorageGet, resetAppStorageForTest } from './appStorage';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { TransportError } from '../transport/wsClient';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

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
  resetStagedBackends();
  delete (document as unknown as Record<string, unknown>).visibilityState;
  vi.useRealTimers();
});

describe('list reads', () => {
  it('lists every project on open, newest first, pinned to the computer the page lists', async () => {
    stageBackend();
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
    }]);
  });
});

describe('filters', () => {
  it('reads again for a status or projects, filters the ref kind in place, and remembers them', async () => {
    const mocks = installMocks();
    attachPipelinesPage(PANE, HOME_BACKEND);
    await settle();
    setPipelinesFilters(PANE, { status: 'failed' });
    await settle();
    expect(mocks.list).toHaveBeenLastCalledWith('p2', { status: 'failed' });
    setPipelinesFilters(PANE, { projects: ['p1'] });
    await settle();
    expect(mocks.list).toHaveBeenLastCalledWith('p1', { status: 'failed' });
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
});

describe('selection', () => {
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
