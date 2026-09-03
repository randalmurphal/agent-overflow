// popoverNav is the pure reducer behind mention popover keyboard
// navigation. The dispatcher (handleMentionPopoverKeydown) is tested
// separately with a stubbed mentions handle.

import { describe, expect, it, vi } from 'vitest';
import {
  popoverNav,
  dispatchComposerInputKeydown,
  handleMentionPopoverKeydown,
  handleSlashPopoverKeydown,
} from './composerKeyboard';
import type { ComposerMentionsHandle } from './composerMentions.svelte';
import type { ComposerSlashHandle } from './composerSlash.svelte';

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

function kdown(key: string, init: KeyboardEventInit = {}): KeyboardEvent {
  const ev = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true, ...init });
  // happy-dom doesn't let tests assert defaultPrevented unless we call
  // preventDefault, so we track it via a spy wrapper.
  return ev;
}

// Tiny stub mirroring stubMentions for the slash dispatcher.
function stubSlash(): ComposerSlashHandle & {
  setSlashActiveIndex: ReturnType<typeof vi.fn>;
  insertCommand: ReturnType<typeof vi.fn>;
  closeSlash: ReturnType<typeof vi.fn>;
} {
  return {
    slashTrigger: null,
    slashOpen: false,
    slashSections: [],
    slashResults: [],
    slashActiveIndex: 0,
    commandError: '',
    setSlashActiveIndex: vi.fn(),
    refreshTrigger: vi.fn(),
    insertCommand: vi.fn(),
    closeSlash: vi.fn(),
    clearCommandError: vi.fn(),
    consumeInterceptedSend: vi.fn(() => false),
    interceptedRanges: vi.fn(() => []),
  } as unknown as ReturnType<typeof stubSlash>;
}

function openMentionPopover(): ReturnType<typeof stubMentions> {
  const mentions = stubMentions();
  (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
  (mentions as { mentionResults: unknown[] }).mentionResults = [{ path: 'a' }, { path: 'b' }];
  return mentions;
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

  it.each(['Enter', 'Tab'])(
    'mention popover: %s mid-IME-composition never inserts and bubbles untouched',
    (key) => {
      const mentions = openMentionPopover();
      (mentions as { mentionActiveIndex: number }).mentionActiveIndex = 1;
      const ev = kdown(key, { isComposing: true });
      expect(handleMentionPopoverKeydown(ev, mentions)).toBe(false);
      expect(ev.defaultPrevented).toBe(false);
      expect(mentions.insertMention).not.toHaveBeenCalled();
    },
  );

  it('mention popover: the legacy 229 sentinel counts as composing', () => {
    const mentions = openMentionPopover();
    const ev = kdown('Enter', { keyCode: 229 });
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(false);
    expect(mentions.insertMention).not.toHaveBeenCalled();
  });

  it('mention popover: Escape and arrows still work mid-composition', () => {
    // Only the insert gesture belongs to the IME — closing the popover and
    // moving the highlight must stay live.
    const closing = openMentionPopover();
    expect(handleMentionPopoverKeydown(kdown('Escape', { isComposing: true }), closing)).toBe(true);
    expect(closing.closeMention).toHaveBeenCalled();

    const moving = openMentionPopover();
    expect(handleMentionPopoverKeydown(kdown('ArrowDown', { isComposing: true }), moving)).toBe(true);
    expect(moving.setMentionActiveIndex).toHaveBeenCalledWith(1);
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

describe('handleSlashPopoverKeydown', () => {
  function openSlashPopover(): ReturnType<typeof stubSlash> {
    const slash = stubSlash();
    (slash as { slashTrigger: unknown }).slashTrigger = { query: '' };
    (slash as { slashOpen: boolean }).slashOpen = true;
    (slash as { slashResults: unknown[] }).slashResults = [{ name: 'plan' }, { name: 'review' }];
    return slash;
  }

  it('slash popover: Enter inserts the currently-active command', () => {
    const slash = openSlashPopover();
    (slash as { slashActiveIndex: number }).slashActiveIndex = 1;
    expect(handleSlashPopoverKeydown(kdown('Enter'), slash)).toBe(true);
    expect(slash.insertCommand).toHaveBeenCalledWith({ name: 'review' });
  });

  it('slash popover: arrowing skips a disabled row rather than stranding on it', () => {
    const slash = openSlashPopover();
    (slash as { slashResults: unknown[] }).slashResults = [
      { name: 'plan' },
      { name: 'off', disabled: true },
      { name: 'review' },
    ];
    expect(handleSlashPopoverKeydown(kdown('ArrowDown'), slash)).toBe(true);
    expect(slash.setSlashActiveIndex).toHaveBeenCalledWith(2);

    (slash as { slashActiveIndex: number }).slashActiveIndex = 2;
    slash.setSlashActiveIndex.mockClear();
    expect(handleSlashPopoverKeydown(kdown('ArrowUp'), slash)).toBe(true);
    expect(slash.setSlashActiveIndex).toHaveBeenCalledWith(0);
  });

  it('slash popover: Enter on a disabled row inserts nothing', () => {
    const slash = openSlashPopover();
    (slash as { slashResults: unknown[] }).slashResults = [{ name: 'off', disabled: true }];
    expect(handleSlashPopoverKeydown(kdown('Enter'), slash)).toBe(false);
    expect(slash.insertCommand).not.toHaveBeenCalled();
  });

  it('slash popover: Enter mid-IME-composition never inserts', () => {
    const slash = openSlashPopover();
    const ev = kdown('Enter', { isComposing: true });
    expect(handleSlashPopoverKeydown(ev, slash)).toBe(false);
    expect(ev.defaultPrevented).toBe(false);
    expect(slash.insertCommand).not.toHaveBeenCalled();
  });
});

// The whole textarea keydown contract, in dispatch order — one owner for
// both hosts (the composer and the in-place message editor), so the send
// gate below is the only place either of them can submit from.
describe('dispatchComposerInputKeydown', () => {
  function deps(overrides: Partial<Parameters<typeof dispatchComposerInputKeydown>[1]> = {}) {
    return {
      mentions: stubMentions(),
      slash: stubSlash(),
      placeholderKeydown: vi.fn(() => false),
      submitEnter: vi.fn(),
      ...overrides,
    };
  }

  it('plain Enter submits and swallows the keystroke', () => {
    const d = deps();
    const ev = kdown('Enter');
    dispatchComposerInputKeydown(ev, d);
    expect(d.submitEnter).toHaveBeenCalledTimes(1);
    expect(ev.defaultPrevented).toBe(true);
  });

  it.each([
    ['shiftKey', { shiftKey: true }],
    ['ctrlKey', { ctrlKey: true }],
    ['metaKey', { metaKey: true }],
    ['altKey', { altKey: true }],
  ])('Enter with %s is a newline, not a send', (_label, init) => {
    const d = deps();
    const ev = kdown('Enter', init);
    dispatchComposerInputKeydown(ev, d);
    expect(d.submitEnter).not.toHaveBeenCalled();
    expect(ev.defaultPrevented).toBe(false);
  });

  it('Enter mid-IME-composition confirms the candidate instead of sending', () => {
    // No preventDefault either: the browser has to deliver this one to the
    // composition, or the IME loses the keystroke it was waiting for.
    const d = deps();
    const ev = kdown('Enter', { isComposing: true });
    dispatchComposerInputKeydown(ev, d);
    expect(d.submitEnter).not.toHaveBeenCalled();
    expect(ev.defaultPrevented).toBe(false);
  });

  it('the legacy keyCode 229 sentinel counts as composing', () => {
    // WebKit and some Android IMEs deliver the composition's final keydown
    // after compositionend, with isComposing already false.
    const d = deps();
    const ev = kdown('Enter', { keyCode: 229 });
    dispatchComposerInputKeydown(ev, d);
    expect(d.submitEnter).not.toHaveBeenCalled();
    expect(ev.defaultPrevented).toBe(false);
  });

  it('an Enter the placeholder handler claims never reaches the send gate', () => {
    const d = deps({ placeholderKeydown: vi.fn(() => true) });
    dispatchComposerInputKeydown(kdown('Enter'), d);
    expect(d.submitEnter).not.toHaveBeenCalled();
  });

  it('an Enter the host claims never reaches the popovers or the send gate', () => {
    const d = deps({ claimKey: vi.fn(() => true) });
    dispatchComposerInputKeydown(kdown('Enter'), d);
    expect(d.placeholderKeydown).not.toHaveBeenCalled();
    expect(d.submitEnter).not.toHaveBeenCalled();
  });

  it('an open completion menu owns ArrowUp before the post-popover claim', () => {
    // History recall lives in claimAfterPopovers precisely so the menus
    // keep their navigation; this pins the ordering.
    const mentions = stubMentions();
    (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
    (mentions as { mentionResults: unknown[] }).mentionResults = [{ path: 'a' }, { path: 'b' }];
    const claimAfterPopovers = vi.fn(() => true);
    const d = deps({ mentions, claimAfterPopovers });
    dispatchComposerInputKeydown(kdown('ArrowUp'), d);
    expect(mentions.setMentionActiveIndex).toHaveBeenCalled();
    expect(claimAfterPopovers).not.toHaveBeenCalled();
  });

  it('a keystroke the menus decline reaches the post-popover claim, which short-circuits the rest', () => {
    const claimAfterPopovers = vi.fn(() => true);
    const d = deps({ claimAfterPopovers });
    dispatchComposerInputKeydown(kdown('ArrowUp'), d);
    expect(claimAfterPopovers).toHaveBeenCalledTimes(1);
    expect(d.placeholderKeydown).not.toHaveBeenCalled();

    // Declined: falls through to the placeholder handler as before.
    const declined = deps({ claimAfterPopovers: vi.fn(() => false) });
    dispatchComposerInputKeydown(kdown('ArrowUp'), declined);
    expect(declined.placeholderKeydown).toHaveBeenCalledTimes(1);
  });

  it('Tab is swallowed with no popover open, and yielded as Shift+Tab', () => {
    const d = deps();
    const tab = kdown('Tab');
    dispatchComposerInputKeydown(tab, d);
    expect(tab.defaultPrevented).toBe(true);

    // Shift+Tab is the global mode.cycle chord: consuming it here would
    // cancel the dispatch the global handler is waiting to make.
    const chord = kdown('Tab', { shiftKey: true });
    dispatchComposerInputKeydown(chord, d);
    expect(chord.defaultPrevented).toBe(false);
  });
});

// Compact layout: Return is a newline and the Send button is the one way
// to send, so the Enter gate yields the key to the textarea untouched.
describe('dispatchComposerInputKeydown under compact', () => {
  it('plain Enter inserts a newline when enterSends is false', () => {
    const submitEnter = vi.fn();
    const ev = kdown('Enter');
    dispatchComposerInputKeydown(ev, {
      mentions: stubMentions(),
      slash: stubSlash(),
      placeholderKeydown: vi.fn(() => false),
      submitEnter,
      enterSends: false,
    });
    expect(submitEnter).not.toHaveBeenCalled();
    expect(ev.defaultPrevented).toBe(false);
  });
});
