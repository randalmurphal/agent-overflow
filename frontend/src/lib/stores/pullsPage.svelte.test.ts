// The Pull requests page store: the list subscription's lifecycle and
// routing, the sequence watermark, the filters, the sort and their
// persistence, the row projection, the needs-you badge, the selection,
// and Open with agent.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import {
  __resetPullsPageForTest,
  applyPullsSnapshot,
  attachPullsPage,
  clearSelection,
  detachPullsPage,
  detailOpen,
  pullsLoading,
  pullsNeedsYouCount,
  pullsProjectErrors,
  pullsProjects,
  pullsError,
  pullsRefreshFailure,
  pullsRowsByGroup,
  pullsStale,
  refreshPullsPage,
  resyncPullsAfterGap,
  selectedPR,
  selectPR,
} from './pullsPage.svelte';
import {
  __resetPullsPageAgentForTest,
  loadThreadsOnPR,
  newThreadCheckedOutBy,
  openPRThread,
  openThread,
  prThreadSeed,
  threadsOnPR,
} from './pullsPageAgent.svelte';
import {
  DEFAULT_MR_SORT,
  MR_FILTERS_KEY,
  mrFilters,
  resetPullsFiltersForTest,
  setMRFilters,
  syncPullsFiltersFromAppStorage,
} from './pullsPageFilters.svelte';
import { applyPullsBadge, getPullsNeedsYouCount, hydratePullsAttention, resetPullsAttentionForTest } from './pullsAttention.svelte';
import { applyPullsUpdatedEvent } from './pullsStore.svelte';
import { __resetPRReviewStoreForTest } from './prReviewStore.svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { resetPanesForTest } from './panes.svelte';
import { __resetReviewPaneStateForTest } from './reviewPane.svelte';
import { setSelectedBackend } from './selectedBackend.svelte';
import type { PRListItem, PRListSnapshot, ProjectPRList } from './bindings';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { prKey, type PRRef } from '../utils/prReference';
import { composeWorkspaceKey } from '../utils/workspaceKey';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';
import { grantBackendScopes, revokeBackendScopes } from '../../test/helpers/scopes';
import { getToasts, resetToastsForTest } from './toast.svelte';

const PANE = 'tools-pulls';

function ref(number: number, repo = 'repo'): PRRef {
  return { forge: 'github', host: 'github.com', namespace: 'owner', repo, number };
}

function rowKey(number: number, backend = HOME_BACKEND, repo = 'repo'): string {
  return composeWorkspaceKey(backend, prKey(ref(number, repo)));
}

function item(number: number, overrides: Partial<PRListItem> = {}, repo = 'repo'): PRListItem {
  return {
    ref: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: repo, Number: number },
    nodeID: `node-${number}`,
    url: `https://github.com/owner/${repo}/pull/${number}`,
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
    state: 'open',
    group: 'waiting',
    reason: { kind: '', commits: 0, replies: 0 },
    approvals: null,
    unresolved: null,
    ci: '',
    ...overrides,
  } as PRListItem;
}

function project(id: string, rows: PRListItem[], overrides: Partial<ProjectPRList> = {}): ProjectPRList {
  return {
    projectId: id,
    name: `Project ${id}`,
    repo: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo' },
    rows,
    ...overrides,
  } as ProjectPRList;
}

function snapshot(seq: number, projects: ProjectPRList[], needsYou = 0, stale = false): PRListSnapshot {
  return { seq, fetchedAt: '2026-10-20T00:00:00Z', needsYou, stale, projects } as PRListSnapshot;
}

interface ListMocks {
  subscribe: ReturnType<typeof vi.fn>;
  unsubscribe: ReturnType<typeof vi.fn>;
  active: ReturnType<typeof vi.fn>;
  refresh: ReturnType<typeof vi.fn>;
  /** The computer each call went to, by binding. */
  routed: Record<string, string[]>;
}

function installListMocks(first: PRListSnapshot = snapshot(1, [project('p1', [item(1)])])): ListMocks {
  const routed: Record<string, string[]> = {};
  const note = (name: string) => {
    (routed[name] ??= []).push(String(takePinnedBackend() ?? ''));
  };
  let n = 0;
  const subscribe = setBindingMock('SubscribePRList', async () => {
    note('SubscribePRList');
    n += 1;
    return { id: `list-${n}`, snapshot: first };
  });
  const unsubscribe = setBindingMock('UnsubscribePRList', async () => {
    note('UnsubscribePRList');
  });
  const active = setBindingMock('SetPRListActive', async () => {
    note('SetPRListActive');
  });
  const refresh = setBindingMock('RefreshPRList', async () => {
    note('RefreshPRList');
  });
  return { subscribe, unsubscribe, active, refresh, routed };
}

async function flush(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

function setDocumentVisibility(value: DocumentVisibilityState): void {
  Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => value });
  document.dispatchEvent(new Event('visibilitychange'));
}

function groupKeys(): string[][] {
  return pullsRowsByGroup(PANE).map((group) => group.rows.map((row) => row.key));
}

beforeEach(() => {
  resetAppStorageForTest();
  resetPullsFiltersForTest();
  resetPullsAttentionForTest();
  resetPanesForTest();
});

afterEach(() => {
  __resetPullsPageForTest();
  __resetPullsPageAgentForTest();
  __resetReviewPaneStateForTest();
  __resetPRReviewStoreForTest();
  resetStagedBackends();
  resetToastsForTest();
  delete (document as unknown as Record<string, unknown>).visibilityState;
  vi.useRealTimers();
});

describe('the list subscription', () => {
  it('subscribes on the first pane, shares it, and unsubscribes when the last pane goes', async () => {
    const mocks = installListMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    expect(pullsLoading(PANE)).toBe(true);
    attachPullsPage('tools-other', HOME_BACKEND);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    expect(pullsLoading(PANE)).toBe(false);
    expect(groupKeys()).toEqual([]);

    detachPullsPage(PANE);
    await flush();
    expect(mocks.unsubscribe).not.toHaveBeenCalled();
    detachPullsPage('tools-other');
    await flush();
    expect(mocks.unsubscribe).toHaveBeenCalledWith('list-1');

    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(2);
  });

  it('attaches from a mount effect without the effect depending on the page', async () => {
    installListMocks();
    const probe = probeReactivity(() => {
      attachPullsPage(PANE, HOME_BACKEND);
      return 0;
    });
    try {
      await flush();
      attachPullsPage('tools-other', HOME_BACKEND);
      selectPR(PANE, rowKey(1));
      applyPullsSnapshot(snapshot(9, [project('p1', [item(1), item(2)])]), HOME_BACKEND);
      detachPullsPage('tools-other');
      flushSync();
      expect(probe.evaluations).toBe(1);
    } finally {
      probe.dispose();
    }
  });

  it('pins the subscribe and every handle call to the computer the page lists', async () => {
    stageBackend();
    await grantBackendScopes('laptop', ['git:operate']);
    const mocks = installListMocks();
    attachPullsPage(PANE, 'laptop');
    await flush();
    await refreshPullsPage(PANE);
    setDocumentVisibility('hidden');
    await flush();
    detachPullsPage(PANE);
    await flush();
    expect(mocks.routed.SubscribePRList).toEqual(['laptop']);
    expect(mocks.routed.RefreshPRList).toEqual(['laptop']);
    expect(mocks.routed.SetPRListActive).toEqual(['laptop']);
    expect(mocks.routed.UnsubscribePRList).toEqual(['laptop']);
  });

  it('votes the pump down while the document is hidden and back up when it shows', async () => {
    const mocks = installListMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setDocumentVisibility('hidden');
    await flush();
    expect(mocks.active).toHaveBeenLastCalledWith('list-1', false);
    setDocumentVisibility('visible');
    await flush();
    expect(mocks.active).toHaveBeenLastCalledWith('list-1', true);
    detachPullsPage(PANE);
    await flush();
    mocks.active.mockClear();
    setDocumentVisibility('hidden');
    await flush();
    expect(mocks.active).not.toHaveBeenCalled();
  });

  it('subscribes again on reconnect and after a gap, keeping the rows on screen', async () => {
    const mocks = installListMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    expect(groupKeys()).toEqual([[rowKey(1)]]);

    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(2);

    resyncPullsAfterGap(HOME_BACKEND);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(3);
    expect(groupKeys()).toEqual([[rowKey(1)]]);
  });

  it('answers a gap resync with the re-subscribe, settled once its snapshot is applied', async () => {
    installListMocks();
    expect(resyncPullsAfterGap(HOME_BACKEND)).toBeUndefined();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    let answer!: (value: unknown) => void;
    setBindingMock('SubscribePRList', () => new Promise((resolve) => {
      answer = resolve;
    }));
    const resync = resyncPullsAfterGap(HOME_BACKEND);
    expect(resync).toBeInstanceOf(Promise);
    let settled = false;
    void resync!.then(() => {
      settled = true;
    });
    await flush();
    expect(settled).toBe(false);
    answer({ id: 'list-9', snapshot: snapshot(4, [project('p1', [item(2)])]) });
    await resync;
    expect(groupKeys()).toEqual([[rowKey(2)]]);

    // A failed re-subscribe settles the fence too; the store retries it.
    setBindingMock('SubscribePRList', async () => {
      throw new Error('backend busy');
    });
    await expect(resyncPullsAfterGap(HOME_BACKEND)).resolves.toBeUndefined();
  });

  it("holds a refused refresh call as the page's refresh failure until the next refresh", async () => {
    setBindingMock('GetErrorLogLines', async () => ({ lines: [], found: false }));
    const mocks = installListMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    mocks.refresh.mockImplementationOnce(async () => {
      throw new Error('backend busy');
    });
    await refreshPullsPage(PANE);
    expect(pullsRefreshFailure(PANE)?.report).toMatchObject({ context: 'Could not refresh the merge requests', chain: ['backend busy'] });
    expect(getToasts()).toEqual([]);
    await refreshPullsPage(PANE);
    expect(pullsRefreshFailure(PANE)).toBeNull();
    expect(getToasts()).toEqual([]);
  });

  it('holds the list only while the computer grants git:operate, and says so when it does not', async () => {
    stageBackend();
    const mocks = installListMocks();
    attachPullsPage(PANE, 'laptop');
    await flush();
    // Absent: nothing is subscribed, and the page says why rather than
    // loading forever.
    expect(mocks.subscribe).not.toHaveBeenCalled();
    expect(pullsLoading(PANE)).toBe(false);
    expect(pullsError(PANE)).toBe('This device is not allowed to read pull requests on this computer.');
    await refreshPullsPage(PANE);
    expect(mocks.refresh).not.toHaveBeenCalled();
    expect(getToasts().map((toast) => toast.message)).toEqual(['This device is not allowed to read pull requests on this computer.']);

    // Granted: the list subscribes.
    await grantBackendScopes('laptop', ['git:operate']);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
    expect(pullsError(PANE)).toBeNull();
    setMRFilters('laptop', { filter: 'all' });
    expect(groupKeys()).toEqual([[rowKey(1, 'laptop')]]);

    // Revoked: the subscription goes with the rows it held.
    revokeBackendScopes('laptop');
    await flush();
    expect(mocks.unsubscribe).toHaveBeenCalledWith('list-1');
    expect(groupKeys()).toEqual([]);
    expect(pullsError(PANE)).toBe('This device is not allowed to read pull requests on this computer.');
    detachPullsPage(PANE);
    await flush();
    expect(mocks.subscribe).toHaveBeenCalledTimes(1);
  });
});

describe('the sequence watermark', () => {
  it('applies a newer frame and drops one at or below the snapshot shown', async () => {
    installListMocks(snapshot(5, [project('p1', [item(1)])]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'all' });

    applyPullsUpdatedEvent(snapshot(4, [project('p1', [item(2)])]), HOME_BACKEND);
    expect(groupKeys()).toEqual([[rowKey(1)]]);
    applyPullsUpdatedEvent(snapshot(5, [project('p1', [item(3)])]), HOME_BACKEND);
    expect(groupKeys()).toEqual([[rowKey(1)]]);
    applyPullsUpdatedEvent(snapshot(6, [project('p1', [item(4)])]), HOME_BACKEND);
    expect(groupKeys()).toEqual([[rowKey(4)]]);
  });

  it('keeps a frame that landed during the subscribe over its older answer', async () => {
    let answer!: (value: unknown) => void;
    setBindingMock('SubscribePRList', () => new Promise((resolve) => {
      answer = resolve;
    }));
    setBindingMock('UnsubscribePRList', async () => undefined);
    attachPullsPage(PANE, HOME_BACKEND);
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    await flush();
    applyPullsSnapshot(snapshot(8, [project('p1', [item(8)])]), HOME_BACKEND);
    answer({ id: 'list-1', snapshot: snapshot(7, [project('p1', [item(7)])]) });
    await flush();
    expect(groupKeys()).toEqual([[rowKey(8)]]);
  });

  it('shows a stale answer as rows, not as loading, until the read lands', async () => {
    installListMocks(snapshot(1, [project('p1', [item(1)])], 0, true));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    expect(pullsLoading(PANE)).toBe(false);
    expect(pullsStale(PANE)).toBe(true);
    expect(groupKeys()).toEqual([[rowKey(1)]]);
    applyPullsSnapshot(snapshot(2, [project('p1', [item(1), item(2)])]), HOME_BACKEND);
    expect(pullsStale(PANE)).toBe(false);
    expect(groupKeys()).toEqual([[rowKey(2), rowKey(1)]]);
  });
});

describe('filters', () => {
  const rows = [
    item(1, { group: 'needs_you', state: 'your_turn', viewerIsReviewer: true, updatedAt: '2026-10-09T00:00:00Z' }),
    item(2, { viewerIsAuthor: true, state: 'draft', updatedAt: '2026-10-11T00:00:00Z' }),
    item(3, { viewerIsReviewer: true, state: 'approved', updatedAt: '2026-10-08T00:00:00Z' }),
  ];

  async function attached(): Promise<void> {
    installListMocks(snapshot(1, [project('p1', rows), project('p2', [item(9, {}, 'other')], { repo: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'other' } })]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
  }

  it('starts on Needs you over every project, newest update first, and filters by each choice', async () => {
    await attached();
    expect(mrFilters(HOME_BACKEND)).toEqual({ filter: 'needs_you', projects: [], sort: DEFAULT_MR_SORT });
    expect(groupKeys()).toEqual([[rowKey(1)]]);
    setMRFilters(HOME_BACKEND, { filter: 'mine' });
    expect(groupKeys()).toEqual([[rowKey(2)]]);
    setMRFilters(HOME_BACKEND, { filter: 'reviewing' });
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(3)]]);
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    expect(pullsRowsByGroup(PANE).map((group) => group.group)).toEqual(['needs_you', 'waiting']);
    // Across projects, newest update first, then the higher number.
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(2), rowKey(9, HOME_BACKEND, 'other'), rowKey(3)]]);
  });

  it('sorts each group by the remembered key, turned around when reversed, with needs-you rows always first', async () => {
    await attached();
    setMRFilters(HOME_BACKEND, { filter: 'all', sort: { key: 'updated', reversed: true } });
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(3), rowKey(9, HOME_BACKEND, 'other'), rowKey(2)]]);
    setMRFilters(HOME_BACKEND, { sort: { key: 'created', reversed: false } });
    // Equal creation times fall to the newest update, then the higher number.
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(2), rowKey(9, HOME_BACKEND, 'other'), rowKey(3)]]);
    setMRFilters(HOME_BACKEND, { sort: { key: 'title', reversed: false } });
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(2), rowKey(3), rowKey(9, HOME_BACKEND, 'other')]]);
    setMRFilters(HOME_BACKEND, { sort: { key: 'attention', reversed: false } });
    // The backend's state order: approved before draft before open.
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(3), rowKey(2), rowKey(9, HOME_BACKEND, 'other')]]);
    setMRFilters(HOME_BACKEND, { sort: { key: 'author', reversed: false } });
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(2), rowKey(9, HOME_BACKEND, 'other'), rowKey(3)]]);
  });

  it('narrows to the picked projects, and lists the projects to pick', async () => {
    await attached();
    expect(pullsProjects(PANE)).toEqual([{ id: 'p1', name: 'Project p1' }, { id: 'p2', name: 'Project p2' }]);
    setMRFilters(HOME_BACKEND, { filter: 'all', projects: ['p2'] });
    expect(groupKeys()).toEqual([[rowKey(9, HOME_BACKEND, 'other')]]);
  });

  it('remembers the filters and the sort per computer and adopts the stored copy on sync', async () => {
    await attached();
    setMRFilters(HOME_BACKEND, { filter: 'mine', projects: ['p1'], sort: { key: 'title', reversed: true } });
    expect(JSON.parse(appStorageGet(MR_FILTERS_KEY, HOME_BACKEND) ?? '{}')).toEqual({ filter: 'mine', projects: ['p1'], sort: { key: 'title', reversed: true } });
    appStorageSet(MR_FILTERS_KEY, JSON.stringify({ filter: 'reviewing', projects: ['p2'], sort: { key: 'author', reversed: false } }), HOME_BACKEND);
    syncPullsFiltersFromAppStorage();
    expect(mrFilters(HOME_BACKEND)).toEqual({ filter: 'reviewing', projects: ['p2'], sort: { key: 'author', reversed: false } });
    // The filter's name before it listed every MR the viewer reviews.
    appStorageSet(MR_FILTERS_KEY, JSON.stringify({ filter: 'review_requested', projects: [] }), HOME_BACKEND);
    syncPullsFiltersFromAppStorage();
    expect(mrFilters(HOME_BACKEND)).toEqual({ filter: 'reviewing', projects: [], sort: DEFAULT_MR_SORT });
    appStorageSet(MR_FILTERS_KEY, '{"filter":"bogus","projects":[3],"sort":{"key":"size"}}', HOME_BACKEND);
    syncPullsFiltersFromAppStorage();
    expect(mrFilters(HOME_BACKEND)).toEqual({ filter: 'needs_you', projects: [], sort: DEFAULT_MR_SORT });
  });
});

describe('row projection', () => {
  it('names the viewer "you", the author by display name, words the state with its reason, and carries the facts', async () => {
    installListMocks(snapshot(1, [project('p1', [
      item(1, { viewerIsAuthor: true, state: 'pipeline_failed', group: 'needs_you', ci: 'failed', approvals: { given: 0, required: 0 } }),
      item(2, { author: { login: 'bob', name: '' }, ci: 'running', approvals: { given: 1, required: 2 }, unresolved: 3 }),
      item(3, { state: 'your_turn', group: 'needs_you', reason: { kind: 'pushed', commits: 2, replies: 0 }, updatedAt: '2026-10-11T00:00:00Z' }),
      item(4, { state: 'your_turn', group: 'needs_you', reason: { kind: 'replied', commits: 0, replies: 1 }, updatedAt: '2026-10-12T00:00:00Z' }),
      item(5, { state: 'approved', reason: { kind: 'reviewed', commits: 0, replies: 0 } }),
    ])]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    const rows = pullsRowsByGroup(PANE).flatMap((group) => group.rows).sort((a, b) => a.number - b.number);
    expect(rows[0]).toMatchObject({ author: 'you', viewerIsAuthor: true, ci: 'failed', label: 'Pipeline failed', tone: 'error', approvals: null, unresolved: null, projectName: 'Project p1' });
    expect(rows[0]!.age).toMatch(/^\d+[mhd]$/);
    expect(rows[1]).toMatchObject({ author: 'bob', label: 'Open', tone: 'muted', ci: 'running', approvals: { given: 1, required: 2 }, unresolved: 3 });
    expect(rows[2]).toMatchObject({ author: 'Alice', label: '+2 commits since your review', tone: 'accent' });
    expect(rows[3]).toMatchObject({ label: '1 reply since your review', tone: 'accent' });
    expect(rows[4]).toMatchObject({ label: 'Reviewed by you', tone: 'muted' });
  });

  it('reports each failed project with its kind', async () => {
    installListMocks(snapshot(1, [project('p1', [], { error: 'No access to owner/repo with this login.', errorKind: 'forbidden' })]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    expect(pullsProjectErrors(PANE)).toEqual([{
      projectId: 'p1', name: 'Project p1', message: 'No access to owner/repo with this login.',
      failure: { kind: 'forbidden', reserve: false, resumeAt: '' }, forge: 'github', cause: null,
    }]);
  });
});

describe('the needs-you badge', () => {
  it('counts the selected computer from its badge frames, its edge read and the page\'s own snapshots', async () => {
    stageBackend();
    await grantBackendScopes('laptop', ['git:operate']);
    const read = setBindingMock('GetPRListNeedsYou', async () => 4);
    applyPullsBadge({ needsYou: 3 }, HOME_BACKEND);
    expect(getPullsNeedsYouCount()).toBe(3);
    await hydratePullsAttention('laptop');
    expect(read).toHaveBeenCalledTimes(1);
    expect(getPullsNeedsYouCount()).toBe(3);
    setSelectedBackend('laptop');
    expect(getPullsNeedsYouCount()).toBe(4);
    setSelectedBackend(HOME_BACKEND);

    // The page's snapshot and the badge never disagree.
    installListMocks(snapshot(1, [project('p1', [item(1, { group: 'needs_you' })])], 1));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'mine' });
    expect(getPullsNeedsYouCount()).toBe(1);
    expect(pullsNeedsYouCount(PANE)).toBe(1);
    applyPullsSnapshot(snapshot(2, [project('p1', [item(1, { group: 'needs_you' }), item(2, { group: 'needs_you' })])], 2), HOME_BACKEND);
    expect(getPullsNeedsYouCount()).toBe(2);
    // The page going does not take the count: the badge is right with no page open.
    detachPullsPage(PANE);
    expect(getPullsNeedsYouCount()).toBe(2);
    // A malformed frame changes nothing; a device without the scope reads nothing.
    applyPullsBadge({} as { needsYou?: number }, HOME_BACKEND);
    expect(getPullsNeedsYouCount()).toBe(2);
    revokeBackendScopes('laptop');
    await hydratePullsAttention('laptop');
    expect(read).toHaveBeenCalledTimes(1);
  });
});

describe('selection', () => {
  it('selects a listed row and clears it, which the compact probe reads', async () => {
    installListMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    expect(selectPR(PANE, rowKey(99))).toBe(false);
    expect(selectPR(PANE, rowKey(1))).toBe(true);
    expect(detailOpen(PANE)).toBe(true);
    expect(selectedPR(PANE)).toMatchObject({ key: rowKey(1), backend: HOME_BACKEND, projectId: 'p1', pr: ref(1) });
    clearSelection(PANE);
    expect(detailOpen(PANE)).toBe(false);
  });

});

describe('open with agent', () => {
  async function selected(overrides: Partial<PRListItem> = {}): Promise<void> {
    installListMocks(snapshot(1, [project('p1', [item(1, overrides)])]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    selectPR(PANE, rowKey(1));
  }

  it('reads the threads on the PR on every menu open and names the one holding its branch', async () => {
    await selected();
    const list = setBindingMock('ListThreadsOnPR', async () => [
      { threadId: 't1', title: 'Fix it', status: 'idle', worktreeBranch: 'feature-1', updatedAt: Date.now() - 3 * 86_400_000 },
    ]);
    await loadThreadsOnPR(PANE);
    expect(list).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledWith({ projectId: 'p1', pr: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 1 }, headRefName: 'feature-1', crossRepository: false });
    expect(threadsOnPR(PANE)).toEqual([{ id: 't1', title: 'Fix it', detail: 'idle · 3d ago' }]);
    expect(newThreadCheckedOutBy(PANE)?.id).toBe('t1');

    // Another client removed the thread: nothing this client holds moved,
    // and the next open reads it gone.
    list.mockImplementation(async () => []);
    await loadThreadsOnPR(PANE);
    expect(list).toHaveBeenCalledTimes(2);
    expect(threadsOnPR(PANE)).toEqual([]);
    expect(newThreadCheckedOutBy(PANE)).toBeNull();
  });

  it('drops the answer of a read a reopened menu superseded', async () => {
    await selected();
    const answers: Array<(threads: unknown[]) => void> = [];
    const list = setBindingMock('ListThreadsOnPR', () => new Promise((resolve) => answers.push(resolve)));
    const first = loadThreadsOnPR(PANE);
    const second = loadThreadsOnPR(PANE);
    expect(list).toHaveBeenCalledTimes(2);
    answers[1]!([{ threadId: 'new', title: 'Newer', status: 'idle', worktreeBranch: '', updatedAt: 0 }]);
    await second;
    answers[0]!([{ threadId: 'old', title: 'Older', status: 'idle', worktreeBranch: '', updatedAt: 0 }]);
    await first;
    expect(threadsOnPR(PANE).map((thread) => thread.id)).toEqual(['new']);
  });

  it('holds nothing for a page that detached with a read in flight', async () => {
    await selected();
    let answer!: (threads: unknown[]) => void;
    setBindingMock('ListThreadsOnPR', () => new Promise((resolve) => {
      answer = resolve;
    }));
    const read = loadThreadsOnPR(PANE);
    detachPullsPage(PANE);
    answer([{ threadId: 't1', title: 'Fix it', status: 'idle', worktreeBranch: 'feature-1', updatedAt: 0 }]);
    await read;
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    selectPR(PANE, rowKey(1));
    expect(threadsOnPR(PANE)).toEqual([]);
    expect(newThreadCheckedOutBy(PANE)).toBeNull();
  });

  it('opens a thread on the PR head, seeds its draft, and mounts it', async () => {
    await selected();
    const open = setBindingMock('OpenPRThread', async () => ({ threadId: 't9', worktreePath: '/wt', branch: 'feature-1' }));
    setBindingMock('GetThread', async () => ({ id: 't9', projectId: 'p1', title: '', updatedAt: 1 }));
    const panes = await import('./panes.svelte');
    const mount = vi.spyOn(panes, 'mountThreadForToolPane').mockImplementation(async () => ({}) as never);
    expect(await openPRThread(PANE)).toBe(true);
    // In the page's slot.
    expect(mount).toHaveBeenCalledWith(expect.objectContaining({ id: 't9' }), PANE);
    expect(open).toHaveBeenCalledWith({
      projectId: 'p1', pr: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 1 },
      headRefName: 'feature-1', crossRepository: false, url: 'https://github.com/owner/repo/pull/1', nodeId: 'node-1', title: 'PR 1',
      seed: 'Reviewing #1: PR 1\nhttps://github.com/owner/repo/pull/1',
    });
    expect(prThreadSeed(selectedPR(PANE)!)).toBe('Reviewing #1: PR 1\nhttps://github.com/owner/repo/pull/1');
    mount.mockRestore();
  });

  it('mounts a late answer in a new pane once the page that asked has gone, even when it reopened', async () => {
    await selected();
    let opened!: (value: unknown) => void;
    setBindingMock('OpenPRThread', () => new Promise((resolve) => {
      opened = resolve;
    }));
    setBindingMock('GetThread', async (id: string) => ({ id, projectId: 'p1', title: '', updatedAt: 1 }));
    const panes = await import('./panes.svelte');
    const mount = vi.spyOn(panes, 'mountThreadForToolPane').mockImplementation(async () => ({}) as never);
    const opening = openPRThread(PANE);
    await flush();
    detachPullsPage(PANE);
    attachPullsPage(PANE, HOME_BACKEND);
    opened({ threadId: 't9', worktreePath: '/wt', branch: 'feature-1' });
    expect(await opening).toBe(true);
    expect(mount).toHaveBeenCalledWith(expect.objectContaining({ id: 't9' }), null);

    // The same for a thread already on the PR read from the backend.
    let read!: (value: unknown) => void;
    setBindingMock('GetThread', () => new Promise((resolve) => {
      read = resolve;
    }));
    await flush();
    const reopening = openThread(PANE, 't4');
    detachPullsPage(PANE);
    read({ id: 't4', projectId: 'p1', title: '', updatedAt: 1 });
    expect(await reopening).toBe(true);
    expect(mount).toHaveBeenLastCalledWith(expect.objectContaining({ id: 't4' }), null);
    mount.mockRestore();
  });

  it('mounts a late answer in the page\'s slot while the page is still there', async () => {
    await selected();
    let read!: (value: unknown) => void;
    setBindingMock('GetThread', () => new Promise((resolve) => {
      read = resolve;
    }));
    const panes = await import('./panes.svelte');
    const mount = vi.spyOn(panes, 'mountThreadForToolPane').mockImplementation(async () => ({}) as never);
    const opening = openThread(PANE, 't4');
    read({ id: 't4', projectId: 'p1', title: '', updatedAt: 1 });
    expect(await opening).toBe(true);
    expect(mount).toHaveBeenCalledWith(expect.objectContaining({ id: 't4' }), PANE);
    mount.mockRestore();
  });

  it('touches nothing when the backend refuses the thread', async () => {
    await selected();
    setBindingMock('OpenPRThread', async () => {
      throw new Error("open PR thread: 'feature-1' is already checked out");
    });
    const del = setBindingMock('DeleteThread', async () => undefined);
    expect(await openPRThread(PANE)).toBe(false);
    expect(del).not.toHaveBeenCalled();
  });
});
