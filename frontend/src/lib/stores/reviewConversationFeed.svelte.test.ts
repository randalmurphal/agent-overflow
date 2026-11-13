// The Conversation feed's arrivals and Hide resolved threads, through a
// review pinned to one PR (the same state a thread's companion review
// uses): an arrival joins at its chronological place without a press;
// it counts toward the "N new" chip only while the reader's place is
// held (scrolled, or focus inside); an explicit refresh or reload shows
// everything it read with no chip, whether the arrival came before,
// during or after its read. Hide resolved decides what it hides on the
// toggle, an explicit refresh or reload and a load, never under the
// reader, and a jump shows a hidden thread.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, tick } from 'svelte';
import { resetAppStorageForTest } from './appStorage';
import { __resetReviewPaneStateForTest, type ReviewPaneState } from './reviewPane.svelte';
import { __resetPinnedReviewStatesForTest, reviewStateForPR, reviewSubjectForPR } from './reviewPanePinned.svelte';
import { applyPRUpdatedEvent } from './prReviewStore.svelte';
import { resetComposerDraftRegistryForTest } from './composerDraftRegistry.svelte';
import { resetPanesForTest } from './panes.svelte';
import { __setTransportHelloForTest } from './transportStatus.svelte';
import { HOME_BACKEND } from '../transport/backendKey';
import { prKey, type PRRef } from '../utils/prReference';
import type { PRDetail, ReviewThread } from '../types/models';
import { installPRDraftsFake } from '../../test/helpers/prDraftsFake';
import { setBindingMock, setReviewDiffMock } from '../../test/mocks/bindings-app';
import { resetToLocalPage } from '../../test/helpers/scopes';
import { resetStagedBackends } from '../../test/helpers/backends';
import { insertArrivals, type ConversationFeedEntry } from './reviewConversationFeed.svelte';

const REF: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };
const KEY = prKey(REF);

function detail(): PRDetail {
  return {
    number: 5, title: 'PR 5', body: '', authorLogin: 'alice', state: 'open', draft: false,
    headRefName: 'feature', baseRefName: 'main', baseRefSHA: 'base', headSHA: 'sha-5',
    url: 'https://github.com/owner/repo/pull/5', additions: 1, deletions: 0, changedFiles: 1,
    viewerIsAuthor: false, reviewDecision: '', latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean', readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: true, viewerCanApprove: true, viewerApproved: false },
  };
}

const PATCH = [
  'diff --git a/src/a.ts b/src/a.ts',
  'index 1111111..2222222 100644',
  '--- a/src/a.ts',
  '+++ b/src/a.ts',
  '@@ -1,1 +1,2 @@',
  ' keep',
  '+added',
].join('\n');

// A thread on src/a.ts line 2, started on `day` of January.
function thread(id: string, day: number, overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id, path: 'src/a.ts', line: 2, side: 'right', isResolvable: true, isResolved: false, isOutdated: false,
    comments: [{ id: `c-${id}`, authorLogin: 'bob', body: `About ${id}.`, createdAt: `2026-01-${String(day).padStart(2, '0')}T00:00:00Z`, databaseID: day, viewerCanEdit: false, viewerCanDelete: false }],
    ...overrides,
  };
}

const OLD = thread('t-old', 1);
const MID = thread('t-mid', 3);
let threads: ReviewThread[] = [];

let seq = 1;
function push(next: ReviewThread[]): void {
  threads = next;
  seq += 1;
  applyPRUpdatedEvent({ prKey: KEY, detail: detail(), threads: next, headSHA: 'sha-5', seq });
  flushSync();
}

async function open(initial: ReviewThread[] = [OLD, MID]): Promise<ReviewPaneState> {
  threads = initial;
  const state = reviewStateForPR('tools-1', reviewSubjectForPR({ ownerPaneId: 'tools-1', backend: HOME_BACKEND, projectId: 'project-1', pr: REF }));
  await vi.waitFor(() => {
    expect(state.loading).toBe(false);
    expect(state.prHeadSHA).toBe('sha-5');
    expect(state.prThreads.length).toBe(initial.length);
  });
  state.setConversationOpen(true);
  return state;
}

async function settle(): Promise<void> {
  for (let i = 0; i < 8; i += 1) await tick();
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

const ids = (state: ReviewPaneState) => state.conversationFeed.map((item) => item.id.slice(2));

// A thread re-read the test answers when it chooses.
function deferThreadReads(): { calls: () => number; answer: (next: ReviewThread[]) => void; fail: () => void } {
  const pending: { resolve: (value: ReviewThread[]) => void; reject: (err: Error) => void }[] = [];
  const list = setBindingMock('ListPRReviewThreads', () => new Promise<ReviewThread[]>((resolve, reject) => { pending.push({ resolve, reject }); }));
  return {
    calls: () => list.mock.calls.length,
    answer: (next) => { threads = next; pending.shift()?.resolve(next); },
    fail: () => { pending.shift()?.reject(new Error('The forge did not answer.')); },
  };
}

beforeEach(() => {
  resetAppStorageForTest();
  __resetPinnedReviewStatesForTest();
  __resetReviewPaneStateForTest();
  resetComposerDraftRegistryForTest();
  resetPanesForTest();
  resetToLocalPage();
  __setTransportHelloForTest({ launchId: 'l', protocolVersion: 1, capabilities: ['pr-review-drafts.v1', 'pr-comment-actions.v1'], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
  seq = 1;
  setBindingMock('SubscribePRUpdates', async () => ({ id: 'sub-5', prKey: KEY, detail: detail(), threads, headSHA: 'sha-5', seq: 1 }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => PATCH);
  setBindingMock('ListPRCommits', async () => []);
  setBindingMock('ListPRReviewThreads', async () => threads);
  setBindingMock('GetPRDetail', async () => detail());
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  installPRDraftsFake();
});

afterEach(() => {
  __resetPinnedReviewStatesForTest();
  __resetReviewPaneStateForTest();
  resetStagedBackends();
  resetToLocalPage();
});

describe('insertArrivals', () => {
  const entry = (id: string, timeMs: number): ConversationFeedEntry => ({ timeMs, item: { kind: 'thread', id, thread: thread(id, 1) } });

  it('places each arrival before the first entry it is newer than, and moves nothing', () => {
    // The held order is not chronological here (b was shown above c):
    // the arrival still lands by its neighbours, and b and c keep places.
    const source = [entry('a', 50), entry('c', 40), entry('b', 30), entry('d', 35), entry('e', 10)];
    expect(insertArrivals(['b', 'c'], source)).toEqual(['a', 'd', 'b', 'c', 'e']);
  });

  it('breaks a tie by id, and leaves an entry gone from the source in place', () => {
    const source = [entry('x', 20), entry('m', 20)];
    expect(insertArrivals(['gone', 'x'], source)).toEqual(['gone', 'm', 'x']);
  });
});

describe('a background arrival', () => {
  it('joins at its place with no press; at the top nothing counts as new', async () => {
    const state = await open();
    expect(ids(state)).toEqual(['t-mid', 't-old']);
    push([OLD, MID, thread('t-new', 5), thread('t-between', 2)]);
    expect(ids(state)).toEqual(['t-new', 't-mid', 't-between', 't-old']);
    expect(state.conversationNewCount).toBe(0);
    expect(state.conversationNewestArrivalId).toBeNull();
  });

  it('counts while the reader is scrolled into the feed, and the chip points at the newest', async () => {
    const state = await open();
    state.setOverviewSectionScrollTop('conversation', 80);
    push([OLD, MID, thread('t-between', 2)]);
    expect(ids(state)).toEqual(['t-mid', 't-between', 't-old']);
    expect(state.conversationNewCount).toBe(1);
    expect(state.conversationNewestArrivalId).toBe('t:t-between');
    push([...threads, thread('t-new', 5)]);
    expect(state.conversationNewCount).toBe(2);
    expect(state.conversationNewestArrivalId).toBe('t:t-new');
  });

  it('counts while focus is inside the feed, at its top too', async () => {
    const state = await open();
    const detach = state.attachConversationFocusProbe(() => true);
    push([OLD, MID, thread('t-new', 5)]);
    expect(state.conversationNewCount).toBe(1);
    // Focus left (or the section unmounted): the next arrival just shows.
    detach();
    push([...threads, thread('t-newer', 6)]);
    expect(ids(state)).toEqual(['t-newer', 't-new', 't-mid', 't-old']);
    expect(state.conversationNewCount).toBe(1);
  });

  it('the chip clears when its newest arrival was seen, and on its press, which stages the scroll', async () => {
    const state = await open();
    state.setOverviewSectionScrollTop('conversation', 80);
    push([OLD, MID, thread('t-new', 5)]);
    state.acknowledgeConversationArrivals();
    expect(state.conversationNewCount).toBe(0);
    expect(state.pendingConversationArrivalId).toBeNull();

    push([...threads, thread('t-newer', 6), thread('t-between', 2)]);
    expect(state.conversationNewCount).toBe(2);
    state.showNewestConversationArrival();
    expect(state.conversationNewCount).toBe(0);
    expect(state.pendingConversationArrivalId).toBe('t:t-newer');
    state.consumePendingConversationArrival();
    expect(state.pendingConversationArrivalId).toBeNull();
  });

  it('never moves or drops a card a remote update changes; a reply grows it in place', async () => {
    const state = await open();
    const replied = { ...MID, isResolved: true, comments: [...MID.comments, { ...MID.comments[0], id: 'c-reply', body: 'Done.', createdAt: '2026-01-09T00:00:00Z' }] };
    push([OLD, replied]);
    expect(ids(state)).toEqual(['t-mid', 't-old']);
    const card = state.conversationFeed[0];
    expect(card.kind === 'thread' && card.thread.comments.length).toBe(2);
    expect(state.conversationNewCount).toBe(0);
  });
});

describe('an explicit refresh shows everything it read, with no chip', () => {
  it('for an arrival before, during and after its read', async () => {
    const state = await open();
    state.setOverviewSectionScrollTop('conversation', 80);
    // Before: a background arrival while scrolled counts.
    push([OLD, MID, thread('t-before', 4)]);
    expect(state.conversationNewCount).toBe(1);

    const reads = deferThreadReads();
    const refresh = state.refreshPRThreads({ explicit: true });
    await vi.waitFor(() => expect(reads.calls()).toBe(1));
    // During: a frame that lands while the read runs joins with no chip.
    push([...threads, thread('t-during', 6)]);
    expect(ids(state)).toContain('t-during');
    expect(state.conversationNewCount).toBe(0);
    reads.answer([...threads, thread('t-read', 7)]);
    await refresh;
    await settle();
    expect(ids(state)).toEqual(['t-read', 't-during', 't-before', 't-mid', 't-old']);
    expect(state.conversationNewCount).toBe(0);

    // After: the next background arrival counts again.
    push([...threads, thread('t-after', 8)]);
    expect(state.conversationNewCount).toBe(1);
  });

  it('a refresh that is not the reader\'s leaves arrivals counted', async () => {
    const state = await open();
    state.setOverviewSectionScrollTop('conversation', 80);
    threads = [OLD, MID, thread('t-read', 7)];
    await state.refreshPRThreads();
    await settle();
    expect(ids(state)).toContain('t-read');
    expect(state.conversationNewCount).toBe(1);
  });

  it('a failed one leaves the chip as it was', async () => {
    const state = await open();
    state.setOverviewSectionScrollTop('conversation', 80);
    push([OLD, MID, thread('t-before', 4)]);
    const reads = deferThreadReads();
    const refresh = state.refreshPRThreads({ explicit: true });
    await vi.waitFor(() => expect(reads.calls()).toBe(1));
    reads.fail();
    await refresh;
    await settle();
    expect(state.conversationNewCount).toBe(1);
  });

  it('the reader\'s reload re-reads the threads and shows them; a reload nobody asked for does not read', async () => {
    const state = await open();
    state.setOverviewSectionScrollTop('conversation', 80);
    const list = setBindingMock('ListPRReviewThreads', async () => threads);
    await state.reload();
    await settle();
    expect(list).not.toHaveBeenCalled();

    threads = [OLD, MID, thread('t-read', 7)];
    await state.reload({ userInitiated: true });
    await vi.waitFor(() => expect(ids(state)).toContain('t-read'));
    await settle();
    expect(list).toHaveBeenCalledTimes(1);
    expect(state.conversationNewCount).toBe(0);
  });
});

describe('Hide resolved threads', () => {
  const DONE = thread('t-done', 2, { isResolved: true });

  async function openWithResolved(): Promise<ReviewPaneState> {
    const state = await open([OLD, DONE, MID]);
    expect(ids(state)).toEqual(['t-mid', 't-done', 't-old']);
    return state;
  }

  it('is off by default, and on hides the resolved from the feed, its count and the diff\'s rows', async () => {
    const state = await openWithResolved();
    expect(state.hideResolved).toBe(false);
    expect(state.hiddenResolvedCount).toBe(0);
    expect(state.shownPRThreads).toBe(state.prThreads);

    state.setHideResolved(true);
    expect(ids(state)).toEqual(['t-mid', 't-old']);
    expect(state.conversationCount).toBe(2);
    expect(state.hiddenResolvedCount).toBe(1);
    expect(state.shownPRThreads.map((each) => each.id)).toEqual(['t-old', 't-mid']);
    // The stepper and the tree's badges count unresolved threads: unchanged.
    expect(state.unresolvedThreads.map((each) => each.id)).toEqual(['t-mid', 't-old']);
    expect(state.prThreads).toHaveLength(3);
  });

  it('off shows them again in their places, and toggled twice hides them again', async () => {
    const state = await openWithResolved();
    state.setHideResolved(true);
    state.setHideResolved(false);
    expect(ids(state)).toEqual(['t-mid', 't-done', 't-old']);
    expect(state.conversationCount).toBe(3);
    expect(state.hiddenResolvedCount).toBe(0);
    expect(state.shownPRThreads).toBe(state.prThreads);
    state.setHideResolved(true);
    state.setHideResolved(false);
    state.setHideResolved(true);
    expect(ids(state)).toEqual(['t-mid', 't-old']);
  });

  it('a thread resolved while on stays, shown resolved, until the next explicit refresh', async () => {
    const state = await openWithResolved();
    state.setHideResolved(true);
    const resolvedMid = { ...MID, isResolved: true };
    push([OLD, DONE, resolvedMid]);
    expect(ids(state)).toEqual(['t-mid', 't-old']);
    const card = state.conversationFeed[0];
    expect(card.kind === 'thread' && card.thread.isResolved).toBe(true);
    expect(state.shownPRThreads.map((each) => each.id)).toContain('t-mid');

    await state.refreshPRThreads({ explicit: true });
    await settle();
    expect(ids(state)).toEqual(['t-old']);
    expect(state.hiddenResolvedCount).toBe(2);
  });

  it('a reload decides again', async () => {
    const state = await openWithResolved();
    state.setHideResolved(true);
    push([OLD, DONE, { ...MID, isResolved: true }]);
    expect(ids(state)).toContain('t-mid');
    await state.reload({ userInitiated: true });
    await settle();
    expect(ids(state)).toEqual(['t-old']);
  });

  it('a hidden thread unresolved since shows again, and an arrival shows whatever its state', async () => {
    const state = await openWithResolved();
    state.setHideResolved(true);
    push([OLD, { ...DONE, isResolved: false }, MID, thread('t-late', 5, { isResolved: true })]);
    expect(ids(state)).toEqual(['t-late', 't-mid', 't-done', 't-old']);
    expect(state.hiddenResolvedCount).toBe(0);
  });

  it('a hidden thread\'s range tints no rows', async () => {
    const ranged = thread('t-range', 2, { isResolved: true, startLine: 1, line: 2 });
    const state = await open([OLD, ranged]);
    state.togglePRThread('t-range');
    expect(state.rangeTintsFor('src/a.ts')).toHaveLength(1);
    state.setHideResolved(true);
    expect(state.rangeTintsFor('src/a.ts')).toHaveLength(0);
  });

  it('never hides a thread that holds the reader\'s draft reply', async () => {
    const state = await openWithResolved();
    state.setReplyBody(DONE.id, 'One more thought.');
    await state.holdPRThreadReply(DONE);
    expect(state.drafts.map((draft) => draft.replyThreadId)).toEqual([DONE.id]);
    state.setHideResolved(true);
    expect(ids(state)).toEqual(['t-mid', 't-done', 't-old']);
    expect(state.shownPRThreads.map((each) => each.id)).toContain(DONE.id);
    expect(state.hiddenResolvedCount).toBe(0);
    // The draft gone, it stays until the next decision, which hides it.
    await state.deleteComment(state.drafts[0].id);
    expect(ids(state)).toEqual(['t-mid', 't-done', 't-old']);
    state.setHideResolved(false);
    state.setHideResolved(true);
    expect(ids(state)).toEqual(['t-mid', 't-old']);
  });

  it('a jump to a hidden thread shows it, in the feed and the diff', async () => {
    const state = await openWithResolved();
    state.setHideResolved(true);
    state.openConversationAt('t-done');
    expect(ids(state)).toEqual(['t-mid', 't-done', 't-old']);
    expect(state.pendingConversationThreadId).toBe('t-done');
    expect(state.hiddenResolvedCount).toBe(0);

    state.setHideResolved(false);
    state.setHideResolved(true);
    expect(state.shownPRThreads.map((each) => each.id)).not.toContain('t-done');
    state.jumpToDiffThread(DONE);
    expect(state.shownPRThreads.map((each) => each.id)).toContain('t-done');
    expect(state.pendingJumpRowKey).toBe('pt:t-done');
  });
});
