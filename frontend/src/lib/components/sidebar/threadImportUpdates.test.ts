// Provider-update check/apply copy mapping. Six statuses come back and only
// one of them opens a dialog; the other five have to SAY something, because
// a menu item that does nothing visible is indistinguishable from a broken
// one. These tests pin which of them speak with the backend's own prose,
// which fall back, and which tone each carries.

import { describe, expect, it, beforeEach, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import {
  applyThreadImportUpdatesAction,
  checkThreadImportUpdatesAction,
} from './threadImportUpdates';
import type { ThreadActionCtx } from './threadRowActions';
import type { Thread } from '../../types/models';

function makeCtx(thread: Partial<Thread> = {}): ThreadActionCtx {
  const t: Thread = {
    id: 'thread-1',
    title: 'Imported thread',
    provider: 'claude',
    projectId: 'project-1',
    workspacePath: '/tmp/work',
    projectPath: '/tmp/work',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
    importSource: 'claude',
    ...thread,
  };
  return {
    thread: t,
    isActive: false,
    clearPane: vi.fn(),
    switchPane: vi.fn(async () => {}),
    reportError: vi.fn(),
  };
}

function toasts(): Array<{ type: string; message: string }> {
  return getToasts().map((t) => ({ type: t.type, message: t.message }));
}

beforeEach(() => {
  resetBindingMocks();
  for (const toast of [...getToasts()]) removeToast(toast.id);
  // refreshSidebarProjections runs for real on the apply path.
  setBindingMock('ListThreads', async () => []);
  setBindingMock('ListProjects', async () => []);
  vi.spyOn(console, 'error').mockImplementation(() => {});
});

describe('checkThreadImportUpdatesAction', () => {
  it('returns the plan — and nothing else — when there is something to apply', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'updates-available',
      newItems: 12,
      newTurns: 3,
      detail: '12 new messages and 3 turns can be added from the session file.',
    }));

    const status = await checkThreadImportUpdatesAction(makeCtx());

    expect(status).toMatchObject({ status: 'updates-available', newItems: 12 });
    // The dialog is the answer here; a toast as well would say it twice.
    expect(toasts()).toEqual([]);
  });

  it('answers the question when nothing changed rather than staying silent', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'up-to-date',
      newItems: 0,
      newTurns: 0,
    }));

    expect(await checkThreadImportUpdatesAction(makeCtx())).toBeNull();
    expect(toasts()).toEqual([{ type: 'info', message: 'No new items.' }]);
  });

  it.each([
    ['diverged-local', 'warning'],
    ['source-missing', 'warning'],
    ['source-diverged', 'warning'],
    ['not-imported', 'info'],
  ])('surfaces the backend prose for %s', async (status, tone) => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status,
      newItems: 0,
      newTurns: 0,
      detail: `because of ${status}`,
    }));

    expect(await checkThreadImportUpdatesAction(makeCtx())).toBeNull();
    expect(toasts()).toEqual([{ type: tone, message: `because of ${status}` }]);
  });

  it('falls back to its own wording when the backend sends no prose', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'source-missing',
      newItems: 0,
      newTurns: 0,
    }));

    await checkThreadImportUpdatesAction(makeCtx());
    expect(toasts()[0]).toMatchObject({ type: 'warning' });
    expect(toasts()[0].message).toMatch(/session file/);
  });

  it('treats a status it does not know as unavailable rather than actionable', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => ({
      threadId: 'thread-1',
      status: 'something-new',
      newItems: 5,
      newTurns: 1,
    }));

    expect(await checkThreadImportUpdatesAction(makeCtx())).toBeNull();
    expect(toasts()[0].message).toMatch(/not available/);
  });

  it('reports a failed check instead of looking like "no updates"', async () => {
    setBindingMock('CheckThreadImportUpdates', async () => {
      throw new Error('read session file: permission denied');
    });

    expect(await checkThreadImportUpdatesAction(makeCtx())).toBeNull();
    expect(toasts()[0]).toMatchObject({ type: 'error' });
    expect(toasts()[0].message).toMatch(/permission denied/i);
  });
});

describe('applyThreadImportUpdatesAction', () => {
  it('applies, reports the count, and resyncs the sidebar', async () => {
    const apply = setBindingMock('ImportThreadUpdates', async () => ({
      appliedItems: 12,
      appliedTurns: 3,
    }));
    const listThreads = setBindingMock('ListThreads', async () => []);

    await applyThreadImportUpdatesAction(makeCtx());

    expect(apply).toHaveBeenCalledWith('thread-1');
    // Both halves of what landed: items are what the timeline gains, turns
    // are how many exchanges they came from. The backend computes both.
    expect(toasts()).toEqual([
      { type: 'info', message: 'Imported 12 new items across 3 turns.' },
    ]);
    expect(listThreads).toHaveBeenCalled();
  });

  it('singularises both counts', async () => {
    setBindingMock('ImportThreadUpdates', async () => ({ appliedItems: 1, appliedTurns: 1 }));
    await applyThreadImportUpdatesAction(makeCtx());
    expect(toasts()[0].message).toBe('Imported 1 new item across 1 turn.');
  });

  it('omits the turn clause when the backend reports none', async () => {
    setBindingMock('ImportThreadUpdates', async () => ({ appliedItems: 4, appliedTurns: 0 }));
    await applyThreadImportUpdatesAction(makeCtx());
    expect(toasts()[0].message).toBe('Imported 4 new items.');
  });

  it('reports a profile-only repair without claiming it imported zero items', async () => {
    setBindingMock('ImportThreadUpdates', async () => ({
      appliedItems: 0,
      appliedTurns: 0,
      restoredModelProfile: true,
    }));
    await applyThreadImportUpdatesAction(makeCtx());
    expect(toasts()[0].message).toBe(
      'Restored the model settings recorded in the provider session.',
    );
  });

  it('reports both effects when history and its newer model profile land together', async () => {
    setBindingMock('ImportThreadUpdates', async () => ({
      appliedItems: 4,
      appliedTurns: 1,
      restoredModelProfile: true,
    }));
    await applyThreadImportUpdatesAction(makeCtx());
    expect(toasts()[0].message).toBe(
      'Imported 4 new items across 1 turn. Restored its recorded model settings.',
    );
  });

  it('surfaces a refusal — the backend re-plans, so an earlier check is not a promise', async () => {
    const listThreads = setBindingMock('ListThreads', async () => []);
    setBindingMock('ImportThreadUpdates', async () => {
      throw new Error('thread has been continued in Agent Overflow since it was imported');
    });

    await applyThreadImportUpdatesAction(makeCtx());

    expect(toasts()[0]).toMatchObject({ type: 'error' });
    // Nothing landed, so nothing is resynced.
    expect(listThreads).not.toHaveBeenCalled();
  });
});
