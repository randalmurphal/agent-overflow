// A review pane mounted on a PR, for component tests of the PR surfaces
// (the overview's sections, the title bar): the source pane's workspace
// has PR #5 open on GitHub, the diff and PR reads are mocked, and the
// pane is switched to pr scope.

import { fireEvent, render, waitFor, type RenderResult } from '@testing-library/svelte';
import { expect } from 'vitest';
import ReviewPane from '../../lib/components/review/ReviewPane.svelte';
import { makeStubPanelContext } from './panelContext';
import { __resetReviewPaneStateForTest } from '../../lib/stores/reviewPane.svelte';
import { resetForTest as resetDiffReviewCommentsForTest } from '../../lib/stores/diffReviewComments.svelte';
import { resetAppStorageForTest } from '../../lib/stores/appStorage';
import { resetToastsForTest } from '../../lib/stores/toast.svelte';
import { setBindingMock, setReviewDiffMock } from '../mocks/bindings-app';
import { __seedGitStatusForTest } from '../../lib/stores/gitStatusStore.svelte';
import { registerPaneForTest, resetPanesForTest } from '../../lib/stores/panes.svelte';
import { createThreadPane } from '../../lib/stores/thread.svelte';
import type { PRDetail, ReviewEvent, ReviewThread } from '../../lib/types/models';

export const PR_KEY = 'github:owner/repo:5';

export function prDetailFixture(overrides: Partial<PRDetail> = {}): PRDetail {
  return {
    number: 5,
    title: 'Add feature',
    body: '',
    authorLogin: 'octocat',
    state: 'open',
    draft: false,
    headRefName: 'feature',
    baseRefName: 'main',
    baseRefSHA: 'base-main',
    headSHA: 'sha-a',
    url: 'https://github.com/owner/repo/pull/5',
    additions: 1,
    deletions: 1,
    changedFiles: 1,
    viewerIsAuthor: false,
    reviewDecision: '',
    latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'clean',
    readiness: { state: 'ready', blockers: [], settled: [{ kind: 'approvals', text: 'Approved' }] },
    merge: {
      methods: ['merge', 'squash'],
      deleteBranchOffered: true,
      deleteBranchDefault: false,
      autoMergeAvailable: true,
      viewerCanMerge: true,
      viewerCanApprove: true,
      viewerApproved: false,
    },
    ...overrides,
  };
}

function patch(): string {
  return [
    'diff --git a/src/app.ts b/src/app.ts',
    'index 1111111..2222222 100644',
    '--- a/src/app.ts',
    '+++ b/src/app.ts',
    '@@ -1 +1 @@',
    '-old',
    '+new',
  ].join('\n');
}

/** Resets every store the pane touches and installs the diff mocks. */
export function resetReviewPanePR(): void {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  resetDiffReviewCommentsForTest();
  resetPanesForTest();
  resetToastsForTest();
  setBindingMock('GetGitStatus', async () => ({}));
  setBindingMock('UpdateThreadBranch', async () => []);
  setReviewDiffMock('OpenWorkspaceDiff', async () => patch());
  setReviewDiffMock('OpenBranchBaseDiff', async () => '');
  setBindingMock('ListBranchCommits', async () => []);
  setReviewDiffMock('OpenCommitDiff', async () => '');
  setBindingMock('ListPRCommits', async () => []);
  setReviewDiffMock('OpenPRCommitDiff', async () => '');
  setBindingMock('ListThreadEditDiffs', async () => ({ entries: [], turnLabels: [] }));
  setReviewDiffMock('OpenTurnEditsDiff', async () => '');
  setReviewDiffMock('OpenEditDiff', async () => '');
  setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: false, isDefault: true }]);
  setBindingMock('ListDiffReviewComments', async () => []);
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => patch());
  setBindingMock('OpenExternalURL', async () => undefined);
  setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
  setBindingMock('HighlightClassNames', async () => ['none']);
}

function seedSourcePanePR(): void {
  const pane = createThreadPane({ paneId: 'source-pane' });
  pane.replaceThread({
    id: 'thread-1',
    title: 'Review',
    provider: 'claude',
    workspacePath: '/repo',
    projectPath: '/repo',
    model: 'm',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
  });
  registerPaneForTest('source-pane', pane);
  __seedGitStatusForTest('/repo', {
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
    forge: 'github',
    openPrUrl: 'https://github.com/owner/repo/pull/5',
    openPrNumber: 5,
  });
}

/** The mounted pane's queries, typed by the component so `getByTestId`
 * answers an element and not the overloads' union. */
export type ReviewPaneView = RenderResult<typeof ReviewPane>;

/** Opens the scope menu and picks pr scope. */
async function pickPRScope(view: ReviewPaneView): Promise<void> {
  const trigger = view.getByTestId('review-scope-select');
  await waitFor(() => {
    expect(trigger).toBeEnabled();
  });
  await fireEvent.click(trigger);
  const row = await waitFor(() => view.getByTestId('review-scope-option-pr'));
  await fireEvent.click(row);
  await waitFor(() => {
    expect(trigger).toHaveAttribute('aria-expanded', 'false');
  });
}

/** Mounts a pane on PR #5 with this snapshot and waits for the header. */
export async function mountReviewPanePR(
  detail: PRDetail,
  options: { threads?: ReviewThread[]; events?: ReviewEvent[] } = {},
): Promise<ReviewPaneView> {
  const threads = options.threads ?? [];
  setBindingMock('SubscribePRUpdates', async () => ({
    id: 'sub-1',
    prKey: PR_KEY,
    detail,
    threads,
    events: options.events ?? [],
    headSHA: detail.headSHA,
    seq: 1,
  }));
  setBindingMock('ListPRReviewThreads', async () => threads);
  setBindingMock('GetPRDetail', async () => detail);
  seedSourcePanePR();
  const view = render(ReviewPane, { ctx: makeStubPanelContext() });
  await waitFor(() => {
    expect(view.getByTestId('review-diff-stats')).toBeInTheDocument();
  });
  await pickPRScope(view);
  await waitFor(() => {
    expect(view.getByTestId('review-pr-header')).toBeInTheDocument();
    expect(view.getByTestId('review-overview')).toBeInTheDocument();
  });
  return view;
}
