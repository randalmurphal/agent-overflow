// popoverNav is the pure reducer behind mention + slash popover
// keyboard navigation. The dispatcher (handleMentionPopoverKeydown)
// is tested separately with a stubbed mentions handle.

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import {
  popoverNav,
  handleMentionPopoverKeydown,
  shouldRetractQueueOnUpArrow,
} from './composerKeyboard';
import type { ComposerMentionsHandle } from './composerMentions.svelte';
import {
  replaceQueueForThread,
  resetForTest as resetSendQueueForTest,
} from '../../stores/sendQueue.svelte';

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
  setSlashActiveIndex: ReturnType<typeof vi.fn>;
  insertSlashCommand: ReturnType<typeof vi.fn>;
  closeSlash: ReturnType<typeof vi.fn>;
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
    slashTrigger: null,
    slashFilteredCommands: [],
    slashActiveIndex: 0,
    setSlashActiveIndex: vi.fn(),
    insertSlashCommand: vi.fn(),
    closeSlash: vi.fn(),
    onThreadChanged: vi.fn(),
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

  it('slash popover: Tab inserts the active command', () => {
    const mentions = stubMentions();
    (mentions as { slashTrigger: unknown }).slashTrigger = { text: 'h' };
    (mentions as { slashFilteredCommands: string[] }).slashFilteredCommands = ['/help', '/hello'];
    (mentions as { slashActiveIndex: number }).slashActiveIndex = 0;
    const ev = kdown('Tab');
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(true);
    expect(mentions.insertSlashCommand).toHaveBeenCalledWith('/help');
  });

  it('slash popover: Escape closes', () => {
    const mentions = stubMentions();
    (mentions as { slashTrigger: unknown }).slashTrigger = { text: 'h' };
    const ev = kdown('Escape');
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(true);
    expect(mentions.closeSlash).toHaveBeenCalled();
  });

  it('mention trigger takes precedence over slash trigger when both are somehow open', () => {
    const mentions = stubMentions();
    (mentions as { mentionTrigger: unknown }).mentionTrigger = { query: '' };
    (mentions as { slashTrigger: unknown }).slashTrigger = { text: 'h' };
    (mentions as { mentionResults: unknown[] }).mentionResults = [{ path: 'a' }];
    (mentions as { slashFilteredCommands: string[] }).slashFilteredCommands = ['/help'];
    const ev = kdown('Enter');
    handleMentionPopoverKeydown(ev, mentions);
    // Mention wins — slash insert was not called.
    expect(mentions.insertMention).toHaveBeenCalled();
    expect(mentions.insertSlashCommand).not.toHaveBeenCalled();
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

  it('slash popover: Shift+Tab is reserved for mode.cycle and falls through', () => {
    const mentions = stubMentions();
    (mentions as { slashTrigger: unknown }).slashTrigger = { text: 'h' };
    (mentions as { slashFilteredCommands: string[] }).slashFilteredCommands = ['/help'];
    const ev = new KeyboardEvent('keydown', {
      key: 'Tab',
      shiftKey: true,
      bubbles: true,
      cancelable: true,
    });
    expect(handleMentionPopoverKeydown(ev, mentions)).toBe(false);
    expect(ev.defaultPrevented).toBe(false);
    expect(mentions.insertSlashCommand).not.toHaveBeenCalled();
  });
});

describe('shouldRetractQueueOnUpArrow', () => {
  beforeEach(() => {
    resetSendQueueForTest();
  });

  afterEach(() => {
    resetSendQueueForTest();
  });

  function makeUpArrow(opts: Partial<KeyboardEventInit> = {}): KeyboardEvent {
    return new KeyboardEvent('keydown', {
      key: 'ArrowUp',
      bubbles: true,
      cancelable: true,
      ...opts,
    });
  }

  function seedQueue(threadId: string): void {
    replaceQueueForThread(threadId, [
      {
        id: 'q-1',
        threadId,
        message: 'queued',
        attachmentIds: [],
        sourceProposedPlan: null,
        revisionSourceProposedPlan: null,
        revisionSourceCommentIds: undefined,
        enqueuedAt: 1,
      },
    ]);
  }

  it('returns true for plain ArrowUp on an empty composer with queued items', () => {
    seedQueue('thread-1');
    const event = makeUpArrow();
    expect(
      shouldRetractQueueOnUpArrow({
        event,
        threadId: 'thread-1',
        hasDraftContent: false,
      }),
    ).toBe(true);
  });

  it('returns false for any modifier key', () => {
    seedQueue('thread-1');
    for (const mod of [{ ctrlKey: true }, { metaKey: true }, { altKey: true }, { shiftKey: true }]) {
      const event = makeUpArrow(mod);
      expect(
        shouldRetractQueueOnUpArrow({
          event,
          threadId: 'thread-1',
          hasDraftContent: false,
        }),
      ).toBe(false);
    }
  });

  it('returns false for non-ArrowUp keys', () => {
    seedQueue('thread-1');
    for (const key of ['ArrowDown', 'ArrowLeft', 'Enter', 'Escape', 'a']) {
      const event = new KeyboardEvent('keydown', { key, bubbles: true, cancelable: true });
      expect(
        shouldRetractQueueOnUpArrow({
          event,
          threadId: 'thread-1',
          hasDraftContent: false,
        }),
      ).toBe(false);
    }
  });

  it('returns false when the composer has draft content', () => {
    seedQueue('thread-1');
    expect(
      shouldRetractQueueOnUpArrow({
        event: makeUpArrow(),
        threadId: 'thread-1',
        hasDraftContent: true,
      }),
    ).toBe(false);
  });

  it('returns false when no thread is selected', () => {
    seedQueue('thread-1');
    expect(
      shouldRetractQueueOnUpArrow({
        event: makeUpArrow(),
        threadId: null,
        hasDraftContent: false,
      }),
    ).toBe(false);
  });

  it('returns false when the queue is empty', () => {
    expect(
      shouldRetractQueueOnUpArrow({
        event: makeUpArrow(),
        threadId: 'thread-1',
        hasDraftContent: false,
      }),
    ).toBe(false);
  });

  it('returns false when the cursor is not at (0, 0)', () => {
    seedQueue('thread-1');
    const textarea = document.createElement('textarea');
    textarea.value = '   ';
    textarea.setSelectionRange(2, 2);
    document.body.appendChild(textarea);
    try {
      const event = new KeyboardEvent('keydown', { key: 'ArrowUp', bubbles: true, cancelable: true });
      // Need the event's `target` to point at the textarea — happy-dom
      // gives us that when we dispatch through it, but the predicate is
      // pure so we just stamp it directly.
      Object.defineProperty(event, 'target', { value: textarea, configurable: true });
      expect(
        shouldRetractQueueOnUpArrow({
          event,
          threadId: 'thread-1',
          hasDraftContent: false,
        }),
      ).toBe(false);
    } finally {
      textarea.remove();
    }
  });
});
