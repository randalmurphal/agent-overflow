import { describe, expect, it } from 'vitest';
import {
  commitListNote,
  commitOrdinal,
  commitSelectionBetween,
  commitSelectionForClick,
  commitSelectionFromHere,
  commitSelectionLabel,
  resolveCommitSelection,
} from './commitRange';
import type { BranchCommit } from '../types/git';

// Newest first, as the PR lists them: f is the head, a the first commit.
const COMMITS: BranchCommit[] = ['f', 'e', 'd', 'c', 'b', 'a'].map((id, index) => ({
  sha: id.repeat(40), shortSha: id.repeat(7), subject: id, body: '', author: 'x', authoredAt: 1000 - index,
}));
const sha = (id: string) => id.repeat(40);

describe('commit selections', () => {
  it('orders a run\'s ends whichever end was clicked first', () => {
    expect(commitSelectionBetween(COMMITS, 6, sha('e'), sha('c'))).toEqual({ first: sha('c'), last: sha('e') });
    expect(commitSelectionBetween(COMMITS, 6, sha('c'), sha('e'))).toEqual({ first: sha('c'), last: sha('e') });
    expect(commitSelectionBetween(COMMITS, 6, sha('d'), sha('d'))).toEqual({ first: sha('d'), last: sha('d') });
  });

  it('turns a run over the whole list into All commits, but keeps a one-commit list\'s commit', () => {
    expect(commitSelectionBetween(COMMITS, 6, sha('a'), sha('f'))).toBeNull();
    expect(commitSelectionFromHere(COMMITS, 6, sha('a'))).toBeNull();
    const only = [COMMITS[0]];
    expect(commitSelectionBetween(only, 1, sha('f'), sha('f'))).toEqual({ first: sha('f'), last: sha('f') });
  });

  it('From here runs to the newest commit', () => {
    expect(commitSelectionFromHere(COMMITS, 6, sha('c'))).toEqual({ first: sha('c'), last: sha('f') });
    expect(commitSelectionFromHere(COMMITS, 6, sha('f'))).toEqual({ first: sha('f'), last: sha('f') });
  });

  it('shift+click spans from the picked commit (a run\'s newer end); a plain click picks one', () => {
    const picked = { first: sha('e'), last: sha('e') };
    expect(commitSelectionForClick(COMMITS, 6, picked, sha('b'), true)).toEqual({ first: sha('b'), last: sha('e') });
    expect(commitSelectionForClick(COMMITS, 6, { first: sha('c'), last: sha('e') }, sha('d'), true)).toEqual({ first: sha('d'), last: sha('e') });
    expect(commitSelectionForClick(COMMITS, 6, picked, sha('b'), false)).toEqual({ first: sha('b'), last: sha('b') });
    expect(commitSelectionForClick(COMMITS, 6, null, sha('b'), true)).toEqual({ first: sha('b'), last: sha('b') });
  });

  it('labels a pick by ordinals from the oldest: N/T, N–M/T, N–T/T to the head', () => {
    expect(commitSelectionLabel(COMMITS, 6, null)).toBe('All commits');
    expect(commitSelectionLabel(COMMITS, 6, { first: sha('d'), last: sha('d') })).toBe('4/6');
    expect(commitSelectionLabel(COMMITS, 6, { first: sha('b'), last: sha('d') })).toBe('2–4/6');
    expect(commitSelectionLabel(COMMITS, 6, { first: sha('c'), last: sha('f') })).toBe('3–6/6');
  });

  it('keeps a held run whose commits are still listed, and falls back, saying so, when one left', () => {
    const run = { first: sha('c'), last: sha('e') };
    expect(resolveCommitSelection(run, COMMITS, COMMITS.length)).toEqual({ selection: run, lost: false });
    const rebased = COMMITS.map((commit) => (commit.sha === sha('c') ? { ...commit, sha: 'z'.repeat(40) } : commit));
    expect(resolveCommitSelection(run, rebased, rebased.length)).toEqual({ selection: null, lost: true });
    // Its ends out of order (a reorder) is gone too.
    expect(resolveCommitSelection({ first: sha('e'), last: sha('c') }, COMMITS, COMMITS.length)).toEqual({ selection: null, lost: true });
    // Nothing held, nothing lost; a run grown to the whole list is the whole diff.
    expect(resolveCommitSelection(null, COMMITS, COMMITS.length)).toEqual({ selection: null, lost: false });
    expect(resolveCommitSelection({ first: sha('a'), last: sha('f') }, COMMITS, COMMITS.length)).toEqual({ selection: null, lost: false });
  });
});

// The list as the backend answers it past its cap: the newest of more
// commits than it holds (`total`), or of an uncounted number (null).
describe('a commit list short of the whole range', () => {
  const lists = [
    { name: 'complete', total: 6 },
    { name: 'truncated', total: 900 },
    { name: 'uncounted', total: null },
  ] as const;

  it.each([
    { name: 'complete', total: 6, newest: 6, oldestListed: 1, run: '3–6/6', single: '4/6', note: null, whole: null },
    { name: 'truncated', total: 900, newest: 900, oldestListed: 895, run: '897–900/900', single: '898/900', note: 'Only the newest 6 of 900 commits are listed.', whole: { first: sha('a'), last: sha('f') } },
    { name: 'uncounted', total: null, newest: null, oldestListed: null, run: 'ccccccc–fffffff', single: 'ddddddd', note: 'Only the newest 6 commits are listed.', whole: { first: sha('a'), last: sha('f') } },
  ])('$name: ordinals, labels, the menu note and the listed ends', ({ total, newest, oldestListed, run, single, note, whole }) => {
    expect(commitOrdinal(0, total)).toBe(newest);
    expect(commitOrdinal(5, total)).toBe(oldestListed);
    expect(commitSelectionLabel(COMMITS, total, { first: sha('c'), last: sha('f') })).toBe(run);
    expect(commitSelectionLabel(COMMITS, total, { first: sha('d'), last: sha('d') })).toBe(single);
    expect(commitSelectionLabel(COMMITS, total, null)).toBe('All commits');
    expect(commitListNote(COMMITS, total)).toBe(note);
    // The listed ends are the whole PR only when the list is complete.
    expect(commitSelectionBetween(COMMITS, total, sha('a'), sha('f'))).toEqual(whole);
    expect(commitSelectionFromHere(COMMITS, total, sha('a'))).toEqual(whole);
    expect(resolveCommitSelection({ first: sha('a'), last: sha('f') }, COMMITS, total)).toEqual({ selection: whole, lost: false });
  });

  it('keeps a pick that left a truncated list lost, as a complete one does', () => {
    for (const { total } of lists) {
      expect(resolveCommitSelection({ first: sha('z'), last: sha('f') }, COMMITS, total)).toEqual({ selection: null, lost: true });
    }
  });
});
