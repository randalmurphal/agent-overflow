// A render throw inside the review pane's body once aborted the flush
// mid-branch and left the previous branch's DOM standing — "Loading…"
// over a fully loaded store, with the only trace in frontend-errors.jsonl
// (MR !309, 2026-09-04). The pane now renders the failure in place and
// records it. This file mocks the diff body to throw, so it stays apart
// from ReviewPane.test.ts, whose cases need the real one.
import { render, waitFor } from '@testing-library/svelte';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import ReviewPane from './ReviewPane.svelte';
import { makeStubPanelContext } from '../../../test/helpers/panelContext';
import { __resetReviewPaneStateForTest } from '../../stores/reviewPane.svelte';
import { resetForTest as resetDiffReviewCommentsForTest } from '../../stores/diffReviewComments.svelte';
import { resetAppStorageForTest } from '../../stores/appStorage';
import { setBindingMock } from '../../../test/mocks/bindings-app';
import {
  frontendErrorCaptureStateForTest,
  resetFrontendErrorCaptureForTest,
} from '../../utils/frontendErrorCapture';

vi.mock('./ReviewDiffBody.svelte', async () => ({
  default: (await import('../../../test/fixtures/ThrowsOnRender.svelte')).default,
}));

const patch = [
  'diff --git a/src/app.ts b/src/app.ts',
  'index 1111111..2222222 100644',
  '--- a/src/app.ts',
  '+++ b/src/app.ts',
  '@@ -1 +1 @@',
  '-old',
  '+new',
].join('\n');

beforeEach(() => {
  resetAppStorageForTest();
  __resetReviewPaneStateForTest();
  resetDiffReviewCommentsForTest();
  resetFrontendErrorCaptureForTest();
  setBindingMock('GetThread', async () => ({ id: 'thread-1', workspacePath: '/repo' }));
  setBindingMock('GetGitStatus', async () => ({}));
  setBindingMock('GetWorkspaceCurrentDiff', async () => patch);
  setBindingMock('GetBranchBaseDiff', async () => '');
  setBindingMock('ListBranchCommits', async () => []);
  setBindingMock('GetCommitDiff', async () => '');
  setBindingMock('ListPRCommits', async () => []);
  setBindingMock('GetPRCommitDiff', async () => '');
  setBindingMock('ListThreadEditDiffs', async () => ({ entries: [], turnLabels: [] }));
  setBindingMock('GetTurnEditsDiff', async () => ({ data: '' }));
  setBindingMock('GetPayloadData', async () => ({ data: '' }));
  setBindingMock('GitListBranches', async () => [{ name: 'main', isCurrent: false, isDefault: true }]);
  setBindingMock('ListDiffReviewComments', async () => []);
  setBindingMock('ReportFrontendErrorBatch', async () => '/tmp/frontend-errors.jsonl');
});

describe('<ReviewPane> render failure', () => {
  it('shows the failure in place of a stuck "Loading…" and records it', async () => {
    const before = frontendErrorCaptureStateForTest().pendingCount;
    const view = render(ReviewPane, { ctx: makeStubPanelContext() });

    const alert = await waitFor(() => view.getByTestId('review-render-error'));
    expect(alert.textContent).toContain('The review pane failed to render: fixture render failure');
    expect(view.queryByText('Loading…')).toBeNull();
    expect(view.getByTestId('review-render-error-retry')).toBeTruthy();
    expect(frontendErrorCaptureStateForTest().pendingCount).toBe(before + 1);
  });
});
