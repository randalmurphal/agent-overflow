import { render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewHeldReplies from './ReviewHeldReplies.svelte';
import type { DiffReviewComment } from '../../types/models';

const reply: DiffReviewComment = {
  id: 'r1', threadId: 'pane-thread', scope: 'pr', sourceKey: 'k', filePath: 'src/old.ts', status: 'draft', side: 'reply',
  replyThreadId: 't1', selectedText: '', body: 'held', createdAt: 0, updatedAt: 0,
};

const review = {
  draftEditBodyFor: () => null,
  setDraftEditBody: vi.fn(),
  updateComment: vi.fn(),
  deleteComment: vi.fn(),
};

// The titles on the reply's card: its author's, and its location's.
function titles(path?: string): (string | null)[] {
  const view = render(ReviewHeldReplies, { props: { review, replies: [reply], path } });
  return [...view.getByTestId('review-held-replies').querySelectorAll('[title]')].map((each) => each.getAttribute('title'));
}

describe('<ReviewHeldReplies>', () => {
  it('names the file of the thread a reply shows under, not the file it was held on', () => {
    const shown = titles('src/new.ts');
    expect(shown).toContain('src/new.ts');
    expect(shown).not.toContain('src/old.ts');
  });

  it('names the file a reply was held on when its thread is gone', () => {
    expect(titles()).toContain('src/old.ts');
  });
});
