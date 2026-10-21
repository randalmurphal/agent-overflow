// The Pull requests page mounted over mocked bindings, for component and
// browser tests of PullsPane: one GitHub project listing PRs #5 and #6 (the
// review pane's own mocks answer for #5), and one GitLab project of
// pipelines whose pipeline 77 has a job with a log.

import { vi } from 'vitest';
import { setBindingMock, setReviewDiffMock } from '../mocks/bindings-app';
import { installPRDraftsFake } from './prDraftsFake';
import { __resetPullsPageForTest } from '../../lib/stores/pullsPage.svelte';
import { __resetPullsPageAgentForTest } from '../../lib/stores/pullsPageAgent.svelte';
import { resetPullsFiltersForTest } from '../../lib/stores/pullsPageFilters.svelte';
import { resetPullsAttentionForTest } from '../../lib/stores/pullsAttention.svelte';
import { __resetPipelinesPageForTest } from '../../lib/stores/pipelinesPage.svelte';
import { __resetPipelineCIForTest } from '../../lib/stores/pipelineCI.svelte';
import { __resetReviewPaneStateForTest } from '../../lib/stores/reviewPane.svelte';
import { __resetPRReviewStoreForTest } from '../../lib/stores/prReviewStore.svelte';
import { resetAppStorageForTest } from '../../lib/stores/appStorage';
import { resetPanesForTest } from '../../lib/stores/panes.svelte';
import { setCompactLayoutForTest } from '../../lib/stores/layoutMode.svelte';
import { resetToastsForTest } from '../../lib/stores/toast.svelte';
import { resetPullsCommandsForTest } from '../../lib/stores/pullsCommands.svelte';
import type { PRListItem, PRListSnapshot, ProjectPRList } from '../../lib/stores/bindings';
import { HOME_BACKEND } from '../../lib/transport/backendKey';
import { prKey, type PRRef } from '../../lib/utils/prReference';
import { composeWorkspaceKey } from '../../lib/utils/workspaceKey';

export const PULLS_PANE = 'tool-pulls';

export function pullRef(number: number): PRRef {
  return { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number };
}

export function pullRowKey(number: number): string {
  return composeWorkspaceKey(HOME_BACKEND, prKey(pullRef(number)));
}

export function pullItem(number: number, overrides: Partial<PRListItem> = {}): PRListItem {
  return {
    ref: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: number },
    nodeID: `node-${number}`,
    url: `https://github.com/owner/repo/pull/${number}`,
    title: `PR ${number}`,
    draft: false,
    author: { login: 'alice', name: 'Alice' },
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: '2026-10-10T00:00:00Z',
    headSHA: `sha-${number}`,
    headRefName: `feature-${number}`,
    baseRefName: 'main',
    crossRepository: false,
    comments: 0,
    commits: 1,
    lastCommitAt: '',
    viewerIsAuthor: false,
    viewerIsReviewer: false,
    reviewRequested: false,
    viewerReview: {},
    reviewDecision: '',
    mergeStatus: 'clean',
    conflicts: false,
    unresolvedBlocking: false,
    checksState: '',
    state: 'your_turn',
    group: 'needs_you',
    detail: null,
    seen: null,
    since: null,
    ...overrides,
  } as PRListItem;
}

export function pullsProject(rows: PRListItem[], overrides: Partial<ProjectPRList> = {}): ProjectPRList {
  return {
    projectId: 'project-1',
    name: 'Repo',
    repo: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo' },
    rows,
    ...overrides,
  } as ProjectPRList;
}

export function pullsSnapshot(projects: ProjectPRList[] = [pullsProject([pullItem(5), pullItem(6)])]): PRListSnapshot {
  return { seq: 1, fetchedAt: '2026-10-20T00:00:00Z', needsYou: 0, projects } as PRListSnapshot;
}

function prDetail(number: number) {
  return {
    number,
    title: `PR ${number}`,
    body: '',
    authorLogin: 'alice',
    state: 'open',
    draft: false,
    headRefName: `feature-${number}`,
    baseRefName: 'main',
    baseRefSHA: 'base-main',
    headSHA: `sha-${number}`,
    url: `https://github.com/owner/repo/pull/${number}`,
    additions: 1,
    deletions: 1,
    changedFiles: 1,
    viewerIsAuthor: false,
    reviewDecision: '',
    latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'conflicts',
    readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: true, viewerCanApprove: true, viewerApproved: false },
  };
}

function patch(): string {
  return ['diff --git a/src/app.ts b/src/app.ts', 'index 1111111..2222222 100644', '--- a/src/app.ts', '+++ b/src/app.ts', '@@ -1 +1 @@', '-old', '+new'].join('\n');
}

export interface PullsPaneMocks {
  refresh: ReturnType<typeof vi.fn>;
  threads: ReturnType<typeof vi.fn>;
  focus: ReturnType<typeof vi.fn>;
}

/** Every binding the page, its pinned review and its Pipelines tab call. */
export function installPullsPaneMocks(snapshot: PRListSnapshot = pullsSnapshot()): PullsPaneMocks {
  setBindingMock('SubscribePRList', async () => ({ id: 'list-1', snapshot }));
  setBindingMock('UnsubscribePRList', async () => undefined);
  setBindingMock('SetPRListActive', async () => undefined);
  const focus = setBindingMock('SetPRListFocus', async () => ({ unknown: [] }));
  const refresh = setBindingMock('RefreshPRList', async () => undefined);
  setBindingMock('MarkPRViewed', async () => null);
  const threads = setBindingMock('ListThreadsOnPR', async () => []);
  setBindingMock('GetErrorLogLines', async () => ({ lines: [], found: false }));
  setBindingMock('OpenExternalURL', async () => undefined);
  // The pinned review.
  setBindingMock('SubscribePRUpdates', async (ref: { Number: number }) => ({
    id: `pr-${ref.Number}`, prKey: `github:owner/repo:${ref.Number}`, detail: prDetail(ref.Number), threads: [], headSHA: `sha-${ref.Number}`, seq: 1,
  }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => patch());
  setBindingMock('ListPRCommits', async () => []);
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  setBindingMock('GetPRMergeConflicts', async () => ({ conflicted: false, paths: [], baseLabel: 'origin/main', headLabel: 'feature' }));
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
  installPRDraftsFake();
  // The Pipelines tab.
  setBindingMock('ListForgeProjects', async () => [
    { projectId: 'project-2', name: 'App', path: '/src/app', repo: { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'app' } },
  ]);
  setBindingMock('ListProjectPipelines', async () => ({
    rows: [{ id: '77', status: 'failed', ref: 'main', refKind: 'branch', sha: 'a'.repeat(40), source: 'push', createdAt: '2026-10-01T00:00:00Z', updatedAt: '2026-10-01T00:00:00Z', url: 'https://gitlab.com/group/app/-/pipelines/77' }],
    fetchedAt: '',
  }));
  setBindingMock('ReadPipelineStages', async (_repo: unknown, ids: string[]) =>
    Object.fromEntries(ids.map((id) => [id, { stages: [{ name: 'test', status: 'failed', jobs: [] }] }])));
  setBindingMock('SubscribePipeline', async () => ({
    id: 'pipe-1', key: 'gitlab:group/app:pipeline:77', error: '', errorKind: '', reserve: false, resumeAt: '', seq: 1,
    pipeline: {
      id: '77', status: 'failed', url: 'https://gitlab.com/group/app/-/pipelines/77',
      stages: [{ name: 'test', status: 'failed', jobs: [{ id: '20', name: 'unit', status: 'failed', logsAvailable: true }] }],
    },
  }));
  setBindingMock('UnsubscribePipeline', async () => undefined);
  setBindingMock('SetPipelineActive', async () => undefined);
  setBindingMock('SetPipelineFollows', async (_id: string, jobs: string[]) => ({
    logs: Object.fromEntries(jobs.map((job) => [job, { version: 1, totalLines: 1, totalBytes: 8, stableLines: 0, sections: [], available: true, downloading: false, error: '', seq: 2 }])),
  }));
  setBindingMock('GetPRCILogLines', async () => ({ from: 0, lines: ['boom'], totalLines: 1, version: 1 }));
  return { refresh, threads, focus };
}

/** Clears every store the page touches. */
export function resetPullsPane(): void {
  __resetPullsPageForTest();
  __resetPullsPageAgentForTest();
  __resetPipelinesPageForTest();
  __resetPipelineCIForTest();
  __resetReviewPaneStateForTest();
  __resetPRReviewStoreForTest();
  resetPullsFiltersForTest();
  resetPullsAttentionForTest();
  resetPullsCommandsForTest();
  resetAppStorageForTest();
  resetPanesForTest();
  resetToastsForTest();
  setCompactLayoutForTest(false);
}
