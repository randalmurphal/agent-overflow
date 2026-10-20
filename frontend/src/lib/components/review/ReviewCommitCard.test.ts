import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ReviewCommitCard from './ReviewCommitCard.svelte';
import type { BranchCommit } from '../../types/git';

const COMMIT: BranchCommit = {
  sha: 'e'.repeat(40),
  shortSha: 'eeeeeee',
  subject: 'Close the compact review tree on Back',
  body: 'On the phone the open tree covers the diff.\n\nSo Back closes it first.',
  author: 'Randy Murphy',
  authoredAt: Date.now() - 2 * 3_600_000,
};

describe('<ReviewCommitCard>', () => {
  it('shows the hash, whole subject, author, age, position and the body with its line breaks', () => {
    const view = render(ReviewCommitCard, { commit: COMMIT, ordinal: 2, count: 12 });
    const card = view.getByTestId('review-commit-card');
    expect(card.getAttribute('data-sha')).toBe(COMMIT.sha);
    expect(card).toHaveTextContent('eeeeeee');
    expect(view.getByTestId('review-commit-card-subject')).toHaveTextContent(COMMIT.subject);
    expect(card).toHaveTextContent('Randy Murphy');
    expect(card).toHaveTextContent('2h');
    expect(card).toHaveTextContent('2 of 12');
    const body = view.getByTestId('review-commit-card-body');
    expect(body.textContent).toBe(COMMIT.body);
    expect(body.className).toContain('whitespace-pre-wrap');
  });

  it('omits the body block for a one-line message and the age for an unknown time', () => {
    const view = render(ReviewCommitCard, { commit: { ...COMMIT, body: '', authoredAt: 0 }, ordinal: 1, count: 1 });
    expect(view.queryByTestId('review-commit-card-body')).toBeNull();
    expect(view.getByTestId('review-commit-card').textContent).toMatch(/Randy Murphy\s*·\s*1 of 1/);
  });
});
