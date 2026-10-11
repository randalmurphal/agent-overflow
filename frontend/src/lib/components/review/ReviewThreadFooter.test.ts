import { fireEvent, render } from '@testing-library/svelte';
import { afterEach, describe, expect, it, vi } from 'vitest';
import ReviewThreadFooter from './ReviewThreadFooter.svelte';
import { getToasts } from '../../stores/toast.svelte';
import { threadClipboardText } from '../../utils/reviewComments';
import type { ReviewThread } from '../../types/models';

const thread: ReviewThread = {
  id: 't1',
  path: 'src/a.ts',
  line: 5,
  side: 'RIGHT',
  isResolvable: true,
  isResolved: false,
  isOutdated: false,
  comments: [
    { authorLogin: 'alice', body: 'First comment.', createdAt: '2026-01-01', databaseID: 1 },
    { authorLogin: 'coderabbitai[bot]', body: '<!-- marker only -->', createdAt: '2026-01-02', databaseID: 2 },
    { authorLogin: 'bob', body: 'A reply.\n\nWith two paragraphs.', createdAt: '2026-01-03', databaseID: 3 },
  ],
};

function renderFooter(overrides: Record<string, unknown> = {}) {
  return render(ReviewThreadFooter, {
    props: {
      thread,
      body: '',
      error: null,
      sending: false,
      replying: false,
      resolving: false,
      resolveError: null,
      onOpenReply: vi.fn(),
      onCloseReply: vi.fn(),
      onBodyChange: vi.fn(),
      onSendReply: vi.fn(),
      ...overrides,
    },
  });
}

function stubClipboard(writeText: (text: string) => Promise<void>) {
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true });
}

afterEach(() => {
  Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
});

describe('threadClipboardText', () => {
  it('leads with the location and lists every visible comment by login', () => {
    expect(threadClipboardText(thread)).toBe(
      'src/a.ts:5\n\n@alice: First comment.\n\n@bob: A reply.\n\nWith two paragraphs.',
    );
  });

  it('omits the location for a PR-level thread and the line for a file-level one', () => {
    expect(threadClipboardText({ ...thread, path: '', line: null })).toBe('@alice: First comment.\n\n@bob: A reply.\n\nWith two paragraphs.');
    expect(threadClipboardText({ ...thread, line: null })).toMatch(/^src\/a\.ts\n\n@alice/);
  });
});

describe('<ReviewThreadFooter> copy', () => {
  it('writes the thread text and shows Copied until the swap resets', async () => {
    vi.useFakeTimers();
    try {
      const writeText = vi.fn(async () => {});
      stubClipboard(writeText);
      const view = renderFooter();
      await fireEvent.click(view.getByTestId('review-thread-copy'));
      await vi.advanceTimersByTimeAsync(0);
      expect(writeText).toHaveBeenCalledWith(threadClipboardText(thread));
      expect(view.getByTestId('review-thread-copy').getAttribute('aria-label')).toBe('Copied');
      await vi.advanceTimersByTimeAsync(2000);
      expect(view.getByTestId('review-thread-copy').getAttribute('aria-label')).toBe('Copy thread');
    } finally {
      vi.useRealTimers();
    }
  });

  it('raises a toast when the clipboard refuses', async () => {
    const before = getToasts().length;
    stubClipboard(async () => { throw new DOMException('denied', 'NotAllowedError'); });
    const view = renderFooter();
    await fireEvent.click(view.getByTestId('review-thread-copy'));
    await vi.waitFor(() => expect(getToasts().length).toBe(before + 1));
    expect(getToasts().at(-1)?.message).toBe('Failed to copy');
    expect(view.getByTestId('review-thread-copy').getAttribute('aria-label')).toBe('Copy thread');
  });
});

describe('<ReviewThreadFooter> actions', () => {
  it('opens the composer from the reply field', async () => {
    const onOpenReply = vi.fn();
    const view = renderFooter({ onOpenReply });
    await fireEvent.click(view.getByTestId('review-thread-reply'));
    expect(onOpenReply).toHaveBeenCalledTimes(1);
  });

  it('keeps copy and resolve beside the open composer', () => {
    const view = renderFooter({ replying: true, onResolve: vi.fn() });
    expect(view.getByTestId('review-thread-composer')).toBeTruthy();
    expect(view.queryByTestId('review-thread-reply')).toBeNull();
    expect(view.getByTestId('review-thread-copy')).toBeTruthy();
    expect(view.getByTestId('review-thread-resolve')).toBeTruthy();
  });

  it('disables Resolve while the flip is in flight', () => {
    const view = renderFooter({ onResolve: vi.fn(), resolving: true });
    expect((view.getByTestId('review-thread-resolve') as HTMLButtonElement).disabled).toBe(true);
  });

  it('disables Reply until there is text, and sends on click', async () => {
    const onSendReply = vi.fn();
    const empty = renderFooter({ replying: true, onSendReply });
    expect((empty.getByText('Reply') as HTMLButtonElement).disabled).toBe(true);
    empty.unmount();
    const view = renderFooter({ replying: true, body: 'text', onSendReply });
    await fireEvent.click(view.getByText('Reply'));
    expect(onSendReply).toHaveBeenCalledTimes(1);
  });
});
