import { render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewPRTitleBar from './ReviewPRTitleBar.svelte';
import type { CIPipeline, PRDetail } from '../../types/models';

function detail(overrides: Partial<PRDetail> = {}): PRDetail {
  return {
    number: 4323,
    title: 'Action Queue: send emails built from an r3 email template',
    body: '',
    authorLogin: 'sabossedgh',
    authorName: 'Sawmon Abossedgh',
    state: 'open',
    draft: false,
    headRefName: 'BLITZ-664',
    baseRefName: 'develop',
    baseRefSHA: 'b',
    headSHA: 'h',
    url: 'https://gitlab.example/mr/4323',
    additions: 10,
    deletions: 2,
    changedFiles: 3,
    viewerIsAuthor: false,
    reviewDecision: '',
    latestReviews: [],
    checks: { total: 0, success: 0, pending: 0, failure: 0, skipped: 0, canceled: 0, checks: [] },
    mergeability: 'mergeable',
    readiness: { state: 'ready', blockers: [], settled: [] },
    merge: { methods: ['merge'], deleteBranchOffered: false, deleteBranchDefault: false, autoMergeAvailable: false, viewerCanMerge: false, viewerCanApprove: false, viewerApproved: false },
    ...overrides,
  };
}

const pipeline: CIPipeline = {
  status: 'success',
  url: 'https://gitlab.example/pipelines/1',
  stages: [
    { name: 'test', status: 'success', jobs: [{ id: '1', name: 'unit', status: 'success', logsAvailable: true }] },
  ],
};

describe('<ReviewPRTitleBar> lines', () => {
  it('puts the author and state on one line, the refs and verdicts on the next, CI on the last', () => {
    const view = render(ReviewPRTitleBar, {
      detail: detail({
        reviewDecision: 'APPROVED',
        latestReviews: [{ authorLogin: 'rmurphy', authorName: 'Randy Murphy', state: 'APPROVED', submittedAt: '2026-10-18T16:00:00Z', body: '', commitSHA: 'h' }],
      }),
      ciPipeline: pipeline,
      onOpenCIJob: vi.fn(),
      onRefreshCI: vi.fn(),
    });
    const authorLine = view.getByTestId('review-pr-author-line');
    expect(authorLine).toContainElement(view.getByTestId('review-pr-author'));
    expect(authorLine).toContainElement(view.getByTestId('review-pr-state'));
    expect(view.getByTestId('review-pr-state').textContent?.trim()).toBe('open');
    const refsLine = view.getByTestId('review-pr-refs-line');
    expect(refsLine).toContainElement(view.getByTestId('review-pr-refs'));
    expect(refsLine.textContent).toContain('approved');
    // The decision pill is redundant beside the same verdict.
    expect(view.queryByTestId('review-pr-decision')).toBeNull();
    const ciLine = view.getByTestId('review-pr-ci-line');
    expect(ciLine).toContainElement(view.getByTestId('review-ci-chip'));
    // The refresh is an item of the chip row, so it wraps with the chips.
    expect(view.getByTestId('review-ci-chips')).toContainElement(view.getByTestId('review-ci-refresh'));
  });

  it('reads a draft as draft and keeps a long branch name readable at both ends', () => {
    const head = 'BLITZ-664-action-queue-send-emails-built-from-template';
    const view = render(ReviewPRTitleBar, { detail: detail({ draft: true, headRefName: head }) });
    expect(view.getByTestId('review-pr-state').textContent?.trim()).toBe('draft');
    const refs = view.getByTestId('review-pr-refs');
    expect(refs.getAttribute('title')).toBe(`develop ← ${head}`);
    expect(refs.textContent).toContain('BLITZ-664');
    expect(refs.textContent).toContain('template');
    expect(refs.textContent).toContain('…');
  });

  it('shows the CI line only when jobs can be opened, with a refresh even before a pipeline lands', () => {
    const without = render(ReviewPRTitleBar, { detail: detail() });
    expect(without.queryByTestId('review-pr-ci-line')).toBeNull();
    without.unmount();
    const loading = render(ReviewPRTitleBar, { detail: detail(), ciLoading: true, onOpenCIJob: vi.fn(), onRefreshCI: vi.fn() });
    expect(loading.getByText('Loading checks…')).toBeInTheDocument();
    expect(loading.getByTestId('review-ci-refresh')).toBeInTheDocument();
  });
});
