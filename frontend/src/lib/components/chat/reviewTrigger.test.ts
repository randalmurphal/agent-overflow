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
  identity: expect.any(String),
  threadId: 'thread-1',
  rowId: 'thread-1',
  workspace: { projectId: 'project-1', workspacePath: '/repo' },
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

  it.each([
    { name: 'with', opts: { filePath: 'src/foo.ts' } },
    { name: 'without', opts: undefined },
  ])('opens workspace scope $name a file target', ({ opts }) => {
    openReviewForItem(makeFakePane(), opts);

    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', SUBJECT, {
      scope: 'workspace',
      filePath: opts?.filePath,
      userInitiated: true,
    });
  });

  it.each([
    { name: 'Cmd-click', init: { metaKey: true }, promotes: true },
    { name: 'Ctrl-click', init: { ctrlKey: true }, promotes: true },
    { name: 'plain click', init: {}, promotes: false },
    { name: 'shift-only click', init: { shiftKey: true }, promotes: false },
  ])('isPromoteModifier is $promotes for $name', ({ init, promotes }) => {
    expect(isPromoteModifier(new MouseEvent('click', init))).toBe(promotes);
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
