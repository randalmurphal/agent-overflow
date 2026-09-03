import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCommentsList from './ReviewCommentsList.svelte';
import type { CommentFileGroup, CommentListItem } from '../../utils/reviewComments';

function item(overrides: Partial<CommentListItem>): CommentListItem {
  return {
    rowKey: 'pt:t1',
    kind: 'pr-thread',
    threadId: 't1',
    filePath: 'src/a.ts',
    line: 5,
    author: 'alice',
    snippet: 'Fix the guard',
    state: 'unresolved',
    orphaned: false,
    inDiff: true,
    replies: 0,
    createdAtMs: Date.now() - 3 * 60 * 60 * 1000,
    comments: [{ author: 'alice', body: 'Fix the guard\nfull body' }],
    ...overrides,
  };
}

describe('<ReviewCommentsList>', () => {
  it('selects every thread item (diff jump or conversation), expands only stranded drafts inline', async () => {
    const onSelect = vi.fn();
    const groups: CommentFileGroup[] = [
      {
        filePath: '',
        inDiff: false,
        items: [
          item({
            rowKey: 'pt:conv',
            filePath: '',
            line: null,
            author: 'coderabbitai',
            snippet: 'Walkthrough of the change',
            state: 'comment',
            inDiff: false,
            comments: [{ author: 'coderabbitai', body: 'Walkthrough of the change\n\nLong body text here.' }],
          }),
        ],
      },
      { filePath: 'src/a.ts', inDiff: true, items: [item({})] },
      {
        filePath: 'zz/gone.ts',
        inDiff: false,
        items: [
          item({
            rowKey: 't:d1',
            kind: 'draft',
            threadId: null,
            filePath: 'zz/gone.ts',
            state: 'draft',
            inDiff: false,
            author: 'You',
            snippet: 'my stranded draft',
            comments: [{ author: 'You', body: 'my stranded draft\n\nLong body text here.' }],
          }),
        ],
      },
    ];
    const view = render(ReviewCommentsList, { groups, onSelect });

    // The conversation group renders with its own label.
    expect(view.getByText('Conversation')).toBeInTheDocument();

    // A conversation thread has a card in the header's Conversation
    // section, so its click routes through onSelect (jumpToComment opens
    // the section) instead of expanding inline.
    const conv = view.getAllByTestId('review-comments-item')[0]!;
    await fireEvent.click(conv);
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ rowKey: 'pt:conv' }));

    // In-diff item: click jumps.
    const diffItem = view.getAllByTestId('review-comments-item')[1]!;
    await fireEvent.click(diffItem);
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ rowKey: 'pt:t1' }));

    // A draft on a file outside the diff has neither a diff row nor a
    // conversation card — the ONLY inline-expanding case left.
    onSelect.mockClear();
    const draft = view.getAllByTestId('review-comments-item')[2]!;
    await fireEvent.click(draft);
    expect(onSelect).not.toHaveBeenCalled();
    expect(view.getByTestId('review-comments-item-body')).toHaveTextContent('Long body text here.');
    await fireEvent.click(draft);
    expect(view.queryByTestId('review-comments-item-body')).toBeNull();
  });

  it('shows the snippet as primary text with author demoted to the meta line', () => {
    const groups: CommentFileGroup[] = [
      { filePath: 'src/a.ts', inDiff: true, items: [item({ author: 'group_13094147_bot_d863839c', replies: 2 })] },
    ];
    const view = render(ReviewCommentsList, { groups, onSelect: () => {} });
    const row = view.getByTestId('review-comments-item');
    expect(row).toHaveTextContent('Fix the guard');
    // The full author survives as a hover title even when truncated.
    expect(view.getByTitle('group_13094147_bot_d863839c')).toBeInTheDocument();
    expect(row).toHaveTextContent('+2');
    expect(row).toHaveTextContent('unresolved');
    expect(view.getByTestId('review-comments-item-time')).toHaveTextContent('3h ago');
  });

  it('omits the timestamp when creation time is unknown', () => {
    const groups: CommentFileGroup[] = [
      { filePath: 'src/a.ts', inDiff: true, items: [item({ createdAtMs: null })] },
    ];
    const view = render(ReviewCommentsList, { groups, onSelect: () => {} });
    expect(view.queryByTestId('review-comments-item-time')).toBeNull();
  });

  it('neutral conversation comments carry no state label', () => {
    const groups: CommentFileGroup[] = [
      { filePath: '', inDiff: false, items: [item({ state: 'comment', inDiff: false, line: null })] },
    ];
    const view = render(ReviewCommentsList, { groups, onSelect: () => {} });
    expect(view.getByTestId('review-comments-item')).not.toHaveTextContent('unresolved');
  });
});
