// The list find's match over each tab's rows, and its effect on the groups
// and the Needs you count the Merge requests tab shows.

import { describe, expect, it } from 'vitest';
import type { PRListItem, PRListSnapshot, ProjectPRList } from './bindings';
import { groupRows, needsYouCount, pipelineRowMatches, pullRowMatches, type PullsListedRow } from './pullsPageRows';
import { DEFAULT_MR_SORT, type MRFilters } from './pullsPageFilters.svelte';
import type { PipelineRowView } from '../components/pulls/pipelineRowView';
import { HOME_BACKEND } from '../transport/backendKey';
import { findNeedle } from '../utils/findMatch';

function item(number: number, overrides: Partial<PRListItem> = {}): PRListItem {
  return {
    ref: { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'widgets', Number: number },
    url: `https://gitlab.com/group/widgets/-/merge_requests/${number}`,
    title: `Change ${number}`,
    draft: false,
    author: { login: 'sab', name: 'Sawmon Abossedgh' },
    createdAt: '2026-10-01T00:00:00Z',
    updatedAt: `2026-10-${String(10 + number).padStart(2, '0')}T00:00:00Z`,
    headRefName: `feature-${number}`,
    crossRepository: false,
    viewerIsAuthor: false,
    viewerIsReviewer: false,
    state: 'open',
    group: 'waiting',
    reason: { kind: '', commits: 0, replies: 0 },
    approvals: null,
    unresolved: null,
    ci: '',
    ...overrides,
  } as PRListItem;
}

function project(id: string, name: string, rows: PRListItem[]): ProjectPRList {
  return { projectId: id, name, repo: { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'widgets' }, rows } as ProjectPRList;
}

function listed(row: PRListItem, name = 'Widgets'): PullsListedRow {
  return {
    key: String(row.ref.Number),
    pr: { forge: 'gitlab', host: 'gitlab.com', namespace: 'group', repo: 'widgets', number: row.ref.Number },
    item: row,
    project: project('p1', name, [row]),
  };
}

const matches = (row: PullsListedRow, query: string) => pullRowMatches(row, findNeedle(query));

describe('a merge request row matches', () => {
  const row = listed(item(4323, { title: 'Queue: retry a failed delivery', headRefName: 'retry-once' }));

  it('on its title, literally and in any case', () => {
    expect(matches(row, 'RETRY a')).toBe(true);
    expect(matches(row, '  failed delivery ')).toBe(true);
    expect(matches(row, 'retr.')).toBe(false);
    expect(matches(row, 'retry * failed')).toBe(false);
  });

  it('on its number as written, with or without the sign', () => {
    expect(matches(row, '4323')).toBe(true);
    expect(matches(row, '#4323')).toBe(true);
    expect(matches(row, '!4323')).toBe(true);
    expect(matches(row, '#432')).toBe(true);
    expect(matches(row, '#5')).toBe(false);
  });

  it('on its source branch, author name or login, project and state label', () => {
    expect(matches(row, 'retry-once')).toBe(true);
    expect(matches(row, 'abossedgh')).toBe(true);
    expect(matches(row, 'sab')).toBe(true);
    expect(matches(row, 'widgets')).toBe(true);
    expect(matches(row, 'open')).toBe(true);
    expect(matches(row, 'main')).toBe(false);
  });

  it('as "you" only on the reader\'s own', () => {
    expect(matches(row, 'you')).toBe(false);
    expect(matches(listed(item(1, { viewerIsAuthor: true })), 'you')).toBe(true);
  });

  it('always for a blank query', () => {
    expect(matches(row, '')).toBe(true);
    expect(matches(row, '   ')).toBe(true);
  });
});

describe('a pipeline row matches', () => {
  const row: PipelineRowView = {
    key: 'k', id: '9002', status: 'failed', name: 'CI', ref: 'v3.9.2', refKind: 'tag', mrNumber: 0, forge: 'gitlab',
    sha: 'b3f0c2d4e5f60718293a4b5c6d7e8f9012345678', shortSHA: 'b3f0c2d', title: 'Release notes', projectName: 'Widgets',
    age: '1h ago', url: '', stages: null, stagesError: null,
  };
  const pipe = (query: string, view: PipelineRowView = row) => pipelineRowMatches(view, findNeedle(query));

  it('on its ref, commit title, status in words and project', () => {
    expect(pipe('V3.9')).toBe(true);
    expect(pipe('release NOTES')).toBe(true);
    expect(pipe('failed')).toBe(true);
    expect(pipe('passed')).toBe(false);
    expect(pipe('widgets')).toBe(true);
  });

  it('on a prefix of its commit sha, never the middle of one', () => {
    expect(pipe('b3f0')).toBe(true);
    expect(pipe('B3F0C2D4E5')).toBe(true);
    expect(pipe('c2d4e5')).toBe(false);
  });

  it('on its id and workflow name, and a merge request ref by its number', () => {
    expect(pipe('#9002')).toBe(true);
    expect(pipe('ci')).toBe(true);
    const mr = { ...row, refKind: 'merge_request', ref: 'refs/merge-requests/12/head', mrNumber: 12 };
    expect(pipe('!12', mr)).toBe(true);
  });
});

describe('the find needle over the Merge requests tab', () => {
  const snapshot = {
    seq: 1, fetchedAt: '', stale: false,
    projects: [
      project('p1', 'Widgets', [
        item(1, { title: 'Retry delivery', group: 'needs_you' }),
        item(2, { title: 'Speed up builds', group: 'needs_you' }),
        item(3, { title: 'Retry webhooks for École' }),
      ]),
      project('p2', 'Gadgets', [{ ...item(4, { title: 'Retry header' }), ref: { Forge: 'gitlab', Host: 'gitlab.com', Namespace: 'group', Repo: 'gadgets', Number: 4 } } as PRListItem]),
    ],
  } as PRListSnapshot;
  const filters: MRFilters = { filter: 'all', projects: [], sort: DEFAULT_MR_SORT };
  const counts = (needle: string, picked = filters) =>
    groupRows(snapshot, HOME_BACKEND, picked, needle).map((group) => [group.group, group.rows.map((row) => row.number)]);

  it('narrows every group, and drops a group with no match', () => {
    expect(counts('')).toEqual([['needs_you', [2, 1]], ['waiting', [4, 3]]]);
    expect(counts(findNeedle('retry'))).toEqual([['needs_you', [1]], ['waiting', [4, 3]]]);
    expect(counts(findNeedle('builds'))).toEqual([['needs_you', [2]]]);
    expect(counts(findNeedle('nothing'))).toEqual([]);
    // Every code point folds, not only ASCII.
    expect(counts(findNeedle('école'))).toEqual([['waiting', [3]]]);
    expect(counts(findNeedle('ÉCOLE'))).toEqual([['waiting', [3]]]);
  });

  it('composes with the project picker and the filter', () => {
    expect(counts(findNeedle('retry'), { ...filters, projects: ['p2'] })).toEqual([['waiting', [4]]]);
    expect(counts(findNeedle('retry'), { ...filters, filter: 'needs_you' })).toEqual([['needs_you', [1]]]);
  });

  it('counts Needs you over the rows that match', () => {
    expect(needsYouCount(snapshot, HOME_BACKEND, [])).toBe(2);
    expect(needsYouCount(snapshot, HOME_BACKEND, [], findNeedle('retry'))).toBe(1);
    expect(needsYouCount(snapshot, HOME_BACKEND, ['p2'], findNeedle('retry'))).toBe(0);
  });
});
