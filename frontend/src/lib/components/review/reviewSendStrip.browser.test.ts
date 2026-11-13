// The send strip holds what the next PR review sends. In real Chromium, a
// review the backend refused because its drafts were already sent keeps
// its summary and verdict in the strip, with no draft left, and the
// strip's Send posts them; once the review posts, the strip goes. A review
// the forge did not confirm keeps the strip whole and says it may already be
// on the PR.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { mount, unmount } from 'svelte';
import '../../../app.css';
import ReviewPane from './ReviewPane.svelte';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { __resetPinnedReviewStatesForTest, disposePRReviewStates } from '../../stores/reviewPanePinned.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { __setTransportHelloForTest, __setTransportStatusForTest } from '../../stores/transportStatus.svelte';
import { resetForTest as resetDiffReviewComments } from '../../stores/diffReviewComments.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { TransportError } from '../../transport/wsClient';
import type { DiffReviewComment, PRDetail } from '../../types/models';
import type { PRRef } from '../../utils/prReference';
import { installPRDraftsFake } from '../../../test/helpers/prDraftsFake';
import { setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';
import { commitList } from '../../../test/helpers/commitList';

const OWNER = 'tools-1';
const PR: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };

function patch(): string {
  return ['diff --git a/src/a.ts b/src/a.ts', 'new file mode 100644', '--- /dev/null', '+++ b/src/a.ts', '@@ -0,0 +1,3 @@', '+one', '+two', '+three'].join('\n') + '\n';
}

function detail(): PRDetail {
  return {
    number: 5, title: 'Strip', body: '', authorLogin: 'octocat', state: 'open', draft: false,
    headRefName: 'feature', baseRefName: 'main', baseRefSHA: 'base-main', headSHA: 'sha-a',
    url: 'https://github.com/owner/repo/pull/5', additions: 3, deletions: 0, changedFiles: 1,
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
  __setTransportStatusForTest({ status: 'connected', nextAttemptAt: null });
  __setTransportHelloForTest({ launchId: 'l', protocolVersion: 1, capabilities: ['pr-review-drafts.v1'], backendId: 'b1', backendName: '', serverTimeMs: 0, clockSkewMs: 0, bundleId: '', bundleVersion: '', minShellBuild: 0 });
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
  __setTransportHelloForTest(null);
});

function q<T extends Element = HTMLElement>(selector: string): T | null {
  return target.querySelector<T>(selector);
}

function strip(): HTMLElement | null {
  return q('[data-testid="review-send-strip"]');
}

function button(root: Element, label: string): HTMLButtonElement {
  const found = Array.from(root.querySelectorAll<HTMLButtonElement>('button')).find((each) => each.textContent?.trim() === label);
  if (!found) throw new Error(`no ${label} button`);
  return found;
}

// Mounts a pinned review of PR 5 whose submit is `submit` and gives it
// one draft; answers the drafts' source key.
async function mountWithDraft(submit: (wire: unknown, review: { drafts?: { ids: string[] } }) => Promise<unknown>) {
  setBindingMock('SubscribePRUpdates', async () => ({
    id: 'sub-1', prKey: 'github:owner/repo:5', detail: detail(), threads: [], headSHA: 'sha-a', seq: 1,
  }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => patch());
  setBindingMock('ListPRCommits', async () => commitList([]));
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  const fake = installPRDraftsFake();
  let sourceKey = '';
  setBindingMock('ListPRDiffReviewComments', async (_wire: unknown, scope: string, key: string) => {
    if (key.startsWith('pr:')) sourceKey = key;
    return (fake.held.get(5) ?? []).filter((each) => each.scope === scope && each.sourceKey === key).map((each) => ({ ...each }));
  });
  const mock = setBindingMock('SubmitPRReview', submit);
  app = mount(ReviewPane, { target, props: { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } } });
  await vi.waitFor(() => expect(sourceKey).not.toBe(''), { timeout: 5_000 });
  fake.held.set(5, [{
    id: 'd1', threadId: '', scope: 'pr', sourceKey, filePath: 'src/a.ts', status: 'draft',
    newLine: 2, side: 'new', selectedText: 'two', body: 'Guard this.', createdAt: 1, updatedAt: 1,
  } as DiffReviewComment]);
  q<HTMLButtonElement>('[data-testid="review-reload"]')!.click();
  await vi.waitFor(() => expect(strip()).not.toBeNull());
  return { fake, submit: mock };
}

describe('the send strip of a PR review', () => {
  it('keeps the summary, verdict and draft of a review the forge did not confirm and says it may be on the PR', async () => {
    const { submit } = await mountWithDraft(async () => ({ postedReview: false, postedFileComments: 0, unconfirmed: true }));
    const summary = strip()!.querySelector<HTMLTextAreaElement>('textarea')!;
    summary.value = 'looks good';
    summary.dispatchEvent(new Event('input', { bubbles: true }));
    button(strip()!, 'Request changes').click();
    q<HTMLButtonElement>('[data-testid="review-send-comments"]')!.click();

    await vi.waitFor(() => expect(target.textContent).toContain('The forge did not confirm the review, so it may already be on the PR. Check the PR before you submit again.'));
    expect(target.textContent).not.toContain('nothing was posted');
    expect(strip()!.textContent).toContain('1 draft');
    expect(strip()!.querySelector<HTMLTextAreaElement>('textarea')!.value).toBe('looks good');
    expect(button(strip()!, 'Request changes').className).toContain('border-accent');
    expect(submit).toHaveBeenCalledTimes(1);
    expect(q<HTMLButtonElement>('[data-testid="review-send-comments"]')!.disabled).toBe(false);
  });

  it('stays with a refused review’s summary and verdict when no draft is left, and sends them', async () => {
    setBindingMock('SubscribePRUpdates', async () => ({
      id: 'sub-1', prKey: 'github:owner/repo:5', detail: detail(), threads: [], headSHA: 'sha-a', seq: 1,
    }));
    setBindingMock('UnsubscribePRUpdates', async () => undefined);
    setReviewDiffMock('OpenPRDiff', async () => patch());
    setBindingMock('ListPRCommits', async () => commitList([]));
    setBindingMock('ListPRReviewThreads', async () => []);
    setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
    const fake = installPRDraftsFake();
    let sourceKey = '';
    setBindingMock('ListPRDiffReviewComments', async (_wire: unknown, scope: string, key: string) => {
      if (key.startsWith('pr:')) sourceKey = key;
      return (fake.held.get(5) ?? []).filter((each) => each.scope === scope && each.sourceKey === key).map((each) => ({ ...each }));
    });
    const submit = setBindingMock('SubmitPRReview', async (_wire: unknown, review: { drafts?: { ids: string[] } }) => {
      if (review.drafts?.ids.length) {
        // Another tab sent the draft first.
        for (const each of fake.held.get(5) ?? []) each.status = 'sent';
        throw new TransportError('review_drafts_not_drafts', 'Some of these comments were already sent or removed, so this review was not posted.');
      }
      return { postedReview: true, postedFileComments: 0, posted: true };
    });
    app = mount(ReviewPane, { target, props: { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } } });
    await vi.waitFor(() => expect(sourceKey).not.toBe(''), { timeout: 5_000 });
    expect(strip()).toBeNull();
    fake.held.set(5, [{
      id: 'd1', threadId: '', scope: 'pr', sourceKey, filePath: 'src/a.ts', status: 'draft',
      newLine: 2, side: 'new', selectedText: 'two', body: 'Guard this.', createdAt: 1, updatedAt: 1,
    } as DiffReviewComment]);
    q<HTMLButtonElement>('[data-testid="review-reload"]')!.click();
    await vi.waitFor(() => expect(strip()).not.toBeNull());

    const summary = strip()!.querySelector<HTMLTextAreaElement>('textarea')!;
    summary.value = 'from tab B';
    summary.dispatchEvent(new Event('input', { bubbles: true }));
    button(strip()!, 'Request changes').click();
    q<HTMLButtonElement>('[data-testid="review-send-comments"]')!.click();

    await vi.waitFor(() => expect(target.textContent).toContain('this review and its summary were not posted'));
    // No draft is left, and the strip still holds what the next review sends.
    expect(strip()).not.toBeNull();
    expect(strip()!.textContent).toContain('0 drafts');
    expect(strip()!.querySelector<HTMLTextAreaElement>('textarea')!.value).toBe('from tab B');
    expect(button(strip()!, 'Request changes').className).toContain('border-accent');

    q<HTMLButtonElement>('[data-testid="review-send-comments"]')!.click();
    await vi.waitFor(() => expect(submit).toHaveBeenCalledTimes(2));
    expect(submit.mock.calls[1]?.[1]).toMatchObject({ verdict: 'request-changes', body: 'from tab B', drafts: { ids: [] } });
    await vi.waitFor(() => expect(strip()).toBeNull());
  });
});
