import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCommitPicker from './ReviewCommitPicker.svelte';
import type { BranchCommit } from '../../types/git';
import type { CommitSelection } from '../../utils/commitRange';

const NOW = Date.now();
const COMMITS: BranchCommit[] = [
  { sha: 'a'.repeat(40), shortSha: 'aaaaaaa', subject: 'Newest: a subject long enough to need the menu row', body: '', author: 'Ann', authoredAt: NOW - 60_000 },
  { sha: 'b'.repeat(40), shortSha: 'bbbbbbb', subject: 'Middle', body: 'Why.\n', author: 'Bob', authoredAt: NOW - 3_600_000 },
  { sha: 'c'.repeat(40), shortSha: 'ccccccc', subject: 'Oldest', body: '', author: 'Cy', authoredAt: NOW - 86_400_000 },
];
const [NEWEST, MIDDLE, OLDEST] = COMMITS;
const one = (commit: BranchCommit): CommitSelection => ({ first: commit.sha, last: commit.sha });

type View = ReturnType<typeof mount>['view'];

function mount(selection: CommitSelection | null, ranges = true) {
  const onSelect = vi.fn();
  const view = render(ReviewCommitPicker, { commits: COMMITS, selection, ranges, onSelect });
  return { view, onSelect };
}

async function open(view: View): Promise<void> {
  await fireEvent.click(view.getByTestId('review-commit-select'));
  await waitFor(() => {
    expect(view.getAllByTestId('review-commit-option')).toHaveLength(COMMITS.length);
  });
}

describe('<ReviewCommitPicker>', () => {
  it('reads "All commits" with nothing picked and the ordinal (oldest is 1) plus hash with a pick, never the subject', () => {
    const { view } = mount(null);
    const trigger = view.getByTestId('review-commit-select');
    expect(trigger).toHaveTextContent('All commits');
    expect(trigger).toHaveAttribute('title', 'All commits (3)');
    view.unmount();

    const picked = mount(one(NEWEST));
    const pickedTrigger = picked.view.getByTestId('review-commit-select');
    expect(picked.view.getByTestId('review-commit-ordinal')).toHaveTextContent('3/3');
    expect(picked.view.getByTestId('review-commit-hash')).toHaveTextContent('aaaaaaa');
    expect(pickedTrigger).not.toHaveTextContent('Newest');
    // The subject is the hover text, where length costs nothing.
    expect(pickedTrigger).toHaveAttribute('title', `aaaaaaa ${NEWEST.subject}`);
  });

  it('reads a run as its ordinals, N–T/T to the head, with no hash or subject', () => {
    const toHead = mount({ first: MIDDLE.sha, last: NEWEST.sha });
    const trigger = toHead.view.getByTestId('review-commit-select');
    expect(toHead.view.getByTestId('review-commit-ordinal')).toHaveTextContent('2–3/3');
    expect(toHead.view.queryByTestId('review-commit-hash')).toBeNull();
    expect(trigger).not.toHaveTextContent('Middle');
    expect(trigger).toHaveAttribute('title', 'bbbbbbb–aaaaaaa (2 commits)');
    toHead.view.unmount();

    const early = mount({ first: OLDEST.sha, last: MIDDLE.sha });
    expect(early.view.getByTestId('review-commit-ordinal')).toHaveTextContent('1–2/3');
  });

  it('lists every commit with its hash, whole subject and age, the picked ones checked, after an "All commits" row', async () => {
    const { view, onSelect } = mount(one(MIDDLE));
    await open(view);
    expect(view.getByTestId('review-commit-position')).toHaveTextContent('Commit 2 of 3');
    const all = view.getByTestId('review-commit-all');
    expect(all).toHaveTextContent('All commits');
    expect(all).toHaveTextContent('3');
    expect(all).not.toHaveTextContent('✓');
    const rows = view.getAllByTestId('review-commit-option');
    expect(rows[0]).toHaveTextContent('aaaaaaa');
    expect(rows[0]).toHaveTextContent(NEWEST.subject);
    expect(rows[0]).toHaveAttribute('title', NEWEST.subject);
    expect(rows[0].textContent).toMatch(/1m ago/);
    expect(rows[1]).toHaveTextContent('✓');
    expect(rows[2]).not.toHaveTextContent('✓');
    await fireEvent.click(rows[2]);
    expect(onSelect).toHaveBeenCalledWith(one(OLDEST));
    // A pick closes the menu.
    await waitFor(() => {
      expect(view.getByTestId('review-commit-select')).toHaveAttribute('aria-expanded', 'false');
    });
    expect(view.queryByTestId('review-commit-option')).toBeNull();
  });

  it('checks every commit of a run and names it in the header', async () => {
    const { view } = mount({ first: MIDDLE.sha, last: NEWEST.sha });
    await open(view);
    expect(view.getByTestId('review-commit-position')).toHaveTextContent('Commits 2–3 of 3');
    const rows = view.getAllByTestId('review-commit-option');
    expect(rows[0]).toHaveTextContent('✓');
    expect(rows[1]).toHaveTextContent('✓');
    expect(rows[2]).not.toHaveTextContent('✓');
  });

  it('shift+click spans from the picked commit to the clicked one, in either direction', async () => {
    const { view, onSelect } = mount(one(NEWEST));
    await open(view);
    await fireEvent.click(view.getAllByTestId('review-commit-option')[1], { shiftKey: true });
    expect(onSelect).toHaveBeenLastCalledWith({ first: MIDDLE.sha, last: NEWEST.sha });
    view.unmount();

    const older = mount(one(OLDEST));
    await open(older.view);
    await fireEvent.click(older.view.getAllByTestId('review-commit-option')[1], { shiftKey: true });
    expect(older.onSelect).toHaveBeenLastCalledWith({ first: OLDEST.sha, last: MIDDLE.sha });
  });

  it('a span over the whole list picks "All commits" (the whole PR has one representation)', async () => {
    const { view, onSelect } = mount(one(NEWEST));
    await open(view);
    await fireEvent.click(view.getAllByTestId('review-commit-option')[2], { shiftKey: true });
    expect(onSelect).toHaveBeenLastCalledWith(null);
  });

  it('a plain click after a shift+click picks one commit; without ranges shift does nothing more', async () => {
    const { view, onSelect } = mount(one(NEWEST), false);
    await open(view);
    await fireEvent.click(view.getAllByTestId('review-commit-option')[1], { shiftKey: true });
    expect(onSelect).toHaveBeenLastCalledWith(one(MIDDLE));
    expect(view.queryByText('From here')).toBeNull();
  });

  it('`From here` picks the commit through the newest; the oldest row has none (that is All commits)', async () => {
    const { view, onSelect } = mount(null);
    await open(view);
    const rows = view.getAllByTestId('review-commit-option');
    expect(rows[2].textContent).not.toContain('From here');
    const fromMiddle = rows[1].querySelector('button[aria-label^="Changes from"]');
    expect(fromMiddle).not.toBeNull();
    // Revealed on hover and always on compact; it holds its place.
    expect(fromMiddle!.className).toContain('opacity-0');
    expect(fromMiddle!.className).toContain('compact:opacity-100');
    await fireEvent.click(fromMiddle!);
    expect(onSelect).toHaveBeenLastCalledWith({ first: MIDDLE.sha, last: NEWEST.sha });
  });

  it('"All commits" selects null', async () => {
    const { view, onSelect } = mount(one(NEWEST));
    await open(view);
    await fireEvent.click(view.getByTestId('review-commit-all'));
    expect(onSelect).toHaveBeenCalledWith(null);
  });

  it('steps to the older commit on the left and the newer on the right, stopping at the ends', async () => {
    const { view, onSelect } = mount(one(MIDDLE));
    await open(view);
    expect(view.getByTestId('review-commit-prev')).toBeEnabled();
    expect(view.getByTestId('review-commit-next')).toBeEnabled();
    await fireEvent.click(view.getByTestId('review-commit-prev'));
    expect(onSelect).toHaveBeenLastCalledWith(one(OLDEST));
    await fireEvent.click(view.getByTestId('review-commit-next'));
    expect(onSelect).toHaveBeenLastCalledWith(one(NEWEST));
    // Stepping keeps the menu open: the header reads the new position.
    expect(view.getByTestId('review-commit-select')).toHaveAttribute('aria-expanded', 'true');
    view.unmount();

    const newest = mount(one(NEWEST));
    await open(newest.view);
    expect(newest.view.getByTestId('review-commit-next')).toBeDisabled();
    expect(newest.view.getByTestId('review-commit-prev')).toBeEnabled();
    newest.view.unmount();

    const oldest = mount(one(OLDEST));
    await open(oldest.view);
    expect(oldest.view.getByTestId('review-commit-next')).toBeEnabled();
    expect(oldest.view.getByTestId('review-commit-prev')).toBeDisabled();
  });

  it('steps out of a run past its ends, and into the list from "All commits"', async () => {
    const run = mount({ first: MIDDLE.sha, last: NEWEST.sha });
    await open(run.view);
    expect(run.view.getByTestId('review-commit-next')).toBeDisabled();
    await fireEvent.click(run.view.getByTestId('review-commit-prev'));
    expect(run.onSelect).toHaveBeenLastCalledWith(one(OLDEST));
    run.view.unmount();

    const { view, onSelect } = mount(null);
    await open(view);
    expect(view.getByTestId('review-commit-position')).toHaveTextContent('3 commits');
    await fireEvent.click(view.getByTestId('review-commit-prev'));
    expect(onSelect).toHaveBeenLastCalledWith(one(NEWEST));
    await fireEvent.click(view.getByTestId('review-commit-next'));
    expect(onSelect).toHaveBeenLastCalledWith(one(OLDEST));
  });

  it('falls back to "All commits" when the picked SHA is not in the list', () => {
    const { view } = mount(one({ ...NEWEST, sha: 'd'.repeat(40) }));
    expect(view.getByTestId('review-commit-select')).toHaveTextContent('All commits');
  });
});
