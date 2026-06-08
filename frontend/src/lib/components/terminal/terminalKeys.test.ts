import { describe, expect, it } from 'vitest';

import { isClipboardChord, type ClipboardChordEvent } from './terminalKeys';

function ev(over: Partial<ClipboardChordEvent>): ClipboardChordEvent {
  return { key: 'a', ctrlKey: false, shiftKey: false, altKey: false, metaKey: false, ...over };
}

describe('isClipboardChord', () => {
  describe('non-macOS (Ctrl+Shift+C / Ctrl+Shift+V)', () => {
    it('matches Ctrl+Shift+C as copy (lower- and upper-case key glyph)', () => {
      expect(isClipboardChord(ev({ key: 'c', ctrlKey: true, shiftKey: true }), 'c', false)).toBe(true);
      // With Shift held the browser may report the upper-case glyph.
      expect(isClipboardChord(ev({ key: 'C', ctrlKey: true, shiftKey: true }), 'c', false)).toBe(true);
    });

    it('matches Ctrl+Shift+V as paste', () => {
      expect(isClipboardChord(ev({ key: 'v', ctrlKey: true, shiftKey: true }), 'v', false)).toBe(true);
    });

    it('does NOT match plain Ctrl+C — must stay SIGINT', () => {
      expect(isClipboardChord(ev({ key: 'c', ctrlKey: true }), 'c', false)).toBe(false);
    });

    it('does NOT match Cmd+C on non-mac', () => {
      expect(isClipboardChord(ev({ key: 'c', metaKey: true }), 'c', false)).toBe(false);
    });

    it('does NOT match when an extra modifier is held', () => {
      expect(
        isClipboardChord(ev({ key: 'c', ctrlKey: true, shiftKey: true, altKey: true }), 'c', false),
      ).toBe(false);
      expect(
        isClipboardChord(ev({ key: 'c', ctrlKey: true, shiftKey: true, metaKey: true }), 'c', false),
      ).toBe(false);
    });

    it('does NOT match a different letter than requested', () => {
      expect(isClipboardChord(ev({ key: 'x', ctrlKey: true, shiftKey: true }), 'c', false)).toBe(false);
      expect(isClipboardChord(ev({ key: 'c', ctrlKey: true, shiftKey: true }), 'v', false)).toBe(false);
    });
  });

  describe('macOS (Cmd+C / Cmd+V)', () => {
    it('matches Cmd+C as copy and Cmd+V as paste', () => {
      expect(isClipboardChord(ev({ key: 'c', metaKey: true }), 'c', true)).toBe(true);
      expect(isClipboardChord(ev({ key: 'v', metaKey: true }), 'v', true)).toBe(true);
    });

    it('does NOT match Ctrl+Shift+C on mac (mac uses Cmd)', () => {
      expect(isClipboardChord(ev({ key: 'c', ctrlKey: true, shiftKey: true }), 'c', true)).toBe(false);
    });

    it('does NOT match plain Ctrl+C on mac — stays SIGINT', () => {
      expect(isClipboardChord(ev({ key: 'c', ctrlKey: true }), 'c', true)).toBe(false);
    });

    it('does NOT match Cmd+Shift+C on mac (Cmd only, no Shift)', () => {
      expect(isClipboardChord(ev({ key: 'c', metaKey: true, shiftKey: true }), 'c', true)).toBe(false);
    });
  });
});
