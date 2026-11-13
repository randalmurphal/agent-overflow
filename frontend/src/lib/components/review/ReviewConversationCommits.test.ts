import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewConversationCommits from './ReviewConversationCommits.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import type { BranchCommit } from '../../types/git';

// Newest first, as the feed groups them.
const GROUP: BranchCommit[] = ['c', 'b'].map((id, index) => ({
  sha: id.repeat(40), shortSha: id.repeat(7), subject: `commit ${id}`, body: '', author: 'Ann', authoredAt: 2000 - index,
}));

describe('a Conversation commit group', () => {
  it('runs Changes from here from the group\'s first commit, and a row picks its own commit', async () => {
    const review = { showChangesFrom: vi.fn(async () => {}), selectCommit: vi.fn(async () => {}) };
    const view = render(ReviewConversationCommits, { review: review as unknown as ReviewPaneState, author: 'Ann', commits: GROUP });
    const changesFrom = view.getByTestId('review-conversation-changes-from');
    expect(changesFrom.getAttribute('title')).toBe('Show the changes from bbbbbbb through the newest commit');
    // Revealed on hover, always shown on compact: it holds its place.
    expect(changesFrom.className).toContain('compact:opacity-100');
    await fireEvent.click(changesFrom);
    expect(review.showChangesFrom).toHaveBeenCalledWith('b'.repeat(40));
    await fireEvent.click(view.getByText('commit c'));
    expect(review.selectCommit).toHaveBeenCalledWith('c'.repeat(40), { userInitiated: true });
  });
});
