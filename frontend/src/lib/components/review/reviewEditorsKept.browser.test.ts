// An open comment editor is a virtualized row: scrolling it out of the
// held range unmounts it. In real Chromium, a new line comment, an edit
// of a draft and a reply to a PR thread keep their unsaved text and their
// open state across that unmount, across a reload of the diff, on a
// thread's companion review and on a review pinned to a PR. Cancel ends
// a new comment or an edit and keeps nothing, as Save does; Cancel on a
// reply closes it and keeps its text until the reply sends. A draft's
// edit stays through other views of the same subject (whitespace, a
// commit and back) and goes with its draft or with the subject.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import '../../../app.css';
import ReviewPane from './ReviewPane.svelte';
import { __resetReviewPaneStateForTest, reviewStateForPane, reviewSubjectForPane } from '../../stores/reviewPane.svelte';
import { __resetPinnedReviewStatesForTest, currentPinnedReviewState, disposePRReviewStates } from '../../stores/reviewPanePinned.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { resetForTest as resetDiffReviewComments } from '../../stores/diffReviewComments.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { DiffReviewComment, PRDetail, ReviewThread } from '../../types/models';
import type { PRRef } from '../../utils/prReference';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { installPRDraftsFake } from '../../../test/helpers/prDraftsFake';
import { setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';

const LINES = 3000;
const OWNER = 'tools-1';
const PR: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };

function patch(): string {
  const out = ['diff --git a/src/long.ts b/src/long.ts', 'new file mode 100644', '--- /dev/null', '+++ b/src/long.ts', `@@ -0,0 +1,${LINES} @@`];
  for (let line = 1; line <= LINES; line += 1) out.push(`+line ${line}`);
  return out.join('\n') + '\n';
}

function draft(scope: 'workspace' | 'pr', sourceKey: string): DiffReviewComment {
  return {
    id: 'd1', threadId: scope === 'pr' ? '' : 'thread-1', scope, sourceKey, filePath: 'src/long.ts', status: 'draft',
    newLine: 6, side: 'new', selectedText: '', body: 'saved body', createdAt: 1, updatedAt: 1,
  } as DiffReviewComment;
}

const THREAD: ReviewThread = {
  id: 'p1', path: 'src/long.ts', line: 4, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
  comments: [{ authorLogin: 'priya', body: 'Why four?', createdAt: '', databaseID: 11 }],
} as ReviewThread;

function detail(): PRDetail {
  return {
    number: 5, title: 'Long', body: '', authorLogin: 'octocat', state: 'open', draft: false,
    headRefName: 'feature', baseRefName: 'main', baseRefSHA: 'base-main', headSHA: 'sha-a',
    url: 'https://github.com/owner/repo/pull/5', additions: LINES, deletions: 0, changedFiles: 1,
    viewerIsAuthor: false, reviewDecision: '', latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean', readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: true, viewerCanApprove: true, viewerApproved: false },
  };
}

let app: ReturnType<typeof mount> | undefined;
let target: HTMLDivElement;

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  __resetPinnedReviewStatesForTest();
  resetPanesForTest();
  resetDiffReviewComments();
  // The pinned review's PR reads wait for a connected transport.
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  target = document.createElement('div');
  target.style.cssText = 'position:fixed;top:0;left:0;width:1100px;height:700px;display:flex;flex-direction:column';
  document.body.append(target);
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  setBindingMock('HighlightPatchWithContext', async () => ({ lines: [] }));
});

afterEach(async () => {
  if (app) await unmount(app);
  app = undefined;
  target.remove();
  disposePRReviewStates(OWNER);
  __resetReviewPaneStateForTest();
});

function q<T extends Element = HTMLElement>(selector: string): T | null {
  return target.querySelector<T>(selector);
}

function scroller(): HTMLElement {
  return q('[data-testid="review-scroll"]')!;
}

async function frames(count = 2): Promise<void> {
  for (let i = 0; i < count; i += 1) await new Promise((done) => requestAnimationFrame(() => done(null)));
}

function type(textarea: HTMLTextAreaElement, text: string): void {
  textarea.value = text;
  textarea.dispatchEvent(new Event('input', { bubbles: true }));
}

function button(root: Element, label: string): HTMLButtonElement {
  const found = Array.from(root.querySelectorAll<HTMLButtonElement>('button')).find((each) => each.textContent?.trim() === label || each.getAttribute('aria-label') === label);
  if (!found) throw new Error(`no ${label} button`);
  return found;
}

async function lineBlock(): Promise<HTMLElement> {
  return vi.waitFor(() => {
    const block = q('[data-testid="review-line-block"]');
    expect(block, target.textContent ?? '').not.toBeNull();
    return block!;
  }, { timeout: 5_000 });
}

async function openNewComment(): Promise<void> {
  const block = await lineBlock();
  block.dispatchEvent(new MouseEvent('mouseover', { bubbles: true }));
  const add = await vi.waitFor(() => {
    const found = target.querySelectorAll<HTMLButtonElement>('[data-testid="review-add-comment"]')[1];
    expect(found).toBeTruthy();
    return found!;
  });
  add.click();
  await vi.waitFor(() => expect(q('[data-testid="review-draft-editor"] textarea')).not.toBeNull());
}

function editor(): HTMLTextAreaElement | null {
  return q('[data-testid="review-draft-editor"] textarea');
}

function editing(): HTMLTextAreaElement | null {
  return q('[data-testid="review-comment-thread"] textarea');
}

function replyComposer(): HTMLTextAreaElement | null {
  return q('[data-testid="review-pr-thread"] [data-testid="review-thread-composer"] textarea');
}

async function startEdit(): Promise<void> {
  const card = await vi.waitFor(() => {
    const found = q('[data-testid="review-comment-thread"]');
    expect(found).not.toBeNull();
    return found!;
  });
  button(card, 'Edit').click();
  await vi.waitFor(() => expect(editing()).not.toBeNull());
}

// Far enough down that the rows near the top leave the held range, and back.
async function scrollAwayAndBack(): Promise<void> {
  scroller().scrollTop = scroller().scrollHeight;
  await vi.waitFor(async () => {
    await frames();
    expect(q('[data-testid="review-comment-thread"]')).toBeNull();
    expect(q('[data-testid="review-draft-editor"]')).toBeNull();
    expect(q('[data-testid="review-pr-thread"]')).toBeNull();
    scroller().scrollTop = scroller().scrollHeight;
  });
  scroller().scrollTop = 0;
  await vi.waitFor(async () => {
    await frames();
    expect(q('[data-testid="review-comment-thread"]')).not.toBeNull();
    scroller().scrollTop = 0;
  });
  await tick();
}

describe('open comment editors across a row unmount', () => {
  it('keep a new comment and a draft edit on a thread companion, through a reload', async () => {
    const held = [draft('workspace', '')];
    setBindingMock('GetThread', async () => ({ id: 'thread-1', workspacePath: '/repo' }));
    setBindingMock('GetGitStatus', async () => ({}));
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch());
    setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: true, isDefault: true }]);
    setBindingMock('ListDiffReviewComments', async (_thread: string, scope: string, sourceKey: string) =>
      held.map((comment) => ({ ...comment, scope, sourceKey })));
    const update = setBindingMock('UpdateDiffReviewComment', async (_thread: string, id: string, input: { body: string }) => {
      const comment = held.find((each) => each.id === id)!;
      comment.body = input.body;
      return { ...comment };
    });
    app = mount(ReviewPane, { target, props: { ctx: makeStubPanelContext() } });

    await openNewComment();
    type(editor()!, 'kept new');
    await startEdit();
    type(editing()!, 'kept edit');

    await scrollAwayAndBack();
    expect(editor()?.value).toBe('kept new');
    expect(editing()?.value).toBe('kept edit');

    // A reload of the same diff keeps them.
    q<HTMLButtonElement>('[data-testid="review-reload"]')!.click();
    await frames(4);
    await vi.waitFor(() => {
      expect(editor()?.value).toBe('kept new');
      expect(editing()?.value).toBe('kept edit');
    });

    // Cancel discards; reopened, the editors are empty or the saved body.
    button(q('[data-testid="review-draft-editor"]')!, 'Cancel').click();
    button(q('[data-testid="review-comment-thread"]')!, 'Cancel').click();
    await vi.waitFor(() => {
      expect(editor()).toBeNull();
      expect(editing()).toBeNull();
    });
    await scrollAwayAndBack();
    expect(editor()).toBeNull();
    expect(editing()).toBeNull();
    await openNewComment();
    expect(editor()!.value).toBe('');
    await startEdit();
    expect(editing()!.value).toBe('saved body');

    // Save ends the edit and keeps nothing.
    type(editing()!, 'saved again');
    button(q('[data-testid="review-comment-thread"]')!, 'Save').click();
    await vi.waitFor(() => expect(update).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(editing()).toBeNull());
    await scrollAwayAndBack();
    expect(editing()).toBeNull();
    await startEdit();
    expect(editing()!.value).toBe('saved again');
  });

  it('keep a new comment, a draft edit and a thread reply on a pinned review', async () => {
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1', prKey: 'github:owner/repo:5', detail: detail(), threads: [THREAD], headSHA: 'sha-a', seq: 1,
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('ListPRCommits', async () => []);
    setBindingMock('ListPRReviewThreads', async () => [THREAD]);
    setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
    const fake = installPRDraftsFake();
    let sourceKey = '';
    setBindingMock('ListPRDiffReviewComments', async (_wire: unknown, scope: string, key: string) => {
      sourceKey = key;
      return (fake.held.get(5) ?? []).filter((each) => each.scope === scope && each.sourceKey === key).map((each) => ({ ...each }));
    });
    app = mount(ReviewPane, { target, props: { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } } });
    await lineBlock();
    await vi.waitFor(() => expect(sourceKey).not.toBe(''));
    fake.held.set(5, [draft('pr', sourceKey)]);
    q<HTMLButtonElement>('[data-testid="review-reload"]')!.click();

    await openNewComment();
    type(editor()!, 'kept new');
    await startEdit();
    type(editing()!, 'kept edit');
    const reply = await vi.waitFor(() => {
      const found = q<HTMLButtonElement>('[data-testid="review-pr-thread"] [data-testid="review-thread-reply"]');
      expect(found).not.toBeNull();
      return found!;
    });
    reply.click();
    await vi.waitFor(() => expect(replyComposer()).not.toBeNull());
    type(replyComposer()!, 'kept reply');

    await scrollAwayAndBack();
    expect(editor()?.value).toBe('kept new');
    expect(editing()?.value).toBe('kept edit');
    expect(replyComposer()?.value).toBe('kept reply');

    // Cancel closes the reply and keeps its text; reopened, it is there.
    button(q('[data-testid="review-thread-composer"]')!, 'Cancel').click();
    await vi.waitFor(() => expect(replyComposer()).toBeNull());
    await scrollAwayAndBack();
    expect(replyComposer()).toBeNull();
    q<HTMLButtonElement>('[data-testid="review-pr-thread"] [data-testid="review-thread-reply"]')!.click();
    await vi.waitFor(() => expect(replyComposer()?.value).toBe('kept reply'));

    // An open composer with nothing typed stays open too.
    type(replyComposer()!, '');
    await scrollAwayAndBack();
    expect(replyComposer()?.value).toBe('');

    // A reply that sends keeps nothing.
    type(replyComposer()!, 'sent reply');
    const sent = setBindingMock('ReplyToPRThread', async () => undefined);
    await tick();
    button(q('[data-testid="review-thread-composer"]')!, 'Reply').click();
    await vi.waitFor(() => expect(sent).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => expect(replyComposer()?.value ?? '').toBe(''));
  });

  it('keep a draft edit across whitespace and commit views of the same PR, and drop it with its draft', async () => {
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1', prKey: 'github:owner/repo:5', detail: detail(), threads: [], headSHA: 'sha-a', seq: 1,
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setReviewDiffMock('OpenPRCommitDiff', async () => patch());
    const commit = 'c'.repeat(40);
    setBindingMock('ListPRCommits', async () => [{ sha: commit, shortSha: 'ccccccc', subject: 'One', author: 'octocat', authoredAt: 1 }]);
    setBindingMock('ListPRReviewThreads', async () => []);
    setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
    const fake = installPRDraftsFake();
    let sourceKey = '';
    setBindingMock('ListPRDiffReviewComments', async (_wire: unknown, scope: string, key: string) => {
      if (key.startsWith('pr:')) sourceKey = key;
      return (fake.held.get(5) ?? []).filter((each) => each.scope === scope && each.sourceKey === key).map((each) => ({ ...each }));
    });
    app = mount(ReviewPane, { target, props: { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } } });
    await lineBlock();
    await vi.waitFor(() => expect(sourceKey).not.toBe(''));
    fake.held.set(5, [draft('pr', sourceKey)]);
    q<HTMLButtonElement>('[data-testid="review-reload"]')!.click();
    await startEdit();
    type(editing()!, 'kept edit');
    const review = currentPinnedReviewState(OWNER)!;

    // Hide whitespace is a new view of the same drafts: the edit stays open.
    await review.setIgnoreWhitespace(true);
    await vi.waitFor(() => expect(editing()?.value).toBe('kept edit'));

    // A commit's view has other drafts; back on the PR the edit is there.
    await review.selectCommit(commit);
    await vi.waitFor(() => expect(editing()).toBeNull());
    await review.selectCommit(null);
    await vi.waitFor(() => expect(editing()?.value).toBe('kept edit'));

    // A read of the drafts without it drops the edit with the draft.
    fake.held.set(5, []);
    await review.reload();
    await vi.waitFor(() => expect(q('[data-testid="review-comment-thread"]')).toBeNull());
    fake.held.set(5, [draft('pr', sourceKey)]);
    await review.reload();
    await startEdit();
    expect(editing()!.value).toBe('saved body');
  });

  it('drop a draft edit when the review moves to another subject', async () => {
    const held = [draft('workspace', '')];
    setBindingMock('GetThread', async () => ({ id: 'thread-1', workspacePath: '/repo' }));
    setBindingMock('GetGitStatus', async () => ({}));
    setReviewDiffMock('OpenWorkspaceDiff', async () => patch());
    setReviewDiffMock('OpenBranchBaseDiff', async () => patch());
    setBindingMock('ListBranchCommits', async () => []);
    setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: true, isDefault: true }]);
    setBindingMock('ListDiffReviewComments', async (_thread: string, scope: string, sourceKey: string) =>
      held.map((comment) => ({ ...comment, scope, sourceKey })));
    const ctx = makeStubPanelContext();
    app = mount(ReviewPane, { target, props: { ctx } });
    await startEdit();
    type(editing()!, 'kept edit');
    const review = reviewStateForPane(ctx.paneId, reviewSubjectForPane(ctx)!);

    await review.setScope('branch');
    await review.setScope('workspace');
    await startEdit();
    expect(editing()!.value).toBe('saved body');
  });
});
