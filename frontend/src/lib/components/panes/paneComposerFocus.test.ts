import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  focusPaneComposer,
  focusPaneComposerIfEditableActive,
  restorePickerFocus,
} from './paneComposerFocus';

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

  describe('restorePickerFocus', () => {
    it('puts the caret back in the composer on an explicit dismissal', () => {
      const textarea = mountPaneComposer('left');
      const focus = vi.spyOn(textarea, 'focus');

      restorePickerFocus(undefined, { paneId: 'left' });
      expect(focus).toHaveBeenCalledWith({ preventScroll: true });

      focus.mockClear();
      restorePickerFocus('escape', { paneId: 'left' });
      expect(focus).toHaveBeenCalledWith({ preventScroll: true });

      focus.mockClear();
      restorePickerFocus('tab', { paneId: 'left' });
      expect(focus).toHaveBeenCalledWith({ preventScroll: true });
    });

    it('falls back to the trigger with preventScroll when the composer is unavailable', () => {
      const textarea = mountPaneComposer('left');
      textarea.disabled = true;
      const trigger = document.createElement('button');
      document.body.appendChild(trigger);
      const focus = vi.spyOn(trigger, 'focus');

      restorePickerFocus(undefined, { paneId: 'left', triggerEl: trigger });

      // A bare trigger focus() would sync-scroll the pane strip back to a
      // scrolled-away trigger — observed as a thread-click reveal gliding
      // from the popup's pane instead of from where the user actually was.
      expect(focus).toHaveBeenCalledWith({ preventScroll: true });
    });

    it('restores nothing when the user engaged something else', () => {
      const textarea = mountPaneComposer('left');
      const trigger = document.createElement('button');
      document.body.appendChild(trigger);
      const textareaFocus = vi.spyOn(textarea, 'focus');
      const triggerFocus = vi.spyOn(trigger, 'focus');

      restorePickerFocus('outside-click', { paneId: 'left', triggerEl: trigger });
      restorePickerFocus('anchor-gone', { paneId: 'left', triggerEl: trigger });

      expect(textareaFocus).not.toHaveBeenCalled();
      expect(triggerFocus).not.toHaveBeenCalled();
    });
  });
});
