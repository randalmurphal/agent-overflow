import { describe, expect, it } from 'vitest';
import { buildReviewSections } from './composerReviewTargets';
import { filterCommandSections, flattenSections } from './composerCommandEntries';
import type { BranchCommit } from '../../types/git';

function commit(shortSha: string, subject: string): BranchCommit {
  return { sha: `${shortSha}0000`, shortSha, subject, author: 'a', authoredAt: 0 } as BranchCommit;
}

describe('buildReviewSections', () => {
  const git = {
    branches: [
      { name: 'main', isDefault: true },
      { name: 'feat/parser', isCurrent: true },
    ],
    commits: [commit('abc1234', 'Fix the parser'), commit('def5678', 'Add a test')],
    loading: false,
    error: '',
  };

  it('always offers the two scopes that need no git data', () => {
    const names = flattenSections(
      buildReviewSections({ branches: [], commits: [], loading: true, error: '' }),
    ).map((e) => e.name);
    expect(names).toContain('uncommitted');
    expect(names).toContain('custom');
  });

  it('inserts only the argument, never a second /review', () => {
    const entries = flattenSections(buildReviewSections(git));
    for (const entry of entries) {
      expect(entry.insertText.startsWith('/review')).toBe(false);
    }
    expect(entries.find((e) => e.name === 'branch main')?.insertText).toBe('branch main ');
    expect(entries.find((e) => e.name === 'commit abc1234')?.insertText).toBe('commit abc1234 ');
  });

  it('shows commits by subject line, and finds them by it', () => {
    const sections = buildReviewSections(git);
    const commits = sections.find((s) => s.id === 'review-commits')!;
    expect(commits.entries[0]).toMatchObject({
      label: 'abc1234',
      description: 'Fix the parser',
    });
    const found = flattenSections(filterCommandSections(sections, 'parser')).map((e) => e.name);
    expect(found).toContain('commit abc1234');
  });

  it('says the git read is still running instead of showing an empty list', () => {
    const sections = buildReviewSections({ branches: [], commits: [], loading: true, error: '' });
    const loading = sections.find((s) => s.id === 'review-git-loading');
    expect(loading?.entries[0].disabled).toBe(true);
  });

  it('surfaces a git failure as a row rather than dropping the section', () => {
    const sections = buildReviewSections({
      branches: [],
      commits: [],
      loading: false,
      error: 'not a git repository',
    });
    const failed = sections.find((s) => s.id === 'review-git-error');
    expect(failed?.entries[0].description).toBe('not a git repository');
    expect(failed?.entries[0].disabled).toBe(true);
    // The scopes that don't need git are still offered.
    expect(flattenSections(sections).map((e) => e.name)).toContain('uncommitted');
  });
});
