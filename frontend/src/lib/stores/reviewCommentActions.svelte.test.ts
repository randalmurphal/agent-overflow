// Send now or hold, ranges, held replies, and edit and delete of sent
// comments, through a review pinned to one PR (the same state a thread's
// companion review uses): the primary-button rule across its transitions,
// the settle rule (pending until the frame at the write's seq is applied,
// a failure keeps the text, seq 0 closes at once), the pane's gates, the
// range checks, and editor text surviving its row unmounting.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import { fireEvent, render } from '@testing-library/svelte';
import { resetAppStorageForTest } from './appStorage';
import { __resetReviewPaneStateForTest, type CommentAnchor, type ReviewPaneState } from './reviewPane.svelte';
import { __resetPinnedReviewStatesForTest, reviewStateForPR, reviewSubjectForPR } from './reviewPanePinned.svelte';
import { applyPRUpdatedEvent } from './prReviewStore.svelte';
import { commentWriteKey } from './reviewCommentWrites.svelte';
import { resetComposerDraftRegistryForTest } from './composerDraftRegistry.svelte';
import { resetPanesForTest } from './panes.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { prKey, type PRRef } from '../utils/prReference';
import type { DiffReviewComment, PRDetail, ReviewComment, ReviewThread } from '../types/models';
import { installPRDraftsFake, type PRDraftsFake } from '../../test/helpers/prDraftsFake';
import { setBindingMock, setReviewDiffMock } from '../../test/mocks/bindings-app';
import { TransportError } from '../transport/wsClient';
import { resetToLocalPage } from '../../test/helpers/scopes';
import { resetStagedBackends } from '../../test/helpers/backends';
import ReviewSentCommentBody from '../components/review/ReviewSentCommentBody.svelte';
import ReviewPRCommentField from '../components/review/ReviewPRCommentField.svelte';
import ReviewThreadFooter from '../components/review/ReviewThreadFooter.svelte';
import ReviewDraftEditor from '../components/review/ReviewDraftEditor.svelte';

const PROJECT = 'project-1';
const REF: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };
const KEY = prKey(REF);

function detail(overrides: Partial<PRDetail> = {}): PRDetail {
  return {
    number: 5, title: 'PR 5', body: '', authorLogin: 'alice', state: 'open', draft: false,
    headRefName: 'feature', baseRefName: 'main', baseRefSHA: 'base', headSHA: 'sha-5',
    url: 'https://github.com/owner/repo/pull/5', additions: 1, deletions: 0, changedFiles: 1,
    viewerIsAuthor: false, reviewDecision: '', latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean', readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: true, viewerCanApprove: true, viewerApproved: false },
    ...overrides,
  };
}

// src/a.ts lines 10-13: a context line, a deletion, two additions, a
// context line.
const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts',
  'index 1111111..2222222 100644',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -10,3 +10,4 @@',
  ' keep ten',
  '-gone eleven',
  '+new eleven',
  '+new twelve',
  ' keep twelve',
].join('\n');

const at = (anchor: Omit<CommentAnchor, 'filePath'>): CommentAnchor => ({ filePath: 'src/a.ts', ...anchor });
const context10 = at({ side: 'context', oldLine: 10, newLine: 10, selectedText: 'keep ten' });
const deleted11 = at({ side: 'old', oldLine: 11, selectedText: 'gone eleven' });
const added11 = at({ side: 'new', newLine: 11, selectedText: 'new eleven' });
const added12 = at({ side: 'new', newLine: 12, selectedText: 'new twelve' });

function comment(overrides: Partial<ReviewComment> = {}): ReviewComment {
  return { id: 'node-1', authorLogin: 'me', body: 'First.', createdAt: '2026-01-01T00:00:00Z', databaseID: 101, viewerCanEdit: true, viewerCanDelete: true, ...overrides };
}

const THREAD: ReviewThread = {
  id: 'thread-1', path: 'src/a.ts', line: 12, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
  comments: [comment(), comment({ id: 'node-2', authorLogin: 'bob', body: 'A reply.', databaseID: 102, viewerCanEdit: false, viewerCanDelete: false })],
};

let threads: ReviewThread[] = [THREAD];
let drafts: PRDraftsFake;

function frame(seq: number, overrides: { headSHA?: string; threads?: ReviewThread[] } = {}): void {
  const head = overrides.headSHA ?? 'sha-5';
  applyPRUpdatedEvent({ prKey: KEY, detail: detail({ headSHA: head }), threads: overrides.threads ?? threads, headSHA: head, seq });
  flushSync();
}

function hello(capabilities: string[] = ['pr-review-drafts.v1', 'pr-comment-actions.v1']): void {
  __setTransportHelloForTest({ launchId: 'l', protocolVersion: 1, capabilities, backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
}

async function open(): Promise<ReviewPaneState> {
  const state = reviewStateForPR('tools-1', reviewSubjectForPR({ ownerPaneId: 'tools-1', backend: HOME_BACKEND, projectId: PROJECT, pr: REF }));
  await vi.waitFor(() => {
    expect(state.loading).toBe(false);
    expect(state.prHeadSHA).toBe('sha-5');
    expect(state.prThreads.length).toBe(threads.length);
  });
  return state;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function editorWith(state: ReviewPaneState, anchor: CommentAnchor, body: string): string {
  const id = state.openDraftEditor(anchor);
  if (id === null) throw new Error(`no line for ${JSON.stringify(anchor)}`);
  state.setDraftBody(id, body);
  return id;
}

beforeEach(() => {
  resetAppStorageForTest();
  __resetPinnedReviewStatesForTest();
  __resetReviewPaneStateForTest();
  resetComposerDraftRegistryForTest();
  resetPanesForTest();
  resetToLocalPage();
  hello();
  threads = [THREAD];
  setBindingMock('SubscribePRUpdates', async () => ({ id: 'sub-5', prKey: KEY, detail: detail(), threads, headSHA: 'sha-5', seq: 1 }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => PATCH);
  setBindingMock('ListPRCommits', async () => []);
  setBindingMock('ListPRReviewThreads', async () => threads);
  setBindingMock('GetPRDetail', async () => detail());
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  drafts = installPRDraftsFake();
});

afterEach(() => {
  __resetPinnedReviewStatesForTest();
  __resetReviewPaneStateForTest();
  resetStagedBackends();
  resetToLocalPage();
});

describe('the primary comment action', () => {
  it('is now with no drafts, a draft once one is held, and now again when none is', async () => {
    const state = await open();
    expect(state.canPostNow).toBe(true);
    expect(state.primaryCommentAction).toBe('now');

    // A line draft.
    await state.createComment(editorWith(state, added11, 'Guard this.'), 'Guard this.');
    expect(state.drafts).toHaveLength(1);
    expect(state.primaryCommentAction).toBe('draft');
    await state.deleteComment(state.drafts[0].id);
    expect(state.drafts).toHaveLength(0);
    expect(state.primaryCommentAction).toBe('now');

    // A held reply counts the same.
    state.setReplyBody(THREAD.id, 'Agreed.');
    await state.holdPRThreadReply(THREAD);
    expect(state.drafts.map((draft) => draft.side)).toEqual(['reply']);
    expect(state.primaryCommentAction).toBe('draft');
    // Both families held at once.
    await state.createComment(editorWith(state, added12, 'And this.'), 'And this.');
    expect(state.drafts).toHaveLength(2);
    expect(state.primaryCommentAction).toBe('draft');
    // A copy: each delete shrinks state.drafts.
    for (const draft of state.drafts.slice()) await state.deleteComment(draft.id);
    expect(state.primaryCommentAction).toBe('now');
  });

  it('offers no now on a backend without the comment actions', async () => {
    hello(['pr-review-drafts.v1']);
    const state = await open();
    expect(state.canPostNow).toBe(false);
    expect(state.canHoldReply).toBe(false);
    expect(state.sentCommentActions).toBe(false);
  });
});

describe('a comment posted now', () => {
  it('stays open and pending until the frame at its seq is applied', async () => {
    const post = setBindingMock('PostPRLineComment', async () => ({ seq: 7 }));
    const state = await open();
    const id = editorWith(state, added12, 'Rename this.');
    await state.createComment(id, 'Rename this.', 'now');
    expect(post).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), {
      headSha: 'sha-5', filePath: 'src/a.ts', side: 'new', oldLine: undefined, newLine: 12, startOldLine: undefined, startNewLine: undefined, body: 'Rename this.',
    });
    const key = commentWriteKey.editor(id);
    expect(state.commentWritePending(key)).toBe(true);
    expect(state.openEditors.map((editor) => editor.id)).toEqual([id]);
    frame(6);
    expect(state.commentWritePending(key)).toBe(true);
    expect(state.draftBodyFor(id)).toBe('Rename this.');
    frame(7);
    expect(state.commentWritePending(key)).toBe(false);
    expect(state.openEditors).toEqual([]);
    expect(state.drafts).toEqual([]);
  });

  it('keeps the editor and its text with the forge\'s sentence when refused', async () => {
    setBindingMock('PostPRLineComment', async () => { throw new Error('The branch moved after you loaded this. Reload and try again.'); });
    const state = await open();
    const id = editorWith(state, added12, 'Rename this.');
    await state.createComment(id, 'Rename this.', 'now');
    const key = commentWriteKey.editor(id);
    expect(state.commentWritePending(key)).toBe(false);
    expect(state.commentWriteError(key)).toBe('The branch moved after you loaded this. Reload and try again.');
    expect(state.openEditors.map((editor) => editor.id)).toEqual([id]);
    expect(state.draftBodyFor(id)).toBe('Rename this.');
  });

  it('renders an unconfirmed write as a failure: open, text kept, the sentence, no retry', async () => {
    const UNCONFIRMED = 'GitHub may have posted this comment. Check the thread before sending it again.';
    const refuse = async () => { throw new TransportError('forge_unconfirmed', UNCONFIRMED); };
    const line = setBindingMock('PostPRLineComment', refuse);
    const reply = setBindingMock('ReplyToPRThread', refuse);
    const prComment = setBindingMock('PostPRComment', refuse);
    const state = await open();
    const id = editorWith(state, added12, 'Rename this.');
    await state.createComment(id, 'Rename this.', 'now');
    state.openReply(THREAD.id, 'diff');
    state.setReplyBody(THREAD.id, 'Agreed.');
    await state.sendPRThreadReply(THREAD);
    state.sentComments.setPRCommentOpen(true);
    state.sentComments.setPRCommentBody('Looks good.');
    await state.sentComments.postPRComment();
    await settle();
    expect([line, reply, prComment].map((mock) => mock.mock.calls.length)).toEqual([1, 1, 1]);
    expect(state.commentWriteError(commentWriteKey.editor(id))).toBe(UNCONFIRMED);
    expect(state.openEditors.map((editor) => editor.id)).toEqual([id]);
    expect(state.draftBodyFor(id)).toBe('Rename this.');
    expect(state.replyErrorFor(THREAD.id)).toBe(UNCONFIRMED);
    expect(state.replyOpen(THREAD.id, 'diff')).toBe(true);
    expect(state.replyBodyFor(THREAD.id)).toBe('Agreed.');
    expect(state.commentWriteError(commentWriteKey.prComment)).toBe(UNCONFIRMED);
    expect(state.sentComments.prCommentOpen).toBe(true);
    expect(state.sentComments.prCommentBody).toBe('Looks good.');
    expect(state.drafts).toEqual([]);
  });

  it('closes at once on seq 0 and reads the threads again', async () => {
    setBindingMock('PostPRLineComment', async () => ({ seq: 0 }));
    const list = setBindingMock('ListPRReviewThreads', async () => threads);
    const state = await open();
    const before = list.mock.calls.length;
    const id = editorWith(state, added12, 'Rename this.');
    await state.createComment(id, 'Rename this.', 'now');
    expect(state.commentWritePending(commentWriteKey.editor(id))).toBe(false);
    expect(state.openEditors).toEqual([]);
    await settle();
    expect(list.mock.calls.length).toBeGreaterThan(before);
  });

  it('is withheld, with no call, while the diff predates the live head', async () => {
    const post = setBindingMock('PostPRLineComment', async () => ({ seq: 9 }));
    const state = await open();
    frame(2, { headSHA: 'sha-5b' });
    expect(state.prStale).toBe(true);
    expect(state.nowWithheldReason).toBe('The pull request changed since you loaded it. Reload to act on it.');
    const id = editorWith(state, added12, 'Rename this.');
    await state.createComment(id, 'Rename this.', 'now');
    expect(post).not.toHaveBeenCalled();
    expect(state.commentWriteError(commentWriteKey.editor(id))).toBe(state.nowWithheldReason);
    expect(state.draftBodyFor(id)).toBe('Rename this.');
  });

  it('is withheld while the pump is failing', async () => {
    const state = await open();
    applyPRUpdatedEvent({ prKey: KEY, error: 'The forge is not answering.', errorKind: 'transient', seq: 2 });
    flushSync();
    expect(state.nowWithheldReason).toBe('Updates from the forge are failing, so its state is unknown.');
  });
});

describe('a range comment', () => {
  it('spans from the open editor to a shift+clicked line, and drops back to one line', async () => {
    const post = setBindingMock('PostPRLineComment', async () => ({ seq: 3 }));
    const state = await open();
    const id = editorWith(state, context10, 'Both of these.');
    expect(state.extendEditorRange(added12)).toBe(id);
    const [editor] = state.openEditors;
    expect(editor.range).toEqual({ filePath: 'src/a.ts', half: 'new', from: 10, to: 12 });
    // The editor sits after the last line.
    expect(editor.anchor).toMatchObject({ side: 'new', newLine: 12 });
    expect(state.rangeTintsFor('src/a.ts')).toEqual([editor.range]);
    expect(state.draftBodyFor(id)).toBe('Both of these.');

    state.dropEditorRange(id);
    expect(state.openEditors[0].range).toBeUndefined();
    expect(state.openEditors[0].anchor).toMatchObject({ side: 'context', oldLine: 10, newLine: 10 });
    expect(state.rangeTintsFor('src/a.ts')).toEqual([]);

    state.extendEditorRange(added12);
    await state.createComment(id, 'Both of these.', 'now');
    expect(post).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ side: 'new', newLine: 12, startOldLine: 10, startNewLine: 10 }));
  });

  it('refuses a deleted line to an added one and says why on the editor', async () => {
    const state = await open();
    const id = editorWith(state, added11, 'x');
    state.extendEditorRange(deleted11);
    expect(state.openEditors).toMatchObject([{ id, range: undefined, rangeRefusal: 'A range stays on one side of the diff.' }]);
    expect(state.openEditors[0].anchor).toMatchObject({ newLine: 11 });
  });

  it('opens a plain editor when none is open', async () => {
    const state = await open();
    const id = state.extendEditorRange(added11);
    expect(id).not.toBeNull();
    expect(state.openEditors).toMatchObject([{ id, range: undefined }]);
  });

  it('saves a range draft with its first line, which the diff tints', async () => {
    const state = await open();
    const id = editorWith(state, context10, 'Range draft.');
    state.extendEditorRange(added12);
    await state.createComment(id, 'Range draft.');
    expect(drafts.create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({ side: 'new', newLine: 12, startOldLine: 10, startNewLine: 10 }));
    expect(state.rangeTintsFor('src/a.ts')).toEqual([{ filePath: 'src/a.ts', half: 'new', from: 10, to: 12 }]);
  });
});

describe('replies', () => {
  it('posts now and keeps the composer open until the frame lands', async () => {
    const reply = setBindingMock('ReplyToPRThread', async () => ({ seq: 4 }));
    const state = await open();
    state.openReply(THREAD.id, 'diff');
    state.setReplyBody(THREAD.id, 'On it.');
    await state.sendPRThreadReply(THREAD);
    expect(reply).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), 'thread-1', 101, 'On it.');
    expect(state.sendingReply(THREAD.id)).toBe(true);
    expect(state.replyOpen(THREAD.id, 'diff')).toBe(true);
    frame(4);
    expect(state.sendingReply(THREAD.id)).toBe(false);
    expect(state.replyOpen(THREAD.id, 'diff')).toBe(false);
    expect(state.replyBodyFor(THREAD.id)).toBe('');
  });

  it('keeps the text and the sentence when the thread is gone', async () => {
    setBindingMock('ReplyToPRThread', async () => { throw new Error('This thread is gone.'); });
    const state = await open();
    state.openReply(THREAD.id, 'conversation');
    state.setReplyBody(THREAD.id, 'On it.');
    await state.sendPRThreadReply(THREAD);
    expect(state.replyErrorFor(THREAD.id)).toBe('This thread is gone.');
    expect(state.replyBodyFor(THREAD.id)).toBe('On it.');
    expect(state.replyOpen(THREAD.id, 'conversation')).toBe(true);
  });

  it('holds a reply as a draft of its thread; Reply then edits the newest one', async () => {
    const state = await open();
    state.openReply(THREAD.id, 'diff');
    state.setReplyBody(THREAD.id, 'Later.');
    await state.holdPRThreadReply(THREAD);
    expect(drafts.create).toHaveBeenCalledWith(expect.anything(), expect.objectContaining({
      scope: 'pr', side: 'reply', filePath: 'src/a.ts', replyThreadId: 'thread-1', replyRootId: 101, body: 'Later.', commitSha: 'sha-5',
    }));
    expect(state.replyOpen(THREAD.id, 'diff')).toBe(false);
    expect(state.replyBodyFor(THREAD.id)).toBe('');
    expect(state.replyDraftsFor(THREAD.id).map((draft) => draft.body)).toEqual(['Later.']);

    // Recovery can leave a second one: both show, and Reply edits the newest.
    const held = drafts.held.get(5)!;
    held.push({ ...held[0], id: 'c-newer', body: 'Newer.', createdAt: held[0].createdAt + 10 } as DiffReviewComment);
    await state.reload();
    await vi.waitFor(() => expect(state.replyDraftsFor(THREAD.id)).toHaveLength(2));
    state.openReply(THREAD.id, 'conversation');
    expect(state.replyOpen(THREAD.id, 'conversation')).toBe(false);
    expect(state.draftEditBodyFor('c-newer')).toBe('Newer.');
    expect(state.draftEditBodyFor(held[0].id)).toBeNull();
  });

  it('marks a held reply whose thread is gone orphaned and leaves it out of the review', async () => {
    const submit = setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0, posted: true, sentDraftIds: [] }));
    const state = await open();
    state.setReplyBody(THREAD.id, 'Later.');
    await state.holdPRThreadReply(THREAD);
    threads = [];
    frame(2, { threads: [] });
    expect(state.orphanedReplyDrafts.map((draft) => draft.body)).toEqual(['Later.']);
    state.setSummaryBody('Summary.');
    await state.submitPRReview();
    expect((submit.mock.calls[0][1] as { drafts: { ids: string[] } }).drafts.ids).toEqual([]);
  });

  it('sends a held reply to an agent with the thread it answers', async () => {
    const state = await open();
    state.setReplyBody(THREAD.id, 'Fix it like this.');
    await state.holdPRThreadReply(THREAD);
    await state.sendDraftsToThread('agent-thread');
    const options = drafts.send.mock.calls[0][5] as { pr: { comments: unknown[] } };
    expect(options.pr.comments).toEqual([{
      commentId: state.comments[0].id,
      hunkExcerpt: '',
      thread: { path: 'src/a.ts', line: 12, startLine: undefined, comments: [{ author: 'me', body: 'First.' }, { author: 'bob', body: 'A reply.' }] },
    }]);
  });
});

describe('edit and delete of a sent comment', () => {
  it('edits with the raw body, pending until the frame lands', async () => {
    const edit = setBindingMock('EditPRComment', async () => ({ seq: 5 }));
    const state = await open();
    const first = THREAD.comments[0];
    state.sentComments.setEditBody(THREAD, first, first.body);
    expect(state.sentComments.editBodyFor(THREAD, first)).toBe('First.');
    state.sentComments.setEditBody(THREAD, first, 'First, edited.');
    await state.sentComments.saveEdit(THREAD, first);
    expect(edit).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), { threadId: 'thread-1', commentId: 'node-1' }, 'First, edited.');
    const key = commentWriteKey.edit(THREAD.id, first.id);
    expect(state.commentWritePending(key)).toBe(true);
    expect(state.sentComments.editBodyFor(THREAD, first)).toBe('First, edited.');
    frame(5);
    expect(state.commentWritePending(key)).toBe(false);
    expect(state.sentComments.editBodyFor(THREAD, first)).toBeNull();
  });

  it('keeps the edit and the refusal when the forge refuses', async () => {
    setBindingMock('EditPRComment', async () => { throw new Error('You cannot edit this comment.'); });
    const state = await open();
    const first = THREAD.comments[0];
    state.sentComments.setEditBody(THREAD, first, 'Edited.');
    await state.sentComments.saveEdit(THREAD, first);
    expect(state.commentWriteError(commentWriteKey.edit(THREAD.id, first.id))).toBe('You cannot edit this comment.');
    expect(state.sentComments.editBodyFor(THREAD, first)).toBe('Edited.');
    // Cancel closes the edit and its sentence.
    state.sentComments.setEditBody(THREAD, first, null);
    expect(state.commentWriteError(commentWriteKey.edit(THREAD.id, first.id))).toBeNull();
  });

  it('confirms a delete in place, and the confirm stays pending until the comment is gone', async () => {
    const del = setBindingMock('DeletePRComment', async () => ({ seq: 6 }));
    const state = await open();
    const first = THREAD.comments[0];
    state.sentComments.setConfirmDelete(THREAD, first, true);
    expect(state.sentComments.confirmingDelete(THREAD, first)).toBe(true);
    state.sentComments.setConfirmDelete(THREAD, first, false);
    expect(state.sentComments.confirmingDelete(THREAD, first)).toBe(false);
    expect(del).not.toHaveBeenCalled();

    state.sentComments.setConfirmDelete(THREAD, first, true);
    await state.sentComments.deleteComment(THREAD, first);
    expect(del).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), { threadId: 'thread-1', commentId: 'node-1' });
    expect(state.commentWritePending(commentWriteKey.delete(THREAD.id, first.id))).toBe(true);
    expect(state.sentComments.confirmingDelete(THREAD, first)).toBe(true);
    threads = [];
    frame(6, { threads: [] });
    expect(state.sentComments.confirmingDelete(THREAD, first)).toBe(false);
    expect(state.prThreads).toEqual([]);
  });

  it('gives the wait up when the backend restarts', async () => {
    setBindingMock('DeletePRComment', async () => ({ seq: 60 }));
    const state = await open();
    const first = THREAD.comments[0];
    await state.sentComments.deleteComment(THREAD, first);
    expect(state.commentWritePending(commentWriteKey.delete(THREAD.id, first.id))).toBe(true);
    __setTransportHelloForTest({ launchId: 'l2', protocolVersion: 1, capabilities: ['pr-review-drafts.v1', 'pr-comment-actions.v1'], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
    flushSync();
    expect(state.commentWritePending(commentWriteKey.delete(THREAD.id, first.id))).toBe(false);
  });
});

describe('a comment on the PR itself', () => {
  it('posts now and clears its field once the frame lands', async () => {
    const post = setBindingMock('PostPRComment', async () => ({ seq: 8 }));
    const state = await open();
    state.sentComments.setPRCommentOpen(true);
    state.sentComments.setPRCommentBody('Looks good overall.');
    await state.sentComments.postPRComment();
    expect(post).toHaveBeenCalledWith(expect.objectContaining({ Number: 5 }), 'Looks good overall.');
    expect(state.commentWritePending(commentWriteKey.prComment)).toBe(true);
    frame(8);
    expect(state.sentComments.prCommentOpen).toBe(false);
    expect(state.sentComments.prCommentBody).toBe('');
  });

  const POSTED: ReviewThread = {
    id: 'pr-comment-1', path: '', side: 'file', isResolvable: false, isResolved: false, isOutdated: false,
    comments: [comment({ id: 'node-9', body: 'Looks good overall.', createdAt: '2026-01-02T00:00:00Z', databaseID: 109 })],
  };
  const feedIds = (state: ReviewPaneState) => state.conversationFeed.map((item) => item.id);

  it('shows the posted comment in a frozen feed, not behind "new"', async () => {
    setBindingMock('PostPRComment', async () => ({ seq: 8 }));
    const state = await open();
    state.setConversationOpen(true);
    state.freezeConversation();
    expect(feedIds(state)).toEqual([`t:${THREAD.id}`]);
    state.sentComments.setPRCommentBody('Looks good overall.');
    await state.sentComments.postPRComment();
    frame(8, { threads: [THREAD, POSTED] });
    await settle();
    expect(feedIds(state)).toContain(`t:${POSTED.id}`);
    expect(state.conversationNewCount).toBe(0);
  });

  it('shows it after the threads\' re-read when the answer has no seq', async () => {
    setBindingMock('PostPRComment', async () => ({ seq: 0 }));
    const state = await open();
    state.setConversationOpen(true);
    state.freezeConversation();
    threads = [THREAD, POSTED];
    state.sentComments.setPRCommentBody('Looks good overall.');
    await state.sentComments.postPRComment();
    expect(state.sentComments.prCommentOpen).toBe(false);
    await vi.waitFor(() => expect(feedIds(state)).toContain(`t:${POSTED.id}`));
    expect(state.conversationNewCount).toBe(0);
  });
});

describe('editor text survives its row unmounting', () => {
  it('keeps a sent comment\'s open edit', async () => {
    const state = await open();
    const first = THREAD.comments[0];
    state.sentComments.setEditBody(THREAD, first, first.body);
    const view = render(ReviewSentCommentBody, { props: { review: state, thread: THREAD, comment: first } });
    await fireEvent.input(view.container.querySelector('textarea')!, { target: { value: 'Typed.' } });
    view.unmount();
    const again = render(ReviewSentCommentBody, { props: { review: state, thread: THREAD, comment: first } });
    expect(again.container.querySelector('textarea')!.value).toBe('Typed.');
    again.unmount();
  });

  it('keeps the PR comment field\'s text and open state', async () => {
    const state = await open();
    const view = render(ReviewPRCommentField, { props: { review: state } });
    await fireEvent.click(view.getByTestId('review-pr-comment-open'));
    await fireEvent.input(view.container.querySelector('textarea')!, { target: { value: 'Typed.' } });
    view.unmount();
    const again = render(ReviewPRCommentField, { props: { review: state } });
    expect(again.container.querySelector('textarea')!.value).toBe('Typed.');
    again.unmount();
  });

  it('keeps a reply composer\'s and a new comment\'s text', async () => {
    const state = await open();
    state.openReply(THREAD.id, 'diff');
    const footerProps = () => ({
      thread: THREAD, body: state.replyBodyFor(THREAD.id), error: null, sending: false, replying: true, resolving: false, resolveError: null,
      onOpenReply: () => {}, onCloseReply: () => {}, onBodyChange: (body: string) => state.setReplyBody(THREAD.id, body), onSendReply: () => {},
      hold: { primary: 'now' as const, onHold: () => {} },
    });
    const footer = render(ReviewThreadFooter, { props: footerProps() });
    await fireEvent.input(footer.container.querySelector('textarea')!, { target: { value: 'Reply typed.' } });
    footer.unmount();
    const footerAgain = render(ReviewThreadFooter, { props: footerProps() });
    expect(footerAgain.container.querySelector('textarea')!.value).toBe('Reply typed.');
    footerAgain.unmount();

    const id = state.openDraftEditor(context10)!;
    state.extendEditorRange(added12);
    const editorProps = () => ({
      editor: state.openEditors[0], body: state.draftBodyFor(id), onBodyChange: (editorId: string, body: string) => state.setDraftBody(editorId, body),
      onCancel: () => {}, onSubmit: () => {}, post: { primary: 'now' as const, withheld: null },
    });
    const editor = render(ReviewDraftEditor, { props: editorProps() });
    await fireEvent.input(editor.container.querySelector('textarea')!, { target: { value: 'Range typed.' } });
    editor.unmount();
    const editorAgain = render(ReviewDraftEditor, { props: editorProps() });
    expect(editorAgain.container.querySelector('textarea')!.value).toBe('Range typed.');
    expect(editorAgain.getByTestId('review-draft-editor-range').textContent).toContain('Lines 10–12');
    editorAgain.unmount();
  });
});
