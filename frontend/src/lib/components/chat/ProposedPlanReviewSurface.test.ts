import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ProposedPlanReviewSurface from './ProposedPlanReviewSurface.svelte';
import type { ProposedPlanComment } from '../../types/models';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';

describe('<ProposedPlanReviewSurface>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
    resetBindingMocks();
    window.getSelection()?.removeAllRanges();
  });

  function firstTextNode(node: Node): Text | null {
    if (node.nodeType === Node.TEXT_NODE) return node as Text;
    for (const child of Array.from(node.childNodes)) {
      const found = firstTextNode(child);
      if (found) return found;
    }
    return null;
  }

  it('keeps resolved and sent comments visible without edit controls', () => {
    const comments: ProposedPlanComment[] = [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'resolved',
      startLine: 1,
      endLine: 1,
      selectedText: '# Plan',
      body: 'Already handled',
      createdAt: 1,
      updatedAt: 2,
    }, {
      id: 'comment-2',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'sent',
      startLine: 1,
      endLine: 1,
      selectedText: '# Plan',
      body: 'Already sent',
      createdAt: 3,
      updatedAt: 4,
    }];

    const { getByText, queryByLabelText } = render(ProposedPlanReviewSurface, {
      props: {
        threadId: 'thread-1',
        planItemId: 'plan-1',
        markdown: '# Plan',
        comments,
        onRefresh: vi.fn(),
        onSendDrafts: vi.fn(),
      },
    });

    expect(getByText('Already handled')).toBeInTheDocument();
    expect(getByText('Already sent')).toBeInTheDocument();
    expect(getByText('Resolved')).toBeInTheDocument();
    expect(getByText('Sent')).toBeInTheDocument();
    expect(queryByLabelText('Edit comment')).not.toBeInTheDocument();
  });

  it('shows the draft comment count on the send button', () => {
    const comments: ProposedPlanComment[] = [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: '# Plan',
      body: 'First',
      createdAt: 1,
      updatedAt: 1,
    }, {
      id: 'comment-2',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: '# Plan',
      body: 'Second',
      createdAt: 2,
      updatedAt: 2,
    }];

    const { getByTestId, getByText } = render(ProposedPlanReviewSurface, {
      props: {
        threadId: 'thread-1',
        planItemId: 'plan-1',
        markdown: '# Plan',
        comments,
        onRefresh: vi.fn(),
        onSendDrafts: vi.fn(),
      },
    });

    expect(getByTestId('plan-comments-send')).toHaveTextContent('Send 2');
    expect(getByText('2')).toBeInTheDocument();
  });

  it('anchors a repeated-text comment to the selected rendered block', async () => {
    const createComment = setBindingMock('CreateProposedPlanComment', async () => ({}));
    const { findAllByText, findByTestId } = render(ProposedPlanReviewSurface, {
      props: {
        threadId: 'thread-1',
        planItemId: 'plan-1',
        markdown: 'Repeat\n\nRepeat',
        comments: [],
        onRefresh: vi.fn(),
        onSendDrafts: vi.fn(),
      },
    });

    const repeats = await findAllByText('Repeat');
    const selectedNode = firstTextNode(repeats[1] as Node);
    if (!selectedNode) throw new Error('text node not found');
    const range = document.createRange();
    range.selectNodeContents(selectedNode);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
    await fireEvent.mouseUp(document);

    await fireEvent.click(await findByTestId('plan-comment-trigger'));
    const composer = await findByTestId('plan-comment-composer');
    const textarea = composer.querySelector('textarea');
    if (!textarea) throw new Error('comment textarea not found');
    await fireEvent.input(textarea, { target: { value: 'Use the second one.' } });
    await fireEvent.click(await findByTestId('plan-comment-save'));

    expect(createComment).toHaveBeenCalledWith('thread-1', {
      planItemId: 'plan-1',
      startLine: 3,
      endLine: 3,
      body: 'Use the second one.',
    });
  });
});
