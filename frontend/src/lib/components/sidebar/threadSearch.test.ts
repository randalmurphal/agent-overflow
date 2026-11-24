import { describe, expect, it } from 'vitest';
import { threadGroupMatchesQuery, threadMatchesQuery } from './threadSearch';
import type { Thread, ThreadGroup } from '../../types/models';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 't1',
    title: 'My Thread',
    provider: 'claude',
    workspacePath: '/home/me/project',
    projectPath: '/home/me/project',
    mode: 'chat',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('threadMatchesQuery', () => {
  it.each([
    ['matches everything when the query is empty (no filter)', {}, '', true],
    ['matches on a title substring', { title: 'Deploy Script' }, 'deploy', true],
    ['matches on a workspace-path substring', { workspacePath: '/var/logs/app' }, 'logs', true],
    [
      'returns false when neither title nor path contains the query',
      { title: 'abc', workspacePath: '/x/y' },
      'zzz',
      false,
    ],
  ] as const)('%s', (_name, overrides, query, expected) => {
    expect(threadMatchesQuery(makeThread(overrides), query)).toBe(expected);
  });

  it('expects a pre-lowercased query — it lowercases the haystack, not the query', () => {
    // Callers normalize (ProjectsSection: query.trim().toLowerCase()). A raw
    // uppercase query is the caller's bug, and this pins that contract.
    expect(threadMatchesQuery(makeThread({ title: 'Deploy' }), 'DEPLOY')).toBe(false);
    expect(threadMatchesQuery(makeThread({ title: 'Deploy' }), 'deploy')).toBe(true);
  });

  it.each(['workflow', 'workflow-studio', 'workflow-triage'] as const)('never matches %s threads', (mode) => {
    expect(threadMatchesQuery(makeThread({ mode, title: 'Exact match' }), '')).toBe(false);
    expect(threadMatchesQuery(makeThread({ mode, title: 'Exact match' }), 'exact')).toBe(false);
  });
});

describe('threadGroupMatchesQuery', () => {
  function makeGroup(overrides: Partial<ThreadGroup> = {}): ThreadGroup {
    return {
      id: 'g1',
      projectId: 'p1',
      name: 'Release Prep',
      createdAt: 0,
      updatedAt: 0,
      ...overrides,
    };
  }

  it('matches everything when the query is empty (no filter)', () => {
    expect(threadGroupMatchesQuery(makeGroup(), '')).toBe(true);
  });

  it('matches on a name substring', () => {
    expect(threadGroupMatchesQuery(makeGroup(), 'release')).toBe(true);
    expect(threadGroupMatchesQuery(makeGroup(), 'prep')).toBe(true);
  });

  it('returns false when the name does not contain the query', () => {
    expect(threadGroupMatchesQuery(makeGroup(), 'zzz')).toBe(false);
  });

  it('expects a pre-lowercased query, like threadMatchesQuery', () => {
    expect(threadGroupMatchesQuery(makeGroup(), 'RELEASE')).toBe(false);
  });
});
