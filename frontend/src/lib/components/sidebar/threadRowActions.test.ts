import { describe, expect, it, beforeEach, vi } from 'vitest';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { openThreadWorkspaceInEditorAction, type ThreadActionCtx } from './threadRowActions';
import { getToasts, removeToast } from '../../stores/toast.svelte';
import type { Thread } from '../../types/models';

function makeCtx(thread: Partial<Thread>): ThreadActionCtx {
  const t: Thread = {
    id: 'thread-1',
    title: 'Test',
    provider: 'claude',
    workspacePath: '/tmp/work',
    projectPath: '/tmp/work',
    model: 'claude-sonnet-4-6',
    createdAt: 0,
    updatedAt: 0,
    archived: false,
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

describe('openThreadWorkspaceInEditorAction', () => {
  beforeEach(() => {
    resetBindingMocks();
    for (const toast of [...getToasts()]) removeToast(toast.id);
  });

  it('calls OpenInEditor with the thread workspace path and (0,0)', async () => {
    const mock = setBindingMock('OpenInEditor', vi.fn(async () => undefined));
    await openThreadWorkspaceInEditorAction(makeCtx({ workspacePath: '/Users/me/repo' }));
    expect(mock).toHaveBeenCalledTimes(1);
    expect(mock.mock.calls[0]).toEqual(['/Users/me/repo', 0, 0]);
  });

  it('toasts the user-facing form of the binding error on failure', async () => {
    setBindingMock('OpenInEditor', vi.fn(async () => {
      throw new Error('no editor available');
    }));
    await openThreadWorkspaceInEditorAction(makeCtx({ workspacePath: '/x' }));
    // userFacingError capitalises and ensures terminal punctuation; the
    // toast carries the polished form, never the verbatim Go wrap text.
    const match = getToasts().find((t) => t.message === 'No editor available.');
    expect(match?.type).toBe('error');
  });
});
