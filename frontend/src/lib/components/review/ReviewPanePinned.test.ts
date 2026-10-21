import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReviewPane from './ReviewPane.svelte';
import {
  __pinnedReviewStatesForTest,
  __resetReviewPaneStateForTest,
  disposePRReviewStates,
} from '../../stores/reviewPane.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { resetPanesForTest } from '../../stores/panes.svelte';
import { prReviewKeys } from '../../stores/prReviewStore.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import type { PRDetail } from '../../types/models';
import type { PRRef } from '../../utils/prReference';
import { setBindingMock, setReviewDiffMock } from '../../../test/mocks/bindings-app';
import { installPRDraftsFake } from '../../../test/helpers/prDraftsFake';
import { __diffSpanCacheStatsForTest, resetDiffSpanCacheForTest } from '../../utils/diffSpanCache.svelte';
import { resetSyntaxClassNamesForTest } from '../../utils/syntaxSpans';

// The review pane on the Pull requests page: a PR with no thread pane.

const OWNER = 'tools-1';
const PR: PRRef = { forge: 'github', host: 'github.com', namespace: 'owner', repo: 'repo', number: 5 };

function detail(): PRDetail {
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
    readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: true, viewerCanApprove: true, viewerApproved: false },
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

function renderPinned(onBack = vi.fn()) {
  const view = render(ReviewPane, {
    pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR },
    onBack,
  });
  return { view, onBack };
}

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  resetPanesForTest();
  setBindingMock('SubscribePRUpdates', async () => ({
    id: 'sub-1',
    prKey: 'github:owner/repo:5',
    detail: detail(),
    threads: [],
    headSHA: 'sha-a',
    seq: 1,
  }));
  setBindingMock('UnsubscribePRUpdates', async () => undefined);
  setReviewDiffMock('OpenPRDiff', async () => patch());
  setBindingMock('ListPRCommits', async () => []);
  setBindingMock('ListPRReviewThreads', async () => []);
  setBindingMock('SetPRCILogFollows', async () => ({ logs: {} }));
  installPRDraftsFake();
});

describe('<ReviewPane> pinned to a PR with no thread', () => {
  it('shows the PR with its readiness and Merge section, Back in Close\'s place, and no scope selector', async () => {
    const { view, onBack } = renderPinned();
    await waitFor(() => {
      expect(view.getByTestId('review-pr-readiness')).toBeInTheDocument();
      expect(view.getByTestId('review-overview')).toBeInTheDocument();
      expect(view.getAllByTestId('review-line-block').length).toBeGreaterThan(0);
    });
    expect(view.queryByTestId('review-scope-select')).toBeNull();
    expect(view.queryByTestId('review-scope-option-edits')).toBeNull();
    expect(view.queryByTestId('review-close')).toBeNull();
    const back = view.getByTestId('review-back');
    expect(back).toHaveAttribute('aria-label', 'Back to the list');
    await fireEvent.click(back);
    expect(onBack).toHaveBeenCalledTimes(1);
  });

  it('drafts a line comment and submits it as a PR review, with no agent target', async () => {
    const create = setBindingMock('CreateDiffReviewComment', async () => ({}));
    const submit = setBindingMock('SubmitPRReview', async () => ({ postedReview: true, postedFileComments: 0 }));
    const { view } = renderPinned();
    await waitFor(() => {
      expect(view.getAllByTestId('review-line-block').length).toBeGreaterThan(0);
    });
    await fireEvent.mouseOver(view.getAllByTestId('review-line-block')[0]!);
    await fireEvent.click(view.getAllByTestId('review-add-comment')[0]!);
    const editor = view.getByTestId('review-draft-editor');
    await fireEvent.input(editor.querySelector('textarea')!, { target: { value: 'Guard this.' } });
    await fireEvent.click(Array.from(editor.querySelectorAll('button')).find((button) => button.textContent?.trim() === 'Add comment')!);

    await waitFor(() => {
      expect(view.getByTestId('review-send-strip')).toBeInTheDocument();
      expect(view.getByText('Guard this.')).toBeInTheDocument();
    });
    expect(create).not.toHaveBeenCalled();
    expect(view.queryByTestId('review-submit-target')).toBeNull();
    await fireEvent.click(view.getByTestId('review-send-comments'));
    await waitFor(() => {
      expect(submit).toHaveBeenCalledWith(
        { Forge: 'github', Host: 'github.com', Namespace: 'owner', Repo: 'repo', Number: 5 },
        expect.objectContaining({ verdict: 'comment', comments: [expect.objectContaining({ path: 'src/app.ts', body: 'Guard this.' })] }),
      );
      expect(view.queryByTestId('review-send-strip')).toBeNull();
    });
  });

  it('leaves its state to the page on unmount; the page disposes it', async () => {
    const { view } = renderPinned();
    await waitFor(() => {
      expect(view.getByTestId('review-overview')).toBeInTheDocument();
    });
    view.unmount();
    expect(__pinnedReviewStatesForTest(OWNER)).toHaveLength(1);
    expect(prReviewKeys()).toHaveLength(1);
    disposePRReviewStates(OWNER);
    expect(__pinnedReviewStatesForTest(OWNER)).toHaveLength(0);
    expect(prReviewKeys()).toEqual([]);
  });

  it('files its diff spans under the PR and drops them with the last state on that PR', async () => {
    resetDiffSpanCacheForTest();
    resetSyntaxClassNamesForTest();
    setBindingMock('HighlightSchemaVersion', async () => 'hv-test');
    setBindingMock('HighlightClassNames', async () => ['none', 'c1']);
    const highlight = async (req: unknown) => ({
      lines: (req as { patch: string }).patch.split('\n').map((row) => (row.startsWith('+') ? { r: [row.length - 1, 1] } : {})),
      incomplete: false,
    });
    setBindingMock('HighlightPatchWithContext', async (_ws: unknown, req: unknown) => highlight(req));
    setBindingMock('HighlightPatch', async (req: unknown) => highlight(req));
    const mine = render(ReviewPane, { pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: 'project-1', pr: PR } });
    const theirs = render(ReviewPane, { pr: { ownerPaneId: 'tools-2', backend: HOME_BACKEND, projectId: 'project-1', pr: PR } });
    await waitFor(() => {
      expect(__diffSpanCacheStatsForTest().entries).toBeGreaterThan(0);
    });
    const filed = __diffSpanCacheStatsForTest().entries;
    mine.unmount();
    disposePRReviewStates(OWNER);
    // The other page on the same PR still shows them.
    expect(__diffSpanCacheStatsForTest().entries).toBe(filed);
    theirs.unmount();
    disposePRReviewStates('tools-2');
    expect(__diffSpanCacheStatsForTest()).toMatchObject({ entries: 0, ownerKeys: 0 });
  });

  it('says why when the PR has no project', () => {
    const view = render(ReviewPane, {
      pr: { ownerPaneId: OWNER, backend: HOME_BACKEND, projectId: '', pr: PR },
    });
    expect(view.getByTestId('review-no-subject')).toHaveTextContent('A pull request review needs the project it belongs to.');
  });
});
