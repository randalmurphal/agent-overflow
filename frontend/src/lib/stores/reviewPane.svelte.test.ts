import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
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
import { registerComposerDraft, resetComposerDraftRegistryForTest } from './composerDraftRegistry.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import type { DiffReviewComment, PRDetail, Thread } from '../types/models';
import { diffSourceKey } from '../utils/diffSourceKey';
import { expansionPredecessor } from '../utils/diffContextExpansion';
import { filePatchDisplayRows, parsePatchFilesCached } from '../utils/patchFiles';
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
  setBindingMock('GetBranchBaseDiff', async () => '');
  setBindingMock('ListBranchCommits', async () => []);
  setBindingMock('GetCommitDiff', async () => '');
  setBindingMock('ListPRCommits', async () => []);
  setBindingMock('GetPRCommitDiff', async () => '');
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
  resetComposerDraftRegistryForTest();
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
    const branch = setBindingMock('GetBranchBaseDiff', async () => 'branch patch');
    setBindingMock('ListBranchCommits', async () => [
      { sha: 'a'.repeat(40), shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
      { sha: 'b'.repeat(40), shortSha: 'bbbbbbb', subject: 'second', author: 'r', authoredAt: 2 },
    ]);
    setBindingMock('GitListBranches', async () => [
      { name: 'develop', isCurrent: false, isDefault: true },
    ]);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    expect(workspace).toHaveBeenCalledWith('thread-1');

    await state.setScope('branch');
    expect(branch).toHaveBeenCalledWith('thread-1', 'develop');
    expect(state.baseBranch).toBe('develop');
    expect(state.patchText).toBe('branch patch');
    expect(state.commits.map((commit) => commit.shortSha)).toEqual(['aaaaaaa', 'bbbbbbb']);
    expect(state.selectedCommitSHA).toBeNull();
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

  it('selects a single commit in branch scope and keys comments by its SHA', async () => {
    const sha = 'a'.repeat(40);
    setBindingMock('GetBranchBaseDiff', async () => 'branch patch');
    setBindingMock('ListBranchCommits', async () => [
      { sha, shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    const commitDiff = setBindingMock('GetCommitDiff', async () => 'commit patch');

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('branch');

    await state.selectCommit(sha);
    expect(state.scope).toBe('branch');
    expect(state.selectedCommitSHA).toBe(sha);
    expect(state.patchText).toBe('commit patch');
    expect(state.sourceKey).toBe(`commit:${sha}`);
    expect(commitDiff).toHaveBeenLastCalledWith('thread-1', sha);

    // Back to the full range.
    await state.selectCommit(null);
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.patchText).toBe('branch patch');
  });

  it('drops a selected commit that left the range and reloads the full diff', async () => {
    const sha = 'a'.repeat(40);
    setBindingMock('GetBranchBaseDiff', async () => 'branch patch');
    setBindingMock('ListBranchCommits', async () => [
      { sha, shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setBindingMock('GetCommitDiff', async () => 'commit patch');

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('branch');
    await state.selectCommit(sha);
    expect(state.patchText).toBe('commit patch');

    // Rebase: the SHA is gone from base..HEAD.
    setBindingMock('ListBranchCommits', async () => []);
    await state.reload();
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.patchText).toBe('branch patch');
  });

  it('resets the selected commit when the scope changes', async () => {
    const sha = 'a'.repeat(40);
    setBindingMock('GetBranchBaseDiff', async () => 'branch patch');
    setBindingMock('ListBranchCommits', async () => [
      { sha, shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setBindingMock('GetCommitDiff', async () => 'commit patch');

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('branch');
    await state.selectCommit(sha);
    expect(state.selectedCommitSHA).toBe(sha);

    await state.setScope('workspace');
    expect(state.selectedCommitSHA).toBeNull();
  });

  it('openReviewCompanion applies scope and pending jump target', async () => {
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 1 }]);
    setBindingMock('GetBranchBaseDiff', async () => 'branch patch');

    const state = await openReviewCompanion('pane-1', 'thread-1', {
      scope: 'branch',
      filePath: 'src/app.ts',
    });

    expect(state).not.toBeNull();
    await waitLoaded(state!);
    expect(state!.scope).toBe('branch');
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

  it('jumpToComment expands the file and thread and stages the row-key jump', async () => {
    const patch = [patchFor('src/small.ts', 2), patchFor('pnpm-lock.yaml', 2)].join('\n');
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(true);

    state.jumpToComment({
      rowKey: 'pt:thread-9',
      kind: 'pr-thread',
      threadId: 'thread-9',
      filePath: 'pnpm-lock.yaml',
      line: 1,
      author: 'alice',
      snippet: 'hm',
      state: 'unresolved',
      orphaned: false,
      inDiff: true,
      replies: 0,
      createdAtMs: null,
      comments: [{ author: 'alice', body: 'hm' }],
    });

    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(false);
    expect(state.expandedPRThreadIds.has('thread-9')).toBe(true);
    expect(state.pendingJumpRowKey).toBe('pt:thread-9');

    state.consumePendingJumpRowKey();
    expect(state.pendingJumpRowKey).toBeNull();

    // Items on files outside the diff have no row to jump to.
    state.jumpToComment({
      rowKey: 'pt:thread-10',
      kind: 'pr-thread',
      threadId: 'thread-10',
      filePath: 'gone.ts',
      line: null,
      author: 'alice',
      snippet: '',
      state: 'unresolved',
      orphaned: false,
      inDiff: false,
      replies: 0,
      createdAtMs: null,
      comments: [],
    });
    expect(state.pendingJumpRowKey).toBeNull();
  });

  it('toggleCollapseAll flips every file and allCollapsed tracks the set', async () => {
    const patch = [patchFor('src/small.ts', 2), patchFor('pnpm-lock.yaml', 2)].join('\n');
    setBindingMock('GetWorkspaceCurrentDiff', async () => patch);

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    expect(state.allCollapsed).toBe(false);

    await state.toggleCollapseAll();
    expect(state.allCollapsed).toBe(true);
    expect(state.collapsedPaths.has('src/small.ts')).toBe(true);
    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(true);

    // Expand-all clears the default lockfile collapse too.
    await state.toggleCollapseAll();
    expect(state.allCollapsed).toBe(false);
    expect(state.collapsedPaths.size).toBe(0);
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
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'branch', baseBranch: 'develop' }));
    const branch = setBindingMock('GetBranchBaseDiff', async () => 'branch patch');

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.scope).toBe('branch');
    expect(branch).toHaveBeenCalledWith('thread-1', 'develop');
  });

  it('falls back to workspace scope for a persisted scope that no longer exists', async () => {
    // 'turn' and 'session' were removed with the checkpoint machinery;
    // stale persisted entries must not wedge the pane.
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'turn', baseBranch: null }));
    const workspace = setBindingMock('GetWorkspaceCurrentDiff', async () => 'workspace patch');

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    expect(state.scope).toBe('workspace');
    expect(workspace).toHaveBeenCalledWith('thread-1');
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
    await state.setScope('branch');
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
  setBindingMock('GetPRCIJobs', async () => ({ status: '', stages: [] }));
  setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));
  setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
  return { subscribe, unsubscribe };
}

describe('reviewPane store — PR scope', () => {
  it('enter subscribes and loads the PR diff; leaving unsubscribes exactly once', async () => {
    const { subscribe, unsubscribe } = installPRMocks();
    const diff = setBindingMock('GetPRDiff', async () => patchFor('src/app.ts', 3));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);

    await state.setScope('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
    // The diff is fetched with the thread id + base ref from the detail so
    // the backend can compute a local diff (past gh/glab's 20k-line cap).
    expect(diff).toHaveBeenCalledWith(
      'thread-1',
      expect.objectContaining({ Number: 5 }),
      'main',
    );
    expect(state.sourceKey).toBe(PR_SOURCE_KEY);
    expect(state.prDetail?.number).toBe(5);
    expect(state.prHeadSHA).toBe('sha-a');
    expect(state.files.length).toBe(1);

    await state.setScope('workspace');
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
  });

  it('selects a single PR commit and keys comments by its SHA', async () => {
    const sha = 'b'.repeat(40);
    installPRMocks();
    const list = setBindingMock('ListPRCommits', async () => [
      { sha, shortSha: 'bbbbbbb', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    const commitDiff = setBindingMock('GetPRCommitDiff', async () => patchFor('src/app.ts', 2));

    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    // The known head SHA rides along so the backend can skip its fetch
    // when the objects are already in the local clone.
    expect(list).toHaveBeenCalledWith('thread-1', expect.objectContaining({ Number: 5 }), 'main', 'sha-a');
    expect(state.commits.map((commit) => commit.sha)).toEqual([sha]);
    expect(state.sourceKey).toBe(PR_SOURCE_KEY);

    await state.selectCommit(sha);
    expect(state.selectedCommitSHA).toBe(sha);
    expect(state.sourceKey).toBe(`commit:${sha}`);
    expect(commitDiff).toHaveBeenLastCalledWith('thread-1', expect.objectContaining({ Number: 5 }), sha);

    // Back to the whole PR.
    await state.selectCommit(null);
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.sourceKey).toBe(PR_SOURCE_KEY);
  });

  it('commit selection reuses the live subscription and commit list (fast path)', async () => {
    const sha = 'b'.repeat(40);
    const { subscribe, unsubscribe } = installPRMocks();
    const list = setBindingMock('ListPRCommits', async () => [
      { sha, shortSha: 'bbbbbbb', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setBindingMock('GetPRCommitDiff', async () => patchFor('src/app.ts', 2));

    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledTimes(1);

    await state.selectCommit(sha);
    await state.selectCommit(null);
    // Picking a commit only refetches the diff — no re-subscribe, no
    // commit-list round-trip, and the pump stays live throughout.
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(list).toHaveBeenCalledTimes(1);
    expect(unsubscribe).not.toHaveBeenCalled();
  });

  it('single-commit view forces the agent target and blocks PR submission', async () => {
    const sha = 'b'.repeat(40);
    installPRMocks();
    setBindingMock('ListPRCommits', async () => [
      { sha, shortSha: 'bbbbbbb', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setBindingMock('GetPRCommitDiff', async () => patchFor('src/app.ts', 2));
    const submit = setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));

    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    state.setSubmitTarget('pr');
    state.setSummaryBody('looks good');
    expect(state.effectiveSubmitTarget).toBe('pr');

    await state.selectCommit(sha);
    // Drafts on a commit diff carry that diff's line numbers — the forge
    // would anchor them against the PR head diff, so PR submission is off.
    expect(state.effectiveSubmitTarget).toBe('agent');
    await state.submitPRReview();
    expect(submit).not.toHaveBeenCalled();

    await state.selectCommit(null);
    expect(state.effectiveSubmitTarget).toBe('pr');
  });

  it('drops a selected PR commit that left the range (force-push) and reloads the full diff', async () => {
    const sha = 'b'.repeat(40);
    installPRMocks();
    setBindingMock('ListPRCommits', async () => [
      { sha, shortSha: 'bbbbbbb', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setBindingMock('GetPRCommitDiff', async () => patchFor('src/app.ts', 2));
    const fullDiff = setBindingMock('GetPRDiff', async () => patchFor('src/app.ts', 3));

    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    await state.selectCommit(sha);
    expect(state.selectedCommitSHA).toBe(sha);

    setBindingMock('ListPRCommits', async () => []);
    await state.reload();
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.sourceKey).toBe(PR_SOURCE_KEY);
    expect(fullDiff).toHaveBeenCalled();
  });

  it('shows no PR commit selector without a local clone (empty commit list)', async () => {
    installPRMocks(); // default ListPRCommits mock resolves []
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    expect(state.commits).toEqual([]);
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.files.length).toBe(1);
  });

  it('switching scope away mid-PR-load lands on the new scope and closes the late subscription', async () => {
    // The scope selector stays enabled while a PR loads, so a slow
    // gh/glab call must not pin the pane: the superseded load's
    // subscription has to be closed when it finally resolves.
    const { unsubscribe } = installPRMocks();
    let releaseDiff!: (patch: string) => void;
    setBindingMock('GetPRDiff', () => new Promise<string>((resolve) => {
      releaseDiff = resolve;
    }));
    setBindingMock('GetWorkspaceCurrentDiff', async () => patchFor('src/app.ts', 2));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);

    const prSwitch = state.setScope('pr'); // hangs on the PR diff
    await vi.waitFor(() => {
      if (!releaseDiff) throw new Error('PR diff not requested yet');
    });
    await state.setScope('workspace');
    expect(state.scope).toBe('workspace');
    expect(state.loading).toBe(false);

    releaseDiff(patchFor('src/app.ts', 3));
    await prSwitch;
    // The stale load's subscription has no owner — it must be closed,
    // and the pane must still be on the scope the user picked.
    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
    expect(state.scope).toBe('workspace');
  });

  it('a diff failure after subscribe unsubscribes and surfaces the error', async () => {
    const { unsubscribe } = installPRMocks();
    setBindingMock('GetPRDiff', async () => {
      throw new Error('diff exploded');
    });
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);

    await state.setScope('pr');
    expect(state.error).toContain('diff exploded');
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

  it('opens conflict view expanded with file content loaded once', async () => {
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

    // Conflict files open expanded like the regular diff: the open
    // itself loads content and clears the collapsed set.
    expect(state.conflictView).toBe(true);
    expect(state.conflicts?.paths).toEqual(['main.go']);
    expect(getFile).toHaveBeenCalledTimes(1);
    expect(getFile).toHaveBeenCalledWith('thread-1', 'tree-1', 'main.go');
    expect(state.conflictCollapsedPaths.has('main.go')).toBe(false);
    // Pseudo-diff shape: hunk header, then ours as del / theirs as add
    // between visible marker rows, relabeled with base/head labels.
    expect(state.conflictFiles[0]?.lines.map((line) => line.type)).toEqual([
      'meta',
      'context',
      'marker',
      'del',
      'marker',
      'add',
      'marker',
      'context',
    ]);
    expect(state.conflictFiles[0]?.lines[2]?.content).toBe('<<<<<<< origin/main');
    expect(state.conflictFiles[0]?.lines[6]?.content).toBe('>>>>>>> feature');
    expect(state.conflictFiles[0]?.conflicts).toBe(1);

    // Collapse/re-expand round-trip reuses the cached content.
    await state.toggleConflictCollapsed('main.go');
    expect(state.conflictCollapsedPaths.has('main.go')).toBe(true);
    await state.toggleConflictCollapsed('main.go');
    expect(getFile).toHaveBeenCalledTimes(1);
    expect(state.conflictCollapsedPaths.has('main.go')).toBe(false);
  });

  it('toggleCollapseAll in conflict view reuses loaded content', async () => {
    installPRMocks();
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-1',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go', 'util.go'],
      messages: [],
    }));
    const getFile = setBindingMock('GetMergeConflictFile', async () => [
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

    // Both files load during open (default-expanded).
    expect(getFile).toHaveBeenCalledTimes(2);
    expect(state.allCollapsed).toBe(false);
    expect(state.conflictCollapsedPaths.size).toBe(0);

    await state.toggleCollapseAll();
    expect(state.allCollapsed).toBe(true);

    // Expand-all reuses the cached content — no refetch.
    await state.toggleCollapseAll();
    expect(state.allCollapsed).toBe(false);
    expect(getFile).toHaveBeenCalledTimes(2);
  });

  it('renders per-path notes in their file and expands note-only files whose content is unfetchable', async () => {
    installPRMocks();
    const note = 'CONFLICT (modify/delete): other.go deleted in origin/main and modified in feature.';
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: true,
      treeOID: 'tree-1',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go', 'other.go'],
      notes: { 'other.go': [note] },
      messages: ['warning: unattributable message'],
    }));
    setBindingMock('GetMergeConflictFile', async (_thread: string, _tree: string, path: string) => {
      if (path === 'other.go') throw new Error('path not in merged tree');
      return '<<<<<<< ours\nleft\n=======\nright\n>>>>>>> theirs';
    });
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    await state.openConflictView();

    // Only unattributed messages stay on the fallback strip.
    expect(state.conflicts?.messages).toEqual(['warning: unattributable message']);

    // The note-bearing file expands despite the failed content load: its
    // body is the note row, and the badge carries the conflict type.
    expect(state.conflictCollapsedPaths.has('other.go')).toBe(false);
    const structural = state.conflictFiles.find((file) => file.path === 'other.go');
    expect(structural?.lines).toEqual([{ content: note, type: 'marker' }]);
    expect(structural?.conflictLabel).toBe('modify/delete');
    // The load failure is not swallowed.
    expect(state.conflictsError).toContain('path not in merged tree');
  });

  it('expands conflict folds per file', async () => {
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
      ...Array.from({ length: 10 }, (_, i) => `c${i + 1}`),
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
    expect(state.conflictFiles[0]?.lines[0]?.fold).toEqual({ id: 0, lines: 7 });

    state.expandConflictFold('main.go', 0);
    const expanded = state.conflictFiles[0];
    expect(expanded?.lines.some((line) => line.fold)).toBe(false);
    expect(expanded?.lines[1]?.content).toBe(' c1');

    // Closing and reopening the view resets expansion state.
    await state.setScope('workspace');
    await state.setScope('pr');
    await state.openConflictView();
    expect(state.conflictFiles[0]?.lines[0]?.fold).toEqual({ id: 0, lines: 7 });
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

    const rows = buildReviewRows({
      files: state.conflictFiles,
      viewMode: 'stacked',
      collapsedPaths: state.conflictCollapsedPaths,
      drafts: [],
      openEditors: [],
      prThreads: [],
      expandedPRThreadIds: new Set(),
    }).rows;

    expect(rows.map((row) => row.kind)).toEqual(['file-header', 'line-block', 'surface-end']);
    expect(rows.some((row) => row.kind === 'comment-thread' || row.kind === 'draft-editor' || row.kind === 'pr-thread')).toBe(false);
  });

  it('loads CI jobs in pr scope and opens a job log view', async () => {
    installPRMocks();
    setBindingMock('GetPRCIJobs', async () => ({
      status: 'failed',
      url: 'https://gl/p/77',
      stages: [
        {
          name: 'test',
          status: 'failed',
          jobs: [
            { id: '20', name: 'unit', status: 'failed', logsAvailable: true, url: 'https://gl/j/20' },
          ],
        },
      ],
    }));
    const getLog = setBindingMock('GetPRCIJobLog', async () => ({
      text: 'line 1\nline 2\n',
      truncated: false,
      totalBytes: 14,
    }));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    await vi.waitFor(() => {
      expect(state.ciPipeline?.status).toBe('failed');
    });
    expect(state.ciPipeline?.stages[0]?.jobs[0]?.name).toBe('unit');

    const job = state.ciPipeline!.stages[0]!.jobs[0]!;
    await state.openCIJobLog('test', job);
    expect(getLog).toHaveBeenCalledWith(
      { Forge: 'github', Namespace: 'owner', Repo: 'repo', Number: 5 },
      '20',
    );
    expect(state.ciLogView?.job.name).toBe('unit');
    expect(state.ciLog?.text).toBe('line 1\nline 2\n');

    state.closeCILogView();
    expect(state.ciLogView).toBeNull();
    expect(state.ciLog).toBeNull();

    // Leaving pr scope drops the pipeline state.
    await state.setScope('workspace');
    expect(state.ciPipeline).toBeNull();
  });

  it('opening the conflict view closes the CI log view and vice versa', async () => {
    installPRMocks();
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: false,
      treeOID: 'tree-clean',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: [],
      messages: [],
    }));
    setBindingMock('GetPRCIJobLog', async () => ({ text: 'x\n', truncated: false, totalBytes: 2 }));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    const job = { id: '20', name: 'unit', status: 'failed', logsAvailable: true };
    await state.openCIJobLog('test', job);
    expect(state.ciLogView).not.toBeNull();

    await state.openConflictView();
    expect(state.ciLogView).toBeNull();
    expect(state.conflictView).toBe(true);

    await state.openCIJobLog('test', job);
    expect(state.conflictView).toBe(false);
    expect(state.ciLogView).not.toBeNull();
  });

  it('sendCILogToChat saves the log and prefills the source pane composer', async () => {
    installPRMocks();
    setBindingMock('GetPRCIJobLog', async () => ({ text: 'boom\n', truncated: false, totalBytes: 5 }));
    const save = setBindingMock('SavePRCIJobLog', async () => '/data/ci-logs/github-owner-repo-pr5-20-unit.log');
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    let content = '';
    const dispose = registerComposerDraft('pane-1', {
      get content() { return content; },
      setContent(next: string) { content = next; },
    } as never);

    try {
      await state.openCIJobLog('test', { id: '20', name: 'unit', status: 'failed', logsAvailable: true });
      await state.sendCILogToChat();

      expect(save).toHaveBeenCalledWith(
        { Forge: 'github', Namespace: 'owner', Repo: 'repo', Number: 5 },
        '20',
        'unit',
      );
      expect(state.ciLogSavedPath).toBe('/data/ci-logs/github-owner-repo-pr5-20-unit.log');
      expect(content).toContain('CI job `unit`');
      expect(content).toContain('status: failed');
      expect(content).toContain('/data/ci-logs/github-owner-repo-pr5-20-unit.log');
    } finally {
      dispose();
    }
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

  function setDocumentVisibility(value: DocumentVisibilityState): void {
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => value,
    });
    document.dispatchEvent(new Event('visibilitychange'));
  }

  afterEach(() => {
    // Drop the per-test visibilityState override so later tests see the
    // environment default ('visible') again.
    delete (document as unknown as Record<string, unknown>).visibilityState;
  });

  it('pauses the pump while the document is hidden and resumes it on visible', async () => {
    installPRMocks();
    const setActive = setBindingMock('SetPRUpdatesActive', async () => undefined);
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(setActive).not.toHaveBeenCalled();

    setDocumentVisibility('hidden');
    expect(setActive).toHaveBeenCalledWith('sub-1', false);

    setDocumentVisibility('visible');
    expect(setActive).toHaveBeenCalledWith('sub-1', true);
    expect(setActive).toHaveBeenCalledTimes(2);
  });

  it('a PR load that finishes while the document is hidden starts its pump paused', async () => {
    installPRMocks();
    const setActive = setBindingMock('SetPRUpdatesActive', async () => undefined);
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);

    setDocumentVisibility('hidden');
    setActive.mockClear();
    await state.setScope('pr');
    expect(setActive).toHaveBeenCalledWith('sub-1', false);
  });
});

describe('hunk-gap context expansion', () => {
  // Leading gap 1..9; trailing gap of unknown size starting at 12.
  const gappedPatch = `diff --git a/src/app.ts b/src/app.ts
--- a/src/app.ts
+++ b/src/app.ts
@@ -10,2 +10,2 @@
 ctx
-old
+new
`;

  it('fetches the gap slice and merges it into the derived files', async () => {
    setBindingMock('GetWorkspaceCurrentDiff', async () => gappedPatch);
    const fetchLines = setBindingMock('GetDiffContextLines', async (_threadId, req) => {
      const { startLine, endLine } = req as { startLine: number; endLine: number };
      return {
        lines: Array.from({ length: endLine - startLine + 1 }, (_, i) => `src ${startLine + i}`),
        startLine,
        eof: false,
        totalLines: 0,
      };
    });

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    const leading = filePatchDisplayRows(state.files[0]).find((row) => row.gap)?.gap;
    expect(leading).toMatchObject({ location: 'leading', startNew: 1, endNew: 9 });

    await state.expandDiffContext('src/app.ts', leading!, 'all');
    expect(fetchLines).toHaveBeenCalledWith('thread-1', expect.objectContaining({
      scope: 'workspace',
      path: 'src/app.ts',
      startLine: 1,
      endLine: 9,
    }));

    const rows = filePatchDisplayRows(state.files[0]);
    expect(rows.some((row) => row.gap?.location === 'leading')).toBe(false);
    expect(rows.find((row) => row.newLine === 1)?.line.content).toBe(' src 1');
    expect(state.error).toBeNull();
  });

  it('retires the trailing gap on an EOF response', async () => {
    setBindingMock('GetWorkspaceCurrentDiff', async () => gappedPatch);
    setBindingMock('GetDiffContextLines', async () => ({
      lines: ['src 12', 'src 13', 'src 14'],
      startLine: 12,
      eof: true,
      totalLines: 14,
    }));

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    const trailing = filePatchDisplayRows(state.files[0])
      .find((row) => row.gap?.location === 'trailing')?.gap;
    expect(trailing).toMatchObject({ startNew: 12, endNew: -1 });

    await state.expandDiffContext('src/app.ts', trailing!, 'down');

    const rows = filePatchDisplayRows(state.files[0]);
    expect(state.files[0].newSideTotal).toBe(14);
    expect(rows.some((row) => row.gap)).toBe(true); // leading gap remains
    expect(rows.some((row) => row.gap?.location === 'trailing')).toBe(false);
    expect(rows.at(-1)).toMatchObject({ oldLine: 14, newLine: 14 });
  });

  it('surfaces a fetch failure and clears expansions on reload', async () => {
    setBindingMock('GetWorkspaceCurrentDiff', async () => gappedPatch);
    setBindingMock('GetDiffContextLines', async () => {
      throw new Error('no clone available');
    });

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    const leading = filePatchDisplayRows(state.files[0]).find((row) => row.gap)?.gap;

    await state.expandDiffContext('src/app.ts', leading!, 'all');
    expect(state.error).toBe('no clone available');

    // A successful expansion then a reload: the merged lines must not
    // survive into the fresh patch (numbering may have shifted).
    setBindingMock('GetDiffContextLines', async () => ({
      lines: Array.from({ length: 9 }, (_, i) => `src ${i + 1}`),
      startLine: 1,
      eof: false,
      totalLines: 0,
    }));
    await state.expandDiffContext('src/app.ts', leading!, 'all');
    expect(filePatchDisplayRows(state.files[0]).some((row) => row.gap?.location === 'leading')).toBe(false);

    await state.reload();
    expect(filePatchDisplayRows(state.files[0]).some((row) => row.gap?.location === 'leading')).toBe(true);
  });
});

describe('collapse overrides across reloads', () => {
  it('keeps user collapse/expand choices through a reload, resets on scope switch', async () => {
    setBindingMock('GetWorkspaceCurrentDiff', async () => patchFor('src/app.ts', 2));
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    expect(state.collapsedPaths.has('src/app.ts')).toBe(false);

    state.toggleCollapsed('src/app.ts');
    expect(state.collapsedPaths.has('src/app.ts')).toBe(true);

    await state.reload();
    expect(state.collapsedPaths.has('src/app.ts')).toBe(true);

    // Overrides beat fresh defaults in BOTH directions: a lockfile-ish
    // file the user expanded stays expanded after reload.
    setBindingMock('GetWorkspaceCurrentDiff', async () => patchFor('go.sum', 2));
    await state.reload();
    expect(state.collapsedPaths.has('go.sum')).toBe(true); // default
    state.toggleCollapsed('go.sum');
    await state.reload();
    expect(state.collapsedPaths.has('go.sum')).toBe(false); // override held

    // Scope switch is a new subject: overrides reset, defaults return.
    setBindingMock('GetBranchBaseDiff', async () => patchFor('go.sum', 2));
    await state.setScope('branch');
    await state.reload();
    expect(state.collapsedPaths.has('go.sum')).toBe(true);
  });
});

describe('comments-only PR refresh', () => {
  it('refreshes detail + threads without reloading the diff', async () => {
    installPRMocks();
    const diff = setBindingMock('GetPRDiff', async () => patchFor('src/app.ts', 3));
    setBindingMock('GetPRDetail', async () => prDetailStub());
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(diff).toHaveBeenCalledTimes(1);

    const threads = setBindingMock('ListPRReviewThreads', async () => [
      { id: 't-1', path: 'src/app.ts', line: 1, comments: [] },
    ]);
    await state.refreshPRThreads();

    expect(threads).toHaveBeenCalledTimes(1);
    expect(state.prThreads.map((thread) => thread.id)).toEqual(['t-1']);
    expect(diff).toHaveBeenCalledTimes(1); // the diff was NOT re-fetched
    expect(state.prStale).toBe(false);
  });

  it('a moved head raises the stale banner instead of swapping the diff', async () => {
    installPRMocks();
    const diff = setBindingMock('GetPRDiff', async () => patchFor('src/app.ts', 3));
    setBindingMock('GetPRDetail', async () => prDetailStub({ headSHA: 'sha-b' }));
    const state = reviewStateForPane('pane-1', 'thread-1', prThreadStub());
    await waitLoaded(state);
    await state.setScope('pr');

    await state.refreshPRThreads();

    expect(state.prStale).toBe(true);
    expect(state.prHeadSHA).toBe('sha-b');
    expect(diff).toHaveBeenCalledTimes(1);
  });

  it('is a no-op outside pr scope', async () => {
    const detail = setBindingMock('GetPRDetail', async () => prDetailStub());
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.refreshPRThreads();
    expect(detail).not.toHaveBeenCalled();
  });
});

describe('reviewPane store — edits scope', () => {
  // A patch whose first hunk starts mid-file: workspace scope would emit
  // a leading hunk-gap row for it, edits scope must not.
  function gappyPatch(): string {
    return [
      'diff --git a/x.go b/x.go',
      'index 1111111..2222222 100644',
      '--- a/x.go',
      '+++ b/x.go',
      '@@ -5,3 +5,3 @@',
      ' ctx',
      '-old',
      '+new',
    ].join('\n');
  }

  function installEditMocks() {
    const entries = [
      { itemId: 'tool:1', payloadId: 'pl-1', turnIndex: 1, title: 'Edited parser.go', paths: ['parser.go'], insertions: 1, deletions: 0, createdAt: 1 },
      { itemId: 'tool:2a', payloadId: 'pl-2a', turnIndex: 2, title: 'Edited lexer.go', paths: ['lexer.go'], insertions: 2, deletions: 1, createdAt: 2 },
      { itemId: 'tool:2b', payloadId: 'pl-2b', turnIndex: 2, title: 'Edited lexer.go', paths: ['lexer.go'], insertions: 1, deletions: 1, createdAt: 3 },
    ];
    const list = setBindingMock('ListThreadEditDiffs', async () => ({
      entries,
      turnLabels: [
        { turnIndex: 1, label: 'fix the parser' },
        { turnIndex: 2, label: 'now the lexer' },
      ],
    }));
    const payload = setBindingMock('GetPayloadData', async () => ({ data: gappyPatch() }));
    const turnDiff = setBindingMock('GetTurnEditsDiff', async () => ({ data: gappyPatch() }));
    return { list, payload, turnDiff };
  }

  it('defaults to the latest turn and keys comments by turn', async () => {
    const { turnDiff } = installEditMocks();
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    await state.setScope('edits');
    expect(state.scope).toBe('edits');
    expect(state.edits.length).toBe(3);
    expect(state.editTurnLabels.get(2)).toBe('now the lexer');
    expect(state.selectedEditKey).toBe('turn:2');
    expect(state.sourceKey).toBe('edit-turn:2');
    expect(turnDiff).toHaveBeenCalledWith('thread-1', 2);
    expect(state.files.length).toBe(1);
  });

  it('emits gap rows for single-section edit files and verifies expansion', async () => {
    installEditMocks();
    const contextLines = setBindingMock('GetDiffContextLines', async () => ({
      lines: ['top 1', 'top 2', 'top 3', 'top 4'],
      startLine: 1,
      eof: false,
      totalLines: 20,
    }));
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    await state.setScope('edits');
    // A single-section historical diff offers hunk-gap expansion like
    // any live scope — the backend verifies the workspace still
    // matches before serving lines.
    expect(state.files[0].suppressGaps).toBeUndefined();
    const gapRow = filePatchDisplayRows(state.files[0]).find((row) => row.gap);
    expect(gapRow).toBeDefined();

    await state.expandDiffContext('x.go', gapRow!.gap!, 'up');
    expect(contextLines).toHaveBeenCalledWith('thread-1', expect.objectContaining({
      scope: 'edits',
      path: 'x.go',
      // The historical patch rides along for drift verification.
      verifyPatch: expect.stringContaining('@@ -5,3 +5,3 @@'),
    }));
    expect(state.error).toBeNull();
  });

  it('retires a file\'s gaps when edits-scope expansion is refused', async () => {
    installEditMocks();
    setBindingMock('GetDiffContextLines', async () => {
      throw new Error('x.go has changed since this edit');
    });
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('edits');

    const gapRow = filePatchDisplayRows(state.files[0]).find((row) => row.gap);
    expect(gapRow).toBeDefined();

    await state.expandDiffContext('x.go', gapRow!.gap!, 'up');
    // Drifted workspace: the file's gap affordances retire quietly —
    // the historical diff itself is still fully valid, so no banner.
    expect(state.error).toBeNull();
    expect(state.files[0].suppressGaps).toBe(true);
    expect(filePatchDisplayRows(state.files[0]).some((row) => row.gap)).toBe(false);
  });

  it('opens pinned to the clicked edit and keys comments by payload id', async () => {
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 1 }]);
    const { payload } = installEditMocks();

    const state = await openReviewCompanion('pane-1', 'thread-1', {
      editItemId: 'tool:2a',
      filePath: 'lexer.go',
    });
    expect(state).not.toBeNull();
    await waitLoaded(state!);

    expect(state!.scope).toBe('edits');
    expect(state!.selectedEditKey).toBe('item:tool:2a');
    expect(state!.sourceKey).toBe('edit:pl-2a');
    expect(payload).toHaveBeenCalledWith('thread-1', 'pl-2a');
    expect(state!.pendingJumpFilePath).toBe('lexer.go');
  });

  it('selection changes reuse the loaded list (fast path)', async () => {
    const { list, payload, turnDiff } = installEditMocks();
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('edits');
    expect(list).toHaveBeenCalledTimes(1);

    await state.selectEdit('item:tool:1');
    expect(state.selectedEditKey).toBe('item:tool:1');
    expect(state.sourceKey).toBe('edit:pl-1');
    expect(payload).toHaveBeenCalledWith('thread-1', 'pl-1');

    await state.selectEdit('turn:1');
    expect(state.selectedEditKey).toBe('turn:1');
    expect(turnDiff).toHaveBeenLastCalledWith('thread-1', 1);

    // Unknown keys resolve to the default (latest turn) instead of erroring.
    await state.selectEdit('item:gone');
    expect(state.selectedEditKey).toBe('turn:2');

    expect(list).toHaveBeenCalledTimes(1);
  });

  it('merges same-path sections of a whole-turn diff into one file', async () => {
    // A file edited twice in one turn appears as two sections in the
    // concatenated whole-turn diff. Duplicate paths crash the review
    // surface's path-keyed each blocks (svelte each_key_duplicate), so
    // the store must collapse them into a single PatchFile.
    installEditMocks();
    const twiceEdited = [
      'diff --git a/x.go b/x.go',
      '--- a/x.go',
      '+++ b/x.go',
      '@@ -5,3 +5,3 @@',
      ' ctx',
      '-old',
      '+new',
      'diff --git a/x.go b/x.go',
      '--- a/x.go',
      '+++ b/x.go',
      '@@ -9,2 +9,3 @@',
      ' ctx2',
      '+later',
    ].join('\n');
    setBindingMock('GetTurnEditsDiff', async () => ({ data: twiceEdited }));

    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('edits');

    expect(state.files.map((file) => file.path)).toEqual(['x.go']);
    expect(state.files[0].additions).toBe(2);
    expect(state.files[0].deletions).toBe(1);
    // Disjoint sections renumber into ONE coherent section — a single
    // meta block with hunks in final-file order — so the merged file
    // verifies, primes, and gap-expands like a single-section diff.
    expect(state.files[0].suppressGaps).toBeUndefined();
    expect(filePatchDisplayRows(state.files[0]).some((row) => row.gap)).toBe(true);
    const contents = state.files[0].lines.map((line) => line.content);
    expect(contents.filter((content) => content.startsWith('diff --git'))).toHaveLength(1);
    expect(contents.filter((content) => content.startsWith('@@'))).toEqual([
      '@@ -5,2 +5,2 @@',
      '@@ -9,1 +9,2 @@',
    ]);
    expect(contents).toContain('+new');
    expect(contents).toContain('+later');
    expect(contents.indexOf('+new')).toBeLessThan(contents.indexOf('+later'));
  });

  it('retires gap arrows up front for edits outside the workspace', async () => {
    installEditMocks();
    const outsideEdit = [
      'diff --git a//home/user/.claude/memory/notes.md b//home/user/.claude/memory/notes.md',
      '--- a//home/user/.claude/memory/notes.md',
      '+++ b//home/user/.claude/memory/notes.md',
      '@@ -5,2 +5,2 @@',
      ' ctx',
      '-old',
      '+new',
      'diff --git a/x.go b/x.go',
      '--- a/x.go',
      '+++ b/x.go',
      '@@ -5,2 +5,2 @@',
      ' ctx',
      '-old',
      '+new',
    ].join('\n');
    setBindingMock('GetTurnEditsDiff', async () => ({ data: outsideEdit }));
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('edits');

    // The absolute-path edit renders but can never expand (the backend
    // only resolves workspace-relative paths) — no dead arrows.
    const outside = state.files.find((file) => file.path.startsWith('/'));
    expect(outside).toBeDefined();
    expect(outside!.suppressGaps).toBe(true);
    expect(filePatchDisplayRows(outside!).some((row) => row.gap)).toBe(false);
    // Workspace-relative files keep their gap affordances.
    const inside = state.files.find((file) => file.path === 'x.go');
    expect(inside!.suppressGaps).toBeUndefined();
    expect(filePatchDisplayRows(inside!).some((row) => row.gap)).toBe(true);
  });

  it('keeps merged-file lines identity stable across expansion rebuilds', async () => {
    installEditMocks();
    const twiceEdited = [
      'diff --git a/x.go b/x.go',
      '--- a/x.go',
      '+++ b/x.go',
      '@@ -5,3 +5,3 @@',
      ' ctx',
      '-old',
      '+new',
      'diff --git a/x.go b/x.go',
      '--- a/x.go',
      '+++ b/x.go',
      '@@ -9,2 +9,3 @@',
      ' ctx2',
      '+later',
    ].join('\n');
    setBindingMock('GetTurnEditsDiff', async () => ({ data: twiceEdited }));
    setBindingMock('GetDiffContextLines', async () => ({
      lines: ['l1', 'l2', 'l3', 'l4'],
      startLine: 1,
      eof: false,
      totalLines: 20,
    }));
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);
    await state.setScope('edits');

    const baseLines = state.files[0].lines;
    const gapRow = filePatchDisplayRows(state.files[0]).find((row) => row.gap);
    expect(gapRow?.gap?.location).toBe('leading');
    await state.expandDiffContext('x.go', gapRow!.gap!, 'all');

    // The rebuilt array must record the EXACT array it superseded: the
    // span cache walks this chain to keep serving the pre-expansion
    // spans while the expanded file's own highlight request is in
    // flight. When the merge minted a fresh base array on every derived
    // re-run, the chain pointed at an unregistered array and the whole
    // file flashed plain on every expansion click.
    const expandedLines = state.files[0].lines;
    expect(expandedLines).not.toBe(baseLines);
    expect(expansionPredecessor(expandedLines)).toBe(baseLines);
  });

  it('shows an empty surface for a thread with no edits', async () => {
    setBindingMock('ListThreadEditDiffs', async () => ({ entries: [], turnLabels: [] }));
    const state = reviewStateForPane('pane-1', 'thread-1');
    await waitLoaded(state);

    await state.setScope('edits');
    expect(state.edits.length).toBe(0);
    expect(state.selectedEditKey).toBeNull();
    expect(state.sourceKey).toBe('');
    expect(state.files.length).toBe(0);
    expect(state.error).toBeNull();
  });
});
