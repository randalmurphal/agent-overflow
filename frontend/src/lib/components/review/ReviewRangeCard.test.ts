import { render } from '@testing-library/svelte';
import { describe, expect, it } from 'vitest';
import ReviewRangeCard from './ReviewRangeCard.svelte';
import type { BranchCommit } from '../../types/git';

const commit = (id: string): BranchCommit => ({ sha: id.repeat(40), shortSha: id.repeat(7), subject: `subject ${id}`, body: '', author: 'Ann', authoredAt: 0 });
// A run of the two newest listed commits.
const RUN = [commit('b'), commit('a')];

describe('<ReviewRangeCard>', () => {
  it.each([
    { name: 'a complete list', total: 3, place: '2–3 of 3 · to the head' },
    { name: 'a truncated list', total: 500, place: '499–500 of 500 · to the head' },
    { name: 'an uncounted list', total: null, place: 'to the head' },
  ])('places the run among the PR\'s commits on $name', ({ total, place }) => {
    const view = render(ReviewRangeCard, { commits: RUN, olderIndex: 1, total, onLeave: () => {} });
    const card = view.getByTestId('review-range-card');
    expect(view.getByTestId('review-range-card-count')).toHaveTextContent('2 commits');
    expect(card).toHaveTextContent(place);
    if (total === null) expect(card.textContent).not.toMatch(/\d+–\d+ of/);
  });
});
