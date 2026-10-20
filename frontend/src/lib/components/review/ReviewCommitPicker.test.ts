import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCommitPicker from './ReviewCommitPicker.svelte';
import type { BranchCommit } from '../../types/git';

const NOW = Date.now();
const COMMITS: BranchCommit[] = [
  { sha: 'a'.repeat(40), shortSha: 'aaaaaaa', subject: 'Newest: a subject long enough to need the menu row', body: '', author: 'Ann', authoredAt: NOW - 60_000 },
  { sha: 'b'.repeat(40), shortSha: 'bbbbbbb', subject: 'Middle', body: 'Why.\n', author: 'Bob', authoredAt: NOW - 3_600_000 },
  { sha: 'c'.repeat(40), shortSha: 'ccccccc', subject: 'Oldest', body: '', author: 'Cy', authoredAt: NOW - 86_400_000 },
];

type View = ReturnType<typeof mount>['view'];

function mount(selectedSha: string | null) {
  const onSelect = vi.fn();
  const view = render(ReviewCommitPicker, { commits: COMMITS, selectedSha, onSelect });
  return { view, onSelect };
}

async function open(view: View): Promise<void> {
  await fireEvent.click(view.getByTestId('review-commit-select'));
  await waitFor(() => {
    expect(view.getAllByTestId('review-commit-option')).toHaveLength(COMMITS.length);
  });
}

describe('<ReviewCommitPicker>', () => {
  it('reads "All commits" with nothing picked and the ordinal plus hash with a pick, never the subject', () => {
    const { view } = mount(null);
    const trigger = view.getByTestId('review-commit-select');
    expect(trigger).toHaveTextContent('All commits');
    expect(trigger).toHaveAttribute('title', 'All commits (3)');
    view.unmount();

    const picked = mount(COMMITS[1].sha);
    const pickedTrigger = picked.view.getByTestId('review-commit-select');
    expect(picked.view.getByTestId('review-commit-ordinal')).toHaveTextContent('2/3');
    expect(picked.view.getByTestId('review-commit-hash')).toHaveTextContent('bbbbbbb');
    expect(pickedTrigger).not.toHaveTextContent('Middle');
    // The subject is the hover text, where length costs nothing.
    expect(pickedTrigger).toHaveAttribute('title', 'bbbbbbb Middle');
  });

  it('lists every commit with its hash, whole subject and age, the current one checked, after an "All commits" row', async () => {
    const { view, onSelect } = mount(COMMITS[1].sha);
    await open(view);
    expect(view.getByTestId('review-commit-position')).toHaveTextContent('Commit 2 of 3');
    const all = view.getByTestId('review-commit-all');
    expect(all).toHaveTextContent('All commits');
    expect(all).toHaveTextContent('3');
    expect(all).not.toHaveTextContent('✓');
    const rows = view.getAllByTestId('review-commit-option');
    expect(rows[0]).toHaveTextContent('aaaaaaa');
    expect(rows[0]).toHaveTextContent(COMMITS[0].subject);
    expect(rows[0]).toHaveAttribute('title', COMMITS[0].subject);
    expect(rows[0].textContent).toMatch(/1m ago/);
    expect(rows[1]).toHaveTextContent('✓');
    expect(rows[2]).not.toHaveTextContent('✓');

    await fireEvent.click(rows[2]);
    expect(onSelect).toHaveBeenCalledWith(COMMITS[2].sha);
    // A pick closes the menu.
    await waitFor(() => {
      expect(view.getByTestId('review-commit-select')).toHaveAttribute('aria-expanded', 'false');
    });
    expect(view.queryByTestId('review-commit-option')).toBeNull();
  });

  it('"All commits" selects null', async () => {
    const { view, onSelect } = mount(COMMITS[0].sha);
    await open(view);
    await fireEvent.click(view.getByTestId('review-commit-all'));
    expect(onSelect).toHaveBeenCalledWith(null);
  });

  it('steps down the list with next and up with previous, stopping at the ends', async () => {
    const { view, onSelect } = mount(COMMITS[1].sha);
    await open(view);
    expect(view.getByTestId('review-commit-prev')).toBeEnabled();
    expect(view.getByTestId('review-commit-next')).toBeEnabled();
    await fireEvent.click(view.getByTestId('review-commit-next'));
    expect(onSelect).toHaveBeenLastCalledWith(COMMITS[2].sha);
    await fireEvent.click(view.getByTestId('review-commit-prev'));
    expect(onSelect).toHaveBeenLastCalledWith(COMMITS[0].sha);
    // Stepping keeps the menu open: the header reads the new position.
    expect(view.getByTestId('review-commit-select')).toHaveAttribute('aria-expanded', 'true');
    view.unmount();

    const newest = mount(COMMITS[0].sha);
    await open(newest.view);
    expect(newest.view.getByTestId('review-commit-prev')).toBeDisabled();
    expect(newest.view.getByTestId('review-commit-next')).toBeEnabled();
    newest.view.unmount();

    const oldest = mount(COMMITS[2].sha);
    await open(oldest.view);
    expect(oldest.view.getByTestId('review-commit-prev')).toBeEnabled();
    expect(oldest.view.getByTestId('review-commit-next')).toBeDisabled();
  });

  it('steps into the list from "All commits": next picks the newest, previous the oldest', async () => {
    const { view, onSelect } = mount(null);
    await open(view);
    expect(view.getByTestId('review-commit-position')).toHaveTextContent('3 commits');
    await fireEvent.click(view.getByTestId('review-commit-next'));
    expect(onSelect).toHaveBeenLastCalledWith(COMMITS[0].sha);
    await fireEvent.click(view.getByTestId('review-commit-prev'));
    expect(onSelect).toHaveBeenLastCalledWith(COMMITS[2].sha);
  });

  it('falls back to "All commits" when the picked SHA is not in the list', () => {
    const { view } = mount('d'.repeat(40));
    expect(view.getByTestId('review-commit-select')).toHaveTextContent('All commits');
  });
});
