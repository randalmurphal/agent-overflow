import { cleanup, fireEvent, render } from '@testing-library/svelte';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import ProposedPlanReviewSurface from './ProposedPlanReviewSurface.svelte';
import type { ProposedPlanComment } from '../../types/models';
import { resetBindingMocks, setBindingMock } from '../../../test/mocks/bindings-app';
import { pairViewOnly, resetToLocalPage } from '../../../test/helpers/scopes';

describe('<ProposedPlanReviewSurface>', () => {
  beforeEach(() => {
    resetBindingMocks();
  });

  afterEach(() => {
    cleanup();
    resetBindingMocks();
    resetToLocalPage();
    window.getSelection()?.removeAllRanges();
  });

  // Creating, editing and deleting comments all write under
  // `threads:operate`. A view-only device reads the plan and its comments,
  // gets no selection trigger, and sees the draft controls inert with the
  // reason.
  it('offers no comment trigger and inert draft controls without threads:operate', async () => {
    await pairViewOnly();
    const comments: ProposedPlanComment[] = [{
      id: 'comment-1',
      threadId: 'thread-1',
      planItemId: 'plan-1',
      status: 'draft',
      startLine: 1,
      endLine: 1,
      selectedText: 'Repeat',
      body: 'Still a draft',
      createdAt: 1,
      updatedAt: 2,
    }];
    const { findAllByText, getByLabelText, getByText, queryByTestId } = render(ProposedPlanReviewSurface, {
      props: {
        threadId: 'thread-1',
        planItemId: 'plan-1',
        markdown: 'Repeat\n\nRepeat',
        comments,
        onRefresh: vi.fn(),
      },
    });

    expect(getByText('Still a draft')).toBeInTheDocument();
    for (const label of ['Edit comment', 'Delete comment']) {
      const control = getByLabelText(label) as HTMLButtonElement;
      expect(control.disabled, label).toBe(true);
      expect(control.title, label).toBe('Not granted to this device');
    }

    const repeats = await findAllByText('Repeat');
    const selectedNode = firstTextNode(repeats[0] as Node);
    if (!selectedNode) throw new Error('text node not found');
    const range = document.createRange();
    range.selectNodeContents(selectedNode);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
    await fireEvent.mouseUp(document);

    expect(queryByTestId('plan-comment-trigger')).toBeNull();
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
      },
    });

    expect(getByText('Already handled')).toBeInTheDocument();
    expect(getByText('Already sent')).toBeInTheDocument();
    expect(getByText('Resolved')).toBeInTheDocument();
    expect(getByText('Sent')).toBeInTheDocument();
    expect(queryByLabelText('Edit comment')).not.toBeInTheDocument();
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
