import { describe, expect, it } from 'vitest';
import {
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
    expect(commitSelectionBetween(COMMITS, sha('e'), sha('c'))).toEqual({ first: sha('c'), last: sha('e') });
    expect(commitSelectionBetween(COMMITS, sha('c'), sha('e'))).toEqual({ first: sha('c'), last: sha('e') });
    expect(commitSelectionBetween(COMMITS, sha('d'), sha('d'))).toEqual({ first: sha('d'), last: sha('d') });
  });

  it('turns a run over the whole list into All commits, but keeps a one-commit list\'s commit', () => {
    expect(commitSelectionBetween(COMMITS, sha('a'), sha('f'))).toBeNull();
    expect(commitSelectionFromHere(COMMITS, sha('a'))).toBeNull();
    const only = [COMMITS[0]];
    expect(commitSelectionBetween(only, sha('f'), sha('f'))).toEqual({ first: sha('f'), last: sha('f') });
  });

  it('From here runs to the newest commit', () => {
    expect(commitSelectionFromHere(COMMITS, sha('c'))).toEqual({ first: sha('c'), last: sha('f') });
    expect(commitSelectionFromHere(COMMITS, sha('f'))).toEqual({ first: sha('f'), last: sha('f') });
  });

  it('shift+click spans from the picked commit (a run\'s newer end); a plain click picks one', () => {
    const picked = { first: sha('e'), last: sha('e') };
    expect(commitSelectionForClick(COMMITS, picked, sha('b'), true)).toEqual({ first: sha('b'), last: sha('e') });
    expect(commitSelectionForClick(COMMITS, { first: sha('c'), last: sha('e') }, sha('d'), true)).toEqual({ first: sha('d'), last: sha('e') });
    expect(commitSelectionForClick(COMMITS, picked, sha('b'), false)).toEqual({ first: sha('b'), last: sha('b') });
    expect(commitSelectionForClick(COMMITS, null, sha('b'), true)).toEqual({ first: sha('b'), last: sha('b') });
  });

  it('labels a pick by ordinals from the oldest: N/T, N–M/T, N–T/T to the head', () => {
    expect(commitSelectionLabel(COMMITS, null)).toBe('All commits');
    expect(commitSelectionLabel(COMMITS, { first: sha('d'), last: sha('d') })).toBe('4/6');
    expect(commitSelectionLabel(COMMITS, { first: sha('b'), last: sha('d') })).toBe('2–4/6');
    expect(commitSelectionLabel(COMMITS, { first: sha('c'), last: sha('f') })).toBe('3–6/6');
  });

  it('keeps a held run whose commits are still listed, and falls back, saying so, when one left', () => {
    const run = { first: sha('c'), last: sha('e') };
    expect(resolveCommitSelection(run, COMMITS)).toEqual({ selection: run, lost: false });
    const rebased = COMMITS.map((commit) => (commit.sha === sha('c') ? { ...commit, sha: 'z'.repeat(40) } : commit));
    expect(resolveCommitSelection(run, rebased)).toEqual({ selection: null, lost: true });
    // Its ends out of order (a reorder) is gone too.
    expect(resolveCommitSelection({ first: sha('e'), last: sha('c') }, COMMITS)).toEqual({ selection: null, lost: true });
    // Nothing held, nothing lost; a run grown to the whole list is the whole diff.
    expect(resolveCommitSelection(null, COMMITS)).toEqual({ selection: null, lost: false });
    expect(resolveCommitSelection({ first: sha('a'), last: sha('f') }, COMMITS)).toEqual({ selection: null, lost: false });
  });
});
