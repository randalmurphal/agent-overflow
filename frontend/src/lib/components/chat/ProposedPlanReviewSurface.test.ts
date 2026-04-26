import { cleanup, render } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import ProposedPlanReviewSurface from './ProposedPlanReviewSurface.svelte';
import type { ProposedPlanComment } from '../../types/models';

describe('<ProposedPlanReviewSurface>', () => {
  afterEach(cleanup);

  it('keeps resolved comments visible without edit controls', () => {
    const comments: ProposedPlanComment[] = [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'resolved',
      startLine: 1,
      endLine: 1,
      selectedText: '# Plan',
      body: 'Already handled',
      createdAt: 1,
      updatedAt: 2,
    }];

    const { getByText, queryByLabelText } = render(ProposedPlanReviewSurface, {
      props: {
        threadId: 'thread-1',
        planItemId: 'plan-1',
        markdown: '# Plan',
        comments,
        onRefresh: vi.fn(),
        onSendDrafts: vi.fn(),
      },
    });

    expect(getByText('Already handled')).toBeInTheDocument();
    expect(getByText('Resolved - Lines 1-1')).toBeInTheDocument();
    expect(queryByLabelText('Edit comment')).not.toBeInTheDocument();
  });
});
