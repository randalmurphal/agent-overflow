// popoverNav is the pure reducer behind mention popover keyboard
// navigation. The dispatcher (handleMentionPopoverKeydown) is tested
// separately with a stubbed mentions handle.

import { describe, expect, it, vi } from 'vitest';
import {
  popoverNav,
  handleMentionPopoverKeydown,
} from './composerKeyboard';
import type { ComposerMentionsHandle } from './composerMentions.svelte';

describe('popoverNav', () => {
  describe('empty list', () => {
    it('treats Escape as close', () => {
      expect(popoverNav({ key: 'Escape', activeIndex: 0, itemCount: 0 })).toEqual({
        kind: 'close',
      });
    });

    it('lets every other key bubble', () => {
      expect(popoverNav({ key: 'ArrowDown', activeIndex: 0, itemCount: 0 })).toEqual({
        kind: 'none',
      });
      expect(popoverNav({ key: 'Enter', activeIndex: 0, itemCount: 0 })).toEqual({
        kind: 'none',
      });
      expect(popoverNav({ key: 'x', activeIndex: 0, itemCount: 0 })).toEqual({
        kind: 'none',
      });
    });
  });

  describe('ArrowDown / ArrowUp movement', () => {
    it('ArrowDown advances by one', () => {
      expect(popoverNav({ key: 'ArrowDown', activeIndex: 0, itemCount: 3 })).toEqual({
        kind: 'move',
        nextIndex: 1,
      });
    });

    it('ArrowDown clamps at the tail (no wrap)', () => {
      expect(popoverNav({ key: 'ArrowDown', activeIndex: 2, itemCount: 3 })).toEqual({
        kind: 'move',
        nextIndex: 2,
      });
    });

    it('ArrowUp retreats by one', () => {
      expect(popoverNav({ key: 'ArrowUp', activeIndex: 2, itemCount: 3 })).toEqual({
        kind: 'move',
        nextIndex: 1,
      });
    });

    it('ArrowUp clamps at the head (no wrap)', () => {
      expect(popoverNav({ key: 'ArrowUp', activeIndex: 0, itemCount: 3 })).toEqual({
        kind: 'move',
        nextIndex: 0,
      });
    });
  });

  describe('insert / close / ignore', () => {
    it.each(['Enter', 'Tab'])('%s triggers insert', (key) => {
      expect(popoverNav({ key, activeIndex: 1, itemCount: 3 })).toEqual({ kind: 'insert' });
    });

    it('Escape triggers close', () => {
      expect(popoverNav({ key: 'Escape', activeIndex: 1, itemCount: 3 })).toEqual({
        kind: 'close',
      });
    });

    it('printable keys return none so the textarea keeps them', () => {
      expect(popoverNav({ key: 'a', activeIndex: 0, itemCount: 3 })).toEqual({ kind: 'none' });
      expect(popoverNav({ key: ' ', activeIndex: 0, itemCount: 3 })).toEqual({ kind: 'none' });
      expect(popoverNav({ key: 'Backspace', activeIndex: 0, itemCount: 3 })).toEqual({
        kind: 'none',
      });
    });
  });
});

// Tiny stub that exposes the handle surface handleMentionPopoverKeydown
// reads. Real ComposerMentionsHandle has many more fields; we only
// provide what the dispatcher touches.
function stubMentions(): ComposerMentionsHandle & {
  setMentionActiveIndex: ReturnType<typeof vi.fn>;
  insertMention: ReturnType<typeof vi.fn>;
  closeMention: ReturnType<typeof vi.fn>;
} {
  return {
    mentionTrigger: null,
    mentionResults: [],
    mentionActiveIndex: 0,
    mentionLoading: false,
    setMentionActiveIndex: vi.fn(),
    insertMention: vi.fn(),
    closeMention: vi.fn(),
    refreshTriggers: vi.fn(),
  } as unknown as ReturnType<typeof stubMentions>;
}

function kdown(key: string): KeyboardEvent {
  const ev = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
  // happy-dom doesn't let tests assert defaultPrevented unless we call
  // preventDefault, so we track it via a spy wrapper.
  return ev;
}

describe('handleMentionPopoverKeydown', () => {
  it('returns false (bubble) when neither popover is open', () => {
    const mentions = stubMentions();
    const ev = kdown('Enter');
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(false);
    expect(ev.defaultPrevented).toBe(false);
  });

  it('mention popover: ArrowDown moves index and consumes the event', () => {
    const mentions = stubMentions();
    (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
    (mentions as { mentionResults: unknown[] }).mentionResults = [{ path: 'a' }, { path: 'b' }];
    const ev = kdown('ArrowDown');
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(true);
    expect(ev.defaultPrevented).toBe(true);
    expect(mentions.setMentionActiveIndex).toHaveBeenCalledWith(1);
  });

  it('mention popover: Enter inserts the currently-active result', () => {
    const mentions = stubMentions();
    (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
    (mentions as { mentionResults: unknown[] }).mentionResults = [{ path: 'a' }, { path: 'b' }];
    (mentions as { mentionActiveIndex: number }).mentionActiveIndex = 1;
    const ev = kdown('Enter');
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(true);
    expect(mentions.insertMention).toHaveBeenCalledWith({ path: 'b' });
  });

  it('mention popover: Enter with empty results falls through to bubble', () => {
    const mentions = stubMentions();
    (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
    // results empty → popoverNav returns { kind: 'none' } from the
    // itemCount=0 branch → dispatcher returns false.
    const ev = kdown('Enter');
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(false);
    expect(mentions.insertMention).not.toHaveBeenCalled();
  });

  it('mention popover: Escape closes and consumes the event', () => {
    const mentions = stubMentions();
    (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
    const ev = kdown('Escape');
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(true);
    expect(mentions.closeMention).toHaveBeenCalled();
  });

  it('mention popover: Shift+Tab is reserved for mode.cycle and falls through', () => {
    const mentions = stubMentions();
    (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
    (mentions as { mentionResults: unknown[] }).mentionResults = [{ path: 'a' }, { path: 'b' }];
    const ev = new KeyboardEvent('keydown', {
      key: 'Tab',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(false);
    expect(ev.defaultPrevented).toBe(false);
    expect(mentions.insertMention).not.toHaveBeenCalled();
  });
});
