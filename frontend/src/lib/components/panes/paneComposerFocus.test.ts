import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  carryCaretIntoPane,
  focusPaneComposer,
  isEditingFocused,
  isEditingFocusedIn,
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

  it('carries the caret to the composer, else to the pane section, without scrolling', () => {
    const target = mountPaneComposer('right');
    const section = target.parentElement!;
    const left = mountPaneComposer('left');
    left.focus();
    expect(isEditingFocused()).toBe(true);
    expect(isEditingFocusedIn('left')).toBe(true);
    expect(isEditingFocusedIn('right')).toBe(false);
    carryCaretIntoPane('right');
    expect(document.activeElement).toBe(target);

    target.disabled = true;
    left.focus();
    const focus = vi.spyOn(section, 'focus');
    carryCaretIntoPane('right');
    expect(focus).toHaveBeenCalledWith({ preventScroll: true });
    expect(document.activeElement).toBe(section);
    expect(isEditingFocused()).toBe(false);
    // Focusable only while it holds this focus: a click never lands on it.
    left.focus();
    expect(section.hasAttribute('tabindex')).toBe(false);
  });

  // The browser starts a keyboard scroll at the focused element, so a pane
  // with no composer that takes input gets the caret in its scroller.
  it('carries the caret to the pane\'s primary scroller before its section', () => {
    const target = mountPaneComposer('right');
    target.disabled = true;
    const section = target.parentElement!;
    const scroller = document.createElement('div');
    scroller.tabIndex = -1;
    scroller.setAttribute('data-pane-scroller', '');
    section.appendChild(scroller);
    mountPaneComposer('left').focus();
    carryCaretIntoPane('right');
    expect(document.activeElement).toBe(scroller);
    expect(section.hasAttribute('tabindex')).toBe(false);
  });

  // A ticket a pull request's review covers keeps its scroller, inert and
  // ahead of the review's: the review's takes the caret.
  it('carries the caret past a scroller inside an inert subtree', () => {
    const target = mountPaneComposer('right');
    target.disabled = true;
    const section = target.parentElement!;
    const covered = document.createElement('div');
    covered.inert = true;
    covered.setAttribute('inert', '');
    const hidden = document.createElement('div');
    hidden.tabIndex = -1;
    hidden.setAttribute('data-pane-scroller', '');
    covered.appendChild(hidden);
    const shown = document.createElement('div');
    shown.tabIndex = -1;
    shown.setAttribute('data-pane-scroller', '');
    section.append(covered, shown);
    mountPaneComposer('left').focus();
    carryCaretIntoPane('right');
    expect(document.activeElement).toBe(shown);
  });

  // A pane still loading takes the caret on its section; what the choice
  // prefers takes it once it mounts, unless the focus went elsewhere.
  it('hands the section\'s focus to the scroller or composer that mounts while it holds it, and to nothing once it moved', async () => {
    const target = mountPaneComposer('right');
    target.remove();
    const section = document.querySelector<HTMLElement>('[data-pane-id="right"]')!;
    const left = mountPaneComposer('left');
    left.focus();
    carryCaretIntoPane('right');
    expect(document.activeElement).toBe(section);
    const scroller = document.createElement('div');
    scroller.tabIndex = -1;
    scroller.setAttribute('data-pane-scroller', '');
    section.appendChild(scroller);
    await vi.waitFor(() => expect(document.activeElement).toBe(scroller));
    expect(section.hasAttribute('tabindex')).toBe(false);

    // The composer comes first when both mount.
    scroller.remove();
    left.focus();
    carryCaretIntoPane('right');
    expect(document.activeElement).toBe(section);
    const later = document.createElement('div');
    later.tabIndex = -1;
    later.setAttribute('data-pane-scroller', '');
    section.append(later, target);
    await vi.waitFor(() => expect(document.activeElement).toBe(target));

    // Moved elsewhere first: what mounts later takes nothing.
    later.remove();
    target.remove();
    left.focus();
    carryCaretIntoPane('right');
    expect(document.activeElement).toBe(section);
    left.focus();
    const last = document.createElement('div');
    last.tabIndex = -1;
    last.setAttribute('data-pane-scroller', '');
    section.appendChild(last);
    await Promise.resolve();
    await Promise.resolve();
    expect(document.activeElement).toBe(left);
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
