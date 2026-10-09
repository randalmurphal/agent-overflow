import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { tick } from 'svelte';
import { appStorageGet, appStorageSet, resetAppStorageForTest } from './appStorage';
import {
  activeDiffReviewSourceForThread,
  replaceDiffReviewCommentsForTest,
} from './diffReviewComments.svelte';
import { projectTurnStarted } from './threadStatuses.svelte';
import {
  __resetReviewPaneStateForTest,
  disposeReviewStateForPane,
  openReviewCompanion,
  reviewStateForPane,
  type ConversationFeedItem,
  type ReviewSubject,
} from './reviewPane.svelte';
import {
  draftAnchorExists,
  EditFileMerge,
  reviewLineCommentForDraft,
  supportsIgnoreWhitespace,
} from './reviewPaneLoad';
import { applyPRUpdatedEvent } from './prReviewStore.svelte';
import { resetCompanionPanesForTest } from './companionPanes.svelte';
import { companionSubjectKey } from './companionSubject';
import { __seedGitStatusForTest } from './gitStatusStore.svelte';
import { registerPaneForTest, resetPanesForTest } from './panes.svelte';
import { createThreadPane } from './thread.svelte';
import { registerComposerDraft, resetComposerDraftRegistryForTest } from './composerDraftRegistry.svelte';
import { resetPaneLayoutForTest, setPaneLayoutItemsForTest } from './paneLayout.svelte';
import type { DiffReviewComment, PRDetail, ReviewThread, Thread } from '../types/models';
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

    const same = reviewStateForPane('pane-1', subjectFor());
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
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    expect(state.collapsedPaths.has('src/small.ts')).toBe(false);
    expect(state.collapsedPaths.has('pnpm-lock.yaml')).toBe(true);
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

    state.consumePendingJumpRowKey();
    expect(state.pendingJumpRowKey).toBeNull();

    // A PR-level conversation thread has no row to jump to.
    state.jumpToDiffThread({ ...reviewThreadStub('thread-10'), path: '' });
    expect(state.pendingJumpRowKey).toBeNull();
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

  it('createComment success closes the draft editor', async () => {
    const patch = patchFor('src/app.ts', 1);
    const sourceKey = diffSourceKey(patch);
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);
    setBindingMock('CreateDiffReviewComment', async () => draft({ sourceKey, body: 'Looks good.' }));
    setBindingMock('ListDiffReviewComments', async () => [draft({ sourceKey, body: 'Looks good.' })]);

    const state = reviewStateForPane('pane-1', subjectFor());
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
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);
    setBindingMock('CreateDiffReviewComment', async () => {
      throw new Error('create failed');
    });

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    const anchor = { filePath: 'src/app.ts', side: 'new' as const, newLine: 1, selectedText: 'line 1' };
    state.openDraftEditor(anchor);

    await expect(state.createComment(anchor, 'Body')).rejects.toThrow('create failed');

    expect(state.openEditors).toHaveLength(1);
    expect(state.error).toBe('create failed');
  });

  it('keeps draft-editor text in the store and focuses exactly once per open', async () => {
    const patch = patchFor('src/app.ts', 1);
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch);
    const state = reviewStateForPane('pane-1', subjectFor());
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
  setBindingMock('GetPRCIJobs', async () => ({ status: '', stages: [] }));
  setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));
  setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
  return { subscribe, unsubscribe };
}

describe('reviewPane store — PR scope', () => {
  it('enter subscribes and loads the PR diff; leaving unsubscribes exactly once', async () => {
    const { subscribe, unsubscribe } = installPRMocks();
    const diff = setReviewDiffMock('OpenPRDiff', async () => patchFor('src/app.ts', 3));
    const state = reviewStateForPane('pane-1', prSubject());
    await waitLoaded(state);

    await state.setScope('pr');
    expect(subscribe).toHaveBeenCalledTimes(1);
    // The diff is opened with the base ref from the detail, which the
    // backend diffs the PR head against in the local clone.
    expect(diff).toHaveBeenCalledWith(
      REVIEW_WS,
      expect.objectContaining({ Number: 5 }),
      'main',
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
    // The known head SHA rides along so the backend can skip its fetch
    // when the objects are already in the local clone.
    expect(list).toHaveBeenCalledWith(REVIEW_WS, expect.objectContaining({ Number: 5 }), 'main', 'sha-a');
    expect(state.commits.map((commit) => commit.sha)).toEqual([sha]);
    expect(state.sourceKey).toBe(PR_SOURCE_KEY);

    await state.selectCommit(sha);
    expect(state.selectedCommitSHA).toBe(sha);
    expect(state.sourceKey).toBe(`commit:${sha}`);
    expect(commitDiff).toHaveBeenLastCalledWith(REVIEW_WS, expect.objectContaining({ Number: 5 }), sha, false);

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

    releaseDiff(patchFor('src/app.ts', 3));
    await reloading;
    expect(state.prHeadSHA).toBe('sha-z');
    expect(state.prStale).toBe(false);
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

    state.expandConflictFold('main.go', 0);
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/acme/widgets/pull/7',
      openPrNumber: 7,
    });

    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);

    await vi.waitFor(() => {
      expect(state.prRef).toEqual({ forge: 'github', namespace: 'acme', repo: 'widgets', number: 7 });
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
      expect(state.prRef).toEqual({ forge: 'gitlab', namespace: 'group/sub', repo: 'repo', number: 3 });
    });
  });

  it('a pane restored into pr scope before git status lands retries when the ref resolves', async () => {
    installPRMocks();
    appStorageSet('reviewScope:thread-1', JSON.stringify({ scope: 'pr' }));
    // No pane registered and no git status yet: the boot load runs before
    // the fetch lands and comes up with no reference.
    const state = reviewStateForPane('pane-1', subjectFor());
    await waitLoaded(state);
    expect(state.scope).toBe('pr');
    expect(state.error).toContain('No PR or MR');

    // The enriched status lands on the shared store; the ref watcher
    // retries the load with no user interaction and the pane recovers.
    seedPaneWorkspaceStatus('pane-1', {
      forge: 'github',
      openPrUrl: 'https://github.com/owner/repo/pull/5',
      openPrNumber: 5,
    });
    await vi.waitFor(() => {
      expect(state.prRef?.number).toBe(5);
      expect(state.error).toBeNull();
    });
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

  it('submit success marks drafts sent against the head SHA and clears the summary', async () => {
    installPRMocks();
    const markSent = setBindingMock('MarkDiffReviewCommentsSent', async () => undefined);
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
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
    const state = reviewStateForPane('pane-1', prSubject());
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

    state.openConversationAt('t-1');
    expect(state.conversationOpen).toBe(true);
    expect(state.conversationThreadExpanded('t-1')).toBe(true);
    expect(state.pendingConversationThreadId).toBe('t-1');
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

    state.dispose();
    expect(release).toHaveBeenCalledTimes(1);
    expect(release).toHaveBeenCalledWith('diff-1');
    expect(patchTextHeldBytes()).toBe(base);
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

    expect(state.awaitingPRDetail).toBe(true);
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
    expect(state.awaitingPRDetail).toBe(false);
    expect(state.prUpdateError).toBeNull();
    expect(state.prDetail?.number).toBe(5);
    expect(diff).toHaveBeenCalledTimes(1);
  });
});
