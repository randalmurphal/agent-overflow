import { fireEvent, render, waitFor } from '@testing-library/svelte';
import { flushSync } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import ReviewCommentEditorShell from './ReviewCommentEditorShell.svelte';
import ReviewReactionChips from './ReviewReactionChips.svelte';
import ReviewReactionPicker from './ReviewReactionPicker.svelte';
import ReviewSentCommentActions from './ReviewSentCommentActions.svelte';
import { suggestionStatus } from './ReviewSuggestionStatus.svelte';
import { createEditorPreviews } from '../../stores/reviewEditorPreview.svelte';
import type { ReviewReactions } from '../../stores/reviewReactions.svelte';
import { SHARED_REACTIONS } from '../../utils/reactionEmoji';
import type { ReviewComment, ReviewThread } from '../../types/models';

// The comment controls a review pane adds: a suggestion's state, the
// reaction chips and popover, the hover actions' order, and the editors'
// Preview toggle, which keeps the text and the caret across a remount.

const COMMENT: ReviewComment = {
  id: 'c1', authorLogin: 'octo', body: 'b', createdAt: '2026-01-01T00:00:00Z', url: 'https://example.test/c1',
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
});

describe('an editor\'s Preview', () => {
  it('shows the rendered body in place of the text, and returns the caret after a remount', async () => {
    let body = 'Some **bold** text.';
    const previews = createEditorPreviews(() => new Set(['editor:1']));
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
