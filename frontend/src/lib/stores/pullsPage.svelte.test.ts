// The Pull requests page store: the list subscription's lifecycle and
// routing, the sequence watermark, the filters and their persistence, the
// row projection, the needs-you badge, the focus set, viewed marks, and
// Open with agent.

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
  pullsRefreshError,
  pullsRowsByGroup,
  refreshPullsPage,
  resyncPullsAfterGap,
  selectedPR,
  selectPR,
  setFocusedRows,
} from './pullsPage.svelte';
import {
  __resetPullsPageAgentForTest,
  loadThreadsOnPR,
  newThreadCheckedOutBy,
  openPRThread,
  prThreadSeed,
  threadsOnPR,
} from './pullsPageAgent.svelte';
import {
  MR_FILTERS_KEY,
  mrFilters,
  resetPullsFiltersForTest,
  setMRFilters,
  syncPullsFiltersFromAppStorage,
} from './pullsPageFilters.svelte';
import { PR_LIST_FOCUS_MAX } from './pullsPageRows';
import { getPullsNeedsYouCount, resetPullsAttentionForTest } from './pullsAttention.svelte';
import { applyPullsUpdatedEvent } from './pullsStore.svelte';
import { __resetPRReviewStoreForTest, applyPRUpdatedEvent, attachPR } from './prReviewStore.svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import { __setTransportStatusForTest } from './transportStatus.svelte';
import { resetPanesForTest } from './panes.svelte';
import { setCompactLayoutForTest, showCompactList } from './layoutMode.svelte';
import { __resetReviewPaneStateForTest } from './reviewPane.svelte';
import type { PRListItem, PRListSnapshot, ProjectPRList } from './bindings';
import { HOME_BACKEND } from '../transport/backendKey';
import { takePinnedBackend } from '../transport/backends';
import { prKey, type PRRef } from '../utils/prReference';
import { sinceYouLookedText } from '../utils/pullsAttention';
import { composeWorkspaceKey } from '../utils/workspaceKey';
import { setBindingMock } from '../../test/mocks/bindings-app';
import { probeReactivity } from '../../test/helpers/reactivity.svelte';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

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
    detail: null,
    seen: null,
    since: null,
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

function snapshot(seq: number, projects: ProjectPRList[], needsYou = 0): PRListSnapshot {
  return { seq, fetchedAt: '2026-10-20T00:00:00Z', needsYou, projects } as PRListSnapshot;
}

interface ListMocks {
  subscribe: ReturnType<typeof vi.fn>;
  unsubscribe: ReturnType<typeof vi.fn>;
  active: ReturnType<typeof vi.fn>;
  focus: ReturnType<typeof vi.fn>;
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
  const focus = setBindingMock('SetPRListFocus', async () => {
    note('SetPRListFocus');
    return { unknown: [] };
  });
  const refresh = setBindingMock('RefreshPRList', async () => {
    note('RefreshPRList');
  });
  return { subscribe, unsubscribe, active, focus, refresh, routed };
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

  it('attaches and reports its rows from a mount effect without the effect depending on the page', async () => {
    installListMocks();
    const probe = probeReactivity(() => {
      attachPullsPage(PANE, HOME_BACKEND);
      setFocusedRows(PANE, [rowKey(1)]);
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
    expect(mocks.routed.SetPRListFocus).toEqual(['laptop']);
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

  it('shows a failed refresh call until the next refresh', async () => {
    const mocks = installListMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    mocks.refresh.mockImplementationOnce(async () => {
      throw new Error('backend busy');
    });
    await refreshPullsPage(PANE);
    expect(pullsRefreshError(PANE)).toBe('backend busy');
    await refreshPullsPage(PANE);
    expect(pullsRefreshError(PANE)).toBeNull();
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
    setBindingMock('SetPRListFocus', async () => ({ unknown: [] }));
    setBindingMock('UnsubscribePRList', async () => undefined);
    attachPullsPage(PANE, HOME_BACKEND);
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    await flush();
    applyPullsSnapshot(snapshot(8, [project('p1', [item(8)])]), HOME_BACKEND);
    answer({ id: 'list-1', snapshot: snapshot(7, [project('p1', [item(7)])]) });
    await flush();
    expect(groupKeys()).toEqual([[rowKey(8)]]);
  });
});

describe('filters', () => {
  const rows = [
    item(1, { group: 'needs_you', state: 'your_turn', reviewRequested: true }),
    item(2, { viewerIsAuthor: true, state: 'draft' }),
    item(3, { reviewRequested: true }),
  ];

  async function attached(): Promise<void> {
    installListMocks(snapshot(1, [project('p1', rows), project('p2', [item(9, {}, 'other')], { repo: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'other' } })]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
  }

  it('starts on Needs you over every project and filters by each chip', async () => {
    await attached();
    expect(mrFilters(HOME_BACKEND)).toEqual({ filter: 'needs_you', projects: [] });
    expect(groupKeys()).toEqual([[rowKey(1)]]);
    setMRFilters(HOME_BACKEND, { filter: 'mine' });
    expect(groupKeys()).toEqual([[rowKey(2)]]);
    setMRFilters(HOME_BACKEND, { filter: 'review_requested' });
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(3)]]);
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    expect(pullsRowsByGroup(PANE).map((group) => group.group)).toEqual(['needs_you', 'waiting']);
    // Across projects in the backend's order: state (draft before open),
    // then the latest update, then the higher number.
    expect(groupKeys()).toEqual([[rowKey(1)], [rowKey(2), rowKey(9, HOME_BACKEND, 'other'), rowKey(3)]]);
  });

  it('narrows to the picked projects, and lists the projects to pick', async () => {
    await attached();
    expect(pullsProjects(PANE)).toEqual([{ id: 'p1', name: 'Project p1' }, { id: 'p2', name: 'Project p2' }]);
    setMRFilters(HOME_BACKEND, { filter: 'all', projects: ['p2'] });
    expect(groupKeys()).toEqual([[rowKey(9, HOME_BACKEND, 'other')]]);
  });

  it('remembers the filters per computer and adopts the stored copy on sync', async () => {
    await attached();
    setMRFilters(HOME_BACKEND, { filter: 'mine', projects: ['p1'] });
    expect(JSON.parse(appStorageGet(MR_FILTERS_KEY, HOME_BACKEND) ?? '{}')).toEqual({ filter: 'mine', projects: ['p1'] });
    appStorageSet(MR_FILTERS_KEY, JSON.stringify({ filter: 'review_requested', projects: ['p2'] }), HOME_BACKEND);
    syncPullsFiltersFromAppStorage();
    expect(mrFilters(HOME_BACKEND)).toEqual({ filter: 'review_requested', projects: ['p2'] });
    appStorageSet(MR_FILTERS_KEY, '{"filter":"bogus","projects":[3]}', HOME_BACKEND);
    syncPullsFiltersFromAppStorage();
    expect(mrFilters(HOME_BACKEND)).toEqual({ filter: 'needs_you', projects: [] });
  });
});

describe('row projection', () => {
  it('names the viewer "you", takes CI from the detail pipeline before the checks, and words what moved', async () => {
    installListMocks(snapshot(1, [project('p1', [
      item(1, { viewerIsAuthor: true, checksState: 'failure' }),
      item(2, { checksState: 'failure', detail: { readinessState: 'blocked', blockers: [], approvalsGiven: 1, approvalsRequired: 2, unresolved: 3, checksState: 'failure', pipelineStatus: 'running', viewerApproved: false } }),
      item(3, { seen: { openedAt: 1, headSHA: 'old', comments: 0, commits: 1 }, since: { headMoved: true, newComments: 2, newCommits: 0 } }),
    ])]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    const [one, two, three] = pullsRowsByGroup(PANE)[0]!.rows.sort((a, b) => a.number - b.number);
    expect(one).toMatchObject({ author: 'you', ci: 'failed', approvals: null, unresolved: null, seen: false, projectName: 'Project p1' });
    expect(two).toMatchObject({ author: 'Alice', ci: 'running', approvals: { given: 1, required: 2 }, unresolved: 3 });
    expect(three!.seen).toBe(true);
    expect(sinceYouLookedText(three!.since)).toBe('pushed · 2 new comments since you looked');
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
  it('pushes the count of every project on each snapshot and clears it when the last page goes', async () => {
    installListMocks(snapshot(1, [project('p1', [item(1, { group: 'needs_you' })])], 1));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'mine' });
    expect(getPullsNeedsYouCount()).toBe(1);
    expect(pullsNeedsYouCount(PANE)).toBe(1);
    applyPullsSnapshot(snapshot(2, [project('p1', [item(1, { group: 'needs_you' }), item(2, { group: 'needs_you' })])], 2), HOME_BACKEND);
    expect(getPullsNeedsYouCount()).toBe(2);
    detachPullsPage(PANE);
    expect(getPullsNeedsYouCount()).toBe(0);
  });
});

describe('the focus set', () => {
  it('names every needs-you row first, then the rows on screen, at most the backend cap', async () => {
    vi.useFakeTimers({ toFake: ['setTimeout', 'clearTimeout', 'Date'] });
    const needsYou = Array.from({ length: 50 }, (_, index) => item(index + 1, { group: 'needs_you' }));
    const waiting = Array.from({ length: 20 }, (_, index) => item(index + 100));
    const mocks = installListMocks(snapshot(1, [project('p1', [...needsYou, ...waiting])]));
    attachPullsPage(PANE, HOME_BACKEND);
    await vi.advanceTimersByTimeAsync(0);
    const first = mocks.focus.mock.calls.at(-1)!;
    expect(first[0]).toBe('list-1');
    expect(first[1]).toEqual(needsYou.map((row) => prKey(ref(row.ref.Number))));

    setFocusedRows(PANE, [rowKey(119), rowKey(118), rowKey(3), ...waiting.slice(0, 15).map((row) => rowKey(row.ref.Number))]);
    await vi.advanceTimersByTimeAsync(2_000);
    const keys = mocks.focus.mock.calls.at(-1)![1] as string[];
    expect(keys).toHaveLength(PR_LIST_FOCUS_MAX);
    expect(keys.slice(0, 50)).toEqual(needsYou.map((row) => prKey(ref(row.ref.Number))));
    // On-screen rows follow in list order, past the needs-you set they
    // already include.
    expect(keys.slice(50)).toEqual(waiting.slice(0, 10).map((row) => prKey(ref(row.ref.Number))));

    const calls = mocks.focus.mock.calls.length;
    setFocusedRows(PANE, [rowKey(119), rowKey(118), rowKey(3), ...waiting.slice(0, 15).map((row) => rowKey(row.ref.Number))]);
    await vi.advanceTimersByTimeAsync(2_000);
    expect(mocks.focus.mock.calls.length).toBe(calls);
  });
});

describe('selection and viewed marks', () => {
  function installPRMocks(): ReturnType<typeof vi.fn> {
    setBindingMock('SubscribePRUpdates', async () => ({ id: 'pr-sub', prKey: prKey(ref(1)), detail: null, threads: [], headSHA: 'sha-1', seq: 1 }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
    return setBindingMock('MarkPRViewed', async (_pr: unknown, headSHA: string) => ({ openedAt: Date.now() + 1000, headSHA, comments: 4, commits: 2 }));
  }

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

  it('marks the selected PR viewed once per head while its review shows, and clears its since text at once', async () => {
    installListMocks(snapshot(1, [project('p1', [item(1, { seen: { openedAt: 1, headSHA: 'old', comments: 0, commits: 1 }, since: { headMoved: true, newComments: 1, newCommits: 0 } })])]));
    const mark = installPRMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    setMRFilters(HOME_BACKEND, { filter: 'all' });
    selectPR(PANE, rowKey(1));
    const held = attachPR(rowKey(1), { ref: ref(1) });
    await flush();
    expect(mark).toHaveBeenCalledTimes(1);
    expect(mark).toHaveBeenCalledWith({ Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 1 }, 'sha-1');
    expect(pullsRowsByGroup(PANE)[0]!.rows[0]!.since).toBeNull();

    // The same head again marks nothing; a push marks the new head.
    applyPRUpdatedEvent({ prKey: prKey(ref(1)), detail: null, threads: [], headSHA: 'sha-1', seq: 2 });
    expect(mark).toHaveBeenCalledTimes(1);
    applyPRUpdatedEvent({ prKey: prKey(ref(1)), detail: null, threads: [], headSHA: 'sha-2', seq: 3 });
    await flush();
    expect(mark).toHaveBeenCalledTimes(2);

    // Hidden, nothing is marked.
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => 'hidden' });
    applyPRUpdatedEvent({ prKey: prKey(ref(1)), detail: null, threads: [], headSHA: 'sha-3', seq: 4 });
    expect(mark).toHaveBeenCalledTimes(2);
    held.release();
  });

  it('marks nothing on the compact list screen, where the review is not showing', async () => {
    installListMocks();
    const mark = installPRMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    selectPR(PANE, rowKey(1));
    setCompactLayoutForTest(true);
    showCompactList();
    const held = attachPR(rowKey(1), { ref: ref(1) });
    await flush();
    expect(mark).not.toHaveBeenCalled();
    held.release();
    setCompactLayoutForTest(false);
  });

  it('marks nothing for a PR the pane has not selected', async () => {
    installListMocks(snapshot(1, [project('p1', [item(1), item(2)])]));
    const mark = installPRMocks();
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    selectPR(PANE, rowKey(2));
    const held = attachPR(rowKey(1), { ref: ref(1) });
    await flush();
    expect(mark).not.toHaveBeenCalled();
    held.release();
  });
});

describe('open with agent', () => {
  async function selected(overrides: Partial<PRListItem> = {}): Promise<void> {
    installListMocks(snapshot(1, [project('p1', [item(1, overrides)])]));
    attachPullsPage(PANE, HOME_BACKEND);
    await flush();
    selectPR(PANE, rowKey(1));
  }

  it('lists the threads on the PR once per snapshot and names the one holding its branch', async () => {
    await selected();
    const list = setBindingMock('ListThreadsOnPR', async () => [
      { threadId: 't1', title: 'Fix it', status: 'idle', worktreeBranch: 'feature-1', updatedAt: Date.now() - 3 * 86_400_000 },
    ]);
    await loadThreadsOnPR(PANE);
    await loadThreadsOnPR(PANE);
    expect(list).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledWith({ projectId: 'p1', pr: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 1 }, headRefName: 'feature-1', crossRepository: false });
    expect(threadsOnPR(PANE)).toEqual([{ id: 't1', title: 'Fix it', detail: 'idle · 3d ago' }]);
    expect(newThreadCheckedOutBy(PANE)?.id).toBe('t1');

    applyPullsSnapshot(snapshot(2, [project('p1', [item(1)])]), HOME_BACKEND);
    await loadThreadsOnPR(PANE);
    expect(list).toHaveBeenCalledTimes(2);
  });

  it('opens a thread on the PR head, seeds its draft, and mounts it', async () => {
    await selected();
    const open = setBindingMock('OpenPRThread', async () => ({ threadId: 't9', worktreePath: '/wt', branch: 'feature-1' }));
    setBindingMock('GetThread', async () => ({ id: 't9', projectId: 'p1', title: '', updatedAt: 1 }));
    const panes = await import('./panes.svelte');
    const mount = vi.spyOn(panes, 'mountThreadInPane').mockImplementation(async () => ({}) as never);
    expect(await openPRThread(PANE)).toBe(true);
    // In the focused pane's place: no target, so the tool pane's slot goes to it.
    expect(mount).toHaveBeenCalledWith(expect.objectContaining({ id: 't9' }));
    expect(mount.mock.calls[0]).toHaveLength(1);
    expect(open).toHaveBeenCalledWith({
      projectId: 'p1', pr: { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 1 },
      headRefName: 'feature-1', crossRepository: false, url: 'https://github.com/owner/repo/pull/1', nodeId: 'node-1', title: 'PR 1',
      seed: 'Reviewing #1: PR 1\nhttps://github.com/owner/repo/pull/1',
    });
    expect(prThreadSeed(selectedPR(PANE)!)).toBe('Reviewing #1: PR 1\nhttps://github.com/owner/repo/pull/1');
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
