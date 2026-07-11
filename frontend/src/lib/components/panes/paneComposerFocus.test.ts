import { afterEach, describe, expect, it, vi } from 'vitest';
import { focusPaneComposer, focusPaneComposerIfEditableActive } from './paneComposerFocus';

function mountPaneComposer(paneId: string): HTMLTextAreaElement {
  const root = document.createElement('section');
  root.dataset.paneId = paneId;
  const textarea = document.createElement('textarea');
  textarea.setAttribute('aria-label', 'Message Input');
  root.appendChild(textarea);
  document.body.appendChild(root);
  return textarea;
}

describe('paneComposerFocus', () => {
  afterEach(() => {
    document.body.innerHTML = '';
    vi.restoreAllMocks();
  });

  it('focuses the composer with preventScroll so the strip reveal stays smooth', () => {
    const textarea = mountPaneComposer('left');
    const focus = vi.spyOn(textarea, 'focus');

    expect(focusPaneComposer('left')).toBe(true);

    // A bare focus() sync-scrolls the horizontal pane strip to the
    // textarea, snapping past PaneHost's rAF-deferred smooth reveal.
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
  });

  it('returns false when the composer is missing or disabled', () => {
    expect(focusPaneComposer('ghost')).toBe(false);

    const textarea = mountPaneComposer('left');
    textarea.disabled = true;
    expect(focusPaneComposer('left')).toBe(false);
  });

  it('moves the caret only when an editable element was active', () => {
    const target = mountPaneComposer('right');
    const focus = vi.spyOn(target, 'focus');

    (document.activeElement as HTMLElement | null)?.blur?.();
    focusPaneComposerIfEditableActive('right');
    expect(focus).not.toHaveBeenCalled();

    mountPaneComposer('left').focus();
    focusPaneComposerIfEditableActive('right');
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
  });
});
