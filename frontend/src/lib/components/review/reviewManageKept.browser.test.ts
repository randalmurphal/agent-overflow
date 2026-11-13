// The PR management controls in real Chromium, on a review pinned to a
// PR: the overview is a virtualized row, so scrolling it out of the held
// range unmounts the Merge and Details sections. The merge method picked
// and the edited commit message survive that unmount together, and a
// merge sends the method with its message; an open picker's selection
// survives it without being written, and is written once when the picker
// closes. A held Enter (real key repeats) on the title bar's `⋯` opens
// the menu and never activates the item focus moved to, and on a Details
// Edit opens its picker and leaves it open.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount, tick, unmount } from 'svelte';
import { userEvent } from 'vitest/browser';
import '../../../app.css';
import ReviewPane from './ReviewPane.svelte';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { __resetPinnedReviewStatesForTest, disposePRReviewStates } from '../../stores/reviewPanePinned.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { resetForTest as resetDiffReviewComments } from '../../stores/diffReviewComments.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { PRDetail, PRManageCan } from '../../types/models';
import type { PRRef } from '../../utils/prReference';
import { installPRDraftsFake } from '../../../test/helpers/prDraftsFake';
import { setBindingMock, setReviewDiffMock, commitList } from '../../../test/mocks/bindings-app';

const LINES = 3000;
const OWNER = 'tools-manage';
const PR: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };

const CAN: PRManageCan = {
  editTitle: true, editDescription: true, reviewers: true, assignees: true, labels: true, close: true, reopen: false,
  convertToDraft: true, rerequest: true, dismiss: false, bypass: false, reviewerTeams: false,
};

function patch(): string {
  const out = ['diff --git a/src/long.ts b/src/long.ts', 'new file mode 100644', '--- /dev/null', '+++ b/src/long.ts', `@@ -0,0 +1,${LINES} @@`];
  for (let line = 1; line <= LINES; line += 1) out.push(`+line ${line}`);
  return out.join('\n') + '\n';
}

function detail(): PRDetail {
  return {
    number: 5, title: 'Long', body: 'The body.', authorLogin: 'octocat', state: 'open', draft: false,
    headRefName: 'feature', baseRefName: 'main', baseRefSHA: 'base-main', headSHA: 'sha-a',
    url: 'https://github.com/owner/repo/pull/5', additions: LINES, deletions: 0, changedFiles: 1,
    viewerIsAuthor: false, reviewDecision: '', latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean', readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge', 'squash'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: true, viewerCanApprove: true, viewerApproved: false },
    manage: {
      reviewers: [{ login: 'alee', name: 'Al Lee', state: 'PENDING' }],
      assignees: [],
      labels: [],
      can: CAN,
    },
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
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  target = document.createElement('div');
  target.style.cssText = 'position:fixed;top:0;left:0;width:1100px;height:700px;display:flex;flex-direction:column';
  document.body.append(target);
  setBindingMock('HighlightSchemaVersion', async () => 'test-schema');
  setBindingMock('HighlightClassNames', async () => ['none']);
  setBindingMock('HighlightPatch', async () => ({ lines: [] }));
  setBindingMock('HighlightPatchWithContext', async () => ({ lines: [] }));
  setBindingMock('SubscribePRUpdates', async () => ({ id: 'sub-1', prKey: 'github:owner/repo:5', detail: detail(), threads: [], headSHA: 'sha-a', seq: 1 }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => patch());
  setBindingMock('ListPRCommits', async () => commitList([]));
  setBindingMock('ListPRReviewThreads', async () => []);
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

function q<T extends Element = HTMLElement>(selector: string): T | null {
  return target.querySelector<T>(selector);
}

function testid<T extends Element = HTMLElement>(id: string): T | null {
  return q<T>(`[data-testid="${id}"]`);
}

async function frames(count = 2): Promise<void> {
  for (let i = 0; i < count; i += 1) await new Promise((done) => requestAnimationFrame(() => done(null)));
}

async function present<T extends Element = HTMLElement>(id: string): Promise<T> {
  return vi.waitFor(() => {
    const found = document.querySelector<T>(`[data-testid="${id}"]`);
    expect(found, id).not.toBeNull();
    return found!;
  }, { timeout: 5_000 });
}

async function mountPinned(): Promise<void> {
  app = mount(ReviewPane, { target, props: { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } } });
  await present('review-pr-details');
  await present('review-line-block');
}

// Far enough down that the overview leaves the held range, and back.
async function scrollAwayAndBack(): Promise<void> {
  const scroller = () => testid('review-scroll')!;
  scroller().scrollTop = scroller().scrollHeight;
  await vi.waitFor(async () => {
    await frames();
    expect(testid('review-overview')).toBeNull();
    scroller().scrollTop = scroller().scrollHeight;
  });
  scroller().scrollTop = 0;
  await vi.waitFor(async () => {
    await frames();
    expect(testid('review-overview')).not.toBeNull();
    scroller().scrollTop = 0;
  });
  await tick();
}

function type(field: HTMLInputElement, text: string): void {
  field.value = text;
  field.dispatchEvent(new Event('input', { bubbles: true }));
}

async function openDetails(): Promise<void> {
  const section = await present('review-pr-details');
  if (section.getAttribute('data-open') !== 'true') section.querySelector<HTMLButtonElement>('button[aria-expanded]')!.click();
  await vi.waitFor(() => expect(testid('review-pr-details')!.getAttribute('data-open')).toBe('true'));
}

describe('the management controls across an overview unmount', () => {
  it('keep the picked method with its edited message, and merge with both', async () => {
    setBindingMock('GetPRMergeMessage', async (_pr: unknown, method: string) => ({ title: `Default ${method}`, body: '' }));
    const merge = setBindingMock('MergePR', async () => ({ seq: 0, outcome: { merged: true, autoMerge: false } }));
    await mountPinned();
    (await present('review-merge-trigger')).click();
    (await present('review-merge-method-squash')).click();
    // A method pick may leave the menu open; open it only if it closed.
    await frames();
    if (!document.querySelector('[data-testid="review-merge-menu"]')) (await present('review-merge-trigger')).click();
    (await present('review-merge-edit-message')).click();
    const title = await present<HTMLInputElement>('review-merge-message-title');
    await vi.waitFor(() => expect(title.value).toBe('Default squash'));
    type(title, 'Squash title');

    await scrollAwayAndBack();
    await vi.waitFor(() => expect(testid<HTMLInputElement>('review-merge-message-title')?.value).toBe('Squash title'));
    (await present('review-merge-trigger')).click();
    (await present('review-merge-now')).click();
    await vi.waitFor(() => expect(merge).toHaveBeenCalledTimes(1));
    expect(merge.mock.calls[0][1]).toEqual(expect.objectContaining({ method: 'squash', commitMessage: { title: 'Squash title', body: '' } }));
  });

  it("keep an open picker's selection unwritten, and write it once on close", async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [{ login: 'jdoe', name: 'Jane Doe' }], labels: [], more: false }));
    const set = setBindingMock('SetPRReviewers', async () => ({ seq: 0 }));
    await mountPinned();
    await openDetails();
    (await present('review-details-edit-reviewers')).click();
    const option = await vi.waitFor(() => {
      const found = document.querySelector<HTMLElement>('[data-testid="review-picker-option"][data-key="u:jdoe"]');
      expect(found).not.toBeNull();
      return found!;
    });
    option.click();
    await vi.waitFor(() => expect(document.querySelector('[data-key="u:jdoe"]')?.getAttribute('data-checked')).toBe('true'));

    await scrollAwayAndBack();
    expect(set).not.toHaveBeenCalled();
    await vi.waitFor(() => expect(document.querySelector('[data-testid="review-picker-option"][data-key="u:jdoe"]')?.getAttribute('data-checked')).toBe('true'));
    (await present('review-picker-search')).focus();
    await userEvent.keyboard('{Escape}');
    await vi.waitFor(() => expect(set).toHaveBeenCalledTimes(1));
    expect(set.mock.calls[0][1]).toEqual({ add: [{ login: 'jdoe', name: 'Jane Doe' }], remove: [] });
  });
});

describe('a held Enter', () => {
  it('opens the state menu and never activates the item focus moved to', async () => {
    const setState = setBindingMock('SetPRState', async () => ({ seq: 0 }));
    await mountPinned();
    (await present('review-pr-state-menu')).focus();
    await userEvent.keyboard('{Enter>6}');
    await present('review-pr-state-items');
    await frames(4);
    expect(setState).not.toHaveBeenCalled();
  });

  it('opens a Details picker and leaves it open', async () => {
    setBindingMock('SearchPRCandidates', async () => ({ people: [], labels: [], more: false }));
    const set = setBindingMock('SetPRReviewers', async () => ({ seq: 0 }));
    await mountPinned();
    await openDetails();
    (await present('review-details-edit-reviewers')).focus();
    await userEvent.keyboard('{Enter>6}');
    await frames(4);
    expect(document.querySelector('[data-testid="review-picker"]')).not.toBeNull();
    expect(set).not.toHaveBeenCalled();
  });
});
