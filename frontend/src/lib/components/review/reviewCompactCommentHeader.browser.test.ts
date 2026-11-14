// In real Chromium, on compact at 360px: a thread's header with every
// comment action the viewer has, an unresolved and an outdated chip, a
// display name, its login and a time stays on one line: the actions are
// one `⋯` there, and the login and time shorten before the chips do.
// A long press on the comment opens its menu, except on selected text or
// a link, which keep the browser's own menu.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { flushSync, mount, unmount } from 'svelte';
import { page } from 'vitest/browser';
import '../../../app.css';
import ReviewPRThreadRow from './ReviewPRThreadRow.svelte';
import { createEditorPreviews } from '../../stores/reviewEditorPreview.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { ReviewPaneState } from '../../stores/reviewPane.svelte';
import type { ReviewThread } from '../../types/models';

const THREAD: ReviewThread = {
  id: 't1', path: 'src/values.ts', line: 5, side: 'right', isResolvable: true, isResolved: false, isOutdated: true,
  comments: [{
    id: 'c1', authorLogin: 'kchen', authorName: 'K Chen', body: 'Old code.', createdAt: new Date(Date.now() - 300 * 86_400_000).toISOString(),
    databaseID: 1, viewerCanEdit: true, viewerCanDelete: true, url: 'https://example.test/c1', editedAt: new Date().toISOString(),
  }],
};

let target: HTMLDivElement;
let app: ReturnType<typeof mount> | undefined;
const previews = createEditorPreviews(() => new Set(), HOME_BACKEND);

beforeEach(async () => {
  await page.viewport(360, 800);
  setCompactLayoutForTest(true);
  target = document.createElement('div');
  target.style.cssText = 'position:fixed;top:0;left:0;width:360px';
  document.body.append(target);
});

afterEach(() => {
  if (app) unmount(app);
  app = undefined;
  target.remove();
  setCompactLayoutForTest(false);
});

function review(): ReviewPaneState {
  return {
    sentComments: { editBodyFor: () => null, confirmingDelete: () => false, setConfirmDelete: () => {}, pending: () => false, error: () => null, setEditBody: () => {}, deleteComment: async () => {} },
    sentCommentActions: true,
    nowWithheldReason: null,
    primaryCommentAction: 'now',
    canHoldReply: false,
    replyDraftsFor: () => [],
    holdPRThreadReply: vi.fn(),
    reactions: { available: true, canAdd: true, withheld: null, reactionsOf: () => [], pending: () => false, error: () => null, toggle: vi.fn(), pickerOpen: () => false, setPickerOpen: vi.fn() },
    suggestions: { available: true, withheld: null, pending: () => false, error: () => null, apply: vi.fn(), currentFile: () => null, threadSource: () => null, lineSource: () => null },
    editorPreviews: previews,
    reactionsUnavailable: null,
  } as unknown as ReviewPaneState;
}

describe('a compact comment header', () => {
  it('stays on one line at 360px with both chips and every action', async () => {
    app = mount(ReviewPRThreadRow, {
      target,
      props: {
        thread: THREAD, anchor: { filePath: 'src/values.ts', newLine: 5, side: 'new' }, collapsed: false, orphaned: false, body: '', replying: false,
        error: null, sending: false, resolving: false, resolveError: null, onToggle: () => {}, onBodyChange: () => {}, onOpenReply: () => {},
        onCloseReply: () => {}, onSendReply: () => {}, onResolve: () => {}, onJumpToConversation: () => {}, review: review(),
      },
    });
    flushSync();
    await new Promise((resolve) => requestAnimationFrame(resolve));
    const header = target.querySelector<HTMLElement>('[data-first-comment]')!;
    const line = header.querySelector<HTMLElement>('button[aria-expanded]')!;
    const texts = [...line.querySelectorAll<HTMLElement>(':scope > span')].filter((span) => span.textContent!.trim() !== '' && span.offsetWidth > 0);
    expect(texts.map((span) => span.textContent!.trim())).toEqual(expect.arrayContaining(['unresolved', 'outdated']));
    const tops = texts.map((span) => Math.round(span.getBoundingClientRect().top + span.getBoundingClientRect().height / 2));
    expect(Math.max(...tops) - Math.min(...tops)).toBeLessThanOrEqual(2);
    // The name and the chips show whole; the login, time and edited may
    // shorten with an ellipsis.
    for (const span of texts.filter((each) => ['K Chen', 'unresolved', 'outdated'].includes(each.textContent!.trim()))) {
      expect(span.scrollWidth).toBeLessThanOrEqual(span.clientWidth + 1);
      expect(span.getBoundingClientRect().right).toBeLessThanOrEqual(line.getBoundingClientRect().right + 0.5);
    }
    expect(header.querySelector('[data-testid="review-comment-menu"]')).not.toBeNull();
    const headBox = header.getBoundingClientRect();
    for (const button of header.querySelectorAll('button')) {
      expect(button.getBoundingClientRect().right).toBeLessThanOrEqual(headBox.right + 0.5);
    }
  });
});

describe('a long press on a compact comment', () => {
  function mountRow(body: string): HTMLElement {
    const thread = { ...THREAD, comments: [{ ...THREAD.comments[0], body }] };
    app = mount(ReviewPRThreadRow, {
      target,
      props: {
        thread, anchor: { filePath: 'src/values.ts', newLine: 5, side: 'new' }, collapsed: false, orphaned: false, body: '', replying: false,
        error: null, sending: false, resolving: false, resolveError: null, onToggle: () => {}, onBodyChange: () => {}, onOpenReply: () => {},
        onCloseReply: () => {}, onSendReply: () => {}, onResolve: () => {}, onJumpToConversation: () => {}, review: review(),
      },
    });
    flushSync();
    return target;
  }

  async function paragraph(): Promise<HTMLElement> {
    await expect.poll(() => target.querySelector('[data-first-comment] p')).not.toBeNull();
    return target.querySelector<HTMLElement>('[data-first-comment] p')!;
  }

  // What the long-press bridge raises: a cancelable contextmenu at the
  // pressed element.
  function press(at: Element): boolean {
    const event = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
    at.dispatchEvent(event);
    flushSync();
    return event.defaultPrevented;
  }

  const menu = () => document.querySelector('[role="menu"][aria-label="Comment actions"]');

  afterEach(() => {
    document.getSelection()?.removeAllRanges();
  });

  it('opens the comment menu on unselected prose', async () => {
    mountRow('Plain prose words.');
    const prose = await paragraph();
    expect(press(prose.firstChild!.parentElement!)).toBe(true);
    await expect.poll(menu).not.toBeNull();
  });

  it('opens the comment menu while the selection is elsewhere', async () => {
    const outside = document.body.appendChild(document.createElement('p'));
    outside.textContent = 'Elsewhere.';
    try {
      mountRow('Plain prose words.');
      const prose = await paragraph();
      document.getSelection()!.selectAllChildren(outside);
      expect(press(prose)).toBe(true);
      await expect.poll(menu).not.toBeNull();
    } finally {
      outside.remove();
    }
  });

  it('leaves the browser\'s menu to selected text in the comment', async () => {
    mountRow('Plain prose words.');
    const prose = await paragraph();
    const range = document.createRange();
    range.setStart(prose.firstChild!, 0);
    range.setEnd(prose.firstChild!, 5);
    document.getSelection()!.removeAllRanges();
    document.getSelection()!.addRange(range);
    expect(press(prose)).toBe(false);
    await new Promise((resolve) => requestAnimationFrame(resolve));
    expect(menu()).toBeNull();
    // Collapsed, the selection is a caret, not selected text.
    document.getSelection()!.collapseToStart();
    expect(press(prose)).toBe(true);
  });

  it('leaves the browser\'s menu to selected text in a reply, and opens it on the reply otherwise', async () => {
    const thread = { ...THREAD, comments: [THREAD.comments[0], { ...THREAD.comments[0], id: 'c2', databaseID: 2, body: 'Reply prose words.' }] };
    app = mount(ReviewPRThreadRow, {
      target,
      props: {
        thread, anchor: { filePath: 'src/values.ts', newLine: 5, side: 'new' }, collapsed: false, orphaned: false, body: '', replying: false,
        error: null, sending: false, resolving: false, resolveError: null, onToggle: () => {}, onBodyChange: () => {}, onOpenReply: () => {},
        onCloseReply: () => {}, onSendReply: () => {}, onResolve: () => {}, onJumpToConversation: () => {}, review: review(),
      },
    });
    flushSync();
    await expect.poll(() => target.querySelector('[data-testid="review-thread-comment"] p')).not.toBeNull();
    const reply = target.querySelector<HTMLElement>('[data-testid="review-thread-comment"] p')!;
    document.getSelection()!.selectAllChildren(reply);
    expect(press(reply)).toBe(false);
    document.getSelection()!.removeAllRanges();
    expect(press(reply)).toBe(true);
    await expect.poll(menu).not.toBeNull();
  });

  it('leaves the browser\'s menu to a link', async () => {
    mountRow('See [the docs](https://example.test/docs).');
    await paragraph();
    const link = target.querySelector('[data-first-comment] a[href]')!;
    expect(press(link)).toBe(false);
  });
});
