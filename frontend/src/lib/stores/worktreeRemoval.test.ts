import { beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import {
  syncRemovedWorktreeThreads,
  TerminalsClosingNote,
  terminalsClosingNote,
  terminalsClosingNoteForThreadDelete,
  withTerminalsNote,
} from './worktreeRemoval.svelte';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { getBindingMock, resetBindingMocks, setBindingMock } from '../../test/mocks/bindings-app';
import { buildPane, makeThread } from '../../test/helpers/chat';
import { resetPanesForTest } from './panes.svelte';
import { resetToLocalPage } from '../../test/helpers/scopes';

vi.mock('../utils/frontendErrorCapture', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../utils/frontendErrorCapture')>()),
  reportFrontendDiagnostic: vi.fn(),
}));

const reported = vi.mocked(reportFrontendDiagnostic);

beforeEach(() => {
  resetBindingMocks();
  resetPanesForTest();
  resetToLocalPage();
  reported.mockClear();
});

describe('terminalsClosingNote', () => {
  it('says nothing for none and counts the rest', () => {
    expect(terminalsClosingNote(0)).toBe('');
    expect(terminalsClosingNote(undefined)).toBe('');
    expect(terminalsClosingNote(1)).toBe('1 terminal will close.');
    expect(terminalsClosingNote(3)).toBe('3 terminals will close.');
    expect(withTerminalsNote('Delete it.', '')).toBe('Delete it.');
    expect(withTerminalsNote('Delete it.', '1 terminal will close.')).toBe('Delete it. 1 terminal will close.');
  });
});

describe('terminalsClosingNoteForThreadDelete', () => {
  it('counts each worktree once and skips threads without one', async () => {
    const status = setBindingMock('GitWorktreeStatus', async (_ws: unknown, path: string) => ({
      path,
      terminals: path === '/wt/a' ? 2 : 1,
    }));
    const note = await terminalsClosingNoteForThreadDelete([
      makeThread({ id: 't1', workspacePath: '/wt/a', worktreePath: '/wt/a' }),
      makeThread({ id: 't2', workspacePath: '/wt/a', worktreePath: '/wt/a' }),
      makeThread({ id: 't3', workspacePath: '/wt/b', worktreePath: '/wt/b' }),
      makeThread({ id: 't4', workspacePath: '/repo', worktreePath: '' }),
    ]);
    expect(note).toBe('3 terminals will close.');
    expect(status).toHaveBeenCalledTimes(2);
  });

  it('reports a count it cannot read and leaves it out', async () => {
    setBindingMock('GitWorktreeStatus', async () => {
      throw new Error('worktree status: exit 128');
    });
    const note = await terminalsClosingNoteForThreadDelete([
      makeThread({ id: 't1', workspacePath: '/wt/a', worktreePath: '/wt/a' }),
    ]);
    expect(note).toBe('');
    expect(reported).toHaveBeenCalledTimes(1);
  });

  it('asks nothing when no thread is on a worktree', async () => {
    const note = await terminalsClosingNoteForThreadDelete([makeThread({ id: 't1', worktreePath: '' })]);
    expect(note).toBe('');
    expect(getBindingMock('GitWorktreeStatus')).toBeUndefined();
  });
});

describe('TerminalsClosingNote', () => {
  it('shows the answer for the current opening only', async () => {
    const note = new TerminalsClosingNote();
    let resolveFirst: (value: string) => void = () => {};
    note.load(() => new Promise((resolve) => { resolveFirst = resolve; }));
    note.load(async () => '1 terminal will close.');
    await Promise.resolve();
    await Promise.resolve();
    flushSync();
    expect(note.note).toBe('1 terminal will close.');
    resolveFirst('9 terminals will close.');
    await Promise.resolve();
    expect(note.note).toBe('1 terminal will close.');

    let resolveLate: (value: string) => void = () => {};
    note.load(() => new Promise((resolve) => { resolveLate = resolve; }));
    note.clear();
    resolveLate('2 terminals will close.');
    await Promise.resolve();
    expect(note.note).toBe('');
  });
});

describe('syncRemovedWorktreeThreads', () => {
  it('applies each moved row to the pane showing it', async () => {
    const onWorktree = makeThread({ id: 'thread-moved', workspacePath: '/wt/a', worktreePath: '/wt/a', branch: 'feat' });
    const pane = await buildPane(onWorktree);
    syncRemovedWorktreeThreads({
      workspace: { workspacePath: '/tmp/workspace', worktreePath: '', branch: 'main' },
      reattached: [{ ...onWorktree, workspacePath: '/tmp/workspace', worktreePath: '', branch: 'main' }],
    });
    expect(pane.thread?.workspacePath).toBe('/tmp/workspace');
    expect(pane.thread?.worktreePath).toBe('');
    expect(pane.thread?.branch).toBe('main');
    syncRemovedWorktreeThreads({ workspace: { workspacePath: '', worktreePath: '', branch: '' }, reattached: null });
    syncRemovedWorktreeThreads(undefined);
  });
});
