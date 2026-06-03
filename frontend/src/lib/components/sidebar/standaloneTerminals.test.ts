import { describe, expect, it } from 'vitest';
import { selectStandaloneTerminals } from './standaloneTerminals';
import type { Thread } from '../../types/models';

function makeThread(overrides: Partial<Thread> = {}): Thread {
  return {
    id: 'term-1',
    title: 'home',
    provider: 'claude',
    workspacePath: '/home/me',
    projectPath: '',
    mode: 'terminal',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    ...overrides,
  };
}

describe('selectStandaloneTerminals', () => {
  it('keeps project-less terminal threads', () => {
    const threads = [makeThread({ id: 'a' }), makeThread({ id: 'b' })];
    expect(selectStandaloneTerminals(threads, '').map((t) => t.id)).toEqual(['a', 'b']);
  });

  it('excludes per-project terminals (projectId set) — they live under their project', () => {
    const threads = [
      makeThread({ id: 'home', projectId: undefined }),
      makeThread({ id: 'scoped', projectId: 'proj-1' }),
    ];
    expect(selectStandaloneTerminals(threads, '').map((t) => t.id)).toEqual(['home']);
  });

  it('excludes non-terminal threads', () => {
    const threads = [
      makeThread({ id: 'term', mode: 'terminal' }),
      makeThread({ id: 'chat', mode: 'chat' }),
      makeThread({ id: 'design', mode: 'design' }),
    ];
    expect(selectStandaloneTerminals(threads, '').map((t) => t.id)).toEqual(['term']);
  });

  it('excludes archived terminals', () => {
    const threads = [
      makeThread({ id: 'live', archived: false }),
      makeThread({ id: 'gone', archived: true }),
    ];
    expect(selectStandaloneTerminals(threads, '').map((t) => t.id)).toEqual(['live']);
  });

  it('preserves the input order (backend returns newest-touched first)', () => {
    const threads = [makeThread({ id: 'x' }), makeThread({ id: 'y' }), makeThread({ id: 'z' })];
    expect(selectStandaloneTerminals(threads, '').map((t) => t.id)).toEqual(['x', 'y', 'z']);
  });

  it('filters by query against title and workspace path', () => {
    const threads = [
      makeThread({ id: 'notes', title: 'Notes', workspacePath: '/home/me' }),
      makeThread({ id: 'logs', title: 'scratch', workspacePath: '/var/logs' }),
    ];
    // Matches the title of the first.
    expect(selectStandaloneTerminals(threads, 'notes').map((t) => t.id)).toEqual(['notes']);
    // Matches the workspace path of the second.
    expect(selectStandaloneTerminals(threads, 'logs').map((t) => t.id)).toEqual(['logs']);
    // No match → empty.
    expect(selectStandaloneTerminals(threads, 'zzz')).toEqual([]);
  });

  it('returns everything when the query is empty', () => {
    const threads = [makeThread({ id: 'a' }), makeThread({ id: 'b' })];
    expect(selectStandaloneTerminals(threads, '')).toHaveLength(2);
  });
});
