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
    { authorLogin: 'alice', body: 'First comment.', createdAt: '2026-01-01', databaseID: 1, id: `n${1}`, viewerCanEdit: false, viewerCanDelete: false },
    { authorLogin: 'coderabbitai[bot]', body: '<!-- marker only -->', createdAt: '2026-01-02', databaseID: 2, id: `n${2}`, viewerCanEdit: false, viewerCanDelete: false },
    { authorLogin: 'bob', body: 'A reply.\n\nWith two paragraphs.', createdAt: '2026-01-03', databaseID: 3, id: `n${3}`, viewerCanEdit: false, viewerCanDelete: false },
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

// happy-dom has no clipboard; the stub is an own property the shared-worker
// guard expects gone (not present as undefined) when the file ends.
const originalClipboard = Object.getOwnPropertyDescriptor(navigator, 'clipboard');

function stubClipboard(writeText: (text: string) => Promise<void>) {
  Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true, writable: true });
}

afterEach(() => {
  delete (navigator as { clipboard?: unknown }).clipboard;
  if (originalClipboard) Object.defineProperty(navigator, 'clipboard', originalClipboard);
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

describe('ReviewThreadFooter send now or hold', () => {
  const buttons = (view: ReturnType<typeof renderFooter>) => ({
    primary: view.container.querySelector<HTMLButtonElement>('[data-editor-control="primary"]')!,
    secondary: view.container.querySelector<HTMLButtonElement>('[data-editor-control="secondary"]'),
  });

  it('offers Reply now and Add draft where a reply can be held, by the primary rule', async () => {
    const onSendReply = vi.fn();
    const onHold = vi.fn();
    const now = renderFooter({ replying: true, body: 'Yes.', onSendReply, hold: { primary: 'now', onHold } });
    expect(buttons(now).primary.textContent).toBe('Reply now');
    expect(buttons(now).secondary?.textContent).toBe('Add draft');
    const textarea = now.container.querySelector('textarea')!;
    await fireEvent.keyDown(textarea, { key: 'Enter', ctrlKey: true });
    expect(onSendReply).toHaveBeenCalledTimes(1);
    await fireEvent.keyDown(textarea, { key: 'Enter', ctrlKey: true, shiftKey: true });
    expect(onHold).toHaveBeenCalledTimes(1);
    now.unmount();

    const held = renderFooter({ replying: true, body: 'Yes.', onSendReply, hold: { primary: 'draft', onHold } });
    expect(buttons(held).primary.textContent).toBe('Add draft');
    expect(buttons(held).secondary?.textContent).toBe('Reply now');
  });

  it('keeps one Reply that posts now where a reply cannot be held', () => {
    const view = renderFooter({ replying: true, body: 'Yes.' });
    expect(buttons(view).primary.textContent).toBe('Reply');
    expect(buttons(view).secondary).toBeNull();
  });

  it('withholds the now reply with its reason, and shows it pending', () => {
    const view = renderFooter({ replying: true, body: 'Yes.', nowWithheld: 'Not granted to this device', hold: { primary: 'now', onHold: vi.fn() } });
    expect(buttons(view).primary.disabled).toBe(true);
    expect(buttons(view).primary.title).toBe('Not granted to this device');
    expect(buttons(view).secondary?.disabled).toBe(false);
    view.unmount();
    const pending = renderFooter({ replying: true, body: 'Yes.', sending: true });
    expect(pending.container.querySelector('textarea')!.readOnly).toBe(true);
    expect(pending.getByTestId('review-editor-pending')).toBeTruthy();
  });
});
