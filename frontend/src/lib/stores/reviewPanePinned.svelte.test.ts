// A review pinned to one PR with no thread (the Pull requests page):
// subject, retention per owner pane, the pinned scope, routing and the
// permission gate on the subject's computer, the PR-owned drafts, and the
// calls that need a chat refusing.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import { resetAppStorageForTest } from './appStorage';
import { __resetReviewPaneStateForTest, reviewStateForPane, type CommentAnchor, type ReviewPaneState } from './reviewPane.svelte';
import {
  __pinnedReviewStatesForTest,
  __resetPinnedReviewStatesForTest,
  currentPinnedReviewState,
  disposePRReviewStates,
  reviewStateForPR,
  reviewSubjectForPR,
} from './reviewPanePinned.svelte';
import { applyPRUpdatedEvent, prReviewKeys } from './prReviewStore.svelte';
import { getPRDiffReviewComments } from './prDiffReviewComments.svelte';
import { registerComposerDraft, resetComposerDraftRegistryForTest } from './composerDraftRegistry.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { __seedGitStatusForTest } from './gitStatusStore.svelte';
import { createThreadPane } from './thread.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { DisconnectedError, TransportError } from '../transport/wsClient';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { takePinnedBackend } from '../transport/backends';
import { composeWorkspaceKey } from '../utils/workspaceKey';
import { prKey, type PRRef } from '../utils/prReference';
import type { DiffReviewComment, PRDetail, PRReviewSubmitOutcome } from '../types/models';
import { installPRDraftsFake, type PRDraftsFake } from '../../test/helpers/prDraftsFake';
import { setBindingMock, setReviewDiffMock } from '../../test/mocks/bindings-app';
import { commitList } from '../../test/helpers/commitList';
import { grantBackendScopes, pairViewOnly, resetToLocalPage, revokeBackendScopes } from '../../test/helpers/scopes';
import { resetStagedBackends, stageBackend } from '../../test/helpers/backends';

const OWNER = 'tools-1';
const PROJECT = 'project-1';
const ROOT = { projectId: PROJECT, workspacePath: '' };

function ref(number: number): PRRef {
  return { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number };
}

function detail(number: number, overrides: Partial<PRDetail> = {}): PRDetail {
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
    deletions: 0,
    changedFiles: 1,
    viewerIsAuthor: false,
    reviewDecision: '',
    latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean',
    readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: true, viewerCanApprove: true, viewerApproved: false },
    ...overrides,
  };
}

// The attempt id a SubmitPRReview call carried.
function attemptOf(submit: { mock: { calls: unknown[][] } }, call: number): string | undefined {
  return (submit.mock.calls[call]?.[1] as { attemptId?: string } | undefined)?.attemptId;
}

function patchFor(path: string, lines: number): string {
  return [
    `diff --git a/${path} b/${path}`,
    'new file mode 100644',
    'index 0000000..1111111',
    '--- /dev/null',
    `+++ b/${path}`,
    `@@ -0,0 +1,${lines} @@`,
    ...Array.from({ length: lines }, (_, index) => `+line ${index + 1}`),
  ].join('\n');
}

interface PRMocks {
  subscribe: ReturnType<typeof vi.fn>;
  unsubscribe: ReturnType<typeof vi.fn>;
  diff: ReturnType<typeof vi.fn>;
  /** The backend each subscribe was routed to. */
  routed: string[];
  drafts: PRDraftsFake;
}

// One pump per PR number: the subscribe answers that PR's snapshot under
// its own subscription id.
function installPRMocks(): PRMocks {
  const routed: string[] = [];
  const subscribe = setBindingMock('SubscribePRUpdates', async (wire: { Number: number }) => {
    routed.push(String(takePinnedBackend() ?? ''));
    return {
      id: `sub-${wire.Number}`,
      prKey: prKey(ref(wire.Number)),
      detail: detail(wire.Number),
      threads: [],
      headSHA: `sha-${wire.Number}`,
      seq: 1,
    };
  });
  const unsubscribe = setBindingMock('UnsubscribePRUpdates', async () => undefined);
  const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
  setBindingMock('ListPRCommits', async () => commitList([]));
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  setBindingMock('UpdateThreadBranch', async () => []);
  const drafts = installPRDraftsFake();
  return { subscribe, unsubscribe, diff, routed, drafts };
}

// Opens an editor on `anchor` of the diff on screen and saves `body` in it.
async function commentOn(state: ReviewPaneState, anchor: CommentAnchor, body: string): Promise<void> {
  const id = state.openDraftEditor(anchor);
  if (id === null) throw new Error(`no line for ${JSON.stringify(anchor)}`);
  await state.createComment(id, body);
}

function pinned(number: number, backend = HOME_BACKEND, owner = OWNER): ReviewPaneState {
  return reviewStateForPR(owner, reviewSubjectForPR({ ownerPaneId: owner, backend, projectId: PROJECT, pr: ref(number) }));
}

async function waitLoaded(state: ReviewPaneState): Promise<void> {
  await vi.waitFor(() => {
    expect(state.loading).toBe(false);
    expect(state.prHeadSHA).not.toBe('');
  });
}

async function settle(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
}

beforeEach(() => {
  resetAppStorageForTest();
  __resetPinnedReviewStatesForTest();
  __resetReviewPaneStateForTest();
  resetComposerDraftRegistryForTest();
  resetPanesForTest();
  resetToLocalPage();
});

afterEach(() => {
  __resetPinnedReviewStatesForTest();
  __resetReviewPaneStateForTest();
  resetStagedBackends();
  resetToLocalPage();
});

describe('reviewSubjectForPR', () => {
  it('names the PR on its computer, the project root, and no thread', () => {
    const subject = reviewSubjectForPR({ ownerPaneId: OWNER, backend: 'laptop', projectId: PROJECT, pr: ref(5) });
    const key = composeWorkspaceKey('laptop', prKey(ref(5)));
    expect(subject.threadId).toBeNull();
    expect(subject.rowId).toBe(`pr:${key}`);
    expect(subject.workspace).toEqual(ROOT);
    expect(subject.pinned).toEqual({ pr: ref(5), backend: 'laptop' });
    // Two pages on one PR are two subjects; one page on two PRs too.
    const otherOwner = reviewSubjectForPR({ ownerPaneId: 'tools-2', backend: 'laptop', projectId: PROJECT, pr: ref(5) });
    const otherPR = reviewSubjectForPR({ ownerPaneId: OWNER, backend: 'laptop', projectId: PROJECT, pr: ref(6) });
    expect(new Set([subject.identity, otherOwner.identity, otherPR.identity]).size).toBe(3);
    expect(otherOwner.rowId).toBe(subject.rowId);
  });

  it('refuses a PR with no project', () => {
    expect(() => reviewSubjectForPR({ ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: ' ', pr: ref(5) }))
      .toThrow('A pull request review needs the project it belongs to.');
  });

  it('is refused by the companion registry', () => {
    const subject = reviewSubjectForPR({ ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: PROJECT, pr: ref(5) });
    expect(() => reviewStateForPane(OWNER, subject)).toThrow('reviewStateForPR');
  });
});

describe('a pinned review', () => {
  it('loads its PR in pr scope at the project root, without git status or a pane', async () => {
    const { subscribe, diff } = installPRMocks();
    const state = pinned(5);
    expect(state.pinned).toBe(true);
    expect(state.scope).toBe('pr');
    expect(state.prRef).toEqual(ref(5));
    await waitLoaded(state);
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(diff).toHaveBeenCalledWith(
      ROOT,
      expect.objectContaining({ Number: 5 }),
      { baseRef: 'main', baseSHA: 'base-main', headSHA: 'sha-5' },
      false,
      false,
    );
    expect(state.files.length).toBe(1);
    expect(state.prDetail?.number).toBe(5);
    expect(state.effectiveSubmitTarget).toBe('pr');
  });

  it('reads attended only when the page says a person opened it', async () => {
    const { diff } = installPRMocks();
    const state = reviewStateForPR(OWNER, reviewSubjectForPR({ ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: PROJECT, pr: ref(5) }), { userInitiated: true });
    await waitLoaded(state);
    expect(diff).toHaveBeenLastCalledWith(ROOT, expect.objectContaining({ Number: 5 }), expect.anything(), false, true);
  });

  it('never reads the owner pane\'s git status, even once that pane shows a thread with another PR', async () => {
    installPRMocks();
    const pane = createThreadPane({ paneId: OWNER });
    pane.replaceThread({
      id: 'thread-1', title: 'T', provider: 'claude', workspacePath: '/ws', projectPath: '/ws',
      model: 'm', createdAt: 0, updatedAt: 0, archived: false,
    });
    registerPaneForTest(OWNER, pane);
    __seedGitStatusForTest('/ws', {
      isRepo: true, branch: 'other', isDefaultBranch: false, hasChanges: false, insertions: 0, deletions: 0,
      fileCount: 0, hasUpstream: true, aheadCount: 0, behindCount: 0, hasOriginRemote: true,
      forge: 'github', openPrUrl: 'https://github.com/owner/repo/pull/7', openPrNumber: 7,
    });
    const state = pinned(5);
    await waitLoaded(state);
    expect(state.prRef?.number).toBe(5);
    expect(state.prDetail?.number).toBe(5);
  });

  it('keeps its scope pinned to pr and persists none', async () => {
    installPRMocks();
    const state = pinned(5);
    await waitLoaded(state);
    await expect(state.setScope('workspace')).rejects.toThrow('A pull request review shows only its pull request.');
    await expect(state.setScope('edits')).rejects.toThrow();
    expect(state.scope).toBe('pr');
    expect(Object.keys(localStorage).filter((key) => key.includes('reviewScope'))).toEqual([]);
  });

  it('keeps an open new-comment editor on its line when the PR head moves and its file did not change', async () => {
    installPRMocks();
    const state = pinned(5);
    await waitLoaded(state);
    const two = { filePath: 'src/app.ts', side: 'new' as const, newLine: 2, selectedText: 'line 2' };
    const id = state.openDraftEditor(two)!;
    state.setDraftBody(id, 'Guard this.');

    // A push changes another file; src/app.ts is the same blob.
    setReviewDiffMock('OpenPRDiff', async () => [
      patchFor('src/app.ts', 3),
      'diff --git a/src/other.ts b/src/other.ts',
      'new file mode 100644',
      'index 0000000..3333333',
      '--- /dev/null',
      '+++ b/src/other.ts',
      '@@ -0,0 +1,1 @@',
      '+other',
    ].join('\n'));
    applyPRUpdatedEvent({ prKey: prKey(ref(5)), detail: detail(5, { headSHA: 'sha-5b' }), threads: [], headSHA: 'sha-5b', seq: 2 });
    await tick();
    await state.reload();
    await waitLoaded(state);

    expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'placed', anchor: two, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
    expect(state.draftBodyFor(id)).toBe('Guard this.');
    await state.createComment(id, 'Guard this.');
    expect(state.drafts.map((comment) => [comment.newLine, comment.selectedText, comment.commitSha])).toEqual([[2, 'line 2', 'sha-5b']]);
  });

  it('detaches an open new-comment editor when a push changes its file, keeping its text', async () => {
    installPRMocks();
    const state = pinned(5);
    await waitLoaded(state);
    const two = { filePath: 'src/app.ts', side: 'new' as const, newLine: 2, selectedText: 'line 2' };
    const id = state.openDraftEditor(two)!;
    state.setDraftBody(id, 'Guard this.');

    // A push adds a line above it: another blob of the file.
    setReviewDiffMock('OpenPRDiff', async () => [
      'diff --git a/src/app.ts b/src/app.ts',
      'new file mode 100644',
      'index 0000000..2222222',
      '--- /dev/null',
      '+++ b/src/app.ts',
      '@@ -0,0 +1,4 @@',
      '+line 0',
      '+line 1',
      '+line 2',
      '+line 3',
    ].join('\n'));
    applyPRUpdatedEvent({ prKey: prKey(ref(5)), detail: detail(5, { headSHA: 'sha-5b' }), threads: [], headSHA: 'sha-5b', seq: 2 });
    await tick();
    await state.reload();
    await waitLoaded(state);

    expect(state.openEditors).toMatchObject([{ id, filePath: 'src/app.ts', side: 'new', state: 'detached', anchor: null }]);
    expect(state.draftBodyFor(id)).toBe('Guard this.');
    await expect(state.createComment(id, 'Guard this.')).rejects.toMatchObject({ name: 'ReviewCommentRefused', reason: 'detached' });
    expect(state.drafts).toEqual([]);
  });

  it('keeps the PR it was opened on after the forge reports it merged', async () => {
    installPRMocks();
    const state = pinned(5);
    await waitLoaded(state);
    applyPRUpdatedEvent({ prKey: prKey(ref(5)), detail: detail(5, { state: 'merged' }), threads: [], headSHA: 'sha-5', seq: 2 });
    await tick();
    expect(state.prRef?.number).toBe(5);
    expect(state.prDetail?.state).toBe('merged');
  });
});

describe('pinned retention per owner pane', () => {
  it('keeps the selected PR and the one before it, and disposes the rest', async () => {
    const { subscribe, unsubscribe } = installPRMocks();
    const five = pinned(5);
    await waitLoaded(five);
    const six = pinned(6);
    await waitLoaded(six);
    expect(__pinnedReviewStatesForTest(OWNER)).toEqual([six, five]);
    expect(prReviewKeys()).toHaveLength(2);
    expect(unsubscribe).not.toHaveBeenCalled();

    // Back to #5: the same state, its diff and subscription intact.
    expect(pinned(5)).toBe(five);
    expect(subscribe).toHaveBeenCalledTimes(2);
    expect(__pinnedReviewStatesForTest(OWNER)).toEqual([five, six]);

    // A third PR pushes the oldest (#6) out.
    const seven = pinned(7);
    await waitLoaded(seven);
    expect(__pinnedReviewStatesForTest(OWNER)).toEqual([seven, five]);
    await settle();
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(unsubscribe).toHaveBeenCalledWith('sub-6');
    expect(prReviewKeys().sort()).toEqual([prKey(ref(5)), prKey(ref(7))].map((key) => composeWorkspaceKey(HOME_BACKEND, key)).sort());

    // Stepping to #6 again creates a fresh state.
    const sixAgain = pinned(6);
    expect(sixAgain).not.toBe(six);
    await waitLoaded(sixAgain);
    expect(subscribe).toHaveBeenCalledTimes(4);
  });

  it('keeps each owner\'s retention separate and disposes an owner\'s states when its page goes', async () => {
    const { unsubscribe } = installPRMocks();
    const mine = pinned(5);
    const theirs = pinned(5, HOME_BACKEND, 'tools-2');
    expect(theirs).not.toBe(mine);
    await waitLoaded(mine);
    await waitLoaded(theirs);
    disposePRReviewStates(OWNER);
    expect(__pinnedReviewStatesForTest(OWNER)).toEqual([]);
    expect(__pinnedReviewStatesForTest('tools-2')).toEqual([theirs]);
    // One pump per PR: the other page still holds it.
    await settle();
    expect(unsubscribe).not.toHaveBeenCalled();
    disposePRReviewStates('tools-2');
    await settle();
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(prReviewKeys()).toEqual([]);
  });

  it('wakes a reader of the current state when the owner steps between PRs', async () => {
    const { drafts: fake } = installPRMocks();
    fake.held.set(6, [{
      id: 'kept', threadId: '', scope: 'pr', sourceKey: `pr:${prKey(ref(6))}`, commitSha: 'sha-6', filePath: 'src/app.ts',
      status: 'draft', newLine: 2, side: 'new', selectedText: 'line 2', body: 'on six', createdAt: 1, updatedAt: 1,
    } as DiffReviewComment]);
    let count = -1;
    let current: ReviewPaneState | null = null;
    const stop = $effect.root(() => {
      $effect(() => {
        current = currentPinnedReviewState(OWNER);
        count = current?.drafts.length ?? 0;
      });
    });
    try {
      flushSync();
      expect(count).toBe(0);
      const five = pinned(5);
      await waitLoaded(five);
      flushSync();
      expect(current).toBe(five);
      expect(count).toBe(0);
      const six = pinned(6);
      await waitLoaded(six);
      await vi.waitFor(() => expect(six.drafts).toHaveLength(1));
      flushSync();
      expect(current).toBe(six);
      expect(count).toBe(1);
      expect(pinned(5)).toBe(five);
      flushSync();
      expect(current).toBe(five);
      expect(count).toBe(0);
      disposePRReviewStates(OWNER);
      flushSync();
      expect(current).toBeNull();
    } finally {
      stop();
    }
  });
});

describe('pinned routing and grants', () => {
  it('holds no PR without git:operate on its computer', async () => {
    const { subscribe } = installPRMocks();
    await pairViewOnly();
    const state = pinned(5);
    await settle();
    expect(subscribe).not.toHaveBeenCalled();
    expect(state.commentsGranted).toBe(false);
  });

  it('routes and checks the computer that listed the PR, not this page\'s home', async () => {
    stageBackend();
    await grantBackendScopes('laptop', ['threads:read', 'git:operate']);
    await pairViewOnly();
    const { subscribe, routed, drafts } = installPRMocks();
    const state = pinned(5, 'laptop');
    await waitLoaded(state);
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(routed).toEqual(['laptop']);
    expect(state.backend).toBe('laptop');
    expect(state.commentsGranted).toBe(true);
    await commentOn(state, { filePath: 'src/app.ts', newLine: 2, side: 'new', selectedText: 'line 2' }, 'here');
    expect(new Set(drafts.routed)).toEqual(new Set(['laptop']));
    revokeBackendScopes('laptop');
  });

  it('does not borrow this page\'s grant for a computer that lacks it', async () => {
    stageBackend();
    await grantBackendScopes('laptop', ['threads:read']);
    const { subscribe } = installPRMocks();
    const state = pinned(5, 'laptop');
    await settle();
    expect(subscribe).not.toHaveBeenCalled();
    expect(state.commentsGranted).toBe(false);
    revokeBackendScopes('laptop');
  });
});

describe('pinned comments and actions', () => {
  const anchor = { filePath: 'src/app.ts', newLine: 2, side: 'new' as const, selectedText: 'line 2' };

  it('drafts under the PR through its own bindings and posts them as a PR review', async () => {
    const { drafts: fake } = installPRMocks();
    const create = setBindingMock('CreateDiffReviewComment', async () => ({}));
    const mark = setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
    const submit = setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));
    const state = pinned(5);
    await waitLoaded(state);
    expect(state.canDraftComments).toBe(true);

    await commentOn(state, anchor, '  needs a test  ');
    expect(create).not.toHaveBeenCalled();
    expect(fake.create).toHaveBeenCalledWith(
      { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 5 },
      expect.objectContaining({ scope: 'pr', sourceKey: `pr:${prKey(ref(5))}`, body: 'needs a test', commitSha: 'sha-5' }),
    );
    expect(state.drafts.map((comment) => comment.body)).toEqual(['needs a test']);
    expect(state.drafts[0]?.commitSha).toBe('sha-5');
    await state.updateComment(state.drafts[0]!.id, 'needs two tests');
    expect(state.drafts[0]?.body).toBe('needs two tests');

    await state.submitPRReview();
    expect(submit).toHaveBeenCalledWith(
      { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 5 },
      { verdict: 'comment', body: '', comments: [{ path: 'src/app.ts', body: 'needs two tests', line: 2, side: 'right' }] },
    );
    expect(mark).not.toHaveBeenCalled();
    expect(fake.mark).toHaveBeenCalledWith(
      { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 5 },
      'pr', `pr:${prKey(ref(5))}`, ['c1'],
    );
    expect(state.drafts).toEqual([]);
    const held = getPRDiffReviewComments(composeWorkspaceKey(HOME_BACKEND, prKey(ref(5))), 'pr', `pr:${prKey(ref(5))}`);
    expect(held.map((comment) => comment.status)).toEqual(['sent']);
  });

  describe('on a backend with the draft submit', () => {
    beforeEach(() => __setTransportHelloForTest({ launchId: 'l', protocolVersion: 1, capabilities: ['pr-review-drafts.v1'], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 }));
    afterEach(() => __setTransportHelloForTest(null));

    // The backend's draft submit over the PR's drafts, recording each
    // attempt's outcome: it refuses drafts that are not drafts (or, with
    // `unmarked`, drafts an earlier review posted), else posts and marks
    // them; `lose` drops the answer after (or, 'never', before) it ran.
    // `unconfirmed` answers as a forge that did not confirm the review.
    function draftBackend(fake: PRDraftsFake, opts: { lose?: 'after' | 'never'; unmarked?: boolean; unconfirmed?: boolean } = {}) {
      const outcomes = new Map<string, PRReviewSubmitOutcome>();
      let lose = opts.lose;
      const submit = setBindingMock('SubmitPRReview', async (wire: { Number: number }, review: { drafts?: { ids: string[] }; attemptId?: string }) => {
        const losing = lose;
        lose = undefined;
        if (losing === 'never') throw new DisconnectedError('socket closed');
        const rows = fake.held.get(wire.Number) ?? [];
        const ids = review.drafts?.ids ?? [];
        let outcome: PRReviewSubmitOutcome;
        if (opts.unconfirmed) {
          outcome = { state: 'unconfirmed', result: { postedReview: false, postedFileComments: 0, unconfirmed: true } };
        } else if (opts.unmarked && ids.length > 0) {
          outcome = { state: 'failed', code: 'review_drafts_unmarked', message: 'These comments were posted by an earlier review and are being marked sent, so this review was not posted.' };
        } else if (ids.some((id) => rows.find((candidate) => candidate.id === id)?.status !== 'draft')) {
          outcome = { state: 'failed', code: 'review_drafts_not_drafts', message: 'Some of these comments were already sent or removed, so this review was not posted.' };
        } else {
          for (const candidate of rows) if (ids.includes(candidate.id)) candidate.status = 'sent';
          outcome = { state: 'posted', result: { postedReview: true, postedFileComments: 0, sentDraftIds: ids, posted: true } };
        }
        if (review.attemptId) outcomes.set(review.attemptId, outcome);
        if (losing === 'after') throw new DisconnectedError('socket closed');
        if (outcome.state === 'failed') throw new TransportError(outcome.code!, outcome.message!);
        return outcome.result;
      });
      const ask = setBindingMock('GetPRReviewSubmitOutcome', async (_wire: unknown, id: string) => outcomes.get(id) ?? { state: 'unknown' });
      return { submit, ask };
    }

    it('names the PR’s drafts, and after a lost answer asks the backend and ends as posted', async () => {
      const { drafts: fake } = installPRMocks();
      const { submit, ask } = draftBackend(fake, { lose: 'after' });
      const state = pinned(5);
      await waitLoaded(state);
      await commentOn(state, anchor, 'needs a test');
      state.setSummaryBody('overall');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

      expect(submit.mock.calls[0]?.[1]).toEqual({ verdict: 'comment', body: 'overall', comments: [], drafts: { threadId: undefined, ids: ['c1'], headSha: 'sha-5' }, attemptId: expect.any(String) });
      expect(ask).toHaveBeenCalledWith(expect.anything(), attemptOf(submit, 0));
      expect(fake.mark).not.toHaveBeenCalled();
      expect(state.drafts).toEqual([]);
      expect(state.summaryBody).toBe('');
      expect(state.submitError).toBeNull();
    });

    it('a second tab refused because drafts another tab posted are not marked yet keeps its summary and verdict', async () => {
      const { drafts: fake } = installPRMocks();
      draftBackend(fake, { unmarked: true });
      const state = pinned(5);
      await waitLoaded(state);
      await commentOn(state, anchor, 'needs a test');
      state.setSummaryBody('from tab B');
      state.setVerdict('request-changes');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(TransportError);

      expect(state.summaryBody).toBe('from tab B');
      expect(state.verdict).toBe('request-changes');
      expect(state.submitError).toBe('These comments were posted by an earlier review and are being marked sent, so this review was not posted.');
    });

    it('a second tab whose refusal answer was lost, the first tab having sent the drafts, keeps its summary and verdict', async () => {
      const { drafts: fake } = installPRMocks();
      const { submit } = draftBackend(fake, { lose: 'after' });
      const state = pinned(5);
      await waitLoaded(state);
      await commentOn(state, anchor, 'needs a test');
      state.setSummaryBody('from tab B');
      state.setVerdict('request-changes');
      // Tab A sent them first.
      for (const row of fake.held.get(5) ?? []) row.status = 'sent';

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

      expect(state.summaryBody).toBe('from tab B');
      expect(state.verdict).toBe('request-changes');
      expect(state.submitError).toBe('Some of these comments were already sent, so this review and its summary were not posted.');
      expect(state.drafts).toEqual([]);
      await state.submitPRReview();
      expect(submit.mock.calls[1]?.[1]).toMatchObject({ verdict: 'request-changes', body: 'from tab B', drafts: { ids: [] } });
    });

    it('keeps the PR’s drafts, summary and verdict when the forge did not confirm the review, and says it may be on the PR', async () => {
      const { drafts: fake } = installPRMocks();
      const { submit } = draftBackend(fake, { unconfirmed: true });
      const state = pinned(5);
      await waitLoaded(state);
      await commentOn(state, anchor, 'needs a test');
      state.setSummaryBody('overall');
      state.setVerdict('approve');

      await state.submitPRReview();

      expect(state.drafts.map((comment) => comment.id)).toEqual(['c1']);
      expect(state.summaryBody).toBe('overall');
      expect(state.verdict).toBe('approve');
      expect(state.submitError).toBe('The forge did not confirm the review, so it may already be on the PR. Check the PR before you submit again.');
      expect(fake.mark).not.toHaveBeenCalled();
      expect(submit).toHaveBeenCalledTimes(1);
    });

    it('keeps the PR’s drafts and summary when the lost request never reached the backend', async () => {
      const { drafts: fake } = installPRMocks();
      draftBackend(fake, { lose: 'never' });
      const state = pinned(5);
      await waitLoaded(state);
      await commentOn(state, anchor, 'needs a test');
      state.setSummaryBody('overall');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

      expect(state.drafts.map((comment) => comment.id)).toEqual(['c1']);
      expect(state.summaryBody).toBe('overall');
      expect(state.submitError).toBe('The connection dropped before the review reached the computer, so nothing was posted.');
    });
  });

  it('sends its drafts to a thread with their hunks, settling them, and only on the whole PR', async () => {
    const { drafts: fake } = installPRMocks();
    const state = pinned(5);
    await waitLoaded(state);
    await commentOn(state, anchor, 'look here');
    const thread = await state.sendDraftsToThread('thread-9');
    expect(thread).toEqual({ id: 'thread-9' });
    const [wire, threadId, scope, sourceKey, ids, input] = fake.send.mock.calls[0]!;
    expect(wire).toEqual({ Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 5 });
    expect([threadId, scope, sourceKey, ids]).toEqual(['thread-9', 'pr', `pr:${prKey(ref(5))}`, ['c1']]);
    expect(input.pr).toMatchObject({ number: 5, url: 'https://github.com/owner/repo/pull/5', comments: [{ commentId: 'c1' }] });
    expect(input.pr.comments[0].hunkExcerpt).toContain('line 2');
    expect(state.drafts).toEqual([]);
    expect(await state.sendDraftsToThread('thread-9')).toBeNull();
    expect(fake.send).toHaveBeenCalledTimes(1);
  });

  it('reads the drafts the PR already holds when it loads', async () => {
    const { drafts: fake } = installPRMocks();
    fake.held.set(5, [{
      id: 'kept', threadId: '', scope: 'pr', sourceKey: `pr:${prKey(ref(5))}`, commitSha: 'sha-5', filePath: 'src/app.ts',
      status: 'draft', newLine: 2, side: 'new', selectedText: 'line 2', body: 'from before', createdAt: 1, updatedAt: 1,
    } as DiffReviewComment]);
    const state = pinned(5);
    await waitLoaded(state);
    await vi.waitFor(() => expect(state.drafts.map((comment) => comment.body)).toEqual(['from before']));
  });

  it('keeps drafts across the state being disposed by the retention bound', async () => {
    installPRMocks();
    const five = pinned(5);
    await waitLoaded(five);
    await commentOn(five, anchor, 'keep me');
    await waitLoaded(pinned(6));
    await waitLoaded(pinned(7));
    expect(__pinnedReviewStatesForTest(OWNER)).not.toContain(five);
    const back = pinned(5);
    await waitLoaded(back);
    expect(back.drafts.map((comment) => comment.body)).toEqual(['keep me']);
    await back.deleteComment(back.drafts[0]!.id);
    expect(back.drafts).toEqual([]);
  });

  it('takes no comment on a picked commit, and sends nothing to a chat', async () => {
    installPRMocks();
    const sha = 'c'.repeat(40);
    setBindingMock('ListPRCommits', async () => commitList([{ sha, shortSha: 'ccccccc', subject: 's', author: 'a', authoredAt: 1 }]));
    setReviewDiffMock('OpenPRCommitDiff', async () => patchFor('src/app.ts', 2));
    const send = setBindingMock('SendDiffReviewComments', async () => ({}));
    const state = pinned(5);
    await waitLoaded(state);
    await commentOn(state, anchor, 'on the head diff');
    await expect(state.sendComments()).rejects.toThrow('This review has no chat to send to.');
    expect(send).not.toHaveBeenCalled();

    await state.selectCommit(sha);
    expect(state.canDraftComments).toBe(false);
    expect(state.drafts).toEqual([]);
    await expect(commentOn(state, anchor, 'on the commit')).rejects.toThrow('pick All commits');
    await state.selectCommit(null);
    expect(state.drafts.map((comment) => comment.body)).toEqual(['on the head diff']);
  });

  it('refuses CI to chat even when its owner pane has a composer', async () => {
    installPRMocks();
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-5', prKey: prKey(ref(5)), detail: detail(5), threads: [], headSHA: 'sha-5', seq: 1,
      ci: { status: 'failed', stages: [{ name: 'test', status: 'failed', jobs: [{ id: '20', name: 'unit', status: 'failed', logsAvailable: true }] }] },
      ciError: '',
    }));
    const save = setBindingMock('SavePRCIJobLog', async () => '/data/log');
    let content = 'mine';
    const dispose = registerComposerDraft(OWNER, {
      get content() { return content; },
      setContent(next: string) { content = next; },
    } as never);
    try {
      const state = pinned(5);
      await waitLoaded(state);
      state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
      await state.sendCILogToChat();
      expect(state.ciLogError).toBe('This review has no chat to send to.');
      expect(save).not.toHaveBeenCalled();
      expect(content).toBe('mine');
    } finally {
      dispose();
    }
  });

  it('runs the Merge section through the store on its PR and computer', async () => {
    installPRMocks();
    const approve = setBindingMock('ApprovePR', async () => {
      expect(takePinnedBackend()).toBe(HOME_BACKEND);
      return { seq: 2 };
    });
    const merge = setBindingMock('MergePR', async () => ({ seq: 3, outcome: { merged: true, autoMerge: false } }));
    const state = pinned(5);
    await waitLoaded(state);
    expect(state.mergeWithheldReason).toBeNull();
    await state.approvePR();
    expect(approve).toHaveBeenCalledWith({ Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 5 }, 'sha-5');
    expect(state.mergeAction).toBe('approve');
    applyPRUpdatedEvent({ prKey: prKey(ref(5)), detail: detail(5), threads: [], headSHA: 'sha-5', seq: 2 });
    expect(state.mergeAction).toBeNull();
    await state.mergePR({ method: 'merge', deleteBranch: false, autoMerge: false });
    expect(merge).toHaveBeenCalledWith(
      { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 5 },
      { method: 'merge', headSHA: 'sha-5', deleteBranch: false, autoMerge: false },
    );
  });
});
