import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewPRThreadRow from './ReviewPRThreadRow.svelte';
import type { ReviewThread } from '../../types/models';
import type { CommentAnchor } from '../../utils/reviewRows';

const thread: ReviewThread = {
  id: 't1',
  path: 'src/a.ts',
  line: 5,
  side: 'RIGHT',
  isResolvable: true,
  isResolved: false,
  isOutdated: false,
  comments: [{ authorLogin: 'alice', body: 'first comment', createdAt: '2026-01-01', databaseID: 1 }],
};

const anchor: CommentAnchor = { filePath: 'src/a.ts', newLine: 5, side: 'new' };

function setup() {
  const onSendReply = vi.fn();
  // A non-empty body mounts the row with its reply composer open.
  // `anchor` is a reserved Svelte mount option, so props must be nested.
  const view = render(ReviewPRThreadRow, {
    props: {
      thread,
      anchor,
      collapsed: true,
      orphaned: false,
      body: 'reply text',
      error: null,
      sending: false,
      isTurnActive: false,
      onToggle: vi.fn(),
      onBodyChange: vi.fn(),
      onSendReply,
      onSendToAgent: vi.fn(),
    },
  });
  const textarea = view.container.querySelector('textarea')!;
  return { onSendReply, textarea };
}

describe('<ReviewPRThreadRow> reply keyboard send', () => {
  it('sends on Ctrl+Enter', async () => {
    const { onSendReply, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter', ctrlKey: true });
    expect(onSendReply).toHaveBeenCalledTimes(1);
  });

  it('sends on Cmd+Enter (macOS)', async () => {
    const { onSendReply, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter', metaKey: true });
    expect(onSendReply).toHaveBeenCalledTimes(1);
  });

  it('does not send on plain Enter', async () => {
    const { onSendReply, textarea } = setup();
    await fireEvent.keyDown(textarea, { key: 'Enter' });
    expect(onSendReply).not.toHaveBeenCalled();
  });
});
