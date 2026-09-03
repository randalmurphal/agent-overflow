import { beforeEach, describe, expect, it, vi } from 'vitest';
import { isPromoteModifier, openReviewForItem } from './reviewTrigger';
import type { ThreadPane } from '../../stores/thread.svelte';
import { openReviewCompanion } from '../../stores/reviewPane.svelte';
import type { Thread } from '../../types/models';

// `reviewSubjectForPane` is the real thing: it IS the derivation under test
// here — what the trigger hands the companion — so mocking it would assert
// nothing. Only the companion open is stubbed.
vi.mock('../../stores/reviewPane.svelte', async (importOriginal) => ({
  ...(await importOriginal<typeof import('../../stores/reviewPane.svelte')>()),
  openReviewCompanion: vi.fn(async () => null),
}));

const THREAD = { id: 'thread-1', workspacePath: '/repo', projectId: 'project-1' } as Thread;
const SUBJECT = {
  identity: 'thread-1',
  threadId: 'thread-1',
  workspace: { projectId: 'project-1', workspacePath: '/repo' },
  thread: THREAD,
};

function makeFakePane(): ThreadPane {
  return {
    paneId: 'pane-1',
    threadId: 'thread-1',
    thread: THREAD,
    workspace: { projectId: 'project-1', workspacePath: '/repo' },
  } as unknown as ThreadPane;
}

describe('reviewTrigger', () => {
  beforeEach(() => {
    vi.mocked(openReviewCompanion).mockClear();
  });

  it('opens workspace scope with a file target', () => {
    openReviewForItem(makeFakePane(), { filePath: 'src/foo.ts' });

    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', SUBJECT, {
      scope: 'workspace',
      filePath: 'src/foo.ts',
    });
  });

  it('opens workspace scope without a file target', () => {
    openReviewForItem(makeFakePane());

    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', SUBJECT, {
      scope: 'workspace',
      filePath: undefined,
    });
  });

  it('isPromoteModifier returns true for Cmd-click and Ctrl-click', () => {
    const cmd = new MouseEvent('click', { metaKey: true });
    const ctrl = new MouseEvent('click', { ctrlKey: true });
    expect(isPromoteModifier(cmd)).toBe(true);
    expect(isPromoteModifier(ctrl)).toBe(true);
  });

  it('isPromoteModifier returns false for plain click and shift-only click', () => {
    const plain = new MouseEvent('click');
    const shift = new MouseEvent('click', { shiftKey: true });
    expect(isPromoteModifier(plain)).toBe(false);
    expect(isPromoteModifier(shift)).toBe(false);
  });
});

describe('reviewTrigger — no checkout', () => {
  it('does nothing for a pane whose thread names no workspace', () => {
    const terminalPane = {
      paneId: 'pane-1',
      threadId: 'thread-1',
      thread: { id: 'thread-1' } as Thread,
      workspace: null,
    } as unknown as ThreadPane;
    // A terminal-only thread has no checkout: the review companion has no
    // subject, and nothing throws.
    expect(() => openReviewForItem(terminalPane, { filePath: 'src/foo.ts' })).not.toThrow();
    expect(openReviewCompanion).not.toHaveBeenCalled();
  });
});
