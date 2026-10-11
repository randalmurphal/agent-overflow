import { fireEvent, render } from '@testing-library/svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewConversationThread from './ReviewConversationThread.svelte';
import { parseReviewFiles } from '../../utils/patchStore';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import type { ReviewComment, ReviewThread } from '../../types/models';

function comment(overrides: Partial<ReviewComment> = {}): ReviewComment {
  return {
    authorLogin: 'octocat',
    body: 'First comment body.',
    createdAt: '2026-01-01T00:00:00Z',
    databaseID: 1,
    ...overrides,
  };
}

function thread(overrides: Partial<ReviewThread> = {}): ReviewThread {
  return {
    id: 't1',
    path: 'src/a.ts',
    line: 2,
    side: 'right',
    isResolvable: true,
    isResolved: false,
    isOutdated: false,
    comments: [comment()],
    ...overrides,
  };
}

function withReplies(count: number): ReviewComment[] {
  return [
    comment(),
    ...Array.from({ length: count }, (_, index) =>
      comment({ authorLogin: `replier${index}`, body: `Reply number ${index + 1}.`, databaseID: index + 2 })),
  ];
}

// Only the members the card reads. The fake is not reactive: a test that
// needs another store state renders again.
function fakeReview(overrides: Partial<Record<keyof ReviewPaneState, unknown>> = {}) {
  const review = {
    files: [],
    unresolvedCursor: null,
    isTurnActive: false,
    replyBodyFor: vi.fn(() => ''),
    replyErrorFor: vi.fn(() => null),
    sendingReply: vi.fn(() => false),
    resolvingThread: vi.fn(() => false),
    resolveErrorFor: vi.fn(() => null),
    conversationThreadExpanded: vi.fn(() => false),
    toggleConversationThread: vi.fn(),
    jumpToDiffThread: vi.fn(),
    setPRThreadResolved: vi.fn(async () => {}),
    setReplyBody: vi.fn(),
    sendPRThreadReply: vi.fn(async () => {}),
    ...overrides,
  };
  return review;
}

function renderCard(t: ReviewThread, options: { inDiff?: boolean; review?: ReturnType<typeof fakeReview> } = {}) {
  const review = options.review ?? fakeReview();
  const view = render(ReviewConversationThread, {
    review: review as unknown as ReviewPaneState,
    thread: t,
    inDiff: options.inDiff ?? false,
  });
  const card = view.getByTestId('review-conversation-thread');
  return { ...view, review, card };
}

describe('<ReviewConversationThread> state', () => {
  it('carries the thread state on data-state', () => {
    const cases: [Partial<ReviewThread>, string][] = [
      [{}, 'unresolved'],
      [{ isResolved: true }, 'resolved'],
      [{ isOutdated: true }, 'outdated'],
      [{ isResolvable: false, path: '', line: null }, 'none'],
    ];
    for (const [overrides, state] of cases) {
      const { card, unmount } = renderCard(thread(overrides));
      expect(card.getAttribute('data-state')).toBe(state);
      unmount();
    }
  });

  it('resolves a live thread from the footer', async () => {
    const t = thread();
    const view = renderCard(t);
    const resolve = view.getByTestId('review-thread-resolve');
    expect(resolve.textContent?.trim()).toBe('Resolve');
    await fireEvent.click(resolve);
    expect(view.review.setPRThreadResolved).toHaveBeenCalledWith(t, true);
  });

  it('unresolves a resolved thread', async () => {
    const t = thread({ isResolved: true });
    const view = renderCard(t);
    const resolve = view.getByTestId('review-thread-resolve');
    expect(resolve.textContent?.trim()).toBe('Unresolve');
    await fireEvent.click(resolve);
    expect(view.review.setPRThreadResolved).toHaveBeenCalledWith(t, false);
  });

  it('omits the resolve control when the thread is outdated or not resolvable', () => {
    const outdated = renderCard(thread({ isOutdated: true }));
    expect(outdated.queryByTestId('review-thread-resolve')).toBeNull();
    outdated.unmount();
    const flat = renderCard(thread({ isResolvable: false, path: '', line: null }));
    expect(flat.queryByTestId('review-thread-resolve')).toBeNull();
  });

  it('shows the resolve failure under the footer', () => {
    const review = fakeReview({ resolveErrorFor: vi.fn(() => 'forge said no') });
    const view = renderCard(thread(), { review });
    expect(view.getByTestId('review-thread-footer').textContent).toContain('forge said no');
  });
});

describe('<ReviewConversationThread> header', () => {
  it('shows the display name and the login when the forge knows a name', () => {
    const view = renderCard(thread({ comments: [comment({ authorName: 'Mona Lisa' })] }));
    expect(view.getByText('Mona Lisa')).toBeTruthy();
    expect(view.getByText('@octocat')).toBeTruthy();
  });

  it('shows the login alone, without a second @login, when there is no name', () => {
    const view = renderCard(thread());
    expect(view.getByText('octocat')).toBeTruthy();
    expect(view.queryByText('@octocat')).toBeNull();
    expect(view.queryByText('bot')).toBeNull();
  });

  it('tags a bot login', () => {
    const view = renderCard(thread({ comments: [comment({ authorLogin: 'coderabbitai[bot]' })] }));
    expect(view.getByText('bot')).toBeTruthy();
  });

  it('renders the author avatar', () => {
    const view = renderCard(thread());
    expect(view.getAllByTestId('review-avatar')[0].textContent).toBe('OC');
  });
});

describe('<ReviewConversationThread> jump to diff', () => {
  it('jumps to the thread in the diff when its file is in the diff', async () => {
    const t = thread();
    const view = renderCard(t, { inDiff: true });
    const jump = view.getByTestId('review-conversation-jump-diff');
    expect(jump.textContent).toBe('a.ts:2');
    await fireEvent.click(jump);
    expect(view.review.jumpToDiffThread).toHaveBeenCalledWith(t);
  });

  it('names the location without a button when the file is not in the diff', () => {
    const view = renderCard(thread(), { inDiff: false });
    expect(view.queryByTestId('review-conversation-jump-diff')).toBeNull();
    expect(view.getByText('a.ts:2')).toBeTruthy();
  });
});

describe('<ReviewConversationThread> replies', () => {
  it('folds a settled thread\'s replies behind a count', async () => {
    const view = renderCard(thread({ isResolved: true, comments: withReplies(2) }));
    const fold = view.getByTestId('review-conversation-replies');
    expect(fold.textContent).toContain('2 replies');
    expect(fold.getAttribute('aria-expanded')).toBe('false');
    expect(view.queryByTestId('review-thread-comment')).toBeNull();
    // The first comment renders in full regardless of the fold.
    expect(view.getByText('First comment body.')).toBeTruthy();
    await fireEvent.click(fold);
    expect(view.review.toggleConversationThread).toHaveBeenCalledWith('t1');
  });

  it('says "1 reply" for a single reply', () => {
    const view = renderCard(thread({ isOutdated: true, comments: withReplies(1) }));
    expect(view.getByTestId('review-conversation-replies').textContent).toContain('1 reply');
    expect(view.getByTestId('review-conversation-replies').textContent).not.toContain('replies');
  });

  it('shows a settled thread\'s replies once the store has them expanded', () => {
    const review = fakeReview({ conversationThreadExpanded: vi.fn(() => true) });
    const view = renderCard(thread({ isResolved: true, comments: withReplies(2) }), { review });
    expect(view.getByTestId('review-conversation-replies').getAttribute('aria-expanded')).toBe('true');
    expect(view.getAllByTestId('review-thread-comment')).toHaveLength(2);
  });

  it('shows an unresolved thread\'s replies with no fold', () => {
    const view = renderCard(thread({ comments: withReplies(2) }));
    expect(view.queryByTestId('review-conversation-replies')).toBeNull();
    const rows = view.getAllByTestId('review-thread-comment');
    // Replies only: the first comment is the card body, not a reply row.
    expect(rows).toHaveLength(2);
    expect(rows[0].textContent).toContain('Reply number 1.');
  });
});

describe('<ReviewConversationThread> reply composer', () => {
  it('opens the composer from the reply field at the foot of the card', async () => {
    const view = renderCard(thread());
    expect(view.queryByTestId('review-thread-composer')).toBeNull();
    // No action lives in the header any more.
    expect(view.queryByLabelText('Send to agent')).toBeNull();
    await fireEvent.click(view.getByTestId('review-thread-reply'));
    expect(view.getByTestId('review-thread-composer')).toBeTruthy();
    expect(view.queryByTestId('review-thread-reply')).toBeNull();
  });

  it('closes the composer from Cancel and from Escape', async () => {
    const view = renderCard(thread());
    await fireEvent.click(view.getByTestId('review-thread-reply'));
    await fireEvent.click(view.getByText('Cancel'));
    expect(view.queryByTestId('review-thread-composer')).toBeNull();
    await fireEvent.click(view.getByTestId('review-thread-reply'));
    await fireEvent.keyDown(view.container.querySelector('textarea')!, { key: 'Escape' });
    expect(view.queryByTestId('review-thread-composer')).toBeNull();
  });

  it('mounts with the composer open when a draft survived', () => {
    const review = fakeReview({ replyBodyFor: vi.fn(() => 'half a reply') });
    const view = renderCard(thread(), { review });
    expect(view.container.querySelector('textarea')!.value).toBe('half a reply');
  });

  it('unfolds a folded thread when replying into it', async () => {
    const view = renderCard(thread({ isResolved: true, comments: withReplies(1) }));
    await fireEvent.click(view.getByTestId('review-thread-reply'));
    expect(view.review.toggleConversationThread).toHaveBeenCalledWith('t1');
  });
});

describe('<ReviewConversationThread> code context', () => {
  const files = parseReviewFiles([
    'diff --git a/src/a.ts b/src/a.ts',
    'new file mode 100644',
    '--- /dev/null',
    '+++ b/src/a.ts',
    '@@ -0,0 +1,3 @@',
    '+a',
    '+b',
    '+c',
  ].join('\n'));

  it('folds the anchored code under the header when the file is in the diff', () => {
    const view = renderCard(thread(), { inDiff: true, review: fakeReview({ files }) });
    expect(view.getByTestId('review-thread-context')).toBeTruthy();
  });

  it('shows no code when the file is not in the diff or the thread has no line', () => {
    const cases: [ReviewThread, boolean][] = [
      [thread(), false],
      [thread({ line: null }), true],
      [thread({ path: 'src/other.ts' }), true],
    ];
    for (const [t, inDiff] of cases) {
      const view = renderCard(t, { inDiff, review: fakeReview({ files }) });
      expect(view.queryByTestId('review-thread-context')).toBeNull();
      view.unmount();
    }
  });
});
