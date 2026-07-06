import { beforeEach, describe, expect, it, vi } from 'vitest';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import {
  activeDiffReviewSourceForThread,
  replaceDiffReviewCommentsForTest,
} from './diffReviewComments.svelte';
import { projectTurnStarted } from './threadStatuses.svelte';
import {
  __resetReviewPaneStateForTest,
  applyPRUpdatedEvent,
  draftAnchorExists,
  disposeReviewStateForPane,
  openReviewCompanion,
  reviewStateForPane,
  reviewLineCommentForDraft,
} from './reviewPane.svelte';
import { resetCompanionPanesForTest } from './companionPanes.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import type { DiffReviewComment, PRDetail, Thread } from '../types/models';
import { diffSourceKey } from '../utils/diffSourceKey';
import { parsePatchFilesCached } from '../utils/patchFiles';
import { buildReviewRows } from '../utils/reviewRows';
import { setBindingMock } from '../../test/mocks/bindings-app';

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

async function waitLoaded(state: ReturnType<typeof reviewStateForPane>): Promise<void> {
  await vi.waitFor(() => {
    expect(state.loading).toBe(false);
  });
}

function installDefaultMocks(): void {
  // The mount/reload PR probe (probePRRef) reads both of these on every
  // state creation; the defaults resolve to "no PR anywhere".
  setBindingMock('GetThread', async () => ({ id: 'thread-1', workspacePath: '/tmp/ws' }) as Thread);
  setBindingMock('GetGitStatus', async () => ({}));
  setBindingMock('GetWorkspaceCurrentDiff', async () => '');
  setBindingMock('GetSessionAgentDiff', async () => '');
  setBindingMock('GetBranchBaseDiff', async () => '');
  setBindingMock('GetMessageCheckpointDiff', async () => '');
  setBindingMock('ListThreadCheckpoints', async () => []);
  setBindingMock('GitListBranches', async () => [{ name: 'develop', isCurrent: false, isDefault: true }]);
  setBindingMock('ListDiffReviewComments', async () => []);
  setBindingMock('CreateDiffReviewComment', async () => ({}));
  setBindingMock('UpdateDiffReviewComment', async () => ({}));
  setBindingMock('DeleteDiffReviewComment', async () => undefined);
  setBindingMock('SendDiffReviewComments', async () => ({}));
}

function draft(overrides: Partial<DiffReviewComment> = {}): DiffReviewComment {
  return {
    id: overrides.id ?? 'comment-1',
    threadId: overrides.threadId ?? 'thread-1',
    scope: overrides.scope ?? 'workspace',
    sourceKey: overrides.sourceKey ?? 'source-1',
    filePath: overrides.filePath ?? 'src/app.ts',
    status: overrides.status ?? 'draft',
    oldLine: overrides.oldLine,
    newLine: overrides.newLine ?? 1,
    side: overrides.side ?? 'new',
    selectedText: overrides.selectedText ?? 'line 1',
    body: overrides.body ?? 'comment',
    createdAt: overrides.createdAt ?? 1,
    updatedAt: overrides.updatedAt ?? 1,
  };
}

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  resetCompanionPanesForTest();
  resetPaneLayoutForTest();
  installDefaultMocks();
});

describe('reviewPane store', () => {
  it('maps draft anchors to PR review line comments', () => {
    expect(reviewLineCommentForDraft(draft({ side: 'new', newLine: 7 }))).toEqual({
      path: 'src/app.ts',
      body: 'comment',
      line: 7,
      side: 'right',
    });
    expect(reviewLineCommentForDraft(draft({ side: 'old', oldLine: 5, newLine: undefined }))).toEqual({
      path: 'src/app.ts',
      body: 'comment',
      line: 5,
      side: 'left',
    });
    expect(reviewLineCommentForDraft(draft({ side: 'context', oldLine: 3, newLine: 3 }))).toEqual({
      path: 'src/app.ts',
      body: 'comment',
      line: 3,
      side: 'right',
    });
    expect(reviewLineCommentForDraft(draft({ side: 'file', oldLine: undefined, newLine: undefined }))).toEqual({
      path: 'src/app.ts',
      body: 'comment',
      side: 'file',
    });
  });

  it('detects drafts orphaned by a vanished line', () => {
    const files = parsePatchFilesCached(patchFor('src/app.ts', 2));
    expect(draftAnchorExists(files, draft({ newLine: 2 }))).toBe(true);
    expect(draftAnchorExists(files, draft({ newLine: 99 }))).toBe(false);
  });

  it('loads the binding for each scope', async () => {
    const workspace = setBindingMock('GetWorkspaceCurrentDiff', async () => 'workspace patch');
    const session = setBindingMock('GetSessionAgentDiff', async () => 'session patch');
    const branch = setBindingMock('GetBranchBaseDiff', async () => 'branch patch');
    const message = setBindingMock('GetMessageCheckpointDiff', async () => 'turn patch');
    setBindingMock('ListThreadCheckpoints', async () => [
      { userItemId: 'old', turnIndex: 1 },
      { userItemId: 'latest', turnIndex: 3 },
    ]);
    setBindingMock('GitListBranches', async () => [
      { name: 'develop', isCurrent: false, isDefault: true },
    ]);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    expect(workspace).toHaveBeenCalledWith('thread-1');

    await state.setScope('session');
    expect(session).toHaveBeenCalledWith('thread-1');
    expect(state.patchText).toBe('session patch');

    await state.setScope('turn');
    expect(message).toHaveBeenCalledWith('thread-1', 'latest');
    expect(state.patchText).toBe('turn patch');
    expect(state.checkpoints.map((checkpoint) => checkpoint.userItemId)).toEqual(['old', 'latest']);

    await state.setScope('branch');
    expect(branch).toHaveBeenCalledWith('thread-1', 'develop');
    expect(state.baseBranch).toBe('develop');
    expect(state.patchText).toBe('branch patch');
  });

  it('persists and restores last-used scope per thread', async () => {
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    await state.setScope('branch', { baseBranch: 'release' });
    expect(appStorageGet('reviewScope:thread-1')).toBe(JSON.stringify({
      scope: 'branch',
      baseBranch: 'release',
    }));

    disposeReviewStateForPane('pane-1');
    const branch = setBindingMock('GetBranchBaseDiff', async () => 'release patch');
    const restored = reviewStateForPane('pane-2', 'thread-1');
    await waitLoaded(restored);

    expect(restored.scope).toBe('branch');
    expect(restored.baseBranch).toBe('release');
    expect(branch).toHaveBeenCalledWith('thread-1', 'release');
  });

  it('defaults first open to workspace scope', async () => {
    const workspace = setBindingMock('GetWorkspaceCurrentDiff', async () => 'workspace patch');

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.scope).toBe('workspace');
    expect(state.patchText).toBe('workspace patch');
    expect(workspace).toHaveBeenCalledTimes(1);
  });

  it('selects a specific turn checkpoint and falls back to latest when invalid', async () => {
    const message = setBindingMock(
      'GetMessageCheckpointDiff',
      async (_threadId: string, userItemId: string) => `patch ${userItemId}`,
    );
    setBindingMock('ListThreadCheckpoints', async () => [
      { userItemId: 'old', turnIndex: 1 },
      { userItemId: 'latest', turnIndex: 3 },
    ]);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    await state.selectCheckpoint('old');
    expect(state.scope).toBe('turn');
    expect(state.selectedCheckpointUserItemId).toBe('old');
    expect(state.patchText).toBe('patch old');
    expect(message).toHaveBeenLastCalledWith('thread-1', 'old');

    await state.selectCheckpoint('missing');
    expect(state.selectedCheckpointUserItemId).toBeNull();
    expect(state.patchText).toBe('patch latest');
    expect(message).toHaveBeenLastCalledWith('thread-1', 'latest');
  });

  it('openReviewCompanion applies scope, checkpoint, and pending jump target', async () => {
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', ratio: 1 }]);
    setBindingMock('GetMessageCheckpointDiff', async () => 'turn patch');
    setBindingMock('ListThreadCheckpoints', async () => [
      { userItemId: 'u1', turnIndex: 1 },
      { userItemId: 'u2', turnIndex: 2 },
    ]);

    const state = await openReviewCompanion('pane-1', 'thread-1', {
      scope: 'turn',
      checkpointUserItemId: 'u1',
      filePath: 'src/app.ts',
    });

    expect(state).not.toBeNull();
    await waitLoaded(state!);
    expect(state!.scope).toBe('turn');
    expect(state!.selectedCheckpointUserItemId).toBe('u1');
    expect(state!.pendingJumpFilePath).toBe('src/app.ts');

    const same = reviewStateForPane('pane-1', 'thread-1');
    expect(same.pendingJumpFilePath).toBe('src/app.ts');
    same.consumePendingJumpFilePath();
    expect(same.pendingJumpFilePath).toBeNull();
  });

  it('collapses lockfile-ish and large files once per load', async () => {
    const patch = [
      patchFor('src/small.ts', 2),
      patchFor('pnpm-lock.yaml', 2),
      patchFor('src/large.ts', 401),
    ].join('\n');
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.collapsedPaths.has('src/small.ts')).toBe(false);
    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(true);
    expect(state.collapsedPaths.has('src/large.ts')).toBe(true);
  });

  it('surfaces binding errors and clears them on successful reload', async () => {
    setBindingMock('GetWorkspaceCurrentDiff', async () => {
      throw new Error('diff exploded');
    });
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.error).toBe('diff exploded');
    expect(state.patchText).toBe('');

    setBindingMock('GetWorkspaceCurrentDiff', async () => 'diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n+ok');
    await state.reload();

    expect(state.error).toBeNull();
    expect(state.files).toHaveLength(1);
  });

  it('restores a persisted scope before the first load', async () => {
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'session', baseBranch: null }));
    const session = setBindingMock('GetSessionAgentDiff', async () => 'session patch');

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.scope).toBe('session');
    expect(session).toHaveBeenCalledWith('thread-1');
  });

  it('refreshes comments and records the active diff source after loading a patch', async () => {
    const patch = patchFor('src/app.ts', 1);
    const sourceKey = diffSourceKey(patch);
    const listComments = setBindingMock('ListDiffReviewComments', async () => [
      draft({ sourceKey }),
    ]);
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.sourceKey).toBe(sourceKey);
    expect(listComments).toHaveBeenCalledWith('thread-1', 'workspace', sourceKey);
    expect(activeDiffReviewSourceForThread('thread-1')).toEqual({
      threadId: 'thread-1',
      scope: 'workspace',
      sourceKey,
    });
    expect(state.comments.map((comment) => comment.id)).toEqual(['comment-1']);
  });

  it('createComment success closes the draft editor', async () => {
    const patch = patchFor('src/app.ts', 1);
    const sourceKey = diffSourceKey(patch);
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);
    setBindingMock('CreateDiffReviewComment', async () => draft({ sourceKey, body: 'Looks good.' }));
    setBindingMock('ListDiffReviewComments', async () => [draft({ sourceKey, body: 'Looks good.' })]);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    const anchor = { filePath: 'src/app.ts', side: 'new' as const, newLine: 1, selectedText: 'line 1' };
    state.openDraftEditor(anchor);
    expect(state.openEditors).toHaveLength(1);

    await state.createComment(anchor, 'Looks good.');

    expect(state.openEditors).toHaveLength(0);
    expect(state.drafts.map((comment) => comment.body)).toEqual(['Looks good.']);
  });

  it('createComment failure keeps the editor open and sets error', async () => {
    const patch = patchFor('src/app.ts', 1);
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);
    setBindingMock('CreateDiffReviewComment', async () => {
      throw new Error('create failed');
    });

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    const anchor = { filePath: 'src/app.ts', side: 'new' as const, newLine: 1, selectedText: 'line 1' };
    state.openDraftEditor(anchor);

    await expect(state.createComment(anchor, 'Body')).rejects.toThrow('create failed');

    expect(state.openEditors).toHaveLength(1);
    expect(state.error).toBe('create failed');
  });

  it('keeps draft-editor text in the store and focuses exactly once per open', async () => {
    const patch = patchFor('src/app.ts', 1);
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    const anchor = { filePath: 'src/app.ts', side: 'new' as const, newLine: 1, selectedText: 'line 1' };

    state.openDraftEditor(anchor);
    state.setDraftBody(anchor, 'half-typed comment');
    // The row component reads back through the store, so a virtualizer
    // unmount/remount cycle (which drops all row-local state) keeps the
    // user's text.
    expect(state.draftBodyFor(anchor)).toBe('half-typed comment');

    // Focus is consumed by the first mount only; a remount at the render
    // buffer's edge must not steal focus.
    expect(state.consumeDraftEditorFocus(anchor)).toBe(true);
    expect(state.consumeDraftEditorFocus(anchor)).toBe(false);

    state.closeDraftEditor(anchor);
    state.openDraftEditor(anchor);
    expect(state.draftBodyFor(anchor)).toBe('');
    expect(state.consumeDraftEditorFocus(anchor)).toBe(true);

    state.setDraftBody(anchor, 'stale text');
    await state.setScope('session');
    expect(state.draftBodyFor(anchor)).toBe('');
  });

  it('sendComments no-ops while a turn is active', async () => {
    const patch = patchFor('src/app.ts', 1);
    const sourceKey = diffSourceKey(patch);
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);
    const send = setBindingMock('SendDiffReviewComments', async () => ({}));

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    replaceDiffReviewCommentsForTest('thread-1', 'workspace', sourceKey, [
      draft({ sourceKey }),
    ]);
    projectTurnStarted('thread-1', 'turn-1', 0, 100);

    await state.sendComments();

    expect(send).not.toHaveBeenCalled();
  });
});

const PR_SOURCE_KEY = 'pr:github:owner/repo:5';

function prThreadStub(): Thread {
  // workspacePath set: a workspace-less thread with a prRef defaults straight
  // into pr scope at creation, which is its own test below.
  return {
    prRef: JSON.stringify({ Forge: 'github', Namespace: 'owner', Repo: 'repo', Number: 5 }),
    workspacePath: '/tmp/ws',
  } as Thread;
}

function prDetailStub(overrides: Partial<PRDetail> = {}): PRDetail {
  return {
    number: 5,
    title: 'PR',
    body: '',
    authorLogin: 'alice',
    state: 'open',
    draft: false,
    headRefName: 'feature',
    baseRefName: 'main',
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

function installPRMocks(): {
  subscribe: ReturnType<typeof vi.fn>;
  unsubscribe: ReturnType<typeof vi.fn>;
} {
  const subscribe = setBindingMock('SubscribePRUpdates', async (threadId: string, pr: unknown) => ({
    id: 'sub-1',
    threadId,
    pr,
    detail: prDetailStub(),
    threads: [],
    headSHA: 'sha-a',
  }));
  const unsubscribe = setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setBindingMock('GetPRDiff', async () => patchFor('src/app.ts', 3));
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));
  setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
  return { subscribe, unsubscribe };
}

describe('reviewPane store — PR scope', () => {
  it('enter subscribes and loads the PR diff; leaving unsubscribes exactly once', async () => {
    const { subscribe, unsubscribe } = installPRMocks();
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);

    await state.setScope('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(state.sourceKey).toBe(PR_SOURCE_KEY);
    expect(state.prDetail?.number).toBe(5);
    expect(state.prHeadSHA).toBe('sha-a');
    expect(state.files.length).toBe(1);

    await state.setScope('workspace');
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
  });

  it('dispose during an in-flight subscribe closes the late-arriving subscription', async () => {
    const { unsubscribe } = installPRMocks();
    let resolveSubscribe: ((value: unknown) => void) | undefined;
    setBindingMock('SubscribePRUpdates', () => new Promise((resolve) => {
      resolveSubscribe = resolve;
    }));

    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    const entering = state.setScope('pr');
    await vi.waitFor(() => {
      expect(resolveSubscribe).toBeDefined();
    });
    disposeReviewStateForPane('pane-1');
    resolveSubscribe?.({
      id: 'sub-late',
      threadId: 'thread-1',
      pr: {},
      detail: prDetailStub(),
      threads: [],
      headSHA: 'sha-a',
    });
    await entering;

    expect(unsubscribe).toHaveBeenCalledWith('sub-late');
  });

  it('replacing a pane state on thread switch disposes the old PR subscription', async () => {
    const { unsubscribe } = installPRMocks();
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    reviewStateForPane('pane-1', 'thread-2');

    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
  });

  it('pr:updated applies live on same head and flags stale on a moved head without touching the diff', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    const filesBefore = state.files;

    applyPRUpdatedEvent({
      subscriptionId: 'sub-1',
      threadId: 'thread-1',
      pr: { forge: 'github', namespace: 'owner', repo: 'repo', number: 5 },
      detail: prDetailStub({ mergeability: 'conflicts' }),
      threads: [],
      headSHA: 'sha-a',
    });
    expect(state.prStale).toBe(false);
    expect(state.prDetail?.mergeability).toBe('conflicts');

    applyPRUpdatedEvent({
      subscriptionId: 'sub-1',
      threadId: 'thread-1',
      pr: { forge: 'github', namespace: 'owner', repo: 'repo', number: 5 },
      detail: prDetailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
    });
    expect(state.prStale).toBe(true);
    expect(state.prHeadSHA).toBe('sha-b');
    expect(state.files).toBe(filesBefore);

    await state.reload();
    expect(state.prStale).toBe(false);
  });

  it('opens conflict view, keeps files collapsed, and loads file content once on expand', async () => {
    installPRMocks();
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-1',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      messages: [],
    }));
    const getFile = setBindingMock('GetMergeConflictFile', async () => [
      'before',
      '<<<<<<< ours',
      'left',
      '=======',
      'right',
      '>>>>>>> theirs',
      'after',
    ].join('\n'));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    await state.openConflictView();

    expect(state.conflictView).toBe(true);
    expect(state.conflicts?.paths).toEqual(['main.go']);
    expect(state.conflictCollapsedPaths.has('main.go')).toBe(true);
    expect(state.conflictFiles[0]?.lines).toHaveLength(0);

    await state.toggleConflictCollapsed('main.go');
    expect(getFile).toHaveBeenCalledTimes(1);
    expect(getFile).toHaveBeenCalledWith('thread-1', 'tree-1', 'main.go');
    expect(state.conflictCollapsedPaths.has('main.go')).toBe(false);
    expect(state.conflictFiles[0]?.lines.map((line) => line.type)).toEqual([
      'context',
      'meta',
      'context',
      'meta',
      'context',
      'meta',
      'context',
    ]);

    await state.toggleConflictCollapsed('main.go');
    await state.toggleConflictCollapsed('main.go');
    expect(getFile).toHaveBeenCalledTimes(1);
    expect(state.conflictCollapsedPaths.has('main.go')).toBe(false);
  });

  it('shows no-conflicts state without setting an error when mergeability was stale', async () => {
    installPRMocks();
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: false,
      treeOID: 'tree-clean',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: [],
      messages: [],
    }));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    await state.openConflictView();

    expect(state.conflictView).toBe(true);
    expect(state.conflictsError).toBeNull();
    expect(state.conflicts).toMatchObject({ baseLabel: 'origin/main', paths: [] });
    expect(state.conflictFiles).toEqual([]);
  });

  it('closes conflict view without disturbing the PR diff and resets on scope switch', async () => {
    installPRMocks();
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-1',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      messages: [],
    }));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    const filesBefore = state.files;
    state.toggleCollapsed('src/app.ts');

    await state.openConflictView();
    state.closeConflictView();

    expect(state.conflictView).toBe(false);
    expect(state.files).toBe(filesBefore);
    expect(state.collapsedPaths.has('src/app.ts')).toBe(true);

    await state.openConflictView();
    expect(state.conflicts).not.toBeNull();
    await state.setScope('workspace');
    expect(state.conflictView).toBe(false);
    expect(state.conflicts).toBeNull();
    expect(state.conflictContentByPath.size).toBe(0);
  });

  it('builds conflict rows without comment, draft, or PR thread rows', async () => {
    installPRMocks();
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-1',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      messages: [],
    }));
    setBindingMock('GetMergeConflictFile', async () => [
      '<<<<<<< ours',
      'left',
      '=======',
      'right',
      '>>>>>>> theirs',
    ].join('\n'));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    await state.openConflictView();
    await state.toggleConflictCollapsed('main.go');

    const rows = buildReviewRows({
      files: state.conflictFiles,
      viewMode: 'stacked',
      collapsedPaths: state.conflictCollapsedPaths,
      drafts: [],
      openEditors: [],
      prThreads: [],
      expandedPRThreadIds: new Set(),
    }).rows;

    expect(rows.map((row) => row.kind)).toEqual(['file-header', 'line-block']);
    expect(rows.some((row) => row.kind === 'comment-thread' || row.kind === 'draft-editor' || row.kind === 'pr-thread')).toBe(false);
  });

  it('detects an open PR from git status at mount without switching scope', async () => {
    // The scope dropdown's PR option is gated on a resolved prRef, so a
    // thread whose BRANCH has an open PR must resolve it proactively —
    // entering pr scope can't be the trigger (the option wouldn't exist).
    setBindingMock('GetGitStatus', async () => ({
      forge: 'github',
      openPrUrl: 'https://github.com/acme/widgets/pull/7',
      openPrNumber: 7,
    }));

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    await vi.waitFor(() => {
      expect(state.prRef).toEqual({ forge: 'github', namespace: 'acme', repo: 'widgets', number: 7 });
    });
    // Detection only surfaces the option; it never hijacks the scope.
    expect(state.scope).toBe('workspace');
  });

  it('detects a PR opened after mount on the next reload', async () => {
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    expect(state.prRef).toBeNull();

    setBindingMock('GetGitStatus', async () => ({
      forge: 'gitlab',
      openPrUrl: 'https://gitlab.com/group/sub/repo/-/merge_requests/3',
      openPrNumber: 3,
    }));
    await state.reload();

    await vi.waitFor(() => {
      expect(state.prRef).toEqual({ forge: 'gitlab', namespace: 'group/sub', repo: 'repo', number: 3 });
    });
  });

  it('a workspace-less thread with a prRef defaults to pr scope', async () => {
    const { subscribe } = installPRMocks();
    const state = reviewStateForPane('pane-1', 'thread-1', {
      prRef: JSON.stringify({ Forge: 'github', Namespace: 'owner', Repo: 'repo', Number: 5 }),
    } as Thread);
    await waitLoaded(state);

    expect(state.scope).toBe('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
  });

  it('restores a persisted pr scope by resolving the reference from the thread', async () => {
    const { subscribe } = installPRMocks();
    setBindingMock('GetThread', async () => prThreadStub());
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'pr' }));

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.scope).toBe('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(state.error).toBeNull();
  });

  it('submit success marks drafts sent against the head SHA and clears the summary', async () => {
    installPRMocks();
    const markSent = setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    replaceDiffReviewCommentsForTest('thread-1', 'pr', PR_SOURCE_KEY, [
      draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' }),
    ]);
    state.setSummaryBody('overall note');

    await state.submitPRReview();

    expect(markSent).toHaveBeenCalledWith('thread-1', 'pr', PR_SOURCE_KEY, ['d-1'], 'pr:sha-a');
    expect(state.summaryBody).toBe('');
    expect(state.submitError).toBeNull();
  });

  it('partial submit keeps unposted file-level drafts and marks line drafts sent', async () => {
    installPRMocks();
    setBindingMock('SubmitPRReview', async () => ({
      postedReview: true,
      postedFileComments: 0,
      partialFailurePath: 'src/app.ts',
      partialFailure: 'boom',
    }));
    const markSent = setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    replaceDiffReviewCommentsForTest('thread-1', 'pr', PR_SOURCE_KEY, [
      // Same file: the line draft rode the batched review and posted; only
      // the file-level draft (standalone follow-up call) failed.
      draft({ id: 'd-file', scope: 'pr', sourceKey: PR_SOURCE_KEY, filePath: 'src/app.ts', side: 'file', newLine: undefined }),
      draft({ id: 'd-line', scope: 'pr', sourceKey: PR_SOURCE_KEY, filePath: 'src/app.ts', newLine: 1, side: 'new' }),
    ]);

    await state.submitPRReview();

    expect(markSent).toHaveBeenCalledWith('thread-1', 'pr', PR_SOURCE_KEY, ['d-line'], 'pr:sha-a');
    expect(state.submitError).toContain('src/app.ts');
  });

  it('a pathless partial failure (approve failed after publish) marks all drafts sent and keeps the error', async () => {
    installPRMocks();
    setBindingMock('SubmitPRReview', async () => ({
      postedReview: true,
      postedFileComments: 0,
      partialFailure: 'glab api approve merge request failed',
    }));
    const markSent = setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    replaceDiffReviewCommentsForTest('thread-1', 'pr', PR_SOURCE_KEY, [
      draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' }),
    ]);
    state.setSummaryBody('overall note');

    await state.submitPRReview();

    expect(markSent).toHaveBeenCalledWith('thread-1', 'pr', PR_SOURCE_KEY, ['d-1'], 'pr:sha-a');
    expect(state.submitError).toContain('approve');
    expect(state.summaryBody).toBe('overall note');
  });

  it('a thrown submit keeps every draft and surfaces the error', async () => {
    installPRMocks();
    setBindingMock('SubmitPRReview', async () => {
      throw new Error('gh api submit review failed');
    });
    const markSent = setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    replaceDiffReviewCommentsForTest('thread-1', 'pr', PR_SOURCE_KEY, [
      draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' }),
    ]);

    await expect(state.submitPRReview()).rejects.toThrow('gh api submit review failed');

    expect(markSent).not.toHaveBeenCalled();
    expect(state.submitError).toContain('gh api submit review failed');
    expect(state.drafts.map((comment) => comment.id)).toEqual(['d-1']);
  });
});
