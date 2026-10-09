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
      resolving: false,
      resolveError: null,
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

describe('<ReviewPRThreadRow> card', () => {
  // The lead sentence differs from the rest of the body, so the snippet
  // and the full body are told apart.
  const BODY = 'Lead sentence here.\n\nSecond paragraph only in the full body.';
  const withReplies: ReviewThread = {
    ...thread,
    isResolved: true,
    comments: [
      { authorLogin: 'alice', body: BODY, createdAt: '2026-01-01', databaseID: 1 },
      { authorLogin: 'bob', body: 'Reply one.', createdAt: '2026-01-02', databaseID: 2 },
      { authorLogin: 'carol', body: 'Reply two.', createdAt: '2026-01-03', databaseID: 3 },
    ],
  };

  function renderRow(overrides: Record<string, unknown> = {}) {
    return render(ReviewPRThreadRow, {
      props: {
        thread: withReplies,
        anchor,
        collapsed: true,
        orphaned: false,
        body: '',
        error: null,
        sending: false,
        isTurnActive: false,
        resolving: false,
        resolveError: null,
        onToggle: vi.fn(),
        onBodyChange: vi.fn(),
        onSendReply: vi.fn(),
        ...overrides,
      },
    });
  }

  it('collapsed, shows the lead sentence and the reply count, not the body', () => {
    const view = renderRow({ collapsed: true });
    expect(view.getByText('Lead sentence here.')).toBeTruthy();
    expect(view.getByText('2 replies')).toBeTruthy();
    expect(view.queryByText('Second paragraph only in the full body.')).toBeNull();
    expect(view.queryByTestId('review-thread-comment')).toBeNull();
    expect(view.getByRole('button', { expanded: false })).toBeTruthy();
  });

  it('expanded, shows the full first comment and the replies', () => {
    const view = renderRow({ collapsed: false });
    expect(view.getByText('Second paragraph only in the full body.')).toBeTruthy();
    expect(view.queryByText('2 replies')).toBeNull();
    const replies = view.getAllByTestId('review-thread-comment');
    expect(replies.map((row) => row.textContent?.includes('Reply one.') || row.textContent?.includes('Reply two.')))
      .toEqual([true, true]);
  });

  it('carries the thread state, reading an orphaned thread as outdated', () => {
    const cases: [ReviewThread, boolean, string][] = [
      [thread, false, 'unresolved'],
      [withReplies, false, 'resolved'],
      [{ ...thread, isOutdated: true }, false, 'outdated'],
      [thread, true, 'outdated'],
      [{ ...thread, isResolvable: false }, false, 'none'],
    ];
    for (const [t, orphaned, state] of cases) {
      const view = renderRow({ thread: t, orphaned });
      expect(view.getByTestId('review-pr-thread').getAttribute('data-state')).toBe(state);
      view.unmount();
    }
  });

  it('renders the conversation jump only when the overview exists', () => {
    expect(renderRow().queryByTestId('review-pr-thread-jump-conversation')).toBeNull();
  });

  it('jumps to the conversation when the overview exists', async () => {
    const onJumpToConversation = vi.fn();
    const view = renderRow({ onJumpToConversation });
    await fireEvent.click(view.getByTestId('review-pr-thread-jump-conversation'));
    expect(onJumpToConversation).toHaveBeenCalledTimes(1);
  });
});
