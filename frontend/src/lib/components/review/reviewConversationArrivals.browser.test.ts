// The Conversation feed's reading place in real Chromium, on a review
// pinned to a PR: an arrival above a scrolled reader, or a reply that
// grows a card above the viewport, leaves the card at the top of the
// section's viewport where it was, to the pixel, and the "N new" chip
// points up at the arrival and goes once it is scrolled to. A reader at
// the top with focus elsewhere sees the arrival push the cards down; with
// focus inside the feed the place is held there too.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import '../../../app.css';
import ReviewPane from './ReviewPane.svelte';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { __resetPinnedReviewStatesForTest, disposePRReviewStates, reviewStateForPR, reviewSubjectForPR } from '../../stores/reviewPanePinned.svelte';
import { applyPRUpdatedEvent } from '../../stores/prReviewStore.svelte';
import { appStorageGet, resetAppStorageForTest } from '../../stores/appStorage';
import { HIDE_RESOLVED_KEY } from '../../stores/reviewHideResolved.svelte';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { __setTransportHelloForTest, __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { resetForTest as resetDiffReviewComments } from '../../stores/diffReviewComments.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { PRDetail, ReviewThread } from '../../types/models';
import type { PRRef } from '../../utils/prReference';
import { installPRDraftsFake } from '../../../test/helpers/prDraftsFake';
import { setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';

const OWNER = 'tools-arrivals';
const PR: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };
const KEY = 'github:owner/repo:5';

function detail(): PRDetail {
  return {
    number: 5, title: 'Arrivals', body: '', authorLogin: 'octocat', state: 'open', draft: false,
    headRefName: 'feature', baseRefName: 'main', baseRefSHA: 'base-main', headSHA: 'sha-a',
    url: 'https://github.com/owner/repo/pull/5', additions: 1, deletions: 0, changedFiles: 1,
    viewerIsAuthor: false, reviewDecision: '', latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean', readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: false, viewerCanApprove: false, viewerApproved: false },
  };
}

// A comment on the PR itself, posted on `day` of January.
function note(id: string, day: number, replies = 0): ReviewThread {
  const at = (hour: number) => `2026-01-${String(day).padStart(2, '0')}T${String(hour).padStart(2, '0')}:00:00Z`;
  return {
    id, path: '', side: 'file', isResolvable: false, isResolved: false, isOutdated: false,
    comments: [
      { id: `c-${id}`, authorLogin: 'bob', body: `Note ${id}.\n\nA second paragraph so the card has some height.`, createdAt: at(0), databaseID: day, viewerCanEdit: false, viewerCanDelete: false },
      ...Array.from({ length: replies }, (_, index) => ({ id: `c-${id}-r${index}`, authorLogin: 'amy', body: `Reply ${index} on ${id}.`, createdAt: at(index + 1), databaseID: day * 100 + index, viewerCanEdit: false, viewerCanDelete: false })),
    ],
  };
}

const INITIAL = Array.from({ length: 12 }, (_, index) => note(`n${index + 1}`, index + 1));
let threads: ReviewThread[] = INITIAL;
let seq = 1;

const HELLO = { launchId: 'l', protocolVersion: 1, capabilities: ['pr-review-drafts.v1', 'pr-comment-actions.v1', 'pr-suggestions-reactions.v1'], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 };

let app: ReturnType<typeof mount> | undefined;
let target: HTMLDivElement;

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  __resetPinnedReviewStatesForTest();
  resetPanesForTest();
  resetDiffReviewComments();
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  // Reactions show on a backend that serves them.
  __setTransportHelloForTest(HELLO);
  threads = INITIAL;
  seq = 1;
  target = document.createElement('div');
  target.style.cssText = 'position:fixed;top:0;left:0;width:1100px;height:900px;display:flex;flex-direction:column';
  document.body.append(target);
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  setBindingMock('HighlightPatchWithContext', async () => ({ lines: [] }));
  setBindingMock('SubscribePRUpdates', async () => ({ id: 'sub-1', prKey: KEY, detail: detail(), threads, headSHA: 'sha-a', seq: 1 }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => 'diff --git a/a.ts b/a.ts\nnew file mode 100644\n--- /dev/null\n+++ b/a.ts\n@@ -0,0 +1,1 @@\n+one\n');
  setBindingMock('ListPRCommits', async () => []);
  setBindingMock('ListPRReviewThreads', async () => threads);
  setBindingMock('GetPRDetail', async () => detail());
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  setBindingMock('ListPRDiffReviewComments', async () => []);
  installPRDraftsFake();
});

afterEach(async () => {
  if (app) await unmount(app);
  app = undefined;
  target.remove();
  disposePRReviewStates(OWNER);
  __resetReviewPaneStateForTest();
});

async function frames(count = 2): Promise<void> {
  for (let i = 0; i < count; i += 1) await new Promise((done) => requestAnimationFrame(() => done(null)));
}

function testid(id: string): HTMLElement | null {
  return target.querySelector<HTMLElement>(`[data-testid="${id}"]`);
}

function push(next: ReviewThread[]): void {
  threads = next;
  seq += 1;
  applyPRUpdatedEvent({ prKey: KEY, detail: detail(), threads: next, headSHA: 'sha-a', seq });
  flushSync();
}

function entry(id: string): HTMLElement {
  const el = target.querySelector<HTMLElement>(`[data-feed-id="t:${id}"]`);
  expect(el, id).not.toBeNull();
  return el!;
}

function scroller(): HTMLElement {
  return testid('review-conversation')!.parentElement!;
}

// Where `id`'s card sits relative to the section's viewport.
function offsetOf(id: string): number {
  return entry(id).getBoundingClientRect().top - scroller().getBoundingClientRect().top;
}

async function openConversation(): Promise<void> {
  app = mount(ReviewPane, { target, props: { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } } });
  const section = await vi.waitFor(() => {
    const found = testid('review-pr-conversation');
    expect(found).not.toBeNull();
    return found!;
  }, { timeout: 5_000 });
  section.querySelector<HTMLButtonElement>('button[aria-expanded]')!.click();
  await vi.waitFor(() => expect(target.querySelectorAll('[data-feed-id]').length).toBe(INITIAL.length));
  await frames(3);
  // The feed overflows the section, or there is no place to hold.
  expect(scroller().scrollHeight).toBeGreaterThan(scroller().clientHeight + 200);
}

// Scrolled so a card in the middle sits part way past the viewport's top.
async function scrollIntoFeed(): Promise<string> {
  scroller().scrollTop = entry('n8').offsetTop + 17;
  await frames(2);
  expect(scroller().scrollTop).toBeGreaterThan(0);
  return 'n8';
}

describe('a reader scrolled into the Conversation feed', () => {
  it('keeps the top card where it was to the pixel when an arrival joins above, and the chip points to it', async () => {
    await openConversation();
    const top = await scrollIntoFeed();
    const before = offsetOf(top);

    push([...threads, note('n13', 13)]);
    await frames(3);
    expect(Math.abs(offsetOf(top) - before)).toBeLessThanOrEqual(1);
    // The arrival is in the feed, above, and the chip says so.
    expect(entry('n13').getBoundingClientRect().bottom).toBeLessThanOrEqual(scroller().getBoundingClientRect().top);
    const chip = testid('review-conversation-new')!;
    expect(chip.textContent).toContain('1 new');
    expect(chip.querySelector('.lucide-arrow-up')).not.toBeNull();

    // The press brings the arrival into view, and the chip goes.
    chip.click();
    await frames(3);
    expect(Math.abs(offsetOf('n13'))).toBeLessThanOrEqual(1);
    expect(testid('review-conversation-new')).toBeNull();
  });

  it('keeps the top card where it was when scrolled to the end of the feed', async () => {
    await openConversation();
    scroller().scrollTop = scroller().scrollHeight;
    await frames(2);
    const top = [...target.querySelectorAll<HTMLElement>('[data-feed-id]')].find((el) => el.getBoundingClientRect().bottom > scroller().getBoundingClientRect().top)!.dataset.feedId!.slice(2);
    const before = offsetOf(top);
    push([...threads, note('n13', 13)]);
    // The arrival lays out taller a moment later (its body rendering),
    // before the scroll the hold made has dispatched its event.
    await new Promise((done) => setTimeout(done, 0));
    entry('n13').style.paddingBottom = '21px';
    await frames(3);
    expect(Math.abs(offsetOf(top) - before)).toBeLessThanOrEqual(1);
    expect(testid('review-conversation-new')!.textContent).toContain('1 new');
  });

  it('keeps the top card where it was when a reply grows a card above it', async () => {
    await openConversation();
    const top = await scrollIntoFeed();
    const before = offsetOf(top);
    const heightBefore = entry('n12').getBoundingClientRect().height;

    push(threads.map((each) => (each.id === 'n12' ? note('n12', 12, 3) : each)));
    await frames(3);
    expect(entry('n12').getBoundingClientRect().height).toBeGreaterThan(heightBefore);
    expect(Math.abs(offsetOf(top) - before)).toBeLessThanOrEqual(1);
    expect(testid('review-conversation-new')).toBeNull();
  });

  it('keeps the top card where it was when a reaction grows a card above it', async () => {
    await openConversation();
    const top = await scrollIntoFeed();
    const before = offsetOf(top);
    const heightBefore = entry('n12').getBoundingClientRect().height;
    push(threads.map((each) => (each.id === 'n12'
      ? { ...each, comments: [{ ...each.comments[0], reactions: [{ name: 'thumbsup', count: 2, viewerReacted: false }] }] }
      : each)));
    await frames(3);
    expect(entry('n12').getBoundingClientRect().height).toBeGreaterThan(heightBefore);
    expect(Math.abs(offsetOf(top) - before)).toBeLessThanOrEqual(1);
  });

  it('keeps the top card where it was when the reactions notice shows above the feed', async () => {
    // The forge's reactions read failed, on a backend that does not serve
    // reactions yet: no notice. The backend gains them (a reconnect to a
    // newer one) with the feed unchanged, and the notice shows.
    __setTransportHelloForTest({ ...HELLO, capabilities: ['pr-review-drafts.v1', 'pr-comment-actions.v1'] });
    await openConversation();
    seq += 1;
    applyPRUpdatedEvent({ prKey: KEY, detail: detail(), threads, headSHA: 'sha-a', seq, reactionsUnavailable: 'GitHub did not answer the reactions.' });
    flushSync();
    await frames(2);
    expect(testid('review-reactions-unavailable')).toBeNull();
    const top = await scrollIntoFeed();
    const before = offsetOf(top);
    __setTransportHelloForTest(HELLO);
    flushSync();
    await frames(3);
    expect(testid('review-reactions-unavailable')).not.toBeNull();
    expect(Math.abs(offsetOf(top) - before)).toBeLessThanOrEqual(1);
  });

  it('loses the chip once the arrival is scrolled into view', async () => {
    await openConversation();
    await scrollIntoFeed();
    push([...threads, note('n13', 13)]);
    await frames(3);
    expect(testid('review-conversation-new')).not.toBeNull();
    scroller().scrollTop = 0;
    await frames(3);
    expect(testid('review-conversation-new')).toBeNull();
  });
});

describe('a reader at the top of the feed', () => {
  it('sees an arrival push the cards down, with no chip', async () => {
    await openConversation();
    expect(scroller().scrollTop).toBe(0);
    const before = offsetOf('n12');
    push([...threads, note('n13', 13)]);
    await frames(3);
    expect(scroller().scrollTop).toBe(0);
    expect(offsetOf('n13')).toBeLessThan(before + 1);
    expect(offsetOf('n12')).toBeGreaterThan(before + 20);
    expect(testid('review-conversation-new')).toBeNull();
  });

  it('with focus inside the feed keeps the top card where it was', async () => {
    await openConversation();
    // Focus on a card, as a reader's click or Tab would put it.
    entry('n12').tabIndex = -1;
    entry('n12').focus();
    expect(testid('review-conversation')!.contains(document.activeElement)).toBe(true);
    const before = offsetOf('n12');
    push([...threads, note('n13', 13)]);
    await frames(3);
    expect(Math.abs(offsetOf('n12') - before)).toBeLessThanOrEqual(1);
    expect(testid('review-conversation-new')!.textContent).toContain('1 new');
  });
});

describe('Hide resolved threads', () => {
  // Two threads on the diff's one line, one resolved.
  function lineThread(id: string, day: number, resolved: boolean): ReviewThread {
    return {
      id, path: 'a.ts', line: 1, side: 'right', isResolvable: true, isResolved: resolved, isOutdated: false,
      comments: [{ id: `c-${id}`, authorLogin: 'bob', body: `Thread ${id}.`, createdAt: `2026-01-0${day}T00:00:00Z`, databaseID: day, viewerCanEdit: false, viewerCanDelete: false }],
    };
  }

  async function openWithThreads(): Promise<void> {
    threads = [lineThread('open', 2, false), lineThread('done', 1, true)];
    app = mount(ReviewPane, { target, props: { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } } });
    const section = await vi.waitFor(() => {
      const found = testid('review-pr-conversation');
      expect(found).not.toBeNull();
      return found!;
    }, { timeout: 5_000 });
    section.querySelector<HTMLButtonElement>('button[aria-expanded]')!.click();
    await vi.waitFor(() => expect(target.querySelectorAll('[data-testid="review-pr-thread"]').length).toBe(2));
  }

  async function toggle(): Promise<void> {
    testid('review-view-options')!.click();
    const item = await vi.waitFor(() => {
      const found = document.querySelector<HTMLElement>('[data-testid="review-hide-resolved-toggle"]');
      expect(found).not.toBeNull();
      return found!;
    });
    item.click();
    await frames(2);
  }

  it('hides the resolved thread from the feed and the diff, says so, and the note turns it off', async () => {
    await openWithThreads();
    expect(target.querySelectorAll('[data-testid="review-conversation-thread"]').length).toBe(2);
    expect(testid('review-conversation-hidden-resolved')).toBeNull();

    await toggle();
    await vi.waitFor(() => expect(target.querySelectorAll('[data-testid="review-pr-thread"]').length).toBe(1));
    expect([...target.querySelectorAll('[data-testid="review-conversation-thread"]')].map((el) => el.getAttribute('data-thread-id'))).toEqual(['open']);
    expect(testid('review-pr-conversation')!.textContent).toContain('1');
    const note = testid('review-conversation-hidden-resolved')!;
    expect(note.textContent).toContain('1 resolved hidden');
    // The note sits in the card column, clear of the avatar rail.
    const card = target.querySelector('[data-testid="review-conversation-thread"]')!.children[1] as HTMLElement;
    expect(note.getBoundingClientRect().left).toBe(card.getBoundingClientRect().left);

    expect(appStorageGet(HIDE_RESOLVED_KEY, HOME_BACKEND)).toBe('1');
    note.click();
    await vi.waitFor(() => expect(target.querySelectorAll('[data-testid="review-pr-thread"]').length).toBe(2));
    // The line turns the remembered preference off, as the menu does.
    expect(appStorageGet(HIDE_RESOLVED_KEY, HOME_BACKEND)).toBe('0');
    expect(target.querySelectorAll('[data-testid="review-conversation-thread"]').length).toBe(2);
    expect(testid('review-conversation-hidden-resolved')).toBeNull();
  });

  it('keeps a thread resolved while on where it is until the next reload', async () => {
    await openWithThreads();
    await toggle();
    await vi.waitFor(() => expect(target.querySelectorAll('[data-testid="review-pr-thread"]').length).toBe(1));
    push([lineThread('open', 2, true), lineThread('done', 1, true)]);
    await frames(2);
    expect(target.querySelectorAll('[data-testid="review-pr-thread"]').length).toBe(1);
    expect(target.querySelector('[data-testid="review-conversation-thread"][data-thread-id="open"]')).not.toBeNull();

    testid('review-reload')!.click();
    await vi.waitFor(() => expect(target.querySelectorAll('[data-testid="review-pr-thread"]').length).toBe(0));
    expect(testid('review-conversation-hidden-resolved')!.textContent).toContain('2 resolved hidden');
  });
});

describe('a failed thread read', () => {
  it('shows its own banner, kept through a diff reload, and its Retry reads again', async () => {
    await openConversation();
    setBindingMock('ListPRReviewThreads', async () => { throw new Error('The threads did not load.'); });
    testid('review-refresh-comments')!.click();
    const banner = await vi.waitFor(() => {
      const found = testid('review-threads-error');
      expect(found).not.toBeNull();
      return found!;
    });
    expect(banner.textContent).toContain('The threads did not load.');
    expect(testid('review-error')).toBeNull();

    // A diff load nobody asked for succeeds; the threads' failure stays.
    setBindingMock('ListPRReviewThreads', async () => threads);
    await reviewStateForPR(OWNER, reviewSubjectForPR({ ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR })).reload();
    await frames(2);
    expect(testid('review-threads-error')).not.toBeNull();

    testid('review-threads-error-retry')!.click();
    await vi.waitFor(() => expect(testid('review-threads-error')).toBeNull());
  });
});
