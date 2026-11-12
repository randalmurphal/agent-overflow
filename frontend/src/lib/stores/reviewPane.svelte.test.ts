import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { getToasts } from './toast.svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import {
  activeDiffReviewSourceForThread,
  replaceDiffReviewCommentsForTest,
} from './diffReviewComments.svelte';
import { projectTurnStarted } from './threadStatuses.svelte';
import { CI_SECTION_INLINE_MAX_BYTES } from './ciSubject.svelte';
import {
  __resetReviewPaneStateForTest,
  __setRelocationBudgetForTest,
  disposeReviewStateForPane,
  openReviewCompanion,
  reviewSubjectForPane,
  reviewStateForPane,
  type ConversationFeedItem,
  type ReviewSubject,
} from './reviewPane.svelte';
import {
  COLLAPSE_ROW_LIMIT,
  draftAnchorExists,
  EditFileMerge,
  reviewLineCommentForDraft,
  supportsIgnoreWhitespace,
} from './reviewPaneLoad';
import { applyPRCILogEvent, applyPRCIUpdatedEvent, applyPRUpdatedEvent, attachPR } from './prReviewStore.svelte';
import { closeCompanion, hideCompanion, openCompanion, resetCompanionPanesForTest } from './companionPanes.svelte';
import { __setTransportHelloForTest, __setTransportStatusForTest } from './transportStatus.svelte';
import { DisconnectedError, TransportError, type TransportHello } from '../transport/wsClient';
import { companionSubjectKey } from './companionSubject';
import { __seedGitStatusForTest } from './gitStatusStore.svelte';
import { getPane, registerPaneForTest, resetPanesForTest, revealPane } from './panes.svelte';
import { noteCompactPaneShown, setCompactLayoutForTest, showCompactList, showCompactThread } from './layoutMode.svelte';
import { persistScope } from './reviewPaneScope';
import { removeThread } from './threads.svelte';
import { createThreadPane } from './thread.svelte';
import { registerComposerDraft, resetComposerDraftRegistryForTest } from './composerDraftRegistry.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import type { DiffReviewComment, PRDetail, PRReviewSubmitOutcome, ReviewThread, Thread } from '../types/models';
import type { GitStatus, WorkspaceRef } from '../types/git';
import { diffSourceKey } from '../utils/diffSourceKey';
import { PATCH_PARSE_CACHE_MAX_ENTRY_CHARS, type PatchDisplayRow, type PatchLine } from '../utils/patchFiles';
import { displayRowCount, FILE_START, materializeRows } from '../utils/patchRows';
import { parseReviewFiles, type ReviewFile } from '../utils/patchStore';
import { buildReviewRows } from '../utils/reviewRows';
import {
  getSpansForReviewLine,
  resetDiffSpanCacheForTest,
  type PaintedSpans,
} from '../utils/diffSpanCache.svelte';
import { getBindingMock, setBindingMock, setReviewDiffMock } from '../../test/mocks/bindings-app';
import { REVIEW_DIFF_READ_BYTES } from './reviewDiffStream';
import { patchTextHeldBytes, setPatchTextBudgetForTest } from '../utils/patchMemory.svelte';
import { PatchTextLost } from '../utils/patchStore';
import { mergePatchFilesByPath, parsePatchFiles } from '../utils/patchFiles';
import { installDiagnosticsCapture } from '../../test/helpers/diagnostics';

function rowsOf(file: ReviewFile): PatchDisplayRow[] {
  return materializeRows(file, FILE_START, displayRowCount(file)).rows;
}

function linesOf(file: ReviewFile | undefined): PatchLine[] | undefined {
  return file?.body.toPatchLines();
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

async function waitLoaded(state: ReturnType<typeof reviewStateForPane>): Promise<void> {
  await vi.waitFor(() => {
    expect(state.loading).toBe(false);
  });
}

/** `target` plus an added filler file large enough to stand for any large
 * review diff. */
function oversizedPatch(target: string): string {
  const fillerLines = Math.ceil(PATCH_PARSE_CACHE_MAX_ENTRY_CHARS / 32);
  const patch = [
    target,
    'diff --git a/zz-filler.txt b/zz-filler.txt',
    'new file mode 100644',
    '--- /dev/null',
    '+++ b/zz-filler.txt',
    `@@ -0,0 +1,${fillerLines} @@`,
    ...Array.from({ length: fillerLines }, (_, index) => `+filler ${String(index).padStart(32, '0')}`),
  ].join('\n');
  expect(patch.length).toBeGreaterThan(PATCH_PARSE_CACHE_MAX_ENTRY_CHARS);
  return patch;
}

const REVIEW_WORKSPACE = '/tmp/ws';
const REVIEW_WS: WorkspaceRef = { projectId: 'project-1', workspacePath: REVIEW_WORKSPACE };

/**
 * The ordinary subject: a started thread whose checkout is the review
 * workspace. Every workspace-scoped RPC is asserted against `REVIEW_WS`;
 * `threadId` is what the thread-scoped ones (edits, comments, steer) take.
 */
function subjectFor(threadId: string | null = 'thread-1'): ReviewSubject {
  const thread = { id: threadId ?? 'draft:pane-1', projectId: REVIEW_WS.projectId } as Thread;
  return {
    identity: companionSubjectKey({ thread, workspace: REVIEW_WS }),
    threadId,
    rowId: thread.id,
    workspace: REVIEW_WS,
  };
}

/**
 * A pane's PR is its workspace's open PR, read from the LIVE git status — the
 * same observation the header badge renders, read from the shared store
 * rather than re-fetched. Reaching it needs the source pane registered (that
 * is how the pane finds the workspace) and that workspace observed.
 */
function seedPaneWorkspaceStatus(paneId: string, overrides: Partial<GitStatus>): void {
  const pane = createThreadPane({ paneId });
  pane.replaceThread({
    id: 'thread-1',
    title: 'Review',
    provider: 'claude',
    workspacePath: REVIEW_WORKSPACE,
    projectPath: REVIEW_WORKSPACE,
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  });
  registerPaneForTest(paneId, pane);
  __seedGitStatusForTest(REVIEW_WORKSPACE, {
    isRepo: true,
    branch: 'feature',
    isDefaultBranch: false,
    hasChanges: false,
    insertions: 0,
    deletions: 0,
    fileCount: 0,
    hasUpstream: true,
    aheadCount: 0,
    behindCount: 0,
    hasOriginRemote: true,
    ...overrides,
  });
}

function installDefaultMocks(): void {
  // Seeding a workspace status runs the shared store's branch reconciliation;
  // no rows come back, which is the ordinary answer for an already-correct
  // cache. Unmocked it only produces console noise, but noise in a passing
  // suite is where a real failure goes to hide.
  setBindingMock('UpdateThreadBranch', async () => []);
  setReviewDiffMock('OpenWorkspaceDiff', async () => '');
  setReviewDiffMock('OpenBranchBaseDiff', async () => '');
  setBindingMock('ListBranchCommits', async () => []);
  setReviewDiffMock('OpenCommitDiff', async () => '');
  setBindingMock('ListPRCommits', async () => []);
  setReviewDiffMock('OpenPRCommitDiff', async () => '');
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
  resetPanesForTest();
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
    expect(reviewLineCommentForDraft(draft({ side: 'old', oldLine: 5, newLine: 0 }))).toEqual({
      path: 'src/app.ts',
      body: 'comment',
      line: 5,
      side: 'left',
    });
    // An unchanged line, from the unified row or either half: both numbers.
    for (const side of ['context', 'old', 'new'] as const) {
      expect(reviewLineCommentForDraft(draft({ side, oldLine: 3, newLine: 4 }))).toEqual({
        path: 'src/app.ts',
        body: 'comment',
        line: 4,
        oldLine: 3,
        side: 'right',
      });
    }
    expect(reviewLineCommentForDraft(draft({ side: 'file', oldLine: undefined, newLine: undefined }))).toEqual({
      path: 'src/app.ts',
      body: 'comment',
      side: 'file',
    });
  });

  it('detects drafts orphaned by a vanished line', () => {
    const files = parseReviewFiles(patchFor('src/app.ts', 2));
    expect(draftAnchorExists(files, draft({ newLine: 2 }))).toBe(true);
    expect(draftAnchorExists(files, draft({ newLine: 99 }))).toBe(false);
  });

  it('keeps the old subject coherent during branch lookup and ignores a superseded lookup', async () => {
    setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('workspace.go', 1));
    const branchDiff = setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    let finish!: (branches: unknown[]) => void;
    setBindingMock('GitListBranches', () => new Promise((resolve) => { finish = resolve; }));
    const stale = state.setScope('branch');
    await tick();
    expect(state.scope).toBe('workspace');
    expect(state.files.map((f) => f.path)).toEqual(['workspace.go']);
    await state.setScope('workspace');
    finish([{ name: 'main', isDefault: true }]);
    await stale;
    expect(state.scope).toBe('workspace');
    expect(state.files.map((f) => f.path)).toEqual(['workspace.go']);
    expect(branchDiff).not.toHaveBeenCalled();
  });

  it('retains content during same-subject refresh but retires it before a commit selection', async () => {
    const patch = patchFor('workspace.go', 1);
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    let finish!: (patch: string) => void;
    setReviewDiffMock('OpenWorkspaceDiff', () => new Promise<string>((resolve) => { finish = resolve; }));
    const refresh = state.reload();
    await tick();
    expect(state.files.map((f) => f.path)).toEqual(['workspace.go']);
    finish(patch);
    await refresh;

    setReviewDiffMock('OpenBranchBaseDiff', async () => patch);
    setBindingMock('ListBranchCommits', async () => [{ sha: 'commit-a', subject: 'change' }]);
    await state.setScope('branch', { baseBranch: 'main' });
    setReviewDiffMock('OpenCommitDiff', () => new Promise<string>((resolve) => { finish = resolve; }));
    const selecting = state.selectCommit('commit-a');
    await tick();
    expect(state.files).toEqual([]);
    expect(state.files).toEqual([]);
    finish(patchFor('commit.go', 1));
    await selecting;
    expect(state.files.map((f) => f.path)).toEqual(['commit.go']);
  });

  it('loads the binding for each scope', async () => {
    const workspace = setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('workspace.go', 1));
    const branch = setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));
    setBindingMock('ListBranchCommits', async () => [
      { sha: 'a'.repeat(40), shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
      { sha: 'b'.repeat(40), shortSha: 'bbbbbbb', subject: 'second', author: 'r', authoredAt: 2 },
    ]);
    setBindingMock('GitListBranches', async () => [
      { name: 'develop', isCurrent: false, isDefault: true },
    ]);

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(workspace).toHaveBeenCalledWith(REVIEW_WS, false);

    await state.setScope('branch');
    expect(branch).toHaveBeenCalledWith(REVIEW_WS, 'develop', false);
    expect(state.baseBranch).toBe('develop');
    expect(state.files.map((f) => f.path)).toEqual(['branch.go']);
    expect(state.commits.map((commit) => commit.shortSha)).toEqual(['aaaaaaa', 'bbbbbbb']);
    expect(state.selectedCommitSHA).toBeNull();
  });

  it('persists and restores last-used scope per thread', async () => {
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    await state.setScope('branch', { baseBranch: 'release' });
    expect(appStorageGet('reviewScope:thread-1')).toBe(JSON.stringify({
      scope: 'branch',
      baseBranch: 'release',
    }));

    disposeReviewStateForPane('pane-1', state);
    const branch = setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('release.go', 1));
    const restored = reviewStateForPane('pane-2', subjectFor());
    await waitLoaded(restored);

    expect(restored.scope).toBe('branch');
    expect(restored.baseBranch).toBe('release');
    expect(branch).toHaveBeenCalledWith(REVIEW_WS, 'release', false);
  });

  it('defaults first open to workspace scope', async () => {
    const workspace = setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('workspace.go', 1));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(state.scope).toBe('workspace');
    expect(state.files.map((f) => f.path)).toEqual(['workspace.go']);
    expect(workspace).toHaveBeenCalledTimes(1);
  });

  it('selects a single commit in branch scope and keys comments by its SHA', async () => {
    const sha = 'a'.repeat(40);
    setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));
    setBindingMock('ListBranchCommits', async () => [
      { sha, shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    const commitDiff = setReviewDiffMock('OpenCommitDiff', async () => patchFor('commit.go', 1));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('branch');

    await state.selectCommit(sha);
    expect(state.scope).toBe('branch');
    expect(state.selectedCommitSHA).toBe(sha);
    expect(state.files.map((f) => f.path)).toEqual(['commit.go']);
    expect(state.sourceKey).toBe(`commit:${sha}`);
    expect(commitDiff).toHaveBeenLastCalledWith(REVIEW_WS, sha, false);

    // Back to the full range.
    await state.selectCommit(null);
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.files.map((f) => f.path)).toEqual(['branch.go']);
  });

  it('drops a selected commit that left the range and reloads the full diff', async () => {
    const sha = 'a'.repeat(40);
    setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));
    setBindingMock('ListBranchCommits', async () => [
      { sha, shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setReviewDiffMock('OpenCommitDiff', async () => patchFor('commit.go', 1));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('branch');
    await state.selectCommit(sha);
    expect(state.files.map((f) => f.path)).toEqual(['commit.go']);

    // Rebase: the SHA is gone from base..HEAD.
    setBindingMock('ListBranchCommits', async () => []);
    await state.reload();
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.files.map((f) => f.path)).toEqual(['branch.go']);
  });

  it('resets the selected commit when the scope changes', async () => {
    const sha = 'a'.repeat(40);
    setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));
    setBindingMock('ListBranchCommits', async () => [
      { sha, shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setReviewDiffMock('OpenCommitDiff', async () => patchFor('commit.go', 1));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('branch');
    await state.selectCommit(sha);
    expect(state.selectedCommitSHA).toBe(sha);

    await state.setScope('workspace');
    expect(state.selectedCommitSHA).toBeNull();
  });

  it('openReviewCompanion applies scope and pending jump target', async () => {
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 1 }]);
    setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));

    const state = await openReviewCompanion('pane-1', subjectFor(), {
      scope: 'branch',
      filePath: 'src/app.ts',
    });

    expect(state).not.toBeNull();
    await waitLoaded(state!);
    expect(state!.scope).toBe('branch');
    expect(state!.pendingJumpFilePath).toBe('src/app.ts');
    // A jump to a file shows it even when the file filter hides it.
    expect([...state!.revealedPaths]).toEqual(['src/app.ts']);

    const same = reviewStateForPane('pane-1', subjectFor());
    expect(same.pendingJumpFilePath).toBe('src/app.ts');
    same.consumePendingJumpFilePath();
    expect(same.pendingJumpFilePath).toBeNull();
  });

  it('collapses lockfile-ish and large files once per load', async () => {
    const patch = [
      patchFor('src/small.ts', 2),
      patchFor('pnpm-lock.yaml', 2),
      patchFor('src/limit.ts', COLLAPSE_ROW_LIMIT),
      patchFor('src/large.ts', COLLAPSE_ROW_LIMIT + 1),
    ].join('\n');
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(state.collapsedPaths.has('src/small.ts')).toBe(false);
    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(true);
    expect(state.collapsedPaths.has('src/limit.ts')).toBe(false);
    expect(state.collapsedPaths.has('src/large.ts')).toBe(true);
  });

  it('jumpToDiffThread expands the file and thread and stages the row-key jump', async () => {
    const patch = [patchFor('src/small.ts', 2), patchFor('pnpm-lock.yaml', 2)].join('\n');
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(true);

    state.jumpToDiffThread({ ...reviewThreadStub('thread-9'), path: 'pnpm-lock.yaml' });

    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(false);
    expect(state.expandedPRThreadIds.has('thread-9')).toBe(true);
    expect(state.pendingJumpRowKey).toBe('pt:thread-9');
    expect([...state.revealedPaths]).toEqual(['pnpm-lock.yaml']);

    state.consumePendingJumpRowKey();
    expect(state.pendingJumpRowKey).toBeNull();

    // A PR-level conversation thread has no row to jump to.
    state.jumpToDiffThread({ ...reviewThreadStub('thread-10'), path: '' });
    expect(state.pendingJumpRowKey).toBeNull();
    expect([...state.revealedPaths]).toEqual(['pnpm-lock.yaml']);

    // A filter change ends the reveals.
    state.clearRevealedPaths();
    expect(state.revealedPaths.size).toBe(0);
  });

  it('a different diff ends the reveals; a refresh of the same diff keeps them', async () => {
    const patch = [patchFor('src/small.ts', 2), patchFor('src/small.test.ts', 2)].join('\n');
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);
    setReviewDiffMock('OpenBranchBaseDiff', async () => patch);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    state.jumpToDiffThread({ ...reviewThreadStub('thread-9'), path: 'src/small.test.ts' });
    await state.reload();
    await waitLoaded(state);
    expect([...state.revealedPaths]).toEqual(['src/small.test.ts']);

    await state.setScope('branch');
    await waitLoaded(state);
    expect(state.revealedPaths.size).toBe(0);
  });

  it('toggleCollapseAll flips every file and allCollapsed tracks the set', async () => {
    const patch = [patchFor('src/small.ts', 2), patchFor('pnpm-lock.yaml', 2)].join('\n');
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);

    const state = reviewStateForPane('pane-1', subjectFor());
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
    setReviewDiffMock('OpenWorkspaceDiff', async () => {
      throw new Error('diff exploded');
    });
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(state.error).toBe('diff exploded');
    expect(state.files).toEqual([]);

    setReviewDiffMock('OpenWorkspaceDiff', async () => 'diff --git a/a b/a\n--- a/a\n+++ b/a\n@@ -1 +1 @@\n+ok');
    await state.reload();

    expect(state.error).toBeNull();
    expect(state.files).toHaveLength(1);
  });

  it('restores a persisted scope before the first load', async () => {
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'branch', baseBranch: 'develop' }));
    const branch = setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(state.scope).toBe('branch');
    expect(branch).toHaveBeenCalledWith(REVIEW_WS, 'develop', false);
  });

  it('falls back to workspace scope for a persisted scope that no longer exists', async () => {
    // 'turn' and 'session' were removed with the checkpoint machinery;
    // stale persisted entries must not wedge the pane.
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'turn', baseBranch: null }));
    const workspace = setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('workspace.go', 1));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(state.scope).toBe('workspace');
    expect(workspace).toHaveBeenCalledWith(REVIEW_WS, false);
  });

  it('refreshes comments and records the active diff source after loading a patch', async () => {
    const patch = patchFor('src/app.ts', 1);
    const sourceKey = diffSourceKey(patch);
    const listComments = setBindingMock('ListDiffReviewComments', async () => [
      draft({ sourceKey }),
    ]);
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);

    const state = reviewStateForPane('pane-1', subjectFor());
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

  it('sendComments no-ops while a turn is active', async () => {
    const patch = patchFor('src/app.ts', 1);
    const sourceKey = diffSourceKey(patch);
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);
    const send = setBindingMock('SendDiffReviewComments', async () => ({}));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    replaceDiffReviewCommentsForTest('thread-1', 'workspace', sourceKey, [
      draft({ sourceKey }),
    ]);
    projectTurnStarted('thread-1', 'turn-1', 0, 100);

    await state.sendComments();

    expect(send).not.toHaveBeenCalled();
  });
});

const PR_KEY = 'github:owner/repo:5';
const PR_SOURCE_KEY = `pr:${PR_KEY}`;

/** The ordinary PR subject: the pane's workspace has PR #5 open. */
function prSubject(paneId = 'pane-1'): ReviewSubject {
  seedPaneWorkspaceStatus(paneId, {
    forge: 'github',
    openPrUrl: 'https://github.com/owner/repo/pull/5',
    openPrNumber: 5,
  });
  return subjectFor();
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
    baseRefSHA: 'base-main',
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
    readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: false, viewerCanApprove: false, viewerApproved: false },
    ...overrides,
  };
}

function reviewThreadStub(id: string): ReviewThread {
  return {
    id,
    path: 'src/app.ts',
    line: 1,
    side: 'right',
    isResolvable: true,
    isResolved: false,
    isOutdated: false,
    comments: [],
  };
}

function installPRMocks(): {
  subscribe: ReturnType<typeof vi.fn>;
  unsubscribe: ReturnType<typeof vi.fn>;
} {
  const subscribe = setBindingMock('SubscribePRUpdates', async () => ({
    id: 'sub-1',
    prKey: PR_KEY,
    detail: prDetailStub(),
    threads: [],
    headSHA: 'sha-a',
  }));
  const unsubscribe = setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));
  setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
  return { subscribe, unsubscribe };
}

function ciPipelineStub(status: string, steps?: { number: number; name: string; status: string }[]) {
  return {
    status,
    stages: [{
      name: 'test',
      status,
      jobs: [{ id: '20', name: 'unit', status, logsAvailable: true, ...(steps ? { steps } : {}) }],
    }],
  };
}

/** installPRMocks with a subscribe result that carries a running pipeline. */
function installCIMocks(): void {
  installPRMocks();
  setBindingMock('SubscribePRUpdates', async () => ({
    id: 'sub-1',
    prKey: PR_KEY,
    detail: prDetailStub(),
    threads: [],
    headSHA: 'sha-a',
    seq: 3,
    ci: ciPipelineStub('running', [{ number: 1, name: 'build', status: 'running' }]),
    ciError: '',
  }));
}

/** The wire state of version `version` of a `lines`-line log. */
function wireLog(version: number, lines: number, seq: number, overrides: Record<string, unknown> = {}) {
  return {
    version, totalLines: lines, totalBytes: lines * 7, stableLines: 0, sections: [],
    available: true, downloading: false, error: '', seq, ...overrides,
  };
}

function installLogFollows(lines: number, seq = 5) {
  return setBindingMock('SetPRCILogFollows', async (_id: string, jobs: string[]) => ({
    logs: Object.fromEntries(jobs.map((job) => [job, wireLog(1, lines, seq)])),
  }));
}

async function flushPane(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await tick();
}

describe('reviewPane store — PR scope', () => {
  it('enter subscribes and loads the PR diff; leaving unsubscribes exactly once', async () => {
    const { subscribe, unsubscribe } = installPRMocks();
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);

    await state.setScope('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
    // The diff is opened at the snapshot's revision: its head and the
    // base branch's tip.
    expect(diff).toHaveBeenCalledWith(
      REVIEW_WS,
      expect.objectContaining({ Number: 5 }),
      { baseRef: 'main', baseSHA: 'base-main', headSHA: 'sha-a' },
      false,
      false,
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
    const commitDiff = setReviewDiffMock('OpenPRCommitDiff', async () => patchFor('src/app.ts', 2));

    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const revision = { baseRef: 'main', baseSHA: 'base-main', headSHA: 'sha-a' };
    expect(list).toHaveBeenCalledWith(REVIEW_WS, expect.objectContaining({ Number: 5 }), revision, false);
    expect(state.commits.map((commit) => commit.sha)).toEqual([sha]);
    expect(state.sourceKey).toBe(PR_SOURCE_KEY);

    await state.selectCommit(sha);
    expect(state.selectedCommitSHA).toBe(sha);
    expect(state.sourceKey).toBe(`commit:${sha}`);
    expect(commitDiff).toHaveBeenLastCalledWith(REVIEW_WS, expect.objectContaining({ Number: 5 }), revision, sha, false, false);

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
    setReviewDiffMock('OpenPRCommitDiff', async () => patchFor('src/app.ts', 2));

    const state = reviewStateForPane('pane-1', prSubject());
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
    setReviewDiffMock('OpenPRCommitDiff', async () => patchFor('src/app.ts', 2));
    const submit = setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));

    const state = reviewStateForPane('pane-1', prSubject());
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
    setReviewDiffMock('OpenPRCommitDiff', async () => patchFor('src/app.ts', 2));
    const fullDiff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));

    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    expect(state.commits).toEqual([]);
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.files.length).toBe(1);
  });

  it('switching scope away mid-PR-load lands on the new scope and closes the late subscription', async () => {
    // The scope selector stays enabled while a PR loads, so a slow
    // PR fetch must not pin the pane: the superseded load's
    // subscription has to be closed when it finally resolves.
    const { unsubscribe } = installPRMocks();
    let releaseDiff!: (patch: string) => void;
    setReviewDiffMock('OpenPRDiff', () => new Promise<string>((resolve) => {
      releaseDiff = resolve;
    }));
    setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('src/app.ts', 2));
    const state = reviewStateForPane('pane-1', prSubject());
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

  it('a diff failure surfaces the error and keeps the PR subscription for the retry', async () => {
    const { unsubscribe } = installPRMocks();
    setReviewDiffMock('OpenPRDiff', async () => {
      throw new Error('diff exploded');
    });
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);

    await state.setScope('pr');
    expect(state.error).toContain('diff exploded');
    // The pane is still on the PR, so the entity hold is still wanted: the
    // detail/threads it feeds render the header, and a retry must not have
    // to re-subscribe.
    expect(unsubscribe).not.toHaveBeenCalled();
    expect(state.prDetail?.number).toBe(5);

    await state.setScope('workspace');
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
  });

  it('a failed refresh keeps the diff on screen, its comment source and editors, under the error', async () => {
    installPRMocks();
    setBindingMock('ListDiffReviewComments', async () => []);
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const filesBefore = state.files;
    const sourceBefore = state.sourceKey;
    expect(filesBefore).toHaveLength(1);

    setReviewDiffMock('OpenPRDiff', async () => {
      throw new Error('open PR diff: fetch pull request commits: git fetch exited with code 128: fatal: unreachable');
    });
    await state.reload();

    expect(state.error).toContain('fatal: unreachable');
    expect(state.files).toBe(filesBefore);
    expect(state.sourceKey).toBe(sourceBefore);
    expect(state.prHeadSHA).toBe('sha-a');
  });

  it('a failed -w refresh puts the toggle back to the diff still on screen', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const filesBefore = state.files;

    setReviewDiffMock('OpenPRDiff', async () => {
      throw new Error('fetch failed');
    });
    await state.setIgnoreWhitespace(true);

    expect(state.error).toContain('fetch failed');
    expect(state.files).toBe(filesBefore);
    expect(state.ignoreWhitespace).toBe(false);
  });

  it('a failed first PR load shows no files and retries on the next snapshot frame', async () => {
    installPRMocks();
    let fail = true;
    const diff = setReviewDiffMock('OpenPRDiff', async () => {
      if (fail) throw new Error('fetch failed');
      return patchFor('src/app.ts', 3);
    });
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(state.error).toContain('fetch failed');
    expect(state.files).toEqual([]);
    const callsAfterFailure = diff.mock.calls.length;
    await flushPane();
    // Nothing retries on its own between frames.
    expect(diff.mock.calls.length).toBe(callsAfterFailure);

    fail = false;
    // The pump re-emits the unchanged snapshot after a failed read.
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub(), threads: [], headSHA: 'sha-a', seq: 50 });
    await vi.waitFor(() => expect(state.files).toHaveLength(1));
    expect(state.error).toBeNull();
    expect(diff.mock.calls.length).toBe(callsAfterFailure + 1);
  });

  it('a failed refresh of a diff whose PR moved leaves the reload to the stale banner', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const filesBefore = state.files;

    const failing = setReviewDiffMock('OpenPRDiff', async () => {
      throw new Error('fetch failed');
    });
    await state.reload();
    expect(state.files).toBe(filesBefore);
    const calls = failing.mock.calls.length;

    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
      seq: 60,
    });
    await flushPane();
    expect(failing.mock.calls.length).toBe(calls);
    expect(state.prStale).toBe(true);
    expect(state.files).toBe(filesBefore);
  });

  it('a failed refresh retries when only the base tip moved, which raises no stale banner', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    let fail = true;
    const diff = setReviewDiffMock('OpenPRDiff', async () => {
      if (fail) throw new Error('fetch failed');
      return patchFor('src/app.ts', 5);
    });
    await state.reload();
    expect(state.error).toContain('fetch failed');

    fail = false;
    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub({ baseRefSHA: 'base-main-2' }),
      threads: [],
      headSHA: 'sha-a',
      seq: 70,
    });
    await vi.waitFor(() => expect(state.error).toBeNull());
    expect(state.prStale).toBe(false);
    expect(diff).toHaveBeenLastCalledWith(
      REVIEW_WS,
      expect.objectContaining({ Number: 5 }),
      { baseRef: 'main', baseSHA: 'base-main-2', headSHA: 'sha-a' },
      false,
      false,
    );
  });

  it('waits while the forge has not reported the base tip, and loads once it does', async () => {
    installPRMocks();
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: PR_KEY,
      detail: prDetailStub({ baseRefSHA: '' }),
      threads: [],
      headSHA: 'sha-a',
      seq: 1,
    }));
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(state.awaitingPR).toBe(true);
    expect(state.error).toBeNull();
    expect(diff).not.toHaveBeenCalled();

    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub(), threads: [], headSHA: 'sha-a', seq: 2 });
    await vi.waitFor(() => expect(state.files).toHaveLength(1));
    expect(state.awaitingPR).toBe(false);
  });

  it('dispose during an in-flight subscribe closes the late-arriving subscription', async () => {
    const { unsubscribe } = installPRMocks();
    let resolveSubscribe: ((value: unknown) => void) | undefined;
    setBindingMock('SubscribePRUpdates', () => new Promise((resolve) => {
      resolveSubscribe = resolve;
    }));

    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    const entering = state.setScope('pr');
    await vi.waitFor(() => {
      expect(resolveSubscribe).toBeDefined();
    });
    disposeReviewStateForPane('pane-1', state);
    resolveSubscribe?.({
      id: 'sub-late',
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads: [],
      headSHA: 'sha-a',
    });
    await entering;

    expect(unsubscribe).toHaveBeenCalledWith('sub-late');
  });

  it('replacing a pane state on thread switch disposes the old PR subscription', async () => {
    const { unsubscribe } = installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    reviewStateForPane('pane-1', subjectFor('thread-2'));

    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
  });

  it('disposal releases only the state it names', async () => {
    const { unsubscribe } = installPRMocks();
    const stale = reviewStateForPane('pane-1', subjectFor('thread-2'));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    // The replaced state's late teardown leaves its successor alone.
    disposeReviewStateForPane('pane-1', stale);
    expect(unsubscribe).not.toHaveBeenCalled();
    expect(reviewStateForPane('pane-1', prSubject())).toBe(state);

    disposeReviewStateForPane('pane-1', state);
    expect(unsubscribe).toHaveBeenCalledWith('sub-1');
    // Reopening the same subject builds a fresh state.
    expect(reviewStateForPane('pane-1', prSubject())).not.toBe(state);
  });

  it('pr:updated applies live on same head and flags stale on a moved head without touching the diff', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const filesBefore = state.files;

    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub({ mergeability: 'conflicts' }),
      threads: [],
      headSHA: 'sha-a',
    });
    expect(state.prStale).toBe(false);
    expect(state.prDetail?.mergeability).toBe('conflicts');

    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub({ headSHA: 'sha-b' }),
      threads: [],
      headSHA: 'sha-b',
    });
    expect(state.prStale).toBe(true);
    // prHeadSHA is the head the RENDERED diff was loaded at, not the live
    // head — that is exactly what makes the banner derivable per pane.
    expect(state.prHeadSHA).toBe('sha-a');
    expect(state.prDetail?.headSHA).toBe('sha-b');
    expect(state.files).toBe(filesBefore);

    await state.reload();
    expect(state.prStale).toBe(false);
    expect(state.prHeadSHA).toBe('sha-b');
  });

  it('leaving the PR drops the head its diff was anchored to', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(state.prHeadSHA).toBe('sha-a');

    await state.setScope('workspace');
    // The anchor describes a diff of THAT PR. Kept around, it would be
    // compared against the NEXT PR's live head and raise a stale banner on
    // a diff that was never loaded.
    expect(state.prHeadSHA).toBe('');
    expect(state.prStale).toBe(false);
  });

  it('a PR→PR switch never compares the old PR\'s loaded head against the new PR\'s live head', async () => {
    // The workspace's open-PR reference is what moves under a pane: a
    // branch's MR merges and a new one opens, and the probe re-resolves it.
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/owner/repo/pull/5',
      openPrNumber: 5,
    });
    installPRMocks();
    setBindingMock('SubscribePRUpdates', async (pr: { Number: number }) => ({
      id: `sub-${pr.Number}`,
      prKey: `github:owner/repo:${pr.Number}`,
      detail: prDetailStub({ number: pr.Number, headSHA: pr.Number === 5 ? 'sha-a' : 'sha-z' }),
      threads: [],
      headSHA: pr.Number === 5 ? 'sha-a' : 'sha-z',
    }));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(state.prRef?.number).toBe(5);
    expect(state.prHeadSHA).toBe('sha-a');
    expect(state.prStale).toBe(false);

    // PR #5 merged; the workspace now points at #7, which another pane has
    // already observed at a completely unrelated head.
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/owner/repo/pull/7',
      openPrNumber: 7,
    });
    let releaseDiff!: (patch: string) => void;
    setReviewDiffMock('OpenPRDiff', () => new Promise<string>((resolve) => {
      releaseDiff = resolve;
    }));

    const reloading = state.reload();
    await vi.waitFor(() => {
      expect(state.prRef?.number).toBe(7);
      expect(releaseDiff).toBeTypeOf('function');
    });
    // The pane is now reading PR #7's live head (sha-z) while the diff on
    // screen was computed for PR #5 at sha-a. Those are two unrelated OIDs:
    // the anchor is stamped with the PR it belongs to, so it simply does not
    // apply here — no banner, and no head quoted from another pull request.
    expect(state.prStale).toBe(false);
    expect(state.prHeadSHA).toBe('');
    // Nor does any merge action run on #7 while its diff is not on screen:
    // the controls are withheld, and the live head is never pinned in the
    // loaded one's place.
    expect(state.mergeWithheldReason).toBe('The diff is still loading.');
    const approve = setBindingMock('ApprovePR', async () => ({ seq: 2 }));
    await state.approvePR();
    expect(approve).not.toHaveBeenCalled();

    releaseDiff(patchFor('src/app.ts', 3));
    await reloading;
    expect(state.prHeadSHA).toBe('sha-z');
    expect(state.prStale).toBe(false);
    expect(state.mergeWithheldReason).toBeNull();
  });

  it('withholds the merge actions after the PR diff failed to load', async () => {
    installPRMocks();
    setReviewDiffMock('OpenPRDiff', async () => {
      throw new Error('fetch pull request diff: HTTP 502');
    });
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    await vi.waitFor(() => {
      expect(state.error).toContain('HTTP 502');
    });
    expect(state.prDetail?.number).toBe(5);
    expect(state.mergeWithheldReason).toBe('The diff has not loaded for this pull request. Reload it to act on it.');
    const approve = setBindingMock('ApprovePR', async () => ({ seq: 2 }));
    await state.approvePR();
    expect(approve).not.toHaveBeenCalled();
  });

  it('refuses a second merge action until the pump\'s answer to the first is on screen', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const approve = setBindingMock('ApprovePR', async () => ({ seq: 2 }));
    const unapprove = setBindingMock('UnapprovePR', async () => ({ seq: 3 }));
    await state.approvePR();
    expect(approve).toHaveBeenCalledTimes(1);
    expect(state.mergeAction).toBe('approve');
    // Still pending: the call answered, the frame it named has not landed.
    await state.approvePR();
    await state.unapprovePR();
    expect(approve).toHaveBeenCalledTimes(1);
    expect(unapprove).not.toHaveBeenCalled();
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub(), threads: [], headSHA: 'sha-a', seq: 2 });
    expect(state.mergeAction).toBeNull();
  });

  it('keeps the merged PR across the checkout move the worktree cleanup makes and a thread switch, until the companion closes', async () => {
    installPRMocks();
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 400 }]);
    const state = reviewStateForPane('pane-1', prSubject());
    const companion = openCompanion('pane-1', 'review');
    expect(companion).not.toBeNull();
    await waitLoaded(state);
    await state.setScope('pr');
    expect(state.prRef?.number).toBe(5);
    // The PR merged: git status stops listing it, the pane holds it.
    seedPaneWorkspaceStatus('pane-1', { forge: 'github' });
    await tick();
    expect(state.prRef?.number).toBe(5);
    // The cleanup moves the thread to Base: the companion subject changes
    // (its checkout is part of it) and CompanionPane remounts the body,
    // destroying the old ReviewPane (which disposes its state) before the
    // new one creates its own. The companion stays open throughout, so the
    // held PR carries across and the merged snapshot stays on screen.
    disposeReviewStateForPane('pane-1', state);
    const base: WorkspaceRef = { projectId: REVIEW_WS.projectId, workspacePath: '/tmp/base' };
    const thread = { id: 'thread-1', projectId: REVIEW_WS.projectId } as Thread;
    const baseSubject: ReviewSubject = {
      identity: companionSubjectKey({ thread, workspace: base }),
      threadId: 'thread-1',
      rowId: 'thread-1',
      workspace: base,
    };
    const moved = reviewStateForPane('pane-1', baseSubject);
    expect(moved.scope).toBe('pr');
    expect(moved.prRef?.number).toBe(5);
    await waitLoaded(moved);
    // A thread switch hides the companion (the stash reopens it with the
    // thread) and destroys its ReviewPane: the held PR stays with the
    // thread.
    hideCompanion(companion!.paneId);
    disposeReviewStateForPane('pane-1', moved);
    const reopened = openCompanion('pane-1', 'review');
    const back = reviewStateForPane('pane-1', baseSubject);
    expect(back.prRef?.number).toBe(5);
    await waitLoaded(back);
    // Hidden again, then the thread is deleted: nothing is left to close,
    // and nothing is held for a thread that is gone.
    hideCompanion(reopened!.paneId);
    disposeReviewStateForPane('pane-1', back);
    removeThread('thread-1');
    const gone = reviewStateForPane('pane-1', baseSubject);
    expect(gone.prRef).toBeNull();
    disposeReviewStateForPane('pane-1', gone);
    // Closing the companion is the end of it too: the next state starts clean.
    const again = openCompanion('pane-1', 'review');
    seedPaneWorkspaceStatus('pane-1', { forge: 'github', openPrUrl: 'https://github.com/owner/repo/pull/5', openPrNumber: 5 });
    const held = reviewStateForPane('pane-1', baseSubject);
    await waitLoaded(held);
    expect(held.prRef?.number).toBe(5);
    seedPaneWorkspaceStatus('pane-1', { forge: 'github' });
    await tick();
    expect(held.prRef?.number).toBe(5);
    closeCompanion(again!.paneId);
    disposeReviewStateForPane('pane-1', held);
    const fresh = reviewStateForPane('pane-1', baseSubject);
    expect(fresh.prRef).toBeNull();
    disposeReviewStateForPane('pane-1', fresh);
  });

  it('waits for the pump\'s answer through a re-subscribe to the same backend and gives it up on a restart', async () => {
    const hello = (launchId: string): TransportHello => ({ launchId, protocolVersion: 1, capabilities: [], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    __setTransportHelloForTest(hello('launch-1'));
    const { subscribe } = installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    setBindingMock('ApprovePR', async () => ({ seq: 100 }));
    await state.approvePR();
    expect(state.mergeAction).toBe('approve');
    // Gap recovery re-subscribes to the same backend process: its pump's
    // seqs are the ones the answer named, so the wait goes on.
    __setTransportStatusForTest({ status: 'disconnected', nextAttemptAt: null });
    __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
    await vi.waitFor(() => {
      expect(subscribe).toHaveBeenCalledTimes(2);
    });
    await tick();
    expect(state.mergeAction).toBe('approve');
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub(), threads: [], headSHA: 'sha-a', seq: 100 });
    expect(state.mergeAction).toBeNull();

    // A second action; this time the backend restarts: a new launch, whose
    // pump numbers from one, and whose snapshot is the forge's answer.
    setBindingMock('ApprovePR', async () => ({ seq: 200 }));
    await state.approvePR();
    expect(state.mergeAction).toBe('approve');
    __setTransportHelloForTest(hello('launch-2'));
    await tick();
    expect(state.mergeAction).toBeNull();
  });

  it('a pr:updated error surfaces as pane state and clears on the next good snapshot', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const filesBefore = state.files;

    applyPRUpdatedEvent({ prKey: PR_KEY, error: 'gh api rate limit exceeded' });
    expect(state.prUpdateError).toContain('rate limit');
    // A failed poll is not a reason to blank the PR the user is reading.
    expect(state.prDetail?.number).toBe(5);
    expect(state.files).toBe(filesBefore);

    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads: [],
      headSHA: 'sha-a',
    });
    expect(state.prUpdateError).toBeNull();
  });

  it('one poll heals every pane on the PR, and staleness stays per pane', async () => {
    const { subscribe } = installPRMocks();
    const first = reviewStateForPane('pane-1', prSubject());
    const second = reviewStateForPane('pane-2', prSubject('pane-2'));
    await waitLoaded(first);
    await waitLoaded(second);
    await first.setScope('pr');
    await second.setScope('pr');
    // Both panes ride one pump: the subscription is refcounted by PR key.
    expect(subscribe).toHaveBeenCalledTimes(1);

    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub({ headSHA: 'sha-b', title: 'retitled' }),
      threads: [reviewThreadStub('t-9')],
      headSHA: 'sha-b',
    });

    // One apply, both panes healed — no second fetch, no per-pane copy.
    expect(first.prDetail?.title).toBe('retitled');
    expect(second.prDetail?.title).toBe('retitled');
    expect(first.prThreads.map((t) => t.id)).toEqual(['t-9']);
    expect(second.prThreads.map((t) => t.id)).toEqual(['t-9']);
    expect(first.prStale).toBe(true);
    expect(second.prStale).toBe(true);

    // Reloading ONE pane clears only that pane's banner: the other is still
    // rendering the old head's diff and must keep saying so.
    await first.reload();
    expect(first.prStale).toBe(false);
    expect(second.prStale).toBe(true);
  });

  it('the last pane to leave the PR releases the shared subscription', async () => {
    const { subscribe, unsubscribe } = installPRMocks();
    const first = reviewStateForPane('pane-1', prSubject());
    const second = reviewStateForPane('pane-2', prSubject('pane-2'));
    await waitLoaded(first);
    await waitLoaded(second);
    await first.setScope('pr');
    await second.setScope('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);

    await first.setScope('workspace');
    expect(unsubscribe).not.toHaveBeenCalled();

    await second.setScope('workspace');
    expect(unsubscribe).toHaveBeenCalledTimes(1);
    expect(unsubscribe).toHaveBeenCalledWith('sub-1');

    // Re-entering after the refcount hit zero subscribes again rather than
    // resurrecting a released handle.
    await first.setScope('pr');
    expect(subscribe).toHaveBeenCalledTimes(2);
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
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    await state.openConflictView();

    // Conflict files open expanded like the regular diff: the open
    // itself loads content and clears the collapsed set.
    expect(state.conflictView).toBe(true);
    expect(state.conflicts?.paths).toEqual(['main.go']);
    expect(getFile).toHaveBeenCalledTimes(1);
    expect(getFile).toHaveBeenCalledWith(REVIEW_WS, 'tree-1', 'main.go');
    expect(state.conflictCollapsedPaths.has('main.go')).toBe(false);
    // Pseudo-diff shape: hunk header, then ours as del / theirs as add
    // between visible marker rows, relabeled with base/head labels.
    expect(linesOf(state.conflictFiles[0])?.map((line) => line.type)).toEqual([
      'meta',
      'context',
      'marker',
      'del',
      'marker',
      'add',
      'marker',
      'context',
    ]);
    expect(linesOf(state.conflictFiles[0])?.[2]?.content).toBe('<<<<<<< origin/main');
    expect(linesOf(state.conflictFiles[0])?.[6]?.content).toBe('>>>>>>> feature');
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    await state.openConflictView();

    // Only unattributed messages stay on the fallback strip.
    expect(state.conflicts?.messages).toEqual(['warning: unattributable message']);

    // The note-bearing file expands despite the failed content load: its
    // body is the note row, and the badge carries the conflict type.
    expect(state.conflictCollapsedPaths.has('other.go')).toBe(false);
    const structural = state.conflictFiles.find((file) => file.path === 'other.go');
    expect(linesOf(structural)).toEqual([{ content: note, type: 'marker' }]);
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
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    await state.openConflictView();
    expect(linesOf(state.conflictFiles[0])?.[0]?.fold).toEqual({ id: 0, lines: 7 });

    // Find reads the folds the view built, not a rebuild per call.
    const folds = state.conflictFoldLines('main.go');
    expect(folds).toBe(state.conflictFoldLines('main.go'));
    expect([...folds!]).toEqual([[0, ['c1', 'c2', 'c3', 'c4', 'c5', 'c6', 'c7']]]);
    expect(state.conflictFoldLines('other.go')).toBeNull();

    state.expandConflictFold('main.go', 0);
    expect(state.conflictFoldLines('main.go')!.size).toBe(0);
    const expanded = state.conflictFiles[0];
    expect(linesOf(expanded)?.some((line) => line.fold)).toBe(false);
    expect(linesOf(expanded)?.[1]?.content).toBe(' c1');

    // Closing and reopening the view resets expansion state.
    await state.setScope('workspace');
    await state.setScope('pr');
    await state.openConflictView();
    expect(linesOf(state.conflictFiles[0])?.[0]?.fold).toEqual({ id: 0, lines: 7 });
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
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

  it('reads CI from the subscription and follows the opened job\'s log', async () => {
    installCIMocks();
    const follows = installLogFollows(2);
    const reads = setBindingMock('GetPRCILogLines', async (_pr: unknown, _job: string, from: number, count: number) => ({
      from, lines: ['line 1', 'line 2', 'line 3'].slice(from, from + count), totalLines: 3, version: 2,
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    expect(state.ciLoading).toBe(false);
    expect(state.ciPipeline?.status).toBe('running');
    const job = state.ciPipeline!.stages[0]!.jobs[0]!;
    state.openCIJobLog('test', job);
    expect(state.ciLogLoading).toBe(true);
    expect(state.ciLog).toBeNull();
    await flushPane();
    expect(follows).toHaveBeenCalledWith('sub-1', ['20']);
    expect(state.ciLogLoading).toBe(false);
    expect(state.ciLogView?.jobId).toBe('20');
    expect(state.ciLog).toMatchObject({ version: 1, totalLines: 2, available: true });

    applyPRCILogEvent({ prKey: PR_KEY, jobId: '20', ...wireLog(2, 3, 6, { stableLines: 2 }) });
    expect(state.ciLog).toMatchObject({ version: 2, totalLines: 3, stableLines: 2 });
    // The open job's lines are read from the computer the PR lives on.
    const window = await state.readCILogLines(1, 2);
    expect(window.lines).toEqual(['line 2', 'line 3']);
    expect(reads).toHaveBeenLastCalledWith(
      { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo' }, '20', 1, 2);

    state.closeCILogView();
    await flushPane();
    expect(follows).toHaveBeenLastCalledWith('sub-1', []);
    expect(state.ciLogView).toBeNull();
    expect(state.ciLog).toBeNull();
    await expect(state.readCILogLines(0, 1)).rejects.toThrow('No CI job log is open.');

    // Leaving pr scope: this pane no longer reports the PR's CI.
    await state.setScope('workspace');
    expect(state.ciPipeline).toBeNull();
  });

  it('moves the open job\'s status and steps with the pipeline', async () => {
    installCIMocks();
    installLogFollows(1);
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
    await flushPane();
    expect(state.ciLogView?.job.status).toBe('running');

    applyPRCIUpdatedEvent({ prKey: PR_KEY, pipeline: ciPipelineStub('failed', [{ number: 1, name: 'build', status: 'failed' }]), seq: 4 });
    expect(state.ciLogView?.job.status).toBe('failed');
    expect(state.ciLogView?.job.steps?.[0]?.status).toBe('failed');

    // A new head's pipeline no longer lists the job: the row captured at
    // open stands rather than the view going blank.
    applyPRCIUpdatedEvent({ prKey: PR_KEY, pipeline: { status: 'running', stages: [] }, seq: 5 });
    expect(state.ciLogView?.jobId).toBe('20');
    expect(state.ciLogView?.job.status).toBe('running');
    expect(state.ciLogView?.stageName).toBe('test');
  });

  it('reads a GitHub job that is still running as unavailable', async () => {
    installCIMocks();
    setBindingMock('SetPRCILogFollows', async () => ({
      logs: { 20: wireLog(0, 0, 5, { available: false }) },
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
    await flushPane();
    expect(state.ciLog?.available).toBe(false);
    expect(state.ciLogError).toBeNull();
  });

  it('refreshCI asks the pump to poll and keeps the pipeline while it runs', async () => {
    installCIMocks();
    let finish!: () => void;
    const refresh = setBindingMock('RefreshPRCI', () => new Promise<void>((resolve) => {
      finish = resolve;
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    const pending = state.refreshCI();
    expect(refresh).toHaveBeenCalledWith('sub-1');
    expect(state.ciRefreshing).toBe(true);
    expect(state.ciLoading).toBe(false);
    expect(state.ciPipeline?.status).toBe('running');
    finish();
    await pending;
    expect(state.ciRefreshing).toBe(false);
  });

  it('refreshCILog re-sends the follow, which refetches the log', async () => {
    installCIMocks();
    const follows = installLogFollows(1);
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
    await flushPane();

    const again = setBindingMock('SetPRCILogFollows', async () => ({ logs: { 20: wireLog(2, 4, 9) } }));
    state.refreshCILog();
    await flushPane();
    expect(follows).toHaveBeenCalledTimes(1);
    expect(again).toHaveBeenCalledWith('sub-1', ['20']);
    expect(state.ciLog).toMatchObject({ version: 2, totalLines: 4 });
  });

  it('clears the follow when the pane is disposed', async () => {
    installCIMocks();
    const follows = installLogFollows(1);
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
    await flushPane();
    // A second pane on the same PR keeps the PR subscribed, so the clear
    // has a live handle to reach.
    const other = attachPR(PR_KEY, { ref: { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 } });
    try {
      disposeReviewStateForPane('pane-1', state);
      await flushPane();
      expect(follows).toHaveBeenLastCalledWith('sub-1', []);
    } finally {
      other.release();
    }
  });

  it('a PR switch closes the old PR\'s job log and clears its follow', async () => {
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/owner/repo/pull/5',
      openPrNumber: 5,
    });
    installPRMocks();
    setBindingMock('SubscribePRUpdates', async (pr: { Number: number }) => ({
      id: `sub-${pr.Number}`,
      prKey: `github:owner/repo:${pr.Number}`,
      detail: prDetailStub({ number: pr.Number }),
      threads: [],
      headSHA: 'sha-a',
      seq: 3,
      ci: ciPipelineStub('running'),
      ciError: '',
    }));
    const follows = installLogFollows(1);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('pr');
    state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
    await flushPane();
    expect(follows).toHaveBeenLastCalledWith('sub-5', ['20']);

    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/owner/repo/pull/7',
      openPrNumber: 7,
    });
    // Until the reload releases PR #5 the open log still reads #5.
    expect(state.ciLogView?.jobId).toBe('20');
    await state.reload();
    await flushPane();
    expect(state.prRef?.number).toBe(7);
    expect(state.ciLogView).toBeNull();
    expect(follows).toHaveBeenLastCalledWith('sub-5', []);
  });

  it('opening the conflict view closes the CI log view and vice versa', async () => {
    installCIMocks();
    installLogFollows(1);
    setBindingMock('GetPRMergeConflicts', async () => ({
      conflicted: false,
      treeOID: 'tree-clean',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: [],
      messages: [],
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    const job = { id: '20', name: 'unit', status: 'failed', logsAvailable: true };
    state.openCIJobLog('test', job);
    expect(state.ciLogView).not.toBeNull();

    await state.openConflictView();
    expect(state.ciLogView).toBeNull();
    expect(state.conflictView).toBe(true);

    state.openCIJobLog('test', job);
    expect(state.conflictView).toBe(false);
    expect(state.ciLogView).not.toBeNull();
  });

  it('sendCILogToChat saves the log and prefills the source pane composer', async () => {
    installCIMocks();
    installLogFollows(1);
    let finishSave!: (path: string) => void;
    const save = setBindingMock('SavePRCIJobLog', () => new Promise<string>((resolve) => {
      finishSave = resolve;
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    let content = '';
    const dispose = registerComposerDraft('pane-1', {
      get content() { return content; },
      setContent(next: string) { content = next; },
    } as never);

    try {
      state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
      await flushPane();
      const sending = state.sendCILogToChat();
      // A pipeline frame while the save runs re-derives the view; the same
      // log is still open, so the save's result lands.
      applyPRCIUpdatedEvent({ prKey: PR_KEY, pipeline: ciPipelineStub('failed'), seq: 4 });
      finishSave('/data/ci-logs/github-owner-repo-pr5-20-unit.log');
      await sending;

      expect(save).toHaveBeenCalledWith(
        { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo' },
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

  it('keeps the open log\'s expanded sections per job, and starts each open collapsed', async () => {
    installCIMocks();
    installLogFollows(1);
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    const job = state.ciPipeline!.stages[0]!.jobs[0]!;

    state.openCIJobLog('test', job);
    state.toggleCILogSection('step:1');
    state.setCILogSectionsOpen(['step:2', 'step:3'], true);
    expect([...state.ciLogOpenSections]).toEqual(['20/step:1', '20/step:2', '20/step:3']);
    state.toggleCILogSection('step:1');
    state.setCILogSectionsOpen(['step:3'], false);
    expect([...state.ciLogOpenSections]).toEqual(['20/step:2']);

    // The same job opened again, without closing first, starts collapsed.
    state.openCIJobLog('test', job);
    expect(state.ciLogOpenSections.size).toBe(0);

    state.toggleCILogSection('step:1');
    state.closeCILogView();
    expect(state.ciLogOpenSections.size).toBe(0);
    // No log open: nothing to expand.
    state.toggleCILogSection('step:1');
    expect(state.ciLogOpenSections.size).toBe(0);
  });

  it('sends a short section inline from its lines and a long one as a file the backend saves', async () => {
    installCIMocks();
    installLogFollows(4);
    const save = setBindingMock('SavePRCIJobLogSection', async () => '/data/ci-logs/section.log');
    const ESC = String.fromCharCode(27);
    const log = ['setup', `${ESC}[31mFAIL${ESC}[0m \`\`\`x\`\`\``, 'exit 2', 'after'];
    const reads = setBindingMock('GetPRCILogLines', async (_pr: unknown, _job: string, from: number, count: number) => ({
      from, lines: log.slice(from, from + count), totalLines: log.length, version: 1,
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    let content = 'Earlier draft';
    const dispose = registerComposerDraft('pane-1', {
      get content() { return content; },
      setContent(next: string) { content = next; },
    } as never);
    try {
      state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
      await flushPane();

      const step = { key: 'step:2', name: 'Run make test', status: 'failed', start: 1, end: 3, lines: 2, bytes: 40 };
      await state.sendCILogSectionToChat(step);
      expect(save).not.toHaveBeenCalled();
      expect(reads).toHaveBeenCalledWith(expect.anything(), '20', 1, 2);
      // Plain text: the colors the view renders are dropped.
      expect(content).toBe([
        'Earlier draft',
        '',
        'Investigate `Run make test` (failed) in CI job `unit` (test) on PR #5, status: running.',
        '````',
        'FAIL ```x```\nexit 2',
        '````',
      ].join('\n'));

      // Over the inline cap: the backend saves it by key and the lines
      // are never read here.
      content = '';
      reads.mockClear();
      const big = { key: 'section:step_script:1', name: 'step_script', status: 'done', start: 0, end: 4, lines: 4, bytes: CI_SECTION_INLINE_MAX_BYTES + 1 };
      await state.sendCILogSectionToChat(big);
      expect(reads).not.toHaveBeenCalled();
      expect(save).toHaveBeenCalledWith(
        { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo' },
        '20',
        'unit',
        'section:step_script:1',
      );
      expect(content).toBe([
        'Investigate `step_script` in CI job `unit` (test) on PR #5, status: running.',
        'Section log saved at: /data/ci-logs/section.log',
      ].join('\n'));

      // A failed save or read reaches the log view and leaves the draft alone.
      setBindingMock('SavePRCIJobLogSection', async () => {
        throw new Error('disk full');
      });
      await state.sendCILogSectionToChat(big);
      expect(state.ciLogError).toBe('disk full');
      setBindingMock('GetPRCILogLines', async () => {
        throw new Error('the downloaded log is gone; refresh to download it again');
      });
      await state.sendCILogSectionToChat(step);
      expect(state.ciLogError).toBe('the downloaded log is gone; refresh to download it again');
      expect(content).toBe([
        'Investigate `step_script` in CI job `unit` (test) on PR #5, status: running.',
        'Section log saved at: /data/ci-logs/section.log',
      ].join('\n'));
    } finally {
      dispose();
    }
  });

  it('copies a short section\'s text and a long one\'s saved path', async () => {
    installCIMocks();
    installLogFollows(2);
    setBindingMock('GetPRCILogLines', async (_pr: unknown, _job: string, from: number, count: number) => ({
      from, lines: ['one', 'two'].slice(from, from + count), totalLines: 2, version: 1,
    }));
    const save = setBindingMock('SavePRCIJobLogSection', async () => '/data/ci-logs/big.log');
    const writes: string[] = [];
    let refuse = false;
    // happy-dom has no clipboard; the stub is an own property the
    // shared-worker guard expects gone when the test ends.
    const original = Object.getOwnPropertyDescriptor(navigator, 'clipboard');
    Object.defineProperty(navigator, 'clipboard', {
      configurable: true,
      value: {
        writeText: async (text: string) => {
          if (refuse) throw new DOMException('denied', 'NotAllowedError');
          writes.push(text);
        },
      },
    });
    try {
      const state = reviewStateForPane('pane-1', prSubject());
      await waitLoaded(state);
      await state.setScope('pr');
      state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
      await flushPane();

      expect(await state.copyCILogSection({ key: 'job', name: 'unit', status: 'running', start: 0, end: 2, lines: 2, bytes: 8 })).toBe(true);
      expect(writes).toEqual(['one\ntwo']);
      expect(save).not.toHaveBeenCalled();

      const big = { key: 'job', name: 'unit', status: 'running', start: 0, end: 2, lines: 2, bytes: CI_SECTION_INLINE_MAX_BYTES + 1 };
      expect(await state.copyCILogSection(big)).toBe(true);
      expect(save).toHaveBeenCalledWith(expect.anything(), '20', 'unit', 'job');
      expect(writes.at(-1)).toBe('/data/ci-logs/big.log');
      expect(state.ciLogSavedPath).toBe('/data/ci-logs/big.log');
      expect(getToasts().at(-1)?.message).toBe('The section is over 64 KB: it was saved to a file, and the path is copied.');

      // A refused clipboard is a failed copy, said in a toast.
      refuse = true;
      expect(await state.copyCILogSection({ key: 'job', name: 'unit', status: 'running', start: 0, end: 2, lines: 2, bytes: 8 })).toBe(false);
      expect(getToasts().at(-1)?.message).toBe('Failed to copy');
    } finally {
      delete (navigator as { clipboard?: unknown }).clipboard;
      if (original) Object.defineProperty(navigator, 'clipboard', original);
    }
  });

  it('shows a save failure over the log and the log\'s own failure after it clears', async () => {
    installCIMocks();
    setBindingMock('SetPRCILogFollows', async () => ({
      logs: { 20: wireLog(1, 1, 5, { error: 'failed to fetch job log (id: q)' }) },
    }));
    setBindingMock('SavePRCIJobLog', async () => {
      throw new Error('disk full');
    });
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    state.openCIJobLog('test', state.ciPipeline!.stages[0]!.jobs[0]!);
    await flushPane();
    expect(state.ciLogError).toBe('failed to fetch job log (id: q)');

    await state.saveCILog();
    expect(state.ciLogError).toBe('disk full');
    state.refreshCILog();
    expect(state.ciLogError).toBe('failed to fetch job log (id: q)');
  });

  it('detects an open PR from git status at mount without switching scope', async () => {
    // The scope dropdown's PR option is gated on a resolved prRef, so a
    // thread whose BRANCH has an open PR must resolve it proactively —
    // entering pr scope can't be the trigger (the option wouldn't exist).
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/acme/widgets/pull/7',
      openPrNumber: 7,
    });

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    await vi.waitFor(() => {
      expect(state.prRef).toEqual({ forge: 'github', host: 'github.com', namespace: 'acme', repo: 'widgets', number: 7 });
    });
    // Detection only surfaces the option; it never hijacks the scope.
    expect(state.scope).toBe('workspace');
  });

  it('detects a PR opened after mount without a reload', async () => {
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.prRef).toBeNull();

    // The push that carries the newly-opened PR lands on the shared store;
    // `prRef` derives from it live, so the option surfaces (and the MR
    // badge's open-review click works) with no reload and no scope entry.
    // Regression: a once-at-mount probe raced the git-status load, leaving
    // a boot-restored pane with no PR option until pr scope was forced.
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'gitlab',
      openPrUrl: 'https://gitlab.com/group/sub/repo/-/merge_requests/3',
      openPrNumber: 3,
    });

    await vi.waitFor(() => {
      expect(state.prRef).toEqual({ forge: 'gitlab', host: 'gitlab.com', namespace: 'group/sub', repo: 'repo', number: 3 });
    });
  });

  it('a pane restored into pr scope before git status lands waits, then loads when the ref resolves', async () => {
    installPRMocks();
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 2));
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'pr' }));
    // No pane registered and no git status yet: the boot load runs before
    // the fetch lands. That is input still coming, not a missing PR, so
    // nothing is said and no diff is asked for.
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.scope).toBe('pr');
    expect(state.awaitingPR).toBe(true);
    expect(state.error).toBeNull();
    expect(diff).not.toHaveBeenCalled();

    // The enriched status lands on the shared store; the watcher loads
    // with no user interaction and nothing else was ever shown.
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/owner/repo/pull/5',
      openPrNumber: 5,
    });
    await vi.waitFor(() => {
      expect(state.files.length).toBe(1);
    });
    expect(state.prRef?.number).toBe(5);
    expect(state.awaitingPR).toBe(false);
    expect(state.error).toBeNull();
    expect(diff).toHaveBeenCalledTimes(1);
  });


  it('a pane restored into pr scope waits through the fast status whose PR lookup is pending', async () => {
    installPRMocks();
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 2));
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'pr' }));
    // The first status a subscribe answers with is the fast one: no PR
    // fields, and the lookup still pending. That is not "no PR".
    seedPaneWorkspaceStatus('pane-1', { forge: 'github', openPrLookupPending: true });
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.awaitingPR).toBe(true);
    expect(state.error).toBeNull();
    expect(diff).not.toHaveBeenCalled();

    // The warmed status replaces it with the PR; the diff loads.
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/owner/repo/pull/5',
      openPrNumber: 5,
    });
    await vi.waitFor(() => {
      expect(state.files.length).toBe(1);
    });
    expect(state.awaitingPR).toBe(false);
    expect(state.error).toBeNull();
  });

  it('a pane restored into pr scope answers "no PR" once git status lands without one', async () => {
    installPRMocks();
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'pr' }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.awaitingPR).toBe(true);

    seedPaneWorkspaceStatus('pane-1', {});
    await vi.waitFor(() => {
      expect(state.error).toContain('No PR or MR');
    });
    expect(state.awaitingPR).toBe(false);
    expect(state.prRef).toBeNull();
  });

  it('restores a persisted pr scope from the workspace\'s open PR', async () => {
    const { subscribe } = installPRMocks();
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'pr' }));

    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);

    expect(state.scope).toBe('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
    expect(state.error).toBeNull();
  });

  // The thread's pr-scope drafts as the backend holds them: listed, marked
  // by the two-call flow's MarkDiffReviewCommentsSent, and by a draft
  // submit that posted them (`posts`: whether the forge takes the review).
  function threadDraftsBackend(rows: DiffReviewComment[]) {
    const list = setBindingMock('ListDiffReviewComments', async (_thread: string, scope: string, sourceKey: string) =>
      rows.filter((row) => row.scope === scope && row.sourceKey === sourceKey).map((row) => ({ ...row })));
    const mark = setBindingMock('MarkDiffReviewCommentsSent', async (_thread: string, _scope: string, _source: string, ids: string[]) => {
      for (const row of rows) if (ids.includes(row.id)) row.status = 'sent';
    });
    return { list, mark };
  }

  function draftSubmitHello(capabilities: string[], launchId = 'l'): void {
    __setTransportHelloForTest({ launchId, protocolVersion: 1, capabilities, backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
  }

  async function prScopeWith(rows: DiffReviewComment[]): Promise<ReturnType<typeof reviewStateForPane>> {
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    replaceDiffReviewCommentsForTest('thread-1', 'pr', PR_SOURCE_KEY, rows.map((row) => ({ ...row })));
    return state;
  }

  describe('a backend without the draft submit (two calls)', () => {
    beforeEach(() => draftSubmitHello([]));
    afterEach(() => __setTransportHelloForTest(null));

    it('posts the comments, marks the drafts sent against the head SHA and clears the summary', async () => {
      installPRMocks();
      const rows = [draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' })];
      const { mark } = threadDraftsBackend(rows);
      const submit = getBindingMock('SubmitPRReview')!;
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await state.submitPRReview();

      expect(submit.mock.calls[0]?.[1]).toEqual({ verdict: 'comment', body: 'overall note', comments: [{ path: 'src/app.ts', body: 'comment', line: 1, side: 'right' }] });
      expect(mark).toHaveBeenCalledWith('thread-1', 'pr', PR_SOURCE_KEY, ['d-1'], 'pr:sha-a');
      expect(state.summaryBody).toBe('');
      expect(state.submitError).toBeNull();
      expect(state.drafts).toEqual([]);
    });

    it('sends no attempt id and, after a lost answer, asks nothing and says the review may be on the PR', async () => {
      installPRMocks();
      const rows = [draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' })];
      threadDraftsBackend(rows);
      const submit = setBindingMock('SubmitPRReview', async () => {
        throw new DisconnectedError('socket closed');
      });
      const ask = setBindingMock('GetPRReviewSubmitOutcome', async () => ({ state: 'unknown' }));
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

      expect(submit.mock.calls[0]?.[1]).not.toHaveProperty('attemptId');
      expect(submit.mock.calls[0]?.[1]).not.toHaveProperty('drafts');
      expect(ask).not.toHaveBeenCalled();
      expect(state.summaryBody).toBe('overall note');
      expect(state.submitError).toContain('may already be on the PR');
    });

    it('a partial submit marks the drafts that posted and keeps the unposted file draft', async () => {
      installPRMocks();
      setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0, partialFailurePath: 'src/app.ts', partialFailure: 'boom' }));
      const rows = [
        // Same file: the line draft rode the batched review and posted; only
        // the file-level draft (standalone follow-up call) failed.
        draft({ id: 'd-file', scope: 'pr', sourceKey: PR_SOURCE_KEY, filePath: 'src/app.ts', side: 'file', newLine: undefined }),
        draft({ id: 'd-line', scope: 'pr', sourceKey: PR_SOURCE_KEY, filePath: 'src/app.ts', newLine: 1, side: 'new' }),
      ];
      const { mark } = threadDraftsBackend(rows);
      const state = await prScopeWith(rows);

      await state.submitPRReview();

      expect(mark).toHaveBeenCalledWith('thread-1', 'pr', PR_SOURCE_KEY, ['d-line'], 'pr:sha-a');
      expect(state.submitError).toContain('src/app.ts');
      expect(state.drafts.map((comment) => comment.id)).toEqual(['d-file']);
    });

    it('a pathless partial failure (approve failed after publish) marks all drafts sent and keeps the error', async () => {
      installPRMocks();
      setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0, partialFailure: 'glab api approve merge request failed' }));
      const rows = [draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' })];
      const { mark } = threadDraftsBackend(rows);
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await state.submitPRReview();

      expect(mark).toHaveBeenCalledWith('thread-1', 'pr', PR_SOURCE_KEY, ['d-1'], 'pr:sha-a');
      expect(state.submitError).toContain('approve');
      expect(state.summaryBody).toBe('overall note');
    });

    it('a mark that fails after the review posted ends as posted and says the drafts must not go again', async () => {
      installPRMocks();
      const rows = [draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' })];
      const { mark } = threadDraftsBackend(rows);
      mark.mockImplementation(async () => {
        throw new DisconnectedError('socket closed');
      });
      const submit = getBindingMock('SubmitPRReview')!;
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');
      state.setVerdict('request-changes');

      await state.submitPRReview();

      expect(submit).toHaveBeenCalledTimes(1);
      expect(state.summaryBody).toBe('');
      expect(state.verdict).toBe('comment');
      expect(state.submitError).toContain('The review posted');
      expect(state.submitError).toContain('not marked sent');
      expect(state.submitError).not.toContain('may already be on the PR');
    });

    it('a thrown submit keeps every draft and surfaces the error', async () => {
      installPRMocks();
      setBindingMock('SubmitPRReview', async () => {
        throw new Error('gh api submit review failed');
      });
      const rows = [draft({ id: 'd-1', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' })];
      const { mark } = threadDraftsBackend(rows);
      const state = await prScopeWith(rows);

      await expect(state.submitPRReview()).rejects.toThrow('gh api submit review failed');

      expect(mark).not.toHaveBeenCalled();
      expect(state.submitError).toContain('gh api submit review failed');
      expect(state.drafts.map((comment) => comment.id)).toEqual(['d-1']);
    });
  });

  describe('a backend with the draft submit (pr-review-drafts.v1)', () => {
    beforeEach(() => draftSubmitHello(['pr-review-drafts.v1']));
    afterEach(() => __setTransportHelloForTest(null));

    const NOT_DRAFTS = 'Some of these comments were already sent, so this review and its summary were not posted.';
    const UNMARKED = 'These comments were posted by an earlier review and are being marked sent, so this review was not posted.';
    const MAY_BE_ON_PR = 'The connection dropped before the forge answered, so the review may already be on the PR. Check the PR before you submit again.';
    const NEVER_ARRIVED = 'The connection dropped before the review reached the computer, so nothing was posted.';
    const UNCONFIRMED = 'The forge did not confirm the review, so it may already be on the PR. Check the PR before you submit again.';
    const FORGOTTEN = 'The computer no longer has this review\'s outcome, so it may already be on the PR. Check the PR before you submit again.';

    function row(id = 'd-1'): DiffReviewComment {
      return draft({ id, scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 1, side: 'new' });
    }

    // The backend's draft submit over `rows`, recording each attempt's
    // outcome as the backend does. It refuses drafts that are not drafts
    // (or, with `unmarked`, drafts an earlier review posted); otherwise it
    // posts and marks them. `lose` drops the answer after the backend
    // handled the call, `never` before it arrived; `restart` also gives
    // the reconnect a new launch.
    // `unconfirmed` answers as a forge that did not confirm the review.
    function draftBackend(rows: DiffReviewComment[], opts: { lose?: 'after' | 'never'; restart?: boolean; unmarked?: boolean; unconfirmed?: boolean } = {}) {
      const outcomes = new Map<string, PRReviewSubmitOutcome>();
      let lose = opts.lose;
      const submit = setBindingMock('SubmitPRReview', async (_wire: unknown, review: { body: string; drafts?: { ids: string[] }; attemptId?: string }) => {
        const losing = lose;
        lose = undefined;
        if (opts.restart) draftSubmitHello(['pr-review-drafts.v1'], 'launch-2');
        if (losing === 'never') throw new DisconnectedError('socket closed');
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
        if (review.attemptId && !opts.restart) outcomes.set(review.attemptId, outcome);
        if (losing === 'after') throw new DisconnectedError('socket closed');
        if (outcome.state === 'failed') throw new TransportError(outcome.code!, outcome.message!);
        return outcome.result;
      });
      const ask = setBindingMock('GetPRReviewSubmitOutcome', async (_wire: unknown, id: string) => outcomes.get(id) ?? { state: 'unknown' });
      return { submit, ask, loseNext: (how: 'after' | 'never') => { lose = how; } };
    }

    it('names the drafts and a fresh attempt in one call, and marks nothing itself', async () => {
      installPRMocks();
      const rows = [row()];
      const { mark } = threadDraftsBackend(rows);
      const { submit } = draftBackend(rows);
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await state.submitPRReview();

      expect(submit.mock.calls[0]?.[1]).toEqual({
        verdict: 'comment', body: 'overall note', comments: [],
        drafts: { threadId: 'thread-1', ids: ['d-1'], headSha: 'sha-a' },
        attemptId: expect.any(String),
      });
      expect(mark).not.toHaveBeenCalled();
      expect(state.summaryBody).toBe('');
      expect(state.submitError).toBeNull();
      expect(state.drafts).toEqual([]);
      state.setSummaryBody('another note');
      await state.submitPRReview();
      expect(attemptOf(submit, 1)).not.toBe(attemptOf(submit, 0));
    });

    for (const withDraft of [true, false]) {
      it(`a lost answer after the review posted asks the backend and ends as posted (${withDraft ? 'with a draft' : 'summary only'})`, async () => {
        installPRMocks();
        const rows = withDraft ? [row()] : [];
        threadDraftsBackend(rows);
        const { submit, ask } = draftBackend(rows, { lose: 'after' });
        const state = await prScopeWith(rows);
        state.setSummaryBody('overall note');

        await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

        expect(ask).toHaveBeenCalledWith(expect.anything(), attemptOf(submit, 0));
        expect(state.summaryBody).toBe('');
        expect(state.submitError).toBeNull();
        expect(state.drafts).toEqual([]);
        expect(submit).toHaveBeenCalledTimes(1);
      });
    }

    it('a lost request that never reached the backend posted nothing, keeps everything, and goes once on the next Send', async () => {
      installPRMocks();
      const rows = [row()];
      threadDraftsBackend(rows);
      const { submit } = draftBackend(rows, { lose: 'never' });
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');
      state.setVerdict('request-changes');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

      expect(state.summaryBody).toBe('overall note');
      expect(state.verdict).toBe('request-changes');
      expect(state.drafts.map((comment) => comment.id)).toEqual(['d-1']);
      expect(state.submitError).toBe(NEVER_ARRIVED);
      await state.submitPRReview();
      expect(submit).toHaveBeenCalledTimes(2);
      expect(attemptOf(submit, 1)).not.toBe(attemptOf(submit, 0));
      expect(state.summaryBody).toBe('');
      expect(state.drafts).toEqual([]);
    });

    it('a lost answer from a backend that restarted keeps the summary, says the review may be on the PR, and posts nothing by itself', async () => {
      installPRMocks();
      const rows = [row()];
      threadDraftsBackend(rows);
      const { submit } = draftBackend(rows, { lose: 'after', restart: true });
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

      expect(state.summaryBody).toBe('overall note');
      expect(state.submitError).toBe(MAY_BE_ON_PR);
      expect(submit).toHaveBeenCalledTimes(1);
    });

    it('a press of Send while the outcome cannot be asked asks again and posts nothing', async () => {
      installPRMocks();
      const rows = [row()];
      threadDraftsBackend(rows);
      const { submit, ask } = draftBackend(rows, { lose: 'after' });
      const answers = ask.getMockImplementation()!;
      ask.mockImplementation(async () => {
        throw new DisconnectedError('socket closed');
      });
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);
      expect(state.summaryBody).toBe('overall note');
      expect(state.submitError).toContain('Send asks again');

      ask.mockImplementation(answers);
      await state.submitPRReview();
      expect(submit).toHaveBeenCalledTimes(1);
      expect(state.summaryBody).toBe('');
      expect(state.submitError).toBeNull();
    });

    it('drafts another tab posted that are not marked yet refuse this review, and keep its summary and verdict', async () => {
      installPRMocks();
      const rows = [row()];
      threadDraftsBackend(rows);
      draftBackend(rows, { unmarked: true });
      const state = await prScopeWith(rows);
      state.setSummaryBody('from tab B');
      state.setVerdict('request-changes');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(TransportError);

      expect(state.summaryBody).toBe('from tab B');
      expect(state.verdict).toBe('request-changes');
      expect(state.submitError).toBe(UNMARKED);
    });

    for (const sentBy of ['another tab', 'a send to an agent'] as const) {
      it(`a refusal whose answer was lost, the drafts sent by ${sentBy}, keeps this summary and verdict`, async () => {
        installPRMocks();
        const rows = [row()];
        threadDraftsBackend(rows);
        const { submit } = draftBackend(rows, { lose: 'after' });
        const state = await prScopeWith(rows);
        state.setSummaryBody('from tab B');
        state.setVerdict('request-changes');
        // Sent before this submit reaches the backend.
        rows[0]!.status = 'sent';
        if (sentBy === 'a send to an agent') rows[0]!.sentTurnId = 'turn-agent';

        await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

        expect(state.summaryBody).toBe('from tab B');
        expect(state.verdict).toBe('request-changes');
        expect(state.submitError).toBe(NOT_DRAFTS);
        expect(state.drafts).toEqual([]);
        // The summary and verdict go on the next Send.
        await state.submitPRReview();
        expect(submit.mock.calls[1]?.[1]).toMatchObject({ verdict: 'request-changes', body: 'from tab B', drafts: { ids: [] } });
        expect(state.summaryBody).toBe('');
      });
    }

    it('settles a lost answer against the scope it was sent from, after the pane switched scope', async () => {
      installPRMocks();
      const rows = [row()];
      const { list } = threadDraftsBackend(rows);
      setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('src/app.ts', 1));
      let switched: Promise<void> | null = null;
      const state = await prScopeWith(rows);
      const { submit } = draftBackend(rows, { lose: 'after' });
      const posting = submit.getMockImplementation()!;
      submit.mockImplementation(async (...args: Parameters<typeof posting>) => {
        switched = state.setScope('workspace');
        return posting(...args);
      });
      state.setSummaryBody('overall note');
      list.mockClear();

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);
      await switched;

      expect(state.scope).toBe('workspace');
      expect(list).toHaveBeenCalledWith('thread-1', 'pr', PR_SOURCE_KEY);
      expect(state.summaryBody).toBe('');
      await state.setScope('pr');
      replaceDiffReviewCommentsForTest('thread-1', 'pr', PR_SOURCE_KEY, rows.map((each) => ({ ...each })));
      expect(state.drafts).toEqual([]);
      expect(state.submitError).toBeNull();
    });

    for (const lost of [false, true]) {
      it(`a review the forge did not confirm keeps everything, says it may be on the PR, and posts again only on Send (${lost ? 'answer lost' : 'answered'})`, async () => {
        installPRMocks();
        const rows = [row()];
        threadDraftsBackend(rows);
        const { submit, ask } = draftBackend(rows, { unconfirmed: true, lose: lost ? 'after' : undefined });
        const state = await prScopeWith(rows);
        state.setSummaryBody('overall note');
        state.setVerdict('request-changes');

        if (lost) await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);
        else await state.submitPRReview();

        expect(ask).toHaveBeenCalledTimes(lost ? 1 : 0);
        expect(state.summaryBody).toBe('overall note');
        expect(state.verdict).toBe('request-changes');
        expect(state.drafts.map((comment) => comment.id)).toEqual(['d-1']);
        expect(state.submitError).toBe(UNCONFIRMED);
        expect(submit).toHaveBeenCalledTimes(1);
        // The attempt is closed: Send posts a new one, not a re-ask.
        await state.submitPRReview();
        expect(submit).toHaveBeenCalledTimes(2);
        expect(attemptOf(submit, 1)).not.toBe(attemptOf(submit, 0));
      });
    }

    it('a lost answer whose outcome the backend no longer keeps says the review may be on the PR', async () => {
      installPRMocks();
      const rows = [row()];
      threadDraftsBackend(rows);
      const { submit, ask } = draftBackend(rows, { lose: 'never' });
      ask.mockImplementation(async () => ({ state: 'forgotten' }));
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await expect(state.submitPRReview()).rejects.toBeInstanceOf(DisconnectedError);

      expect(state.summaryBody).toBe('overall note');
      expect(state.drafts.map((comment) => comment.id)).toEqual(['d-1']);
      expect(state.submitError).toBe(FORGOTTEN);
      expect(submit).toHaveBeenCalledTimes(1);
    });

    it('a review that posted with its drafts left unmarked ends as posted and says so', async () => {
      installPRMocks();
      const rows = [row()];
      threadDraftsBackend(rows);
      setBindingMock('SubmitPRReview', async () => ({
        postedReview: true, postedFileComments: 0, posted: true,
        partialFailure: 'Its comments are not marked sent yet. They will not post again, and the app marks them on its next read.',
        partialFailureKind: 'unmarked',
      }));
      const state = await prScopeWith(rows);
      state.setSummaryBody('overall note');

      await state.submitPRReview();

      expect(state.summaryBody).toBe('');
      expect(state.submitError).toContain('not marked sent yet');
    });

    it('a draft edited while the review posted stays a draft and is named', async () => {
      installPRMocks();
      const rows = [row('d-1'), draft({ id: 'd-2', scope: 'pr', sourceKey: PR_SOURCE_KEY, newLine: 2, side: 'new' })];
      threadDraftsBackend(rows);
      setBindingMock('SubmitPRReview', async () => {
        rows[0]!.status = 'sent';
        return { postedReview: true, postedFileComments: 0, posted: true, sentDraftIds: ['d-1'], editedDraftIds: ['d-2'] };
      });
      const state = await prScopeWith(rows);

      await state.submitPRReview();

      expect(state.drafts.map((comment) => comment.id)).toEqual(['d-2']);
      expect(state.submitError).toContain('edited while it posted');
    });
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
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    expect(setActive).not.toHaveBeenCalled();

    setDocumentVisibility('hidden');
    expect(setActive).toHaveBeenCalledWith('sub-1', false);
    // Votes are serialized per subscription, so the next one leaves only
    // once this one has answered — see prReviewStore.svelte.test.ts.
    await tick();

    setDocumentVisibility('visible');
    await tick();
    expect(setActive).toHaveBeenCalledWith('sub-1', true);
    expect(setActive).toHaveBeenCalledTimes(2);
  });

  it('a PR load that finishes while the document is hidden starts its pump paused', async () => {
    installPRMocks();
    const setActive = setBindingMock('SetPRUpdatesActive', async () => undefined);
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);

    setDocumentVisibility('hidden');
    setActive.mockClear();
    await state.setScope('pr');
    expect(setActive).toHaveBeenCalledWith('sub-1', false);
  });
});

/**
 * A diff the backend serves in `size`-character chunks: Open returns the
 * first under handle `id`, ReadReviewDiff the rest by offset. `hold`
 * delays the read at that offset until the returned release runs.
 */
function installChunkedDiff(
  name: 'OpenWorkspaceDiff' | 'OpenBranchBaseDiff' | 'OpenPRDiff' | 'OpenTurnEditsDiff',
  patch: string,
  size: number,
  opts: { id?: string; hold?: number; headSha?: string } = {},
) {
  const id = opts.id ?? 'diff-1';
  const chunkAt = (offset: number) => {
    const next = Math.min(patch.length, offset + size);
    return { data: patch.slice(offset, next), offset, nextOffset: next, eof: next === patch.length };
  };
  let unblock!: () => void;
  const held = new Promise<void>((resolve) => { unblock = resolve; });
  const open = setBindingMock(name, async () => ({ id, chunk: chunkAt(0), headSha: opts.headSha ?? '' }));
  const read = setBindingMock('ReadReviewDiff', async (handle: string, offset: number) => {
    if (handle !== id) throw new Error('review diff: not open');
    if (offset === opts.hold) await held;
    return chunkAt(offset);
  });
  const release = setBindingMock('ReleaseReviewDiff', async () => undefined);
  return { open, read, release, unblock };
}

describe('reviewPane store — streamed diffs', () => {
  const patch = [patchFor('src/a.ts', 3), patchFor('src/b.ts', 3), patchFor('src/c.ts', 3)].join('\n');

  it('reads a diff across chunks and releases its handle at the end', async () => {
    const { open, read, release } = installChunkedDiff('OpenWorkspaceDiff', patch, 40);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(open).toHaveBeenCalledTimes(1);
    expect(read.mock.calls.map((call) => call[1])).toEqual(
      Array.from({ length: Math.ceil(patch.length / 40) - 1 }, (_, index) => (index + 1) * 40),
    );
    expect(read.mock.calls.every((call) => call[0] === 'diff-1' && call[2] === REVIEW_DIFF_READ_BYTES)).toBe(true);
    expect(release).toHaveBeenCalledTimes(1);
    expect(release).toHaveBeenCalledWith('diff-1');
    expect(state.files.map((file) => file.path)).toEqual(['src/a.ts', 'src/b.ts', 'src/c.ts']);
    expect(state.sourceKey).toBe(diffSourceKey(patch));
    expect(state.error).toBeNull();
  });

  it('shows the files that arrived while the rest of the diff loads', async () => {
    const second = patch.indexOf('diff --git a/src/c.ts');
    const { unblock, release } = installChunkedDiff('OpenWorkspaceDiff', patch, second, { hold: second });
    const state = reviewStateForPane('pane-1', subjectFor());
    // A file is published once the next section header proves it
    // complete, so the chunk ending before c.ts shows a.ts only.
    await vi.waitFor(() => {
      expect(state.files.map((file) => file.path)).toEqual(['src/a.ts']);
    });
    expect(state.loading).toBe(true);
    // Comments key by the whole patch, which is not known yet.
    expect(state.sourceKey).toBe('');

    unblock();
    await waitLoaded(state);
    expect(state.files.map((file) => file.path)).toEqual(['src/a.ts', 'src/b.ts', 'src/c.ts']);
    expect(state.sourceKey).toBe(diffSourceKey(patch));
    expect(release).toHaveBeenCalledTimes(1);
  });

  it('keeps the previous diff on screen until a reload completes', async () => {
    setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('old.ts', 1));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    const previous = state.files;

    // The first chunk completes a.ts, which an empty pane would show.
    const boundary = patch.indexOf('diff --git a/src/c.ts');
    const { unblock } = installChunkedDiff('OpenWorkspaceDiff', patch, boundary, { hold: boundary });
    const reloading = state.reload();
    await vi.waitFor(() => {
      expect(getBindingMock('ReadReviewDiff')).toHaveBeenCalled();
    });
    expect(state.files).toBe(previous);
    expect(state.sourceKey).toBe(diffSourceKey(patchFor('old.ts', 1)));

    unblock();
    await reloading;
    expect(state.files.map((file) => file.path)).toEqual(['src/a.ts', 'src/b.ts', 'src/c.ts']);
  });

  it('stops reading and releases the handle when a newer load supersedes it', async () => {
    const { read, release, unblock } = installChunkedDiff('OpenWorkspaceDiff', patch, 40, { hold: 80 });
    const state = reviewStateForPane('pane-1', subjectFor());
    await vi.waitFor(() => {
      expect(read).toHaveBeenCalledTimes(2);
    });

    setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('branch.go', 1));
    await state.setScope('branch');
    unblock();
    await vi.waitFor(() => {
      expect(release).toHaveBeenCalledWith('diff-1');
    });
    expect(read).toHaveBeenCalledTimes(2);
    expect(state.files.map((file) => file.path)).toEqual(['branch.go']);
  });

  it('releases the handle of a diff read by a disposed pane', async () => {
    const { read, release, unblock } = installChunkedDiff('OpenWorkspaceDiff', patch, 40, { hold: 40 });
    const state = reviewStateForPane('pane-1', subjectFor());
    await vi.waitFor(() => {
      expect(read).toHaveBeenCalledTimes(1);
    });

    disposeReviewStateForPane('pane-1', state);
    unblock();
    await vi.waitFor(() => {
      expect(release).toHaveBeenCalledWith('diff-1');
    });
    expect(read).toHaveBeenCalledTimes(1);
  });

  it('opens the diff again once when its handle was lost mid-read', async () => {
    const chunked = installChunkedDiff('OpenWorkspaceDiff', patch, 40);
    let lost = false;
    setBindingMock('ReadReviewDiff', async (handle: string, offset: number) => {
      if (!lost) {
        lost = true;
        throw new Error('review diff: not open');
      }
      return chunked.read(handle as never, offset as never);
    });
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(chunked.open).toHaveBeenCalledTimes(2);
    expect(state.error).toBeNull();
    expect(state.files.map((file) => file.path)).toEqual(['src/a.ts', 'src/b.ts', 'src/c.ts']);
    expect(state.sourceKey).toBe(diffSourceKey(patch));
  });

  it('surfaces a read failure that a fresh open does not fix', async () => {
    installChunkedDiff('OpenWorkspaceDiff', patch, 40);
    const read = setBindingMock('ReadReviewDiff', async () => {
      throw new Error('review diff: not open');
    });
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(read).toHaveBeenCalledTimes(2);
    expect(state.error).toBe('review diff: not open');
    expect(state.files).toEqual([]);
  });

  it('does not retry an ordinary read failure', async () => {
    const { open } = installChunkedDiff('OpenWorkspaceDiff', patch, 40);
    setBindingMock('ReadReviewDiff', async () => {
      throw new Error('read review diff: git exited 128');
    });
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(open).toHaveBeenCalledTimes(1);
    expect(state.error).toBe('read review diff: git exited 128');
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
    setReviewDiffMock('OpenWorkspaceDiff', async () => gappedPatch);
    const fetchLines = setBindingMock('GetDiffContextLines', async (_ws, req) => {
      const { startLine, endLine } = req as { startLine: number; endLine: number };
      return {
        lines: Array.from({ length: endLine - startLine + 1 }, (_, i) => `src ${startLine + i}`),
        startLine,
        eof: false,
        totalLines: 0,
      };
    });

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    const leading = rowsOf(state.files[0]).find((row) => row.gap)?.gap;
    expect(leading).toMatchObject({ location: 'leading', startNew: 1, endNew: 9 });

    await state.expandDiffContext('src/app.ts', leading!, 'all');
    expect(fetchLines).toHaveBeenCalledWith(REVIEW_WS, expect.objectContaining({
      scope: 'workspace',
      path: 'src/app.ts',
      startLine: 1,
      endLine: 9,
    }));

    const rows = rowsOf(state.files[0]);
    expect(rows.some((row) => row.gap?.location === 'leading')).toBe(false);
    expect(rows.find((row) => row.newLine === 1)?.line.content).toBe(' src 1');
    expect(state.error).toBeNull();
  });

  it('retires the trailing gap on an EOF response', async () => {
    setReviewDiffMock('OpenWorkspaceDiff', async () => gappedPatch);
    setBindingMock('GetDiffContextLines', async () => ({
      lines: ['src 12', 'src 13', 'src 14'],
      startLine: 12,
      eof: true,
      totalLines: 14,
    }));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    const trailing = rowsOf(state.files[0])
      .find((row) => row.gap?.location === 'trailing')?.gap;
    expect(trailing).toMatchObject({ startNew: 12, endNew: -1 });

    await state.expandDiffContext('src/app.ts', trailing!, 'down');

    const rows = rowsOf(state.files[0]);
    expect(state.files[0].newSideTotal).toBe(14);
    expect(rows.some((row) => row.gap)).toBe(true); // leading gap remains
    expect(rows.some((row) => row.gap?.location === 'trailing')).toBe(false);
    expect(rows.at(-1)).toMatchObject({ oldLine: 14, newLine: 14 });
  });

  it('surfaces a fetch failure and clears expansions on reload', async () => {
    setReviewDiffMock('OpenWorkspaceDiff', async () => gappedPatch);
    setBindingMock('GetDiffContextLines', async () => {
      throw new Error('no clone available');
    });

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    const leading = rowsOf(state.files[0]).find((row) => row.gap)?.gap;

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
    expect(rowsOf(state.files[0]).some((row) => row.gap?.location === 'leading')).toBe(false);

    await state.reload();
    expect(rowsOf(state.files[0]).some((row) => row.gap?.location === 'leading')).toBe(true);
  });
});

describe('collapse overrides across reloads', () => {
  it('keeps user collapse/expand choices through a reload, resets on scope switch', async () => {
    setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('src/app.ts', 2));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.collapsedPaths.has('src/app.ts')).toBe(false);

    state.toggleCollapsed('src/app.ts');
    expect(state.collapsedPaths.has('src/app.ts')).toBe(true);

    await state.reload();
    expect(state.collapsedPaths.has('src/app.ts')).toBe(true);

    // Overrides beat fresh defaults in BOTH directions: a lockfile-ish
    // file the user expanded stays expanded after reload.
    setReviewDiffMock('OpenWorkspaceDiff', async () => patchFor('go.sum', 2));
    await state.reload();
    expect(state.collapsedPaths.has('go.sum')).toBe(true); // default
    state.toggleCollapsed('go.sum');
    await state.reload();
    expect(state.collapsedPaths.has('go.sum')).toBe(false); // override held

    // Scope switch is a new subject: overrides reset, defaults return.
    setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('go.sum', 2));
    await state.setScope('branch');
    await state.reload();
    expect(state.collapsedPaths.has('go.sum')).toBe(true);
  });
});

describe('comments-only PR refresh', () => {
  it('refreshes detail + threads without reloading the diff', async () => {
    installPRMocks();
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
    setBindingMock('GetPRDetail', async () => prDetailStub());
    const state = reviewStateForPane('pane-1', prSubject());
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
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
    setBindingMock('GetPRDetail', async () => prDetailStub({ headSHA: 'sha-b' }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');

    await state.refreshPRThreads();

    expect(state.prStale).toBe(true);
    // The pane keeps reporting the head its diff came from; the moved head
    // lives on the shared detail.
    expect(state.prHeadSHA).toBe('sha-a');
    expect(state.prDetail?.headSHA).toBe('sha-b');
    expect(diff).toHaveBeenCalledTimes(1);
  });

  // A refresh asked for while one is running (a merge failure during a
  // manual refresh) is not dropped: the running one may predate what the
  // later ask wants to see.
  it('runs a refresh asked for during one in flight once it ends', async () => {
    installPRMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    let releaseFirst!: () => void;
    const held = new Promise<void>((resolve) => { releaseFirst = resolve; });
    let calls = 0;
    const detail = setBindingMock('GetPRDetail', async () => {
      calls += 1;
      if (calls === 1) await held;
      return prDetailStub({ title: `read ${calls}` });
    });

    const first = state.refreshPRThreads();
    await flushPane();
    void state.refreshPRThreads();
    void state.refreshPRThreads();
    releaseFirst();
    await first;
    await flushPane();
    expect(detail).toHaveBeenCalledTimes(2);
    expect(state.prDetail?.title).toBe('read 2');
    expect(state.refreshingPRData).toBe(false);
  });

  it('is a no-op outside pr scope', async () => {
    const detail = setBindingMock('GetPRDetail', async () => prDetailStub());
    const state = reviewStateForPane('pane-1', subjectFor());
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
    const payload = setReviewDiffMock('OpenEditDiff', async () => gappyPatch());
    const turnDiff = setReviewDiffMock('OpenTurnEditsDiff', async () => gappyPatch());
    // Load-time expandability pass: by default every candidate verifies,
    // so gap arrows appear once the (fire-and-forget) result lands.
    const verify = setBindingMock('VerifyEditDiffs', async (_threadId, req) => ({
      expandablePaths: (req as { files: { path: string }[] }).files.map((file) => file.path),
    }));
    return { list, payload, turnDiff, verify };
  }

  // The verification pass is fire-and-forget off the load; assertions on
  // gap affordances wait for its result to land.
  async function waitGapsVerified(state: ReturnType<typeof reviewStateForPane>, path: string): Promise<void> {
    await vi.waitFor(() => {
      expect(state.files.find((file) => file.path === path)?.suppressGaps).toBeUndefined();
    });
  }

  it('defaults to the latest turn and keys comments by turn', async () => {
    const { turnDiff } = installEditMocks();
    const state = reviewStateForPane('pane-1', subjectFor());
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
    const contextLines = setBindingMock('GetEditDiffContextLines', async () => ({
      lines: ['top 1', 'top 2', 'top 3', 'top 4'],
      startLine: 1,
      eof: false,
      totalLines: 20,
    }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    await state.setScope('edits');
    // A single-section historical diff offers hunk-gap expansion like
    // any live scope — once the load-time verification pass proves the
    // backend can serve it (snapshot or still-matching workspace).
    await waitGapsVerified(state, 'x.go');
    const gapRow = rowsOf(state.files[0]).find((row) => row.gap);
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
    setBindingMock('GetEditDiffContextLines', async () => {
      throw new Error('x.go has changed since this edit');
    });
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');
    await waitGapsVerified(state, 'x.go');

    const gapRow = rowsOf(state.files[0]).find((row) => row.gap);
    expect(gapRow).toBeDefined();

    await state.expandDiffContext('x.go', gapRow!.gap!, 'up');
    // A click-time refusal (the rare load-to-click race): the file's
    // gap affordances retire quietly — the historical diff itself is
    // still fully valid, so no banner.
    expect(state.error).toBeNull();
    expect(state.files[0].suppressGaps).toBe(true);
    expect(rowsOf(state.files[0]).some((row) => row.gap)).toBe(false);
  });

  it('retires a file\'s gaps when a refusal follows a successful expansion', async () => {
    installEditMocks();
    const contextLines = setBindingMock('GetEditDiffContextLines', async () => ({
      lines: ['top 3', 'top 4'],
      startLine: 3,
      eof: false,
      totalLines: 20,
    }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');
    await waitGapsVerified(state, 'x.go');

    await state.expandDiffContext('x.go', rowsOf(state.files[0]).find((row) => row.gap)!.gap!, 'up');
    expect(contextLines).toHaveBeenCalledTimes(1);
    const gap = rowsOf(state.files[0]).find((row) => row.gap)?.gap;
    expect(gap).toBeDefined();

    // The file drifted after the first click: the next one is refused.
    setBindingMock('GetEditDiffContextLines', async () => {
      throw new Error('x.go has changed since this edit');
    });
    await state.expandDiffContext('x.go', gap!, 'up');
    expect(state.error).toBeNull();
    expect(state.files[0].suppressGaps).toBe(true);
    // The fetched context stays; the arrows go.
    expect(state.files[0].body.toPatchLines().some((line) => line.content === ' top 4')).toBe(true);
    expect(rowsOf(state.files[0]).some((row) => row.gap)).toBe(false);
  });

  it('verifies a large turn in bounded batches', async () => {
    installEditMocks();
    const modified = (path: string, filler = 0) => [
      `diff --git a/${path} b/${path}`,
      `--- a/${path}`,
      `+++ b/${path}`,
      '@@ -5,2 +5,2 @@',
      ` ${'x'.repeat(filler)}`,
      '-old',
      '+new',
    ].join('\n');
    const sections = Array.from({ length: 450 }, (_, index) => modified(`src/f${String(index).padStart(3, '0')}.go`));
    // Two files large enough that each fills a batch on its own.
    sections.push(modified('src/big1.go', 5 * 1024 * 1024), modified('src/big2.go', 5 * 1024 * 1024));
    setReviewDiffMock('OpenTurnEditsDiff', async () => sections.join('\n'));
    const verify = setBindingMock('VerifyEditDiffs', async (_threadId, req) => ({
      expandablePaths: (req as { files: { path: string }[] }).files.map((file) => file.path),
    }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');

    await vi.waitFor(() => {
      expect(state.files.every((file) => file.suppressGaps === undefined)).toBe(true);
    });
    const batches = verify.mock.calls.map((call) => (call[1] as { files: { path: string; verifyPatch: string }[] }).files);
    expect(batches.flat().map((file) => file.path).sort()).toEqual(state.files.map((file) => file.path).sort());
    for (const batch of batches) {
      expect(batch.length).toBeLessThanOrEqual(200);
      expect(batch.reduce((chars, file) => chars + file.verifyPatch.length, 0)).toBeLessThanOrEqual(8 * 1024 * 1024);
    }
    expect(batches.length).toBe(4);
  });

  it('gates gap arrows on load-time verification', async () => {
    const { verify } = installEditMocks();
    setBindingMock('VerifyEditDiffs', async () => ({ expandablePaths: [] }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');

    // Nothing verified → no arrows, ever: an unservable expansion
    // affordance must never render (drifted pre-snapshot history,
    // remote clients whose ungranted RPC rejects).
    expect(state.files[0].suppressGaps).toBe(true);
    expect(rowsOf(state.files[0]).some((row) => row.gap)).toBe(false);
    // The batch carried the edit selection and per-file verify patch.
    expect(verify).not.toHaveBeenCalled();
    const batch = getBindingMock('VerifyEditDiffs')!.mock.calls.at(-1);
    expect(batch?.[0]).toBe('thread-1');
    expect(batch?.[1]).toMatchObject({
      editPayloadId: '',
      editTurnIndex: 2,
      files: [
        { path: 'x.go', verifyPatch: expect.stringContaining('@@ -5,3 +5,3 @@') },
      ],
    });
  });

  it('opens pinned to the clicked edit and keys comments by payload id', async () => {
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 1 }]);
    const { payload } = installEditMocks();

    const state = await openReviewCompanion('pane-1', subjectFor(), {
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

  it('keeps an open new-comment editor when the selected edit is opened again, and drops it for another edit', async () => {
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 1 }]);
    installEditMocks();
    const state = await openReviewCompanion('pane-1', subjectFor(), { editItemId: 'tool:2a' });
    await waitLoaded(state!);
    const anchor = { filePath: 'x.go', side: 'new' as const, newLine: 6, selectedText: 'new' };
    const id = state!.openDraftEditor(anchor)!;
    state!.setDraftBody(id, 'Guard this.');

    expect(await openReviewCompanion('pane-1', subjectFor(), { editItemId: 'tool:2a' })).toBe(state);
    await waitLoaded(state!);
    expect(state!.selectedEditKey).toBe('item:tool:2a');
    expect(state!.openEditors.map((editor) => [editor.id, editor.anchor])).toEqual([[id, anchor]]);
    expect(state!.draftBodyFor(id)).toBe('Guard this.');

    await openReviewCompanion('pane-1', subjectFor(), { editItemId: 'tool:1' });
    await waitLoaded(state!);
    expect(state!.selectedEditKey).toBe('item:tool:1');
    expect(state!.openEditors).toEqual([]);
    expect(state!.draftBodyFor(id)).toBe('');
  });

  it('selection changes reuse the loaded list (fast path)', async () => {
    const { list, payload, turnDiff } = installEditMocks();
    const state = reviewStateForPane('pane-1', subjectFor());
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
    setReviewDiffMock('OpenTurnEditsDiff', async () => twiceEdited);

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');

    expect(state.files.map((file) => file.path)).toEqual(['x.go']);
    expect(state.files[0].additions).toBe(2);
    expect(state.files[0].deletions).toBe(1);
    // Disjoint sections renumber into ONE coherent section — a single
    // meta block with hunks in final-file order — so the merged file
    // verifies, primes, and gap-expands like a single-section diff.
    await waitGapsVerified(state, 'x.go');
    expect(rowsOf(state.files[0]).some((row) => row.gap)).toBe(true);
    const contents = state.files[0].body.toPatchLines().map((line) => line.content);
    expect(contents.filter((content) => content.startsWith('diff --git'))).toHaveLength(1);
    expect(contents.filter((content) => content.startsWith('@@'))).toEqual([
      '@@ -5,2 +5,2 @@',
      '@@ -9,1 +9,2 @@',
    ]);
    expect(contents).toContain('+new');
    expect(contents).toContain('+later');
    expect(contents.indexOf('+new')).toBeLessThan(contents.indexOf('+later'));
  });

  it('retires a whole-turn patch before awaiting a selected edit, without disturbing a no-op selection', async () => {
    installEditMocks();
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');
    const files = state.files;
    expect(files).not.toEqual([]);
    await state.selectEdit(state.selectedEditKey);
    expect(state.files).toBe(files);
    let finish!: (patch: string) => void;
    setReviewDiffMock('OpenEditDiff', () => new Promise<string>((resolve) => { finish = resolve; }));
    const selecting = state.selectEdit('item:tool:1');
    await tick();
    expect(state.files).toEqual([]);
    expect(state.files).toEqual([]);
    finish(patchFor('selected.go', 1));
    await selecting;
    expect(state.files.map((f) => f.path)).toEqual(['selected.go']);
  });

  it('retires the old diff before a pending scope switch can reinterpret repeated edit paths', async () => {
    installEditMocks();
    setReviewDiffMock('OpenTurnEditsDiff', async () => `${gappyPatch()}\n${gappyPatch()}`);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');
    expect(state.files).toHaveLength(1);

    let finish!: (patch: string) => void;
    setReviewDiffMock('OpenWorkspaceDiff', () => new Promise<string>((resolve) => { finish = resolve; }));
    const switching = state.setScope('workspace');
    await tick();
    try {
      const rows = buildReviewRows({ files: state.files, viewMode: 'stacked', collapsedPaths: new Set(), drafts: [], openEditors: [] });
      expect(new Set(rows.rowKeys).size).toBe(rows.rowKeys.length);
      expect(state.loading).toBe(true);
      expect(state.files).toEqual([]);
    } finally {
      finish(patchFor('workspace.go', 1));
      await switching;
    }
    expect(state.files.map((file) => file.path)).toEqual(['workspace.go']);
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
    setReviewDiffMock('OpenTurnEditsDiff', async () => outsideEdit);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');

    // The absolute-path edit renders but can never expand (the backend
    // only resolves workspace-relative paths): it is never even sent
    // for verification — no dead arrows.
    await waitGapsVerified(state, 'x.go');
    const outside = state.files.find((file) => file.path.startsWith('/'));
    expect(outside).toBeDefined();
    expect(outside!.suppressGaps).toBe(true);
    expect(rowsOf(outside!).some((row) => row.gap)).toBe(false);
    const verifyBatch = getBindingMock('VerifyEditDiffs')!.mock.calls.at(-1)?.[1] as {
      files: { path: string }[];
    };
    expect(verifyBatch.files.map((file) => file.path)).toEqual(['x.go']);
    // Workspace-relative files keep their gap affordances.
    const inside = state.files.find((file) => file.path === 'x.go');
    expect(inside!.suppressGaps).toBeUndefined();
    expect(rowsOf(inside!).some((row) => row.gap)).toBe(true);
  });

  it('parses an oversized merged diff once across expansion rebuilds', async () => {
    // Past the parse cache's entry cap every parse is fresh. The pane
    // parses once per patch text, so an expansion click rebuilds only
    // the expanded file: every other file keeps its lines identity and
    // with it every per-array memo (display rows, span keys).
    installEditMocks();
    setReviewDiffMock('OpenTurnEditsDiff', async () => (
      oversizedPatch([
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
      ].join('\n'))
    ));
    setBindingMock('GetEditDiffContextLines', async () => ({
      lines: ['l1', 'l2', 'l3', 'l4'],
      startLine: 1,
      eof: false,
      totalLines: 20,
    }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');
    await waitGapsVerified(state, 'x.go');

    const filler = state.files.find((file) => file.path === 'zz-filler.txt')!;
    const x = state.files.find((file) => file.path === 'x.go')!;
    const gapRow = rowsOf(x).find((row) => row.gap);
    expect(gapRow?.gap?.location).toBe('leading');
    await state.expandDiffContext('x.go', gapRow!.gap!, 'all');

    expect(state.files.find((file) => file.path === 'x.go')!.body).not.toBe(x.body);
    expect(state.files.find((file) => file.path === 'zz-filler.txt')).toBe(filler);
  });

  it('shows an empty surface for a thread with no edits', async () => {
    setBindingMock('ListThreadEditDiffs', async () => ({ entries: [], turnLabels: [] }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    await state.setScope('edits');
    expect(state.edits.length).toBe(0);
    expect(state.selectedEditKey).toBeNull();
    expect(state.sourceKey).toBe('');
    expect(state.files.length).toBe(0);
    expect(state.error).toBeNull();
  });

  function editSection(path: string, start: number, text: string): string {
    return [
      `diff --git a/${path} b/${path}`,
      `--- a/${path}`,
      `+++ b/${path}`,
      `@@ -${start},1 +${start},1 @@`,
      `-old ${text}`,
      `+new ${text}`,
    ].join('\n');
  }

  it('merges repeated paths incrementally, rebuilding only a path that gained a section', () => {
    const all = parseReviewFiles([
      editSection('x.go', 1, 'a'),
      editSection('y.go', 1, 'b'),
      editSection('x.go', 9, 'c'),
      editSection('z.go', 1, 'd'),
    ].join('\n'));
    const merge = new EditFileMerge();
    const sections: ReviewFile[] = [all[0], all[1]];

    const first = merge.files(sections);
    expect(first.map((file) => file.path)).toEqual(['x.go', 'y.go']);
    expect(first[0]).toBe(all[0]);

    sections.push(all[2]);
    const second = merge.files(sections);
    expect(second.map((file) => file.path)).toEqual(['x.go', 'y.go']);
    expect(second[0].additions).toBe(2);
    expect(second[1]).toBe(all[1]);

    sections.push(all[3]);
    const third = merge.files(sections);
    expect(third.map((file) => file.path)).toEqual(['x.go', 'y.go', 'z.go']);
    expect(third[0]).toBe(second[0]);

    // A retried read fills a new parser, and its files start over.
    expect(merge.files([all[1]]).map((file) => file.path)).toEqual(['y.go']);
  });

  it('streams a whole turn with one file per path at every publish', async () => {
    installEditMocks();
    const patch = [
      editSection('x.go', 1, 'a'),
      editSection('y.go', 1, 'b'),
      editSection('x.go', 9, 'c'),
      editSection('z.go', 1, 'd'),
    ].join('\n');
    const lineEnd = (header: string, from = 0) => patch.indexOf('\n', patch.indexOf(header, from)) + 1;
    // Each chunk ends after the header that completes the section before it.
    const firstCut = lineEnd('diff --git a/x.go', 1);
    const secondCut = lineEnd('diff --git a/z.go');
    const chunkAt = (offset: number) => {
      const next = offset === 0 ? firstCut : offset === firstCut ? secondCut : patch.length;
      return { data: patch.slice(offset, next), offset, nextOffset: next, eof: next === patch.length };
    };
    let unblock!: () => void;
    const held = new Promise<void>((resolve) => { unblock = resolve; });
    setBindingMock('OpenTurnEditsDiff', async () => ({ id: 'edits-1', chunk: chunkAt(0) }));
    setBindingMock('ReadReviewDiff', async (_id: string, offset: number) => {
      if (offset === secondCut) await held;
      return chunkAt(offset);
    });
    setBindingMock('ReleaseReviewDiff', async () => undefined);
    // Every chunk publishes: the throttle measures a clock that always
    // moves past it.
    let clock = 0;
    const now = vi.spyOn(performance, 'now').mockImplementation(() => (clock += 1000));
    try {
      const state = reviewStateForPane('pane-1', subjectFor());
      await waitLoaded(state);
      void state.setScope('edits');

      await vi.waitFor(() => {
        expect(state.files.find((file) => file.path === 'x.go')?.additions).toBe(2);
      });
      expect(state.loading).toBe(true);
      expect(state.files.map((file) => file.path)).toEqual(['x.go', 'y.go']);

      unblock();
      await waitLoaded(state);
      expect(state.files.map((file) => file.path)).toEqual(['x.go', 'y.go', 'z.go']);
      expect(state.files[0].additions).toBe(2);
    } finally {
      now.mockRestore();
    }
  });

  function spanSeed(file: ReviewFile) {
    return {
      path: file.path,
      contentKey: file.body.contentKey(),
      lines: Array.from({ length: file.body.lineCount }, () => ({ r: [0, 1] })),
    };
  }

  it("seeds each joined payload's persisted spans in patch order", async () => {
    resetDiffSpanCacheForTest();
    installEditMocks();
    const turn = [patchFor('src/a.ts', 2), patchFor('src/b.ts', 2)].join('\n');
    const sections = parseReviewFiles(turn);
    setReviewDiffMock('OpenTurnEditsDiff', async () => turn, { payloadIds: ['pl-2a', 'pl-2b'] });
    setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
    setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
    const seeds = setBindingMock('GetPayloadPatchSpans', async (_threadId: string, payloadId: string) => (
      [spanSeed(sections[payloadId === 'pl-2a' ? 0 : 1])]
    ));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');

    await vi.waitFor(() => {
      for (const file of state.files) {
        expect(getSpansForReviewLine(file, 1, linesOf(file)![1])).toEqual({ r: [0, 1] });
      }
    });
    expect(seeds.mock.calls).toEqual([['thread-1', 'pl-2a'], ['thread-1', 'pl-2b']]);
  });

  it('stops seeding spans when a newer load replaces the diff', async () => {
    resetDiffSpanCacheForTest();
    installEditMocks();
    const turn = [patchFor('src/a.ts', 2), patchFor('src/b.ts', 2)].join('\n');
    setReviewDiffMock('OpenTurnEditsDiff', async () => turn, { payloadIds: ['pl-2a', 'pl-2b'] });
    setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
    setBindingMock('HighlightClassNames', async () => ['none', 'keyword']);
    let finish!: () => void;
    const seeds = setBindingMock('GetPayloadPatchSpans', () => new Promise<[]>((resolve) => {
      finish = () => resolve([]);
    }));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');
    await vi.waitFor(() => expect(seeds).toHaveBeenCalledTimes(1));

    await state.selectEdit('item:tool:1');
    finish();
    await new Promise((resolve) => setTimeout(resolve, 0));
    expect(seeds).toHaveBeenCalledTimes(1);
    expect(state.files.map((file) => file.path)).toEqual(['x.go']);
  });
});

// A file whose only change is a re-indent, and one with a real edit.
// Under `-w` git drops the first file entirely and renders the second
// with the same line numbers it uses canonically.
const CANONICAL_PATCH = [
  'diff --git a/src/indent.ts b/src/indent.ts',
  '--- a/src/indent.ts',
  '+++ b/src/indent.ts',
  '@@ -1,3 +1,3 @@',
  ' function run() {',
  '-  work();',
  '+    work();',
  ' }',
  'diff --git a/src/real.ts b/src/real.ts',
  '--- a/src/real.ts',
  '+++ b/src/real.ts',
  '@@ -1,2 +1,2 @@',
  ' const a = 1;',
  '-const b = 2;',
  '+const b = 3;',
].join('\n');

const IGNORED_PATCH = [
  'diff --git a/src/real.ts b/src/real.ts',
  '--- a/src/real.ts',
  '+++ b/src/real.ts',
  '@@ -1,2 +1,2 @@',
  ' const a = 1;',
  '-const b = 2;',
  '+const b = 3;',
].join('\n');

describe('reviewPane hide-whitespace toggle', () => {
  it('is off by default and re-requests the diff on each flip', async () => {
    const workspace = setReviewDiffMock('OpenWorkspaceDiff',
      async (_threadId: string, ignoreWhitespace: boolean) =>
        (ignoreWhitespace ? IGNORED_PATCH : CANONICAL_PATCH),
    );

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.ignoreWhitespace).toBe(false);
    expect(workspace).toHaveBeenLastCalledWith(REVIEW_WS, false);
    expect(state.files.map((file) => file.path)).toEqual(['src/indent.ts', 'src/real.ts']);

    await state.setIgnoreWhitespace(true);
    await waitLoaded(state);
    expect(state.ignoreWhitespace).toBe(true);
    // The flip is a full re-request, not a client-side filter.
    expect(workspace).toHaveBeenLastCalledWith(REVIEW_WS, true);
    expect(state.files.map((file) => file.path)).toEqual(['src/real.ts']);

    await state.setIgnoreWhitespace(false);
    await waitLoaded(state);
    expect(state.ignoreWhitespace).toBe(false);
    expect(workspace).toHaveBeenLastCalledWith(REVIEW_WS, false);
    expect(state.files.map((file) => file.path)).toEqual(['src/indent.ts', 'src/real.ts']);
  });

  it('ignores a flip to the value already set', async () => {
    const workspace = setReviewDiffMock('OpenWorkspaceDiff', async () => CANONICAL_PATCH);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(workspace).toHaveBeenCalledTimes(1);

    await state.setIgnoreWhitespace(false);
    expect(workspace).toHaveBeenCalledTimes(1);
  });

  it('reports which diff sources can honor -w', () => {
    // Gitdiff-backed.
    expect(supportsIgnoreWhitespace('workspace', null)).toBe(true);
    expect(supportsIgnoreWhitespace('branch', null)).toBe(true);
    expect(supportsIgnoreWhitespace('branch', 'a'.repeat(40))).toBe(true);
    expect(supportsIgnoreWhitespace('pr', 'a'.repeat(40))).toBe(true);
    expect(supportsIgnoreWhitespace('pr', null)).toBe(true);
    // Edits replay persisted tool-call patches, never a git recomputation.
    expect(supportsIgnoreWhitespace('edits', null)).toBe(false);
    expect(supportsIgnoreWhitespace('edits', 'a'.repeat(40))).toBe(false);
  });

  it('disables the toggle in edits scope and never sends -w there', async () => {
    setBindingMock('ListThreadEditDiffs', async () => ({
      entries: [{
        itemId: 'item-1',
        payloadId: 'payload-1',
        turnIndex: 0,
        title: 'Edit',
        paths: ['src/real.ts'],
        insertions: 1,
        deletions: 1,
        createdAt: 1,
      }],
      turnLabels: [{ turnIndex: 0, label: 'turn' }],
    }));
    const turnDiff = setReviewDiffMock('OpenTurnEditsDiff', async () => CANONICAL_PATCH);
    setBindingMock('VerifyEditDiffs', async () => ({ verified: [] }));

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.canIgnoreWhitespace).toBe(true);

    await state.setIgnoreWhitespace(true);
    await waitLoaded(state);
    await state.setScope('edits');
    await waitLoaded(state);

    // The flag stays set (flip back to a supported scope and it applies
    // again) but the edits load can't honor it and isn't asked to.
    expect(state.ignoreWhitespace).toBe(true);
    expect(state.canIgnoreWhitespace).toBe(false);
    expect(turnDiff).toHaveBeenCalledTimes(1);
    for (const call of turnDiff.mock.calls) {
      expect(call).not.toContain(true);
    }
  });

  // ---- comment-anchor interplay -------------------------------------
  //
  // The decision: comment creation stays ENABLED under `-w`. git's `-w`
  // patch carries true file line numbers (proved Go-side by
  // TestIgnoreWhitespaceKeepsCanonicalLineNumbers), so an anchor taken
  // from it names the same physical line as the canonical patch and the
  // `path:line` handed to the provider cannot drift.
  //
  // The one residual hazard is a draft that outlives the patch it was
  // written against, which can only happen where the sourceKey is stable
  // across a patch change. These two tests pin both halves.

  it('re-keys drafts by patch content, so a flip cannot re-anchor them', async () => {
    setReviewDiffMock('OpenWorkspaceDiff',
      async (_threadId: string, ignoreWhitespace: boolean) =>
        (ignoreWhitespace ? IGNORED_PATCH : CANONICAL_PATCH),
    );
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    const canonicalKey = diffSourceKey(CANONICAL_PATCH);
    // Persisted against the canonical patch only — the store re-fetches by
    // sourceKey on every reload, which is exactly the guard under test.
    setBindingMock('ListDiffReviewComments', async (_threadId: string, _scope: string, sourceKey: string) =>
      (sourceKey === canonicalKey
        ? [draft({ id: 'd-1', sourceKey: canonicalKey, filePath: 'src/indent.ts', newLine: 2 })]
        : []));
    await state.reload();
    await waitLoaded(state);
    expect(state.sourceKey).toBe(canonicalKey);
    expect(state.drafts.map((entry) => entry.id)).toEqual(['d-1']);

    await state.setIgnoreWhitespace(true);
    await waitLoaded(state);

    // The patch changed, so the content-hashed key changed with it: the
    // draft is not re-anchored against a diff it was never written
    // against. It is hidden, never deleted, and comes back on flip-back.
    expect(state.sourceKey).toBe(diffSourceKey(IGNORED_PATCH));
    expect(state.sourceKey).not.toBe(canonicalKey);
    expect(state.drafts).toEqual([]);

    await state.setIgnoreWhitespace(false);
    await waitLoaded(state);
    expect(state.sourceKey).toBe(canonicalKey);
    expect(state.drafts.map((entry) => entry.id)).toEqual(['d-1']);
  });

  it('marks a carried-over draft orphaned when -w drops its line', async () => {
    // A selected commit keys by SHA, so drafts DO survive the flip. One
    // anchored to a whitespace-only line has nowhere to land in the -w
    // patch — it must be reported orphaned rather than silently vanish
    // from the diff body while still counting toward the tally.
    const sha = 'a'.repeat(40);
    setReviewDiffMock('OpenBranchBaseDiff', async () => CANONICAL_PATCH);
    setBindingMock('ListBranchCommits', async () => [
      { sha, shortSha: 'aaaaaaa', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    setReviewDiffMock('OpenCommitDiff',
      async (_threadId: string, _sha: string, ignoreWhitespace: boolean) =>
        (ignoreWhitespace ? IGNORED_PATCH : CANONICAL_PATCH),
    );

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('branch');
    await state.selectCommit(sha);
    await waitLoaded(state);
    expect(state.sourceKey).toBe(`commit:${sha}`);

    // The commit key is stable, so these come back on every reload —
    // including the one the whitespace flip triggers.
    setBindingMock('ListDiffReviewComments', async () => [
      // On the re-indented line, which -w renders away entirely.
      draft({ id: 'ws-only', scope: 'branch', sourceKey: `commit:${sha}`, filePath: 'src/indent.ts', newLine: 2 }),
      // On the real edit, which -w keeps at the same line number.
      draft({ id: 'real', scope: 'branch', sourceKey: `commit:${sha}`, filePath: 'src/real.ts', newLine: 2 }),
    ]);
    await state.reload();
    await waitLoaded(state);
    // Both anchors resolve against the canonical patch.
    expect(state.drafts.map((entry) => entry.id).sort()).toEqual(['real', 'ws-only']);
    expect([...state.orphanedDraftIds()]).toEqual([]);

    await state.setIgnoreWhitespace(true);
    await waitLoaded(state);

    expect(state.drafts.map((entry) => entry.id).sort()).toEqual(['real', 'ws-only']);
    expect([...state.orphanedDraftIds()]).toEqual(['ws-only']);

    // Flipping back clears the orphan flag — nothing was mutated.
    await state.setIgnoreWhitespace(false);
    await waitLoaded(state);
    expect([...state.orphanedDraftIds()]).toEqual([]);
  });
});

describe('reviewPane store — conversation section and resolve', () => {
  function convThread(id: string, overrides: Partial<ReviewThread> = {}): ReviewThread {
    return {
      ...reviewThreadStub(id),
      comments: [{ authorLogin: 'alice', body: `body of ${id}`, createdAt: '2026-01-01', databaseID: 1 }],
      ...overrides,
    };
  }

  async function statePRWithThreads(threads: ReviewThread[]) {
    installPRMocks();
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads,
      headSHA: 'sha-a',
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    return state;
  }

  function feedThreadIds(state: { conversationFeed: readonly ConversationFeedItem[] }): string[] {
    return state.conversationFeed.flatMap((entry) =>
      (entry.kind === 'thread' ? [entry.thread.id] : []));
  }

  it('freezes ordering while open: arrivals wait behind the new chip, reveal folds them in', async () => {
    const resolvedEarly = convThread('t-resolved', {
      isResolved: true,
      comments: [{ authorLogin: 'bob', body: 'old', createdAt: '2026-01-01', databaseID: 1 }],
    });
    const openLater = convThread('t-open', {
      comments: [{ authorLogin: 'alice', body: 'new', createdAt: '2026-01-02', databaseID: 2 }],
    });
    const state = await statePRWithThreads([resolvedEarly, openLater]);

    // Closed by default; opening freezes the order in hand.
    expect(state.conversationOpen).toBe(false);
    expect(state.conversationFeed).toEqual([]);
    state.setConversationOpen(true);
    expect(state.conversationFrozen).toBe(true);
    // Chronological, newest first; replies unfold by default only on the
    // unresolved thread.
    expect(feedThreadIds(state)).toEqual(['t-open', 't-resolved']);
    expect(state.conversationThreadExpanded('t-open')).toBe(true);
    expect(state.conversationThreadExpanded('t-resolved')).toBe(false);

    // A thread arriving mid-read neither reorders nor appears; it counts.
    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads: [resolvedEarly, openLater, convThread('t-arrived')],
      headSHA: 'sha-a',
    });
    expect(feedThreadIds(state)).toEqual(['t-open', 't-resolved']);
    expect(state.conversationNewCount).toBe(1);

    state.revealNewConversationThreads();
    expect(state.conversationNewCount).toBe(0);
    // The arrival interleaves by its own time (2026-01-01, tie broken by
    // id) rather than jumping the whole feed.
    expect(feedThreadIds(state)).toEqual(['t-open', 't-arrived', 't-resolved']);

    // Closing forgets the frozen view; an explicit reopen freezes what is
    // in hand.
    state.setConversationOpen(false);
    expect(state.conversationFeed).toEqual([]);
    expect(state.conversationFrozen).toBe(false);
    state.setConversationOpen(true);
    expect(state.conversationFrozen).toBe(true);
    expect(feedThreadIds(state)).toEqual(['t-open', 't-arrived', 't-resolved']);
    expect(state.conversationNewCount).toBe(0);
    // Leaving pr scope drops the view but keeps the section open.
    await state.setScope('workspace');
    expect(state.conversationOpen).toBe(true);
    expect(state.conversationFrozen).toBe(false);
  });

  it('interleaves verdicts and commit pushes chronologically, newest first', async () => {
    installPRMocks();
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: PR_KEY,
      detail: prDetailStub({
        latestReviews: [{
          authorLogin: 'rev', state: 'APPROVED',
          submittedAt: '2026-01-03T00:00:00Z', body: '', commitSHA: '',
        }],
      }),
      threads: [convThread('t-1', {
        comments: [{ authorLogin: 'alice', body: 'hm', createdAt: '2026-01-02T00:00:00Z', databaseID: 1 }],
      })],
      headSHA: 'sha-a',
    }));
    // Newest first, one author: one contiguous push row, timed by its
    // newest commit and keyed by its oldest.
    setBindingMock('ListPRCommits', async () => [
      { sha: 'b'.repeat(40), shortSha: 'bbbbbbb', subject: 'second', author: 'ann', authoredAt: Date.parse('2026-01-04T00:00:00Z') },
      { sha: 'a'.repeat(40), shortSha: 'aaaaaaa', subject: 'first', author: 'ann', authoredAt: Date.parse('2026-01-01T12:00:00Z') },
    ]);
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    await waitLoaded(state);

    state.setConversationOpen(true);
    expect(state.conversationFeed.map((entry) => entry.kind)).toEqual([
      'commits', 'verdict', 'thread',
    ]);
    const push = state.conversationFeed[0];
    expect(push?.kind === 'commits' && push.commits.map((commit) => commit.shortSha))
      .toEqual(['bbbbbbb', 'aaaaaaa']);
  });

  it('a remote resolve never folds replies the reader has open', async () => {
    const open = convThread('t-1');
    const state = await statePRWithThreads([open]);
    state.setConversationOpen(true);
    expect(state.conversationThreadExpanded('t-1')).toBe(true);

    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads: [{ ...open, isResolved: true }],
      headSHA: 'sha-a',
    });
    // Content converges (the pill flips), position and the fold hold.
    const first = state.conversationFeed[0];
    expect(first?.kind === 'thread' && first.thread.isResolved).toBe(true);
    expect(state.conversationThreadExpanded('t-1')).toBe(true);

    // ...even across a reveal (new arrivals fold in, open cards stay open).
    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads: [{ ...open, isResolved: true }, convThread('t-2')],
      headSHA: 'sha-a',
    });
    state.revealNewConversationThreads();
    expect(state.conversationThreadExpanded('t-1')).toBe(true);
  });

  it('openConversationAt opens, reveals, expands, and stages the scroll target', async () => {
    const resolved = convThread('t-1', { isResolved: true });
    const state = await statePRWithThreads([resolved]);

    state.setOverviewSectionScrollTop('conversation', 140);
    state.openConversationAt('t-1');
    expect(state.conversationOpen).toBe(true);
    expect(state.conversationThreadExpanded('t-1')).toBe(true);
    expect(state.pendingConversationThreadId).toBe('t-1');
    // The jump's scroll replaces the remembered reading offset, which a
    // remounting overview row would otherwise restore over it.
    expect(state.overviewSectionScrollTop('conversation')).toBe(0);
    state.consumePendingConversationThreadId();
    expect(state.pendingConversationThreadId).toBeNull();

    // The title-bar stepper routes a thread outside the diff here too, and
    // stages the jump back to the overview row.
    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads: [resolved, convThread('t-2', { path: 'gone.ts' })],
      headSHA: 'sha-a',
    });
    state.stepUnresolvedThread(1, false);
    expect(state.unresolvedCursor).toBe('t-2');
    expect(state.pendingConversationThreadId).toBe('t-2');
    expect(state.pendingJumpRowKey).toBe('overview');
  });

  it('jumpToConversationThread opens the card and brings the overview back', async () => {
    const state = await statePRWithThreads([convThread('t-1', { isResolved: true })]);
    state.setConversationOpen(false);
    state.jumpToConversationThread('t-1');
    expect(state.conversationOpen).toBe(true);
    expect(state.conversationThreadExpanded('t-1')).toBe(true);
    expect(state.pendingConversationThreadId).toBe('t-1');
    expect(state.pendingJumpRowKey).toBe('overview');
  });

  it('stepUnresolvedThread cycles open threads: diff rows from the title bar, cards from the overview', async () => {
    const inDiff = convThread('t-in', { path: 'src/app.ts', line: 2 });
    const outside = convThread('t-out', { path: 'gone.ts' });
    const settled = convThread('t-done', { isResolved: true });
    const state = await statePRWithThreads([settled, inDiff, outside]);
    expect(state.unresolvedThreads.map((thread) => thread.id)).toEqual(['t-in', 't-out']);

    // From the title bar: a thread on the diff jumps to its row.
    state.stepUnresolvedThread(1, false);
    expect(state.unresolvedCursor).toBe('t-in');
    expect(state.pendingJumpRowKey).toBe('pt:t-in');
    expect(state.expandedPRThreadIds.has('t-in')).toBe(true);
    // The row it jumps to shows even when the file filter hides its file.
    expect([...state.revealedPaths]).toEqual(['src/app.ts']);
    state.consumePendingJumpRowKey();

    // Next: outside the diff, so its card in the overview.
    state.stepUnresolvedThread(1, false);
    expect(state.unresolvedCursor).toBe('t-out');
    expect(state.pendingConversationThreadId).toBe('t-out');
    expect(state.pendingJumpRowKey).toBe('overview');
    state.consumePendingJumpRowKey();
    state.consumePendingConversationThreadId();

    // Wraps, and from the overview every step stays on the cards.
    state.stepUnresolvedThread(1, true);
    expect(state.unresolvedCursor).toBe('t-in');
    expect(state.pendingConversationThreadId).toBe('t-in');
    expect(state.pendingJumpRowKey).toBeNull();

    // Backwards from the first wraps to the last.
    state.stepUnresolvedThread(-1, true);
    expect(state.unresolvedCursor).toBe('t-out');
  });

  it('resolves optimistically, outranks stale polls, and reverts on failure', async () => {
    const open = convThread('t-1');
    const state = await statePRWithThreads([open]);
    const resolve = setBindingMock('SetPRThreadResolved', async () => undefined);

    await state.setPRThreadResolved(state.prThreads[0]!, true);
    expect(resolve).toHaveBeenCalledWith(
      expect.objectContaining({ Number: 5 }), 't-1', true,
    );
    expect(state.prThreads[0]?.isResolved).toBe(true);

    // A poll snapshot fetched before the mutation landed must not flap it.
    applyPRUpdatedEvent({
      prKey: PR_KEY,
      detail: prDetailStub(),
      threads: [open],
      headSHA: 'sha-a',
    });
    expect(state.prThreads[0]?.isResolved).toBe(true);

    // One that agrees retires the override; a genuine reopen then shows.
    applyPRUpdatedEvent({
      prKey: PR_KEY, detail: prDetailStub(), threads: [{ ...open, isResolved: true }], headSHA: 'sha-a',
    });
    applyPRUpdatedEvent({
      prKey: PR_KEY, detail: prDetailStub(), threads: [open], headSHA: 'sha-a',
    });
    expect(state.prThreads[0]?.isResolved).toBe(false);

    // Failure: revert and surface, never silently.
    setBindingMock('SetPRThreadResolved', async () => {
      throw new Error('forge said no');
    });
    await state.setPRThreadResolved(state.prThreads[0]!, true);
    expect(state.prThreads[0]?.isResolved).toBe(false);
    expect(state.resolveErrorFor('t-1')).toBe('forge said no');
  });

  it('jumpToDiffThread expands the file and thread and stages the row key', async () => {
    const anchored = convThread('t-1');
    const state = await statePRWithThreads([anchored]);
    state.toggleCollapsed('src/app.ts');

    state.jumpToDiffThread(state.prThreads[0]!);
    expect(state.collapsedPaths.has('src/app.ts')).toBe(false);
    expect(state.expandedPRThreadIds.has('t-1')).toBe(true);
    expect(state.pendingJumpRowKey).toBe('pt:t-1');
  });
});

describe('reviewPane store: painted span retention', () => {
  function paint(spans: PaintedSpans, file: ReviewFile): void {
    for (const line of file.body.toPatchLines()) spans.note(file.path, line, { r: [1, 1] });
  }

  function colored(spans: PaintedSpans, file: ReviewFile): boolean {
    return file.body.toPatchLines().some((line) => spans.lookup(file.path, line) !== null);
  }

  it('keeps each surface\'s colors for the files it shows and releases them when it closes', async () => {
    installPRMocks();
    setReviewDiffMock('OpenPRDiff', async () => [patchFor('src/a.ts', 2), patchFor('src/b.ts', 2)].join('\n'));
    const tree = {
      conflicted: true,
      treeOID: 'tree-1',
      baseLabel: 'origin/main',
      headLabel: 'feature',
      paths: ['main.go'],
      messages: [],
    };
    setBindingMock('GetPRMergeConflicts', async () => tree);
    setBindingMock('GetMergeConflictFile', async () => ['<<<<<<< ours', 'left', '=======', 'right', '>>>>>>> theirs'].join('\n'));
    const state = reviewStateForPane('pane-1', prSubject());
    // The mounted pane: a push recomputes only a view on screen.
    state.attachSurface('companion-1');
    await waitLoaded(state);
    await state.setScope('pr');
    const [a, b] = state.files;
    paint(state.paintedSpans, a);
    paint(state.paintedSpans, b);

    // A reload that drops a file drops that file's colors only.
    setReviewDiffMock('OpenPRDiff', async () => patchFor('src/a.ts', 2));
    await state.reload();
    await vi.waitFor(() => {
      expect(colored(state.paintedSpans, b)).toBe(false);
    });
    expect(colored(state.paintedSpans, a)).toBe(true);

    await state.openConflictView();
    const conflict = state.conflictFiles[0];
    paint(state.conflictPaintedSpans, conflict);

    // A push recomputes the merge: the tree is empty until the new one
    // lands, and the colors carry across that gap.
    let finish!: () => void;
    const recomputed = new Promise<void>((resolve) => { finish = resolve; });
    setBindingMock('GetPRMergeConflicts', async () => {
      await recomputed;
      return tree;
    });
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b' });
    expect(state.conflicts).toBeNull();
    await tick();
    expect(colored(state.conflictPaintedSpans, conflict)).toBe(true);
    finish();
    await vi.waitFor(() => {
      expect(state.conflictsLoading).toBe(false);
    });
    await tick();
    expect(colored(state.conflictPaintedSpans, conflict)).toBe(true);

    state.closeConflictView();
    await tick();
    expect(colored(state.conflictPaintedSpans, conflict)).toBe(false);
    expect(colored(state.paintedSpans, a)).toBe(true);

    disposeReviewStateForPane('pane-1', state);
    expect(colored(state.paintedSpans, a)).toBe(false);
  });
});

describe('reviewPane store — diffs past the memory budget', () => {
  const diagnostics = installDiagnosticsCapture();
  afterEach(() => {
    // Their diffs keep handles: let them go while the release mock stands.
    __resetReviewPaneStateForTest();
    setPatchTextBudgetForTest(null);
  });

  const patch = [patchFor('src/a.ts', 3), patchFor('src/b.ts', 3), patchFor('src/c.ts', 3)].join('\n');

  it('keeps the handle to read evicted text again, and releases it with the diff', async () => {
    const base = patchTextHeldBytes();
    setPatchTextBudgetForTest(100);
    const { read, release } = installChunkedDiff('OpenWorkspaceDiff', patch, 40);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(release).not.toHaveBeenCalled();
    expect(patchTextHeldBytes() - base).toBeLessThanOrEqual(100);

    const last = state.files[2];
    expect(last.body.resident()).toBe(false);
    read.mockClear();
    const lines = await last.body.whenResident(() => last.body.toPatchLines());
    expect(lines.map((line) => line.content)).toEqual(patchFor('src/c.ts', 3).split('\n'));
    expect(read.mock.calls.length).toBeGreaterThan(0);
    for (const [id, offset, size] of read.mock.calls as [string, number, number][]) {
      expect(id).toBe('diff-1');
      expect(size).toBe(Math.min(40, patch.length - offset));
    }

    const files = state.files;
    state.dispose();
    expect(release).toHaveBeenCalledTimes(1);
    expect(release).toHaveBeenCalledWith('diff-1');
    // The diff's own text is gone. The global count is not compared exactly:
    // stores other tests never disposed stop being counted when the garbage
    // collector finds them, which can happen at any point of this test.
    for (const file of files) expect(file.body.resident()).toBe(false);
    expect(patchTextHeldBytes()).toBeLessThanOrEqual(base);
  });

  it('lets the previous diff go when a reload replaces it', async () => {
    setPatchTextBudgetForTest(100);
    const { release } = installChunkedDiff('OpenWorkspaceDiff', patch, 40);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    const first = state.files[2];

    await state.reload();
    expect(release).toHaveBeenCalledTimes(1);
    await expect(first.body.loadLines(0, 1)).rejects.toBeInstanceOf(PatchTextLost);
  });

  it('reads the diff again when its evicted text can no longer be read', async () => {
    setPatchTextBudgetForTest(100);
    const { open, read } = installChunkedDiff('OpenWorkspaceDiff', patch, 40);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    read.mockImplementationOnce(async () => { throw new Error('review diff: not open'); });

    await expect(state.files[2].body.loadLines(0, 1)).rejects.toBeInstanceOf(PatchTextLost);
    await vi.waitFor(() => {
      expect(open).toHaveBeenCalledTimes(2);
    });
    await waitLoaded(state);
    expect(state.error).toBeNull();
    expect(state.files.map((file) => file.path)).toEqual(['src/a.ts', 'src/b.ts', 'src/c.ts']);
    expect(await diagnostics.messages()).toContain('review diff: evicted text could not be read again');
  });

  function editSection(path: string, start: number, text: string): string {
    return [
      `diff --git a/${path} b/${path}`,
      `--- a/${path}`,
      `+++ b/${path}`,
      `@@ -${start},1 +${start},1 @@`,
      `-old ${text}`,
      `+new ${text}`,
    ].join('\n');
  }

  function installEdits(turn: string) {
    setBindingMock('ListThreadEditDiffs', async () => ({
      entries: [{ itemId: 'tool:1', payloadId: 'pl-1', turnIndex: 1, title: 'Edited', paths: ['x.go'], insertions: 1, deletions: 1, createdAt: 1 }],
      turnLabels: [{ turnIndex: 1, label: 'edit' }],
    }));
    installChunkedDiff('OpenTurnEditsDiff', turn, 30);
    return setBindingMock('VerifyEditDiffs', async (_threadId, req) => ({
      expandablePaths: (req as { files: { path: string }[] }).files.map((file) => file.path),
    }));
  }

  it('merges a path whose sections were evicted while the turn arrived', async () => {
    const turn = [editSection('x.go', 1, 'a'), editSection('y.go', 1, 'b'), editSection('x.go', 9, 'c')].join('\n');
    installEdits(turn);
    setPatchTextBudgetForTest(1);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');

    const expected = mergePatchFilesByPath(parsePatchFiles(turn));
    expect(state.files.map((file) => file.path)).toEqual(['x.go', 'y.go']);
    const merged = state.files.find((file) => file.path === 'x.go')!;
    expect(await merged.body.whenResident(() => merged.body.toPatchLines())).toEqual(expected[0].lines);
  });

  it('verifies edit files whose text was evicted', async () => {
    const turn = [editSection('x.go', 1, 'a'), editSection('y.go', 1, 'b')].join('\n');
    const verify = installEdits(turn);
    setPatchTextBudgetForTest(1);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('edits');

    await vi.waitFor(() => {
      expect(state.files.every((file) => file.suppressGaps === undefined)).toBe(true);
    });
    const sent = verify.mock.calls.flatMap((call) => (call[1] as { files: { path: string; verifyPatch: string }[] }).files);
    expect(sent.map((file) => file.verifyPatch)).toEqual(parseReviewFiles(turn).map((file) => file.body.patchText()));
  });

  it('expands a gap in a file whose hunk headings were evicted', async () => {
    const filler = patchFor('src/big.ts', 20);
    const gapped = [
      'diff --git a/src/app.ts b/src/app.ts',
      '--- a/src/app.ts',
      '+++ b/src/app.ts',
      '@@ -10,2 +10,2 @@ function app()',
      ' ctx',
      '-old',
      '+new',
    ].join('\n');
    installChunkedDiff('OpenWorkspaceDiff', `${filler}\n${gapped}\n`, 30);
    setBindingMock('GetDiffContextLines', async (_ws, req) => {
      const { startLine, endLine } = req as { startLine: number; endLine: number };
      return {
        lines: Array.from({ length: endLine - startLine + 1 }, (_, i) => `src ${startLine + i}`),
        startLine,
        eof: false,
        totalLines: 0,
      };
    });
    setPatchTextBudgetForTest(100);
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    const app = () => state.files.find((file) => file.path === 'src/app.ts')!;
    expect(app().body.resident()).toBe(false);
    const leading = rowsOf(await app().body.whenResident(() => app())).find((row) => row.gap)?.gap;

    await state.expandDiffContext('src/app.ts', leading!, 'all');
    expect(state.error).toBeNull();
    const expanded = app();
    expect(await expanded.body.whenResident(() => expanded.body.text(3))).toBe('@@ -1,11 +1,11 @@ function app()');
  });
});

describe('reviewPane store — PR whose first fetch failed', () => {
  it('waits for the poller instead of failing, and loads once the snapshot lands', async () => {
    installPRMocks();
    // The pump was created on a failing first fetch: a caller-safe failure
    // and the wire's zero detail, which is no snapshot at all.
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1',
      prKey: PR_KEY,
      detail: { ...prDetailStub(), number: 0, headSHA: '' },
      threads: [],
      headSHA: '',
      error: 'failed to refresh pull request (id: abc)',
      seq: 1,
    }));
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 2));

    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr');
    await waitLoaded(state);

    expect(state.awaitingPR).toBe(true);
    expect(state.error).toBeNull();
    expect(state.prUpdateError).toContain('failed to refresh');
    expect(state.prDetail).toBeNull();
    expect(state.files).toEqual([]);
    expect(diff).not.toHaveBeenCalled();

    // The pump's recovery frame: the diff loads on its own.
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub(), threads: [], headSHA: 'sha-a', seq: 2 });
    await waitLoaded(state);
    await vi.waitFor(() => {
      expect(state.files.length).toBe(1);
    });
    expect(state.awaitingPR).toBe(false);
    expect(state.prUpdateError).toBeNull();
    expect(state.prDetail?.number).toBe(5);
    expect(diff).toHaveBeenCalledTimes(1);
  });
});

// A PR read may ask for git credentials only when a person asked for it.
// Every store entry point reads unattended unless its caller says so, and
// the reads the store starts itself (a retry on a pump frame) never do.
describe('reviewPane store — PR read intent', () => {
  const sha = 'b'.repeat(40);
  const revision = { baseRef: 'main', baseSHA: 'base-main', headSHA: 'sha-a' };
  const pr = expect.objectContaining({ Number: 5 });
  function installIntentMocks() {
    installPRMocks();
    const list = setBindingMock('ListPRCommits', async () => [
      { sha, shortSha: 'bbbbbbb', subject: 'first', author: 'r', authoredAt: 1 },
    ]);
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
    const commitDiff = setReviewDiffMock('OpenPRCommitDiff', async () => patchFor('src/app.ts', 2));
    return { list, diff, commitDiff };
  }

  it('reads unattended from every entry point that does not say a person asked', async () => {
    const { list, diff, commitDiff } = installIntentMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);

    await state.setScope('pr');
    expect(list).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false);
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, false);
    await state.reload();
    expect(list).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false);
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, false);
    await state.selectCommit(sha);
    expect(commitDiff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, sha, false, false);
    await state.setIgnoreWhitespace(true);
    expect(commitDiff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, sha, true, false);
  });

  it('reads attended for the entry points a person drove', async () => {
    const { list, diff, commitDiff } = installIntentMocks();
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);

    await state.setScope('pr', { userInitiated: true });
    expect(list).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, true);
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);
    await state.reload();
    await state.reload({ userInitiated: true });
    expect(list).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, true);
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);
    await state.selectCommit(sha, { userInitiated: true });
    expect(commitDiff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, sha, false, true);
    await state.setIgnoreWhitespace(true, { userInitiated: true });
    expect(commitDiff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, sha, true, true);
  });

  it('retries a failed user read unattended on the next pump frame', async () => {
    installIntentMocks();
    let fail = true;
    const diff = setReviewDiffMock('OpenPRDiff', async () => {
      if (fail) throw new Error('fetch failed');
      return patchFor('src/app.ts', 3);
    });
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr', { userInitiated: true });
    expect(state.error).toContain('fetch failed');
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);

    fail = false;
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub(), threads: [], headSHA: 'sha-a', seq: 50 });
    await vi.waitFor(() => expect(state.files).toHaveLength(1));
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, false);
  });

  it('keeps a person\'s open across the wait for the PR snapshot', async () => {
    const { diff } = installIntentMocks();
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1', prKey: PR_KEY, detail: prDetailStub({ baseRefSHA: '' }), threads: [], headSHA: 'sha-a', seq: 1,
    }));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);
    await state.setScope('pr', { userInitiated: true });
    expect(state.awaitingPR).toBe(true);
    expect(diff).not.toHaveBeenCalled();

    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub(), threads: [], headSHA: 'sha-a', seq: 2 });
    await vi.waitFor(() => expect(state.files).toHaveLength(1));
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);
  });

  it('keeps a person\'s open across the wait for the workspace status', async () => {
    const { diff } = installIntentMocks();
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('pr', { userInitiated: true });
    expect(state.awaitingPR).toBe(true);
    expect(diff).not.toHaveBeenCalled();

    prSubject();
    await vi.waitFor(() => expect(state.files).toHaveLength(1));
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);
  });

  it('reads unattended when a PR appears after the status answered none, even after a person\'s wait', async () => {
    const { diff } = installIntentMocks();
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('pr', { userInitiated: true });
    expect(state.awaitingPR).toBe(true);

    // The status lands with no PR: the open ends in its error.
    seedPaneWorkspaceStatus('pane-1', {});
    await vi.waitFor(() => expect(state.error).toContain('No PR'));
    expect(diff).not.toHaveBeenCalled();

    // A PR opened later is not the person's open.
    prSubject();
    await vi.waitFor(() => expect(state.files).toHaveLength(1));
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, false);
  });

  it.each([
    ['toggleReviewPane', () => getPane('pane-1')!.toggleReviewPane()],
    ['setShowReviewPane', () => getPane('pane-1')!.setShowReviewPane(true)],
  ] as const)('the pane\'s %s is a person\'s open', async (_name, open) => {
    const { diff } = installIntentMocks();
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 1 }]);
    persistScope('thread-1', 'pr', null);
    prSubject();
    // The pane's thread names its project, so the pane has a checkout.
    getPane('pane-1')!.replaceThread({
      id: 'thread-1', title: 'Review', provider: 'claude', projectId: REVIEW_WS.projectId,
      workspacePath: REVIEW_WORKSPACE, projectPath: REVIEW_WORKSPACE, model: 'm', createdAt: 0, updatedAt: 0, archived: false,
    });
    expect(reviewSubjectForPane(getPane('pane-1')!)?.workspace).toEqual(REVIEW_WS);
    open();
    await vi.waitFor(() => expect(diff).toHaveBeenCalled());
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);
  });

  it('openReviewCompanion reads attended only when its caller says a person opened it', async () => {
    const { diff } = installIntentMocks();
    setPaneLayoutItemsForTest([{ id: 'pane-1', paneId: 'pane-1', kind: 'thread', widthPx: 1 }]);
    const opened = await openReviewCompanion('pane-1', prSubject(), { scope: 'pr', userInitiated: true });
    await waitLoaded(opened!);
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);
    disposeReviewStateForPane('pane-1', opened!);

    // No scope: the state's own first load carries the open.
    const reopened = await openReviewCompanion('pane-1', prSubject(), { userInitiated: true });
    await waitLoaded(reopened!);
    expect(reopened!.scope).toBe('pr');
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, true);
    disposeReviewStateForPane('pane-1', reopened!);

    // A state made by a mount (a restored layout) reads unattended.
    const mounted = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(mounted);
    expect(mounted.scope).toBe('pr');
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, false);
    disposeReviewStateForPane('pane-1', mounted);

    const unsaid = await openReviewCompanion('pane-1', prSubject(), { scope: 'pr' });
    await waitLoaded(unsaid!);
    expect(diff).toHaveBeenLastCalledWith(REVIEW_WS, pr, revision, false, false);
  });
});

describe('reviewPane store — the conflict view\'s permit follows the mounted pane', () => {
  function installConflictTree() {
    return setBindingMock('GetPRMergeConflicts', async (_ws: unknown, _ref: unknown, rev: { headSHA: string }) => ({
      conflicted: false, treeOID: `tree-${rev.headSHA}`, baseLabel: 'origin/main', headLabel: 'feature', paths: [], messages: [],
    }));
  }
  const heads = (mock: { mock: { calls: unknown[][] } }) => mock.mock.calls.map((call) => [(call[2] as { headSHA: string }).headSHA, call[4]]);

  it('reads nothing for an open view no mounted pane renders, and catches up once one does', async () => {
    installPRMocks();
    const conflicts = installConflictTree();
    const state = reviewStateForPane('pane-1', prSubject());
    const detach = state.attachSurface('companion-1');
    await waitLoaded(state);
    await state.setScope('pr');
    await state.openConflictView();
    expect(heads(conflicts)).toEqual([['sha-a', true]]);

    detach();
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b', seq: 30 });
    await flushPane();
    expect(conflicts).toHaveBeenCalledTimes(1);

    const reattach = state.attachSurface('companion-1');
    await vi.waitFor(() => expect(conflicts).toHaveBeenCalledTimes(2));
    expect(heads(conflicts)[1]).toEqual(['sha-b', false]);
    expect(state.conflictView).toBe(true);
    reattach();
  });

  it('on compact, holds the permit only while the companion is the pane on screen', async () => {
    setCompactLayoutForTest(true);
    // No strip is mounted here: this is what PaneHost's measurement writes
    // when the compact strip comes to rest on the pane.
    const showOnStrip = (paneId: string): void => {
      showCompactThread();
      noteCompactPaneShown(paneId);
    };
    try {
      installPRMocks();
      const conflicts = installConflictTree();
      const state = reviewStateForPane('pane-1', prSubject());
      state.attachSurface('companion-1');
      showOnStrip('companion-1');
      await waitLoaded(state);
      await state.setScope('pr');
      await state.openConflictView();
      expect(heads(conflicts)).toEqual([['sha-a', true]]);

      // Back to the sidebar screen: a frame reads nothing.
      showCompactList();
      await flushPane();
      applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b', seq: 40 });
      await flushPane();
      expect(conflicts).toHaveBeenCalledTimes(1);
      // Back to the companion: the snapshot moved, so it reads.
      showOnStrip('companion-1');
      await vi.waitFor(() => expect(conflicts).toHaveBeenCalledTimes(2));
      expect(heads(conflicts)[1]).toEqual(['sha-b', false]);

      // Another pane on screen: likewise.
      showOnStrip('pane-1');
      await flushPane();
      applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub({ headSHA: 'sha-c' }), threads: [], headSHA: 'sha-c', seq: 41 });
      await flushPane();
      expect(conflicts).toHaveBeenCalledTimes(2);
      showOnStrip('companion-1');
      await vi.waitFor(() => expect(conflicts).toHaveBeenCalledTimes(3));
      expect(heads(conflicts)[2]).toEqual(['sha-c', false]);

      // A return with nothing moved reads nothing.
      showOnStrip('pane-1');
      await flushPane();
      showOnStrip('companion-1');
      await flushPane();
      expect(conflicts).toHaveBeenCalledTimes(3);
    } finally {
      setCompactLayoutForTest(false);
    }
  });

  it('on the desktop, every mounted pane keeps its permit whichever pane was revealed', async () => {
    installPRMocks();
    const conflicts = installConflictTree();
    const state = reviewStateForPane('pane-1', prSubject());
    state.attachSurface('companion-1');
    await waitLoaded(state);
    await state.setScope('pr');
    await state.openConflictView();
    revealPane('pane-2');
    showCompactList();
    await flushPane();
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b', seq: 42 });
    await vi.waitFor(() => expect(conflicts).toHaveBeenCalledTimes(2));
  });

  it('a reopen of a PR that moved while the view was closed is the person\'s read', async () => {
    installPRMocks();
    const conflicts = installConflictTree();
    const state = reviewStateForPane('pane-1', prSubject());
    state.attachSurface('companion-1');
    await waitLoaded(state);
    await state.setScope('pr');
    await state.openConflictView();
    state.closeConflictView();
    applyPRUpdatedEvent({ prKey: PR_KEY, detail: prDetailStub({ headSHA: 'sha-b' }), threads: [], headSHA: 'sha-b', seq: 31 });
    await flushPane();
    expect(conflicts).toHaveBeenCalledTimes(1);

    await state.openConflictView();
    expect(heads(conflicts)).toEqual([['sha-a', true], ['sha-b', true]]);
  });
});

describe('reviewPane store — open new-comment editors', () => {
  // A modified file of the workspace diff: `index` names its blob ids.
  function changed(index: string, hunk: string, lines: readonly string[], path = 'src/app.ts'): string {
    return [
      `diff --git a/${path} b/${path}`,
      `index ${index} 100644`,
      `--- a/${path}`,
      `+++ b/${path}`,
      hunk,
      ...lines,
    ].join('\n');
  }

  // Comments as the backend holds them: created from the input, listed
  // back by scope and source.
  function commentsBackend() {
    const rows: DiffReviewComment[] = [];
    const create = setBindingMock('CreateDiffReviewComment', async (threadId: string, input: Partial<DiffReviewComment> & { sourceKey: string; scope: DiffReviewComment['scope'] }) => {
      const row = { ...draft({ ...input, id: `c-${rows.length + 1}`, threadId }), oldLine: input.oldLine, newLine: input.newLine };
      rows.push(row);
      return row;
    });
    setBindingMock('ListDiffReviewComments', async (_thread: string, scope: string, sourceKey: string) =>
      rows.filter((row) => row.scope === scope && row.sourceKey === sourceKey).map((row) => ({ ...row })));
    return { rows, create };
  }

  async function workspaceWith(patch: () => string) {
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch());
    const backend = commentsBackend();
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    return { state, ...backend };
  }

  const refused = (reason: string) => expect.objectContaining({ name: 'ReviewCommentRefused', reason });

  it('keys an editor\'s text, focus and save by its id', async () => {
    const { state, create } = await workspaceWith(() => patchFor('src/app.ts', 2));
    const anchor = { filePath: 'src/app.ts', side: 'new' as const, newLine: 1, selectedText: 'line 1' };
    const id = state.openDraftEditor(anchor)!;
    expect(id).toBeTruthy();
    expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'placed', anchor, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
    state.setDraftBody(id, 'half-typed');
    expect(state.draftBodyFor(id)).toBe('half-typed');
    expect(state.consumeDraftEditorFocus(id)).toBe(true);
    expect(state.consumeDraftEditorFocus(id)).toBe(false);
    // The line already has an editor: it is focused, not doubled.
    expect(state.openDraftEditor(anchor)).toBe(id);
    expect(state.consumeDraftEditorFocus(id)).toBe(true);
    expect(state.openEditors).toHaveLength(1);

    await state.createComment(id, 'Looks good.');
    expect(create.mock.calls[0]?.[1]).toMatchObject({ scope: 'workspace', newLine: 1, side: 'new', selectedText: 'line 1', body: 'Looks good.' });
    expect(state.openEditors).toEqual([]);
    expect(state.draftBodyFor(id)).toBe('');
    expect(state.drafts.map((comment) => comment.body)).toEqual(['Looks good.']);

    const next = state.openDraftEditor(anchor)!;
    state.setDraftBody(next, 'other subject');
    await state.setScope('branch');
    expect(state.draftBodyFor(next)).toBe('');
  });

  it('keeps a failed save\'s editor open with the error', async () => {
    const { state } = await workspaceWith(() => patchFor('src/app.ts', 1));
    setBindingMock('CreateDiffReviewComment', async () => {
      throw new Error('create failed');
    });
    const id = state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 1, selectedText: 'line 1' })!;
    await expect(state.createComment(id, 'Body')).rejects.toThrow('create failed');
    expect(state.openEditors.map((editor) => editor.id)).toEqual([id]);
    expect(state.error).toBe('create failed');
  });

  it('opens no editor on a line the diff does not have, and refuses a quote longer than a comment may hold', async () => {
    const { state, create } = await workspaceWith(() => patchFor('src/app.ts', 1));
    expect(state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 9, selectedText: 'x' })).toBeNull();
    expect(state.openDraftEditor({ filePath: 'src/gone.ts', side: 'file' })).toBeNull();
    const id = state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 1, selectedText: 'é'.repeat(5000) })!;
    await expect(state.createComment(id, 'Body')).rejects.toEqual(refused('too-long'));
    expect(create).not.toHaveBeenCalled();
  });

  it('saves a file comment while its file is in the diff', async () => {
    const { state, create } = await workspaceWith(() => patchFor('src/app.ts', 1));
    const id = state.openDraftEditor({ filePath: 'src/app.ts', side: 'file' })!;
    await state.createComment(id, 'About the whole file.');
    expect(create.mock.calls[0]?.[1]).toMatchObject({ filePath: 'src/app.ts', side: 'file', selectedText: '', body: 'About the whole file.' });
  });

  describe('across a reload', () => {
    // The old head adds a primary and a fallback handler between two
    // unchanged base lines.
    const HEAD_A = changed('aaaaaaa..bbbbbbb', '@@ -1,2 +1,8 @@', [' top', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom']);
    const report = { filePath: 'src/app.ts', side: 'new' as const, newLine: 3, selectedText: '  report();' };

    async function editorThen(after: string, anchor = report) {
      let patch = HEAD_A;
      const loaded = await workspaceWith(() => patch);
      const id = loaded.state.openDraftEditor(anchor)!;
      loaded.state.setDraftBody(id, 'Guard this.');
      patch = after;
      await loaded.state.reload();
      await waitLoaded(loaded.state);
      return { ...loaded, id };
    }

    it('does not follow the primary handler\'s text onto the fallback handler', async () => {
      // The primary block goes and the fallback now reports: the base is
      // unchanged and `report();` is on one row each time.
      const { state, create, id } = await editorThen(changed('aaaaaaa..ccccccc', '@@ -1,2 +1,5 @@', [' top', '+if (fallback) {', '+  report();', '+}', ' bottom']));
      expect(state.openEditors).toMatchObject([{ id, filePath: 'src/app.ts', side: 'new', state: 'detached', anchor: null }]);
      expect(state.draftBodyFor(id)).toBe('Guard this.');
      await expect(state.createComment(id, 'Guard this.')).rejects.toEqual(refused('detached'));
      expect(create).not.toHaveBeenCalled();
    });

    it('detaches an added line\'s editor when lines are added above it', async () => {
      const { state, create, id } = await editorThen(changed('aaaaaaa..ddddddd', '@@ -1,2 +1,9 @@', [' top', '+inserted', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom']));
      expect(state.openEditors[0]).toMatchObject({ id, state: 'detached', anchor: null });
      expect(state.draftBodyFor(id)).toBe('Guard this.');
      await expect(state.createComment(id, 'x')).rejects.toEqual(refused('detached'));
      expect(create).not.toHaveBeenCalled();
    });

    it('keeps an editor on its line when its file did not change', async () => {
      const other = changed('1111111..2222222', '@@ -1,1 +1,1 @@', ['-x', '+y'], 'src/other.ts');
      const { state, create, id } = await editorThen(`${HEAD_A}\n${other}`);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'placed', anchor: report, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
      await state.createComment(id, 'Guard this.');
      expect(create.mock.calls[0]?.[1]).toMatchObject({ newLine: 3, selectedText: '  report();' });
    });

    it('follows an unchanged line by its base version; its split halves keep their sides', async () => {
      const before = changed('1111111..2222222', '@@ -1,3 +1,3 @@', [' keep 1', '-old 2', '+new 2', ' keep 3']);
      const after = changed('1111111..3333333', '@@ -1,3 +1,4 @@', [' keep 1', '-old 2', '+new 2', '+more', ' keep 3']);
      let patch = before;
      const { state, create } = await workspaceWith(() => patch);
      const left = state.openDraftEditor({ filePath: 'src/app.ts', side: 'old', oldLine: 3, selectedText: ' keep 3' })!;
      const right = state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 3, selectedText: ' keep 3' })!;
      state.setDraftBody(left, 'Left.');
      state.setDraftBody(right, 'Right.');
      patch = after;
      await state.reload();
      await waitLoaded(state);

      expect(state.openEditors.map((editor) => [editor.id, editor.anchor])).toEqual([
        [left, { filePath: 'src/app.ts', side: 'old', oldLine: 3, newLine: 4, selectedText: ' keep 3' }],
        [right, { filePath: 'src/app.ts', side: 'new', oldLine: 3, newLine: 4, selectedText: ' keep 3' }],
      ]);
      expect([state.draftBodyFor(left), state.draftBodyFor(right)]).toEqual(['Left.', 'Right.']);
      await state.createComment(left, 'Left.');
      await state.createComment(right, 'Right.');
      expect((create.mock.calls as [string, DiffReviewComment][]).map(([, input]) => [input.side, input.oldLine, input.newLine])).toEqual([['old', 3, 4], ['new', 3, 4]]);
    });

    it('keeps its editors and their lines across the whitespace toggle', async () => {
      const { state, id } = await editorThen(HEAD_A);
      await state.setIgnoreWhitespace(true);
      await waitLoaded(state);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'placed', anchor: report, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
      expect(state.draftBodyFor(id)).toBe('Guard this.');
    });

    it('keeps the editor of a reverted file to copy from, and places it again when the file comes back', async () => {
      const { state, id } = await editorThen('');
      expect(state.files).toEqual([]);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'gone', anchor: null, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
      expect(state.draftBodyFor(id)).toBe('Guard this.');

      setReviewDiffMock('OpenWorkspaceDiff', async () => HEAD_A);
      await state.reload();
      await waitLoaded(state);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'placed', anchor: report, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
    });

    it('re-entering the scope on screen keeps the editor and its text', async () => {
      const { state, id } = await editorThen(HEAD_A);
      await state.setScope('workspace');
      await waitLoaded(state);
      expect(state.openEditors.map((editor) => editor.id)).toEqual([id]);
      expect(state.draftBodyFor(id)).toBe('Guard this.');
    });

    it('renders every editor of the diff once, in its file or the group, whatever placed it', async () => {
      const other = changed('1111111..2222222', '@@ -1,1 +1,1 @@', ['-x', '+y'], 'src/other.ts');
      const moved = changed('aaaaaaa..ddddddd', '@@ -1,2 +1,9 @@', [' top', '+inserted', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom']);
      for (const [label, next, budget] of [
        ['same', HEAD_A, null], ['moved', moved, null], ['file left', other, null], ['empty', '', null],
        ['file left, budget spent', other, 1], ['moved, budget spent', `${moved}\n${other}`, 1],
      ] as const) {
        __resetReviewPaneStateForTest();
        const restore = budget === null ? () => {} : __setRelocationBudgetForTest(budget);
        try {
          const { state, id } = await editorThen(next);
          const built = buildReviewRows({ files: state.files, viewMode: 'stacked', collapsedPaths: new Set(), drafts: [], openEditors: state.openEditors });
          expect(built.rowKeys.filter((key) => key === `d:${id}`), label).toHaveLength(1);
          const editor = state.openEditors[0]!;
          expect(editor.state === 'gone', label).toBe(!state.files.some((file) => file.path === editor.filePath));
        } finally {
          restore();
        }
      }
    });

    it('detaches what the relocation budget does not reach and still shows the diff', async () => {
      const restore = __setRelocationBudgetForTest(1);
      try {
        const other = changed('1111111..2222222', '@@ -1,1 +1,1 @@', ['-x', '+y'], 'src/other.ts');
        const { state, id } = await editorThen(`${HEAD_A}\n${other}`);
        expect(state.files.map((file) => file.path)).toEqual(['src/app.ts', 'src/other.ts']);
        expect(state.openEditors).toMatchObject([{ id, filePath: 'src/app.ts', side: 'new', state: 'detached', anchor: null }]);
        expect(state.error).toBeNull();
      } finally {
        restore();
      }
    });
  });

  describe('detached by a change to their file', () => {
    const HEAD = changed('aaaaaaa..bbbbbbb', '@@ -1,2 +1,8 @@', [' top', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom']);
    // One line inserted above: `report();` is one row lower.
    const MOVED = changed('aaaaaaa..ddddddd', '@@ -1,2 +1,9 @@', [' top', '+inserted', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom']);
    const OTHER = changed('1111111..2222222', '@@ -1,1 +1,2 @@', [' one', '+two'], 'src/other.ts');
    const report = { filePath: 'src/app.ts', side: 'new' as const, newLine: 3, selectedText: '  report();' };

    async function detachedAfter(next: string) {
      let patch = `${HEAD}\n${OTHER}`;
      const loaded = await workspaceWith(() => patch);
      const id = loaded.state.openDraftEditor(report)!;
      loaded.state.setDraftBody(id, 'Guard this.');
      patch = next;
      await loaded.state.reload();
      await waitLoaded(loaded.state);
      return { ...loaded, id, setPatch: (text: string) => { patch = text; } };
    }

    it('shows after the row its text is on, which saves nothing until the reader takes it', async () => {
      const { state, create, id } = await detachedAfter(`${MOVED}\n${OTHER}`);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'detached', anchor: null, suggested: { filePath: 'src/app.ts', side: 'new', newLine: 4, selectedText: '  report();' }, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
      await expect(state.createComment(id, 'Guard this.')).rejects.toEqual(refused('detached'));
      expect(create).not.toHaveBeenCalled();

      expect(state.useSuggestedLine(id, state.openEditors[0]!)).toBe(true);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'placed', anchor: { filePath: 'src/app.ts', side: 'new', newLine: 4, selectedText: '  report();' }, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
      expect(state.draftBodyFor(id)).toBe('Guard this.');
      await state.createComment(id, 'Guard this.');
      expect(create.mock.calls[0]?.[1]).toMatchObject({ newLine: 4, side: 'new', selectedText: '  report();', body: 'Guard this.' });
    });

    it('takes the suggestion the reader saw, never one a reload made since', async () => {
      const { state, id, setPatch } = await detachedAfter(`${MOVED}\n${OTHER}`);
      const seen = state.openEditors[0]!;
      expect(seen.suggested).toMatchObject({ newLine: 4 });
      // A reload lands between the press and its activation: two lines
      // above now, so the suggestion is another row.
      setPatch(`${changed('aaaaaaa..eeeeeee', '@@ -1,2 +1,10 @@', [' top', '+inserted', '+inserted 2', '+if (primary) {', '+  report();', '+}', '+if (fallback) {', '+  retry();', '+}', ' bottom'])}\n${OTHER}`);
      await state.reload();
      await waitLoaded(state);
      expect(state.openEditors[0]!.suggested).toMatchObject({ newLine: 5 });
      expect(state.useSuggestedLine(id, seen)).toBe(false);
      expect(state.openEditors[0]).toMatchObject({ id, state: 'detached', anchor: null });
      expect(state.useSuggestedLine(id, state.openEditors[0]!)).toBe(true);
      expect(state.openEditors[0]).toMatchObject({ id, state: 'placed', anchor: { newLine: 5 } });
    });

    it('takes a pick only in the diff the reader made it in', async () => {
      const { state, id } = await detachedAfter(`${MOVED}\n${OTHER}`);
      state.moveEditor(id);
      const pressedIn = state.openEditors[0]!.generation;
      await state.reload();
      await waitLoaded(state);
      const two = { filePath: 'src/other.ts', side: 'new' as const, newLine: 2, selectedText: 'two' };
      expect(state.openDraftEditor(two, pressedIn)).toBeNull();
      expect(state.openEditors.map((editor) => [editor.id, editor.state])).toEqual([[id, 'detached']]);
      expect(state.openDraftEditor(two, state.openEditors[0]!.generation)).not.toBeNull();
    });

    it('moves to the line the reader picks next, in its file or another, with its text', async () => {
      const { state, create, id } = await detachedAfter(`${MOVED}\n${OTHER}`);
      state.moveEditor(id);
      expect(state.armedEditorId).toBe(id);
      expect(state.openEditors[0]?.armed).toBe(true);
      expect(state.openDraftEditor({ filePath: 'src/other.ts', side: 'new', newLine: 2, selectedText: 'two' })).toBe(id);
      expect(state.armedEditorId).toBeNull();
      expect(state.consumeDraftEditorFocus(id)).toBe(true);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/other.ts', side: 'new', state: 'placed', anchor: { filePath: 'src/other.ts', side: 'new', newLine: 2, selectedText: 'two' }, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
      expect(state.draftBodyFor(id)).toBe('Guard this.');
      await state.createComment(id, 'Guard this.');
      expect(create.mock.calls[0]?.[1]).toMatchObject({ filePath: 'src/other.ts', newLine: 2, selectedText: 'two' });
    });

    it('moves within its file; a line with no row leaves it armed', async () => {
      const { state, id } = await detachedAfter(`${MOVED}\n${OTHER}`);
      state.moveEditor(id);
      expect(state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 99, selectedText: 'x' })).toBeNull();
      expect(state.armedEditorId).toBe(id);
      expect(state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 7, selectedText: '  retry();' })).toBe(id);
      expect(state.openEditors[0]).toMatchObject({ state: 'placed', anchor: { newLine: 7 } });
      expect(state.openEditors).toHaveLength(1);
    });

    it('arms one editor at a time, and a cancel, a reload, a subject change or a discard disarms', async () => {
      const { state, id } = await detachedAfter(`${MOVED}\n${OTHER}`);
      // A second detached editor, by the same change to its file.
      state.moveEditor(id);
      state.cancelEditorMove();
      expect(state.armedEditorId).toBeNull();
      state.moveEditor(id);
      await state.reload();
      await waitLoaded(state);
      expect(state.armedEditorId).toBeNull();
      state.moveEditor(id);
      state.closeDraftEditor(id);
      expect(state.armedEditorId).toBeNull();
      // Placed editors cannot be armed.
      const placed = state.openDraftEditor({ filePath: 'src/other.ts', side: 'new', newLine: 2, selectedText: 'two' })!;
      state.moveEditor(placed);
      expect(state.armedEditorId).toBeNull();
    });

    it('disarms on a subject change', async () => {
      const { state, id } = await detachedAfter(`${MOVED}\n${OTHER}`);
      state.moveEditor(id);
      await state.setScope('branch');
      expect(state.armedEditorId).toBeNull();
    });

    it('goes back to its line by itself when a later diff proves it, unless the reader chose one', async () => {
      const first = await detachedAfter(`${MOVED}\n${OTHER}`);
      first.setPatch(`${HEAD}\n${OTHER}`);
      await first.state.reload();
      await waitLoaded(first.state);
      expect(first.state.openEditors[0]).toMatchObject({ id: first.id, state: 'placed', anchor: report });

      __resetReviewPaneStateForTest();
      const second = await detachedAfter(`${MOVED}\n${OTHER}`);
      second.state.useSuggestedLine(second.id, second.state.openEditors[0]!);
      second.setPatch(`${HEAD}\n${OTHER}`);
      await second.state.reload();
      await waitLoaded(second.state);
      // The reader's line (4 of MOVED) is not proven in HEAD's other blob.
      expect(second.state.openEditors[0]).toMatchObject({ id: second.id, state: 'detached' });
    });

    it('is gone when its file leaves the diff, and comes back to its line with the file', async () => {
      const { state, id, setPatch } = await detachedAfter(OTHER);
      expect(state.openEditors).toEqual([{ id, filePath: 'src/app.ts', side: 'new', state: 'gone', anchor: null, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
      state.moveEditor(id);
      expect(state.armedEditorId).toBeNull();
      setPatch(`${HEAD}\n${OTHER}`);
      await state.reload();
      await waitLoaded(state);
      expect(state.openEditors[0]).toMatchObject({ id, state: 'placed', anchor: report });
    });
  });

  describe('on rows an expansion supplied', () => {
    // Lines 1..9 sit in the leading gap of each file.
    const gapped = (path: string) => changed('1111111..2222222', '@@ -10,2 +10,2 @@', [' ctx', '-old', '+new'], path);
    const patch = `${gapped('src/a.ts')}\n${gapped('src/b.ts')}`;

    function contextLines() {
      return setBindingMock('GetDiffContextLines', async (_ws, req) => {
        const { startLine, endLine } = req as { startLine: number; endLine: number };
        return { lines: Array.from({ length: endLine - startLine + 1 }, (_, i) => `src ${startLine + i}`), startLine, eof: false, totalLines: 0 };
      });
    }

    const leadingGap = (file: ReviewFile | undefined) => rowsOf(file!).find((row) => row.gap)!.gap!;

    it('saves one in the load it was opened in, and detaches it at the next', async () => {
      contextLines();
      const { state, create } = await workspaceWith(() => patch);
      await state.expandDiffContext('src/a.ts', leadingGap(state.files[0]), 'all');
      const one = { filePath: 'src/a.ts', side: 'context' as const, oldLine: 1, newLine: 1, selectedText: ' src 1' };
      const saved = state.openDraftEditor(one)!;
      await state.createComment(saved, 'In this load.');
      expect(create.mock.calls[0]?.[1]).toMatchObject({ side: 'context', oldLine: 1, newLine: 1 });

      const kept = state.openDraftEditor(one)!;
      await state.reload();
      await waitLoaded(state);
      // No version proves a row an expansion supplied (editorPlacement's
      // unit tests show no proof is recorded for it).
      expect(state.openEditors).toMatchObject([{ id: kept, filePath: 'src/a.ts', side: 'context', state: 'detached', anchor: null }]);
      await state.expandDiffContext('src/a.ts', leadingGap(state.files[0]), 'all');
      await state.reload();
      await waitLoaded(state);
      expect(state.openEditors[0]).toMatchObject({ state: 'detached', anchor: null });
    });

    it('lets an editor in one file save after another file expands, and after its own file does', async () => {
      contextLines();
      const { state, create } = await workspaceWith(() => patch);
      const onA = state.openDraftEditor({ filePath: 'src/a.ts', side: 'new', newLine: 11, selectedText: 'new' })!;
      await state.expandDiffContext('src/b.ts', leadingGap(state.files[1]), 'all');
      await state.createComment(onA, 'After B expanded.');
      const again = state.openDraftEditor({ filePath: 'src/a.ts', side: 'new', newLine: 11, selectedText: 'new' })!;
      await state.expandDiffContext('src/a.ts', leadingGap(state.files[0]), 'all');
      await state.createComment(again, 'After A expanded.');
      expect((create.mock.calls as [string, DiffReviewComment][]).map(([, input]) => [input.filePath, input.newLine, input.body])).toEqual([
        ['src/a.ts', 11, 'After B expanded.'],
        ['src/a.ts', 11, 'After A expanded.'],
      ]);
    });
  });

  describe('while their placement waits on evicted text', () => {
    afterEach(() => {
      __resetReviewPaneStateForTest();
      setPatchTextBudgetForTest(null);
    });

    it('neither resurrects an editor discarded during the wait nor places one opened during it', async () => {
      const patch = [patchFor('src/a.ts', 3), patchFor('src/b.ts', 3), patchFor('src/c.ts', 3)].join('\n');
      setPatchTextBudgetForTest(100);
      let hold: Promise<void> | null = null;
      let release!: () => void;
      let heldReads = 0;
      let opened = 0;
      const chunkAt = (offset: number) => {
        const next = Math.min(patch.length, offset + 40);
        return { data: patch.slice(offset, next), offset, nextOffset: next, eof: next === patch.length };
      };
      setBindingMock('OpenWorkspaceDiff', async () => ({ id: `diff-${++opened}`, chunk: chunkAt(0), headSha: '' }));
      let furthest = 0;
      setBindingMock('ReadReviewDiff', async (handle: string, offset: number) => {
        // The reloaded diff reads its evicted text again for the editors'
        // rows.
        if (handle === 'diff-2' && hold && offset < furthest) {
          heldReads += 1;
          await hold;
        }
        if (handle === 'diff-2') furthest = Math.max(furthest, offset + 40);
        return chunkAt(offset);
      });
      setBindingMock('ReleaseReviewDiff', async () => undefined);
      const state = reviewStateForPane('pane-1', subjectFor());
      await waitLoaded(state);
      const line = (path: string) => ({ filePath: path, side: 'new' as const, newLine: 2, selectedText: 'line 2' });
      const discarded = state.openDraftEditor(line('src/c.ts'))!;
      const kept = state.openDraftEditor(line('src/b.ts'))!;
      state.setDraftBody(discarded, 'Gone.');

      hold = new Promise<void>((resolve) => { release = resolve; });
      const reloading = state.reload();
      await vi.waitFor(() => expect(heldReads).toBeGreaterThan(0));
      state.closeDraftEditor(discarded);
      const late = state.openDraftEditor(line('src/a.ts'))!;
      hold = null;
      release();
      await reloading;
      await waitLoaded(state);

      expect(state.openEditors.map((editor) => [editor.id, editor.anchor?.newLine ?? null, editor.state])).toEqual([
        [kept, 2, 'placed'],
        [late, null, 'detached'],
      ]);
      expect(state.draftBodyFor(discarded)).toBe('');
      expect(state.files.map((file) => file.path)).toEqual(['src/a.ts', 'src/b.ts', 'src/c.ts']);
    });

    // A diff of `src/a.ts` and `count` other files, then a reload in which
    // every one of them is renamed. Its text is held to 100 bytes, so each
    // file's text is evicted as the next arrives; `reread` lists the byte
    // offsets the new diff's text is read again at.
    async function renamedWithEvictedText(count: number) {
      const others = Array.from({ length: count }, (_, index) => `src/f${index}.ts`);
      const before = [patchFor('src/a.ts', 3), ...others.map((path) => patchFor(path, 3))].join('\n');
      const renamed = (from: string, to: string): string => [
        `diff --git a/${from} b/${to}`,
        'similarity index 90%',
        `rename from ${from}`,
        `rename to ${to}`,
        'index 1111111..2222222 100644',
        `--- a/${from}`,
        `+++ b/${to}`,
        '@@ -1,3 +1,3 @@',
        ' line 1',
        '-line 2',
        '+line two',
        ' line 3',
      ].join('\n');
      const first = renamed('src/a.ts', 'src/moved.ts');
      const after = [first, ...others.map((path) => renamed(path, path.replace('/f', '/g')))].join('\n');
      setPatchTextBudgetForTest(100);
      let current = before;
      let opened = 0;
      const reread: number[] = [];
      const furthest = new Map<string, number>();
      const chunkAt = (patch: string, offset: number) => {
        const next = Math.min(patch.length, offset + 40);
        return { data: patch.slice(offset, next), offset, nextOffset: next, eof: next === patch.length };
      };
      const texts = new Map<string, string>();
      setBindingMock('OpenWorkspaceDiff', async () => {
        const handle = `diff-${++opened}`;
        texts.set(handle, current);
        furthest.set(handle, 40);
        return { id: handle, chunk: chunkAt(current, 0), headSha: '' };
      });
      setBindingMock('ReadReviewDiff', async (handle: string, offset: number) => {
        if (offset < furthest.get(handle)!) {
          if (handle !== 'diff-1') reread.push(offset);
        } else furthest.set(handle, offset + 40);
        return chunkAt(texts.get(handle)!, offset);
      });
      setBindingMock('ReleaseReviewDiff', async () => undefined);
      const state = reviewStateForPane('pane-1', subjectFor());
      await waitLoaded(state);
      const id = state.openDraftEditor({ filePath: 'src/a.ts', side: 'new', newLine: 2, selectedText: 'line 2' })!;
      current = after;
      await state.reload();
      await waitLoaded(state);
      return { state, id, reread, firstLength: first.length, count };
    }

    it('finds a renamed editor\'s file by the path it came from, reading no other renamed file again', async () => {
      const { state, id, reread, firstLength, count } = await renamedWithEvictedText(40);
      expect(state.files).toHaveLength(count + 1);
      expect(state.openEditors).toMatchObject([{ id, filePath: 'src/moved.ts', state: 'detached' }]);
      // Only the editor's own file is read again: its rows, for the suggestion.
      expect(reread.every((offset) => offset < firstLength)).toBe(true);
    });

    it('publishes the diff with the relocation budget spent and every renamed file\'s text evicted', async () => {
      const restore = __setRelocationBudgetForTest(1, 1);
      try {
        const { state, id, reread, count } = await renamedWithEvictedText(40);
        expect(state.files).toHaveLength(count + 1);
        expect(state.openEditors).toMatchObject([{ id, filePath: 'src/moved.ts', state: 'detached', anchor: null }]);
        expect(reread).toEqual([]);
      } finally {
        restore();
      }
    });
  });

  describe('of the Edits scope', () => {
    function editSection(index: string | null, start: number, from: string, to: string): string {
      return [
        'diff --git a/x.go b/x.go',
        ...(index ? [`index ${index} 100644`] : []),
        '--- a/x.go',
        '+++ b/x.go',
        `@@ -${start},1 +${start},1 @@`,
        `-${from}`,
        `+${to}`,
      ].join('\n');
    }

    function editsWith(turnPatch: () => string) {
      setBindingMock('ListThreadEditDiffs', async () => ({
        entries: [{ itemId: 'tool:1', payloadId: 'pl-1', turnIndex: 1, title: 'Edited x.go', paths: ['x.go'], insertions: 2, deletions: 2, createdAt: 1 }],
        turnLabels: [{ turnIndex: 1, label: 'edit' }],
      }));
      setReviewDiffMock('OpenTurnEditsDiff', async () => turnPatch());
      setBindingMock('VerifyEditDiffs', async () => ({ expandablePaths: [] }));
    }

    it('keeps a placed editor of a merged turn file through a reload of the same bytes, and detaches it when another edit joins', async () => {
      let turn = [editSection('1111111..2222222', 3, 'a', 'b'), editSection('2222222..3333333', 9, 'c', 'd')].join('\n');
      editsWith(() => turn);
      const { state, create } = await workspaceWith(() => '');
      await state.setScope('edits');
      await waitLoaded(state);
      expect(state.files.map((file) => [file.path, file.mergedSections])).toEqual([['x.go', 2]]);
      const nine = { filePath: 'x.go', side: 'new' as const, newLine: 9, selectedText: 'd' };
      const id = state.openDraftEditor(nine)!;

      await state.reload();
      await waitLoaded(state);
      expect(state.openEditors).toEqual([{ id, filePath: 'x.go', side: 'new', state: 'placed', anchor: nine, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);

      // A third edit of the file: its first section's ids are unchanged,
      // and prove nothing of the whole.
      turn = [editSection('1111111..2222222', 3, 'a', 'b'), editSection('2222222..3333333', 9, 'c', 'd'), editSection('3333333..4444444', 20, 'e', 'f')].join('\n');
      await state.reload();
      await waitLoaded(state);
      expect(state.openEditors[0]).toMatchObject({ id, state: 'detached', anchor: null });
      await expect(state.createComment(id, 'x')).rejects.toEqual(refused('detached'));
      expect(create).not.toHaveBeenCalled();
    });

    it('saves no comment on a line number two rows of the file share, and lets the editor move', async () => {
      // Line 1 changed twice in one turn, a to b and then b to c: the two
      // added rows both have new line 1, and a saved line cannot say which.
      editsWith(() => [editSection('1111111..2222222', 1, 'a', 'b'), editSection('2222222..3333333', 1, 'b', 'c')].join('\n'));
      const { state, create } = await workspaceWith(() => '');
      await state.setScope('edits');
      await waitLoaded(state);
      const file = state.files[0]!;
      const onC = Array.from({ length: file.body.lineCount }, (_, index) => index).find((index) => file.body.text(index) === '+c')!;
      const id = state.openDraftEditor({ filePath: 'x.go', side: 'new', newLine: 1, selectedText: 'c', lineIndex: onC })!;
      expect(id).toBeTruthy();
      expect(state.openEditors[0]).toMatchObject({ id, state: 'placed', repeated: true, occurrence: 1 });
      await expect(state.createComment(id, 'On c.')).rejects.toEqual(refused('repeated'));
      expect(create).not.toHaveBeenCalled();
      state.moveEditor(id);
      expect(state.armedEditorId).toBe(id);
    });

    it('keeps a placed editor of a section with no index line through a reload of the same bytes', async () => {
      editsWith(() => editSection(null, 3, 'a', 'b'));
      const { state } = await workspaceWith(() => '');
      await state.setScope('edits');
      await waitLoaded(state);
      const three = { filePath: 'x.go', side: 'new' as const, newLine: 3, selectedText: 'b' };
      const id = state.openDraftEditor(three)!;
      await state.reload();
      await waitLoaded(state);
      expect(state.openEditors).toEqual([{ id, filePath: 'x.go', side: 'new', state: 'placed', anchor: three, suggested: null, armed: false, occurrence: 0, repeated: false, generation: expect.any(Number) }]);
    });
  });

  it('commits the subject the load resolved, not the one asked for', async () => {
    setBindingMock('ListBranchCommits', async () => [{ sha: 'commit-a', subject: 'change' }]);
    setReviewDiffMock('OpenBranchBaseDiff', async () => patchFor('src/app.ts', 1));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('branch');
    await waitLoaded(state);
    // A commit the list does not have: the load shows the whole branch.
    await state.selectCommit('commit-gone');
    await waitLoaded(state);
    expect(state.selectedCommitSHA).toBeNull();
    expect(state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 1, selectedText: 'line 1' })).not.toBeNull();
  });
});

describe('reviewPane store — open new-comment editors of the PR on screen', () => {
  it('opens and saves against the diff on screen while the controls name another PR, and after its load fails', async () => {
    seedPaneWorkspaceStatus('pane-1', { forge: 'github', openPrUrl: 'https://github.com/owner/repo/pull/5', openPrNumber: 5 });
    installPRMocks();
    setBindingMock('SubscribePRUpdates', async (pr: { Number: number }) => ({
      id: `sub-${pr.Number}`,
      prKey: `github:owner/repo:${pr.Number}`,
      detail: prDetailStub({ number: pr.Number, headSHA: pr.Number === 5 ? 'sha-a' : 'sha-z' }),
      threads: [],
      headSHA: pr.Number === 5 ? 'sha-a' : 'sha-z',
    }));
    const create = setBindingMock('CreateDiffReviewComment', async (_thread: string, input: Partial<DiffReviewComment>) => draft(input));
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    await state.setScope('pr');
    await waitLoaded(state);
    const line = { filePath: 'src/app.ts', side: 'new' as const, newLine: 1, selectedText: 'line 1' };
    const onFive = state.openDraftEditor(line)!;
    expect(onFive).toBeTruthy();

    // The workspace's PR is now #7; its diff is loading.
    seedPaneWorkspaceStatus('pane-1', { forge: 'github', openPrUrl: 'https://github.com/owner/repo/pull/7', openPrNumber: 7 });
    let failDiff!: (err: Error) => void;
    setReviewDiffMock('OpenPRDiff', () => new Promise<string>((_resolve, reject) => { failDiff = reject; }));
    const reloading = state.reload();
    await vi.waitFor(() => {
      expect(state.prRef?.number).toBe(7);
      expect(failDiff).toBeTypeOf('function');
    });
    expect(state.openDraftEditor({ ...line, newLine: 2, selectedText: 'line 2' })).toBeNull();
    await state.createComment(onFive, 'On #5.');
    expect(create.mock.calls[0]?.[1]).toMatchObject({ scope: 'pr', sourceKey: PR_SOURCE_KEY, commitSha: 'sha-a', newLine: 1 });

    failDiff(new Error('fetch pull request diff: HTTP 502'));
    await reloading;
    await waitLoaded(state);
    expect(state.error).toContain('HTTP 502');
    expect(state.prRef?.number).toBe(7);
    expect(state.files.map((file) => file.path)).toEqual(['src/app.ts']);
    expect(state.openDraftEditor({ ...line, newLine: 3, selectedText: 'line 3' })).toBeNull();
    expect(state.openDraftEditor(line)).toBeNull();
  });

  it('excludes a saved draft with one number on an unchanged line from the review, as before', async () => {
    __setTransportHelloForTest({ launchId: 'l', protocolVersion: 1, capabilities: [], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    try {
      installPRMocks();
      setReviewDiffMock('OpenPRDiff', async () => ['diff --git a/src/app.ts b/src/app.ts', 'index 1111111..2222222 100644', '--- a/src/app.ts', '+++ b/src/app.ts', '@@ -1,3 +1,4 @@', ' keep 1', '-old 2', '+new 2', '+added', ' keep 3'].join('\n'));
      const stored = [
        { ...draft({ id: 'left-only', scope: 'pr', sourceKey: PR_SOURCE_KEY, filePath: 'src/app.ts', side: 'old', selectedText: 'keep 3', body: 'Left.' }), oldLine: 3, newLine: undefined },
        { ...draft({ id: 'right-only', scope: 'pr', sourceKey: PR_SOURCE_KEY, filePath: 'src/app.ts', side: 'new', selectedText: 'keep 3', body: 'Right.' }), oldLine: undefined, newLine: 4 },
        { ...draft({ id: 'added', scope: 'pr', sourceKey: PR_SOURCE_KEY, filePath: 'src/app.ts', side: 'new', selectedText: 'added', body: 'Added.' }), oldLine: undefined, newLine: 3 },
      ];
      setBindingMock('ListDiffReviewComments', async (_thread: string, scope: string) => (scope === 'pr' ? stored.map((row) => ({ ...row })) : []));
      setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
      const submit = getBindingMock('SubmitPRReview')!;
      const state = reviewStateForPane('pane-1', prSubject());
      await waitLoaded(state);
      await state.setScope('pr');
      await waitLoaded(state);
      await vi.waitFor(() => expect(state.drafts).toHaveLength(3));
      expect([...state.orphanedDraftIds()].sort()).toEqual(['left-only', 'right-only']);
      await state.submitPRReview();
      expect(submit.mock.calls[0]?.[1]).toMatchObject({ comments: [{ path: 'src/app.ts', body: 'Added.', line: 3, side: 'right' }] });
    } finally {
      __setTransportHelloForTest(null);
    }
  });

  it('saves both halves of a split context row with both numbers, and the review posts them as the unchanged line', async () => {
    __setTransportHelloForTest({ launchId: 'l', protocolVersion: 1, capabilities: [], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    try {
      installPRMocks();
      setReviewDiffMock('OpenPRDiff', async () => [
        'diff --git a/src/app.ts b/src/app.ts',
        'index 1111111..2222222 100644',
        '--- a/src/app.ts',
        '+++ b/src/app.ts',
        '@@ -1,3 +1,4 @@',
        ' keep 1',
        '-old 2',
        '+new 2',
        '+added',
        ' keep 3',
      ].join('\n'));
      const rows: DiffReviewComment[] = [];
      setBindingMock('CreateDiffReviewComment', async (_thread: string, input: Partial<DiffReviewComment>) => {
        const row = { ...draft({ ...input, id: `c-${rows.length + 1}` }), oldLine: input.oldLine, newLine: input.newLine };
        rows.push(row);
        return row;
      });
      setBindingMock('ListDiffReviewComments', async (_thread: string, scope: string, sourceKey: string) =>
        rows.filter((row) => row.scope === scope && row.sourceKey === sourceKey && row.status === 'draft').map((row) => ({ ...row })));
      setBindingMock('MarkDiffReviewCommentsSent', async (_t: string, _s: string, _k: string, ids: string[]) => {
        for (const row of rows) if (ids.includes(row.id)) row.status = 'sent';
      });
      const submit = getBindingMock('SubmitPRReview')!;
      const state = reviewStateForPane('pane-1', prSubject());
      await waitLoaded(state);
      await state.setScope('pr');
      await waitLoaded(state);

      const left = state.openDraftEditor({ filePath: 'src/app.ts', side: 'old', oldLine: 3, selectedText: ' keep 3' })!;
      const right = state.openDraftEditor({ filePath: 'src/app.ts', side: 'new', newLine: 4, selectedText: ' keep 3' })!;
      await state.createComment(left, 'Left half.');
      await state.createComment(right, 'Right half.');
      // An unchanged line carries both numbers, whichever half was used.
      expect(state.drafts.map((comment) => [comment.side, comment.oldLine, comment.newLine])).toEqual([['old', 3, 4], ['new', 3, 4]]);
      expect(state.orphanedDraftIds().size).toBe(0);
      // Each renders on its row, not under the file header.
      const built = buildReviewRows({ files: state.files, viewMode: 'split', collapsedPaths: new Set(), drafts: state.drafts, openEditors: [] });
      const threadAt = built.rows.findIndex((row) => row.kind === 'comment-thread');
      expect(built.rows[threadAt - 1]?.kind).toBe('line-block');

      await state.submitPRReview();
      expect(submit.mock.calls[0]?.[1]).toMatchObject({
        comments: [
          { path: 'src/app.ts', body: 'Left half.', line: 4, oldLine: 3, side: 'right' },
          { path: 'src/app.ts', body: 'Right half.', line: 4, oldLine: 3, side: 'right' },
        ],
      });
    } finally {
      __setTransportHelloForTest(null);
    }
  });
});
