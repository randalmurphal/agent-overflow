import { describe, expect, it } from 'vitest';
import { orderPickerRows, TAG_ROW_CAP } from './branchPickerRows';
import type { GitBranch, GitTag } from '../../../types/git';

function branch(name: string, date: number, extra: Partial<GitBranch> = {}): GitBranch {
  return { name, isCurrent: false, isDefault: false, date, ...extra };
}

function tag(name: string, date: number): GitTag {
  return { name, date };
}

const refs = (result: ReturnType<typeof orderPickerRows>) => result.rows.map((row) => row.ref);

describe('orderPickerRows', () => {
  const branches = [
    branch('feat/new', 300),
    branch('main', 100, { isDefault: true }),
    branch('feat/old', 50),
  ];
  const tags = [tag('v3', 400), tag('v2', 200), tag('v1', 10)];

  it('puts unpicked tags after every branch with no search text', () => {
    expect(refs(orderPickerRows({ branches, tags, recent: [], query: '' }))).toEqual([
      'main',
      'feat/new',
      'feat/old',
      'refs/tags/v3',
      'refs/tags/v2',
      'refs/tags/v1',
    ]);
  });

  it('lifts recent picks, tags included, below the default branch by recency', () => {
    const recent = ['refs/tags/v1', 'feat/old', 'main'];
    expect(refs(orderPickerRows({ branches, tags, recent, query: '' }))).toEqual([
      'main',
      'refs/tags/v1',
      'feat/old',
      'feat/new',
      'refs/tags/v3',
      'refs/tags/v2',
    ]);
  });

  it('keeps a branch and a tag of one name distinct in recent picks', () => {
    const result = orderPickerRows({
      branches: [branch('main', 1, { isDefault: true }), branch('v2', 5)],
      tags: [tag('v2', 9)],
      recent: ['refs/tags/v2'],
      query: '',
    });
    expect(refs(result)).toEqual(['main', 'refs/tags/v2', 'v2']);
  });

  it('merges matching branches and tags by date while searching', () => {
    const result = orderPickerRows({
      branches: [...branches, branch('v-branch', 250)],
      tags,
      recent: ['feat/old'],
      query: 'V',
    });
    // `main` does not match; the recent pick does not match either.
    expect(refs(result)).toEqual([
      'refs/tags/v3',
      'v-branch',
      'refs/tags/v2',
      'refs/tags/v1',
    ]);
  });

  it('keeps default and recent picks ahead of the date merge while searching', () => {
    const result = orderPickerRows({ branches, tags, recent: ['refs/tags/v1'], query: 'e' });
    // Every name with an "e": main has none; feat/* do; no tag has one.
    expect(refs(result)).toEqual(['feat/new', 'feat/old']);
    const withTag = orderPickerRows({ branches, tags, recent: ['refs/tags/v1'], query: '' });
    expect(withTag.rows[1].ref).toBe('refs/tags/v1');
  });

  it('caps unpicked tag rows at the newest matches and counts the rest', () => {
    const many = Array.from({ length: TAG_ROW_CAP + 7 }, (_, i) => tag(`t${i}`, 10_000 - i));
    const result = orderPickerRows({ branches: [], tags: many, recent: ['refs/tags/t55'], query: '' });
    const tagRefs = refs(result);
    expect(tagRefs[0]).toBe('refs/tags/t55');
    expect(tagRefs).toHaveLength(TAG_ROW_CAP + 1);
    expect(tagRefs[1]).toBe('refs/tags/t0');
    expect(tagRefs.at(-1)).toBe(`refs/tags/t${TAG_ROW_CAP - 1}`);
    // 57 tags: one recent, 50 rendered, 6 hidden.
    expect(result.hiddenTags).toBe(6);
  });

  it('applies the cap to matching tags only', () => {
    const many = Array.from({ length: TAG_ROW_CAP * 2 }, (_, i) => tag(i % 2 ? `odd${i}` : `even${i}`, 1000 - i));
    const result = orderPickerRows({ branches: [], tags: many, recent: [], query: 'odd' });
    expect(result.rows).toHaveLength(TAG_ROW_CAP);
    expect(result.hiddenTags).toBe(0);
    expect(result.rows.every((row) => row.name.startsWith('odd'))).toBe(true);
  });

  it('keeps a tag named exactly as the query even when newer matches fill the cap', () => {
    const builds = Array.from({ length: TAG_ROW_CAP + 5 }, (_, i) => tag(`v1.0-build-${i}`, 10_000 - i));
    const result = orderPickerRows({ branches: [], tags: [...builds, tag('v1.0', 1)], recent: [], query: 'V1.0' });
    expect(refs(result)).toContain('refs/tags/v1.0');
    expect(result.rows).toHaveLength(TAG_ROW_CAP);
    expect(result.hiddenTags).toBe(builds.length + 1 - TAG_ROW_CAP);
  });

  it('keeps tags starting with the query ahead of other matches when the cap bites', () => {
    const inner = Array.from({ length: TAG_ROW_CAP }, (_, i) => tag(`x-rc-${i}`, 10_000 - i));
    const prefixed = [tag('rc-1', 5), tag('rc-2', 4)];
    const result = orderPickerRows({ branches: [], tags: [...inner, ...prefixed], recent: [], query: 'rc' });
    expect(refs(result)).toEqual(expect.arrayContaining(['refs/tags/rc-1', 'refs/tags/rc-2']));
    expect(result.rows).toHaveLength(TAG_ROW_CAP);
    expect(result.hiddenTags).toBe(2);
  });
});
