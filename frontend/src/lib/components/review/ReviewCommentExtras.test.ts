import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
import ReviewReactionChips from './ReviewReactionChips.svelte';
import ReviewReactionPicker from './ReviewReactionPicker.svelte';
import ReviewSentCommentActions from './ReviewSentCommentActions.svelte';
import ReviewSuggestionStatus, { suggestionStatus } from './ReviewSuggestionStatus.svelte';
import { createEditorPreviews } from '../../stores/reviewEditorPreview.svelte';
import { HOME_BACKEND } from '../../transport/backendKey';
import { setCompactLayoutForTest } from '../../stores/layoutMode.svelte';
import type { ReviewSuggestions } from '../../stores/reviewSuggestions.svelte';
import type { ReviewReactions } from '../../stores/reviewReactions.svelte';
import { SHARED_REACTIONS } from '../../utils/reactionEmoji';
import type { ReviewComment, ReviewThread } from '../../types/models';

// The comment controls a review pane adds: a suggestion's state, the
// reaction chips and popover, the hover actions' order (one `⋯` menu on
// compact, by tap or long press), and the editors' Preview toggle, which
// keeps the text and the caret across a remount.

const COMMENT: ReviewComment = {
  id: 'c1', authorLogin: 'octo', body: 'b', createdAt: '2026-01-01T00:00:00Z', databaseID: 1, url: 'https://example.test/c1',
  viewerCanEdit: true, viewerCanDelete: true,
  reactions: [
    { name: 'thumbsup', count: 2, viewerReacted: true, users: ['octo', 'ao-viewer'] },
    { name: 'party_parrot', count: 1, viewerReacted: false },
  ],
};
const THREAD: ReviewThread = { id: 't1', path: 'a.ts', line: 1, side: 'right', isResolvable: true, isResolved: false, isOutdated: false, comments: [COMMENT] };

function reactionsStub(over: Partial<ReviewReactions> = {}): ReviewReactions {
  return {
    available: true, canAdd: true, withheld: null,
    reactionsOf: (comment) => comment.reactions ?? [],
    pending: () => false,
    error: () => null,
    toggle: vi.fn(async () => {}),
    pickerOpen: () => false,
    setPickerOpen: vi.fn(),
    ...over,
  };
}

describe('a suggestion\'s state', () => {
  it('is Applied once applied, Outdated on a moved thread, Apply where the forge allows it', () => {
    const live = { isOutdated: false };
    expect(suggestionStatus(live, { appliable: true, applied: true }, true)).toBe('applied');
    expect(suggestionStatus({ isOutdated: true }, { appliable: false }, true)).toBe('outdated');
    expect(suggestionStatus({ isOutdated: true }, { appliable: false, applied: true }, true)).toBe('applied');
    expect(suggestionStatus(live, { appliable: true }, true)).toBe('apply');
    expect(suggestionStatus(live, { appliable: true }, false)).toBe('none');
    expect(suggestionStatus(live, { appliable: false }, true)).toBe('none');
  });
});

describe('reaction chips', () => {
  it('show each reaction with its count, the viewer\'s pressed, an unknown award by name, and toggle on press', async () => {
    const reactions = reactionsStub();
    const { container } = render(ReviewReactionChips, { reactions, thread: THREAD, comment: COMMENT });
    const chips = [...container.querySelectorAll<HTMLButtonElement>('[data-testid="review-reaction-chip"]')];
    expect(chips.map((chip) => chip.dataset.reaction)).toEqual(['thumbsup', 'party_parrot']);
    expect(chips[0].textContent).toContain('👍');
    expect(chips[0].textContent).toContain('2');
    expect(chips[0].getAttribute('aria-pressed')).toBe('true');
    expect(chips[0].title).toContain('octo, ao-viewer');
    expect(chips[1].textContent).toContain(':party_parrot:');
    expect(chips[1].getAttribute('aria-pressed')).toBe('false');
    await fireEvent.click(chips[0]);
    expect(reactions.toggle).toHaveBeenCalledWith(THREAD, COMMENT, 'thumbsup', false);
    await fireEvent.click(chips[1]);
    expect(reactions.toggle).toHaveBeenCalledWith(THREAD, COMMENT, 'party_parrot', true);
  });

  it('render no row for a comment without reactions', () => {
    const { container } = render(ReviewReactionChips, { reactions: reactionsStub(), thread: THREAD, comment: { ...COMMENT, reactions: [] } });
    expect(container.querySelector('[data-testid="review-reaction-chips"]')).toBeNull();
  });

  it('hold a pending chip and say why a withheld one is', () => {
    const reactions = reactionsStub({ pending: (_t, _c, name) => name === 'thumbsup', withheld: 'The pane is offline.' });
    const { container } = render(ReviewReactionChips, { reactions, thread: THREAD, comment: COMMENT });
    const chips = [...container.querySelectorAll<HTMLButtonElement>('[data-testid="review-reaction-chip"]')];
    expect(chips[0].disabled).toBe(true);
    expect(chips[0].title).toMatch(/Waiting/);
    expect(chips[1].title).toBe('The pane is offline.');
  });
});

describe('the Add reaction popover', () => {
  it('offers the eight shared reactions, and a pick toggles one and closes it', async () => {
    const reactions = reactionsStub({ pickerOpen: () => true });
    const anchor = document.body.appendChild(document.createElement('button'));
    try {
      // `anchor` is also a render option: pass the props by name.
      render(ReviewReactionPicker, { props: { reactions, thread: THREAD, comment: COMMENT, anchor } });
      const options = await waitFor(() => {
        const found = [...document.querySelectorAll<HTMLButtonElement>('[data-testid="review-reaction-option"]')];
        expect(found).toHaveLength(8);
        return found;
      });
      expect(options.map((option) => option.dataset.reaction)).toEqual([...SHARED_REACTIONS]);
      await fireEvent.click(options.find((option) => option.dataset.reaction === 'thumbsup')!);
      expect(reactions.setPickerOpen).toHaveBeenCalledWith(THREAD, COMMENT, false);
      expect(reactions.toggle).toHaveBeenCalledWith(THREAD, COMMENT, 'thumbsup', false);
    } finally {
      anchor.remove();
    }
  });
});

describe('a sent comment\'s hover actions', () => {
  it('run Add reaction, Copy link, Edit, Delete in that order', () => {
    const review = {
      sentComments: {
        editBodyFor: () => null, confirmingDelete: () => false, setConfirmDelete: () => {}, pending: () => false,
        setEditBody: () => {}, deleteComment: async () => {},
      },
      sentCommentActions: true,
      nowWithheldReason: null,
      reactions: reactionsStub(),
    } as never;
    const { container } = render(ReviewSentCommentActions, { review, thread: THREAD, comment: COMMENT, first: true });
    const labels = [...container.querySelectorAll('[data-testid="review-comment-actions"] button')].map((button) => button.getAttribute('aria-label'));
    expect(labels).toEqual(['Add reaction', 'Copy link', 'Edit', 'Delete']);
  });

  function actionsReview(withheld: string | null = null) {
    return {
      sentComments: {
        editBodyFor: () => null, confirmingDelete: () => false, setConfirmDelete: vi.fn(), pending: () => false,
        setEditBody: vi.fn(), deleteComment: async () => {},
      },
      sentCommentActions: true,
      nowWithheldReason: withheld,
      reactions: reactionsStub(),
    };
  }

  it('are one menu on compact, with the same rows, order and reasons', async () => {
    setCompactLayoutForTest(true);
    try {
      const review = actionsReview('The pane is offline.');
      const { container } = render(ReviewSentCommentActions, { review: review as never, thread: THREAD, comment: COMMENT });
      const actions = container.querySelector('[data-testid="review-comment-actions"]')!;
      expect([...actions.querySelectorAll('button')].map((button) => button.getAttribute('aria-label'))).toEqual(['Comment actions']);
      await fireEvent.click(actions.querySelector('[data-testid="review-comment-menu"]')!);
      const rows = () => [...document.querySelectorAll<HTMLElement>('[role="menuitem"]')];
      expect(rows().map((row) => row.textContent!.trim())).toEqual(['Add reaction', 'Copy link', 'Edit', 'Delete']);
      const edit = rows()[2];
      expect(edit.getAttribute('aria-disabled') === 'true' || (edit as HTMLButtonElement).disabled).toBe(true);
      expect(edit.title).toBe('The pane is offline.');
      await fireEvent.click(rows()[0]);
      expect(review.reactions.setPickerOpen).toHaveBeenCalledWith(THREAD, COMMENT, true);
      expect(rows()).toHaveLength(0);
    } finally {
      setCompactLayoutForTest(false);
    }
  });

  it('open from a long press on the comment, on compact only', async () => {
    const review = actionsReview();
    const host = document.body.appendChild(document.createElement('div'));
    host.className = 'group/comment';
    const body = host.appendChild(document.createElement('p'));
    try {
      const desktop = render(ReviewSentCommentActions, { target: host, props: { review: review as never, thread: THREAD, comment: COMMENT } });
      const plain = new MouseEvent('contextmenu', { bubbles: true, cancelable: true });
      body.dispatchEvent(plain);
      expect(plain.defaultPrevented).toBe(false);
      desktop.unmount();
      setCompactLayoutForTest(true);
      render(ReviewSentCommentActions, { target: host, props: { review: review as never, thread: THREAD, comment: COMMENT } });
      flushSync();
      const press = new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 40, clientY: 50 });
      body.dispatchEvent(press);
      flushSync();
      expect(press.defaultPrevented).toBe(true);
      expect([...document.querySelectorAll('[role="menuitem"]')].map((row) => row.textContent!.trim())).toEqual(['Add reaction', 'Copy link', 'Edit', 'Delete']);
    } finally {
      setCompactLayoutForTest(false);
      host.remove();
    }
  });
});

describe('a suggestion\'s footer', () => {
  it('puts a refusal\'s sentence on the left of the row and Apply on its right', () => {
    const suggestions = {
      available: true, withheld: null, pending: () => false, error: () => 'GitHub did not apply the suggestion.', apply: vi.fn(),
      currentFile: () => null, threadSource: () => null, lineSource: () => null,
    } satisfies ReviewSuggestions;
    const row = document.body.appendChild(document.createElement('div'));
    try {
      render(ReviewSuggestionStatus, { target: row, props: { suggestions, thread: THREAD, comment: COMMENT, suggestion: { index: 0, to: ['x'], appliable: true, bodyStartLine: 0, bodyEndLine: 2, bodyPlaced: true, digest: 'a'.repeat(64) } } });
      const kids = [...row.children].filter((kid) => kid.tagName === 'SPAN');
      expect(kids[0].querySelector('[data-testid="review-suggestion-error"]')).not.toBeNull();
      expect(kids[1].querySelector('[data-testid="review-suggestion-apply"]')).not.toBeNull();
    } finally {
      row.remove();
    }
  });
});

describe('an editor\'s Preview', () => {
  it('shows the rendered body in place of the text, and returns the caret after a remount', async () => {
    let body = 'Some **bold** text.';
    const previews = createEditorPreviews(() => new Set(['editor:1']), HOME_BACKEND);
    const props = {
      get body() { return body; },
      onBodyChange: (next: string) => { body = next; },
      primary: { label: 'Comment', run: () => {} },
      onCancel: () => {},
      preview: previews.control('editor:1'),
    };
    const first = render(ReviewCommentEditorShell, props);
    const box = first.container.querySelector('textarea')!;
    box.focus();
    box.setSelectionRange(5, 9);
    await fireEvent.click(first.getByTestId('review-editor-preview-toggle'));
    flushSync();
    expect(first.container.querySelector('textarea')).toBeNull();
    expect(first.getByTestId('review-editor-preview').querySelector('strong')?.textContent).toBe('bold');
    first.unmount();

    // The editor's row scrolled away and back: still in preview.
    const second = render(ReviewCommentEditorShell, props);
    expect(second.container.querySelector('textarea')).toBeNull();
    await fireEvent.click(second.getByTestId('review-editor-preview-toggle'));
    const restored = await waitFor(() => second.container.querySelector('textarea')!);
    await waitFor(() => expect(document.activeElement).toBe(restored));
    expect(restored.value).toBe('Some **bold** text.');
    expect([restored.selectionStart, restored.selectionEnd]).toEqual([5, 9]);
    second.unmount();
    previews.dispose();
  });
});
