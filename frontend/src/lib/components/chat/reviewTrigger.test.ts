import { beforeEach, describe, expect, it, vi } from 'vitest';
import { isPromoteModifier, openReviewForItem } from './reviewTrigger';
import type { ThreadPane } from '../../stores/thread.svelte';
import { openReviewCompanion } from '../../stores/reviewPane.svelte';

vi.mock('../../stores/reviewPane.svelte', () => ({
  openReviewCompanion: vi.fn(async () => null),
}));

function makeFakePane(): ThreadPane {
  return {
    paneId: 'pane-1',
    threadId: 'thread-1',
    checkpoints: {
      checkpoints: [
        { userItemId: 'user-1', turnIndex: 1 },
        { userItemId: 'user-3', turnIndex: 3 },
      ],
    },
  } as unknown as ThreadPane;
}

describe('reviewTrigger', () => {
  beforeEach(() => {
    vi.mocked(openReviewCompanion).mockClear();
  });

  it('opens the matching turn checkpoint with a file target', () => {
    openReviewForItem(makeFakePane(), { turnIndex: 3, filePath: 'src/foo.ts' });

    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', 'thread-1', {
      scope: 'turn',
      checkpointUserItemId: 'user-3',
      filePath: 'src/foo.ts',
    });
  });

  it('falls back to workspace scope when the turn has no checkpoint', () => {
    openReviewForItem(makeFakePane(), { turnIndex: 2, filePath: 'src/foo.ts' });

    expect(openReviewCompanion).toHaveBeenCalledWith('pane-1', 'thread-1', {
      scope: 'workspace',
      filePath: 'src/foo.ts',
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
