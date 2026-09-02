import { describe, expect, it } from 'vitest';

import {
  applyStickyCtrl,
  controlCodeFor,
  isClipboardChord,
  type ClipboardChordEvent,
} from './terminalKeys';

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

describe('controlCodeFor', () => {
  it('maps the full lower-case alphabet to \\x01..\\x1a', () => {
    for (let i = 0; i < 26; i++) {
      const letter = String.fromCharCode(0x61 + i);
      expect(controlCodeFor(letter)).toBe(String.fromCharCode(i + 1));
    }
    // Spot-check the ones a terminal user actually reaches for.
    expect(controlCodeFor('c')).toBe('\x03'); // SIGINT
    expect(controlCodeFor('d')).toBe('\x04'); // EOF
    expect(controlCodeFor('z')).toBe('\x1a'); // suspend
  });

  it('maps the upper-case alphabet identically (shifted soft-keyboard glyphs)', () => {
    for (let i = 0; i < 26; i++) {
      const letter = String.fromCharCode(0x41 + i);
      expect(controlCodeFor(letter)).toBe(String.fromCharCode(i + 1));
    }
  });

  it('maps the punctuation control codes', () => {
    expect(controlCodeFor('[')).toBe('\x1b');
    expect(controlCodeFor('\\')).toBe('\x1c');
    expect(controlCodeFor(']')).toBe('\x1d');
    expect(controlCodeFor('^')).toBe('\x1e');
    expect(controlCodeFor('_')).toBe('\x1f');
    expect(controlCodeFor('?')).toBe('\x7f');
  });

  it('returns null for characters with no control code and for non-single chunks', () => {
    expect(controlCodeFor('5')).toBeNull();
    expect(controlCodeFor('-')).toBeNull();
    expect(controlCodeFor('~')).toBeNull();
    // An arrow's escape sequence, a paste, and the empty chunk are all
    // multi-or-zero length: never converted.
    expect(controlCodeFor('\x1b[A')).toBeNull();
    expect(controlCodeFor('git status')).toBeNull();
    expect(controlCodeFor('')).toBeNull();
  });
});

describe('applyStickyCtrl', () => {
  it('passes input through untouched when Ctrl is not armed', () => {
    expect(applyStickyCtrl('c', false)).toEqual({ data: 'c', armed: false });
    expect(applyStickyCtrl('\x1b[A', false)).toEqual({ data: '\x1b[A', armed: false });
  });

  it('converts a convertible character and spends the arm', () => {
    expect(applyStickyCtrl('c', true)).toEqual({ data: '\x03', armed: false });
    expect(applyStickyCtrl('D', true)).toEqual({ data: '\x04', armed: false });
    expect(applyStickyCtrl('?', true)).toEqual({ data: '\x7f', armed: false });
  });

  it('spends the arm without converting anything else', () => {
    // A digit, one of the row's own literal keys, and an arrow sequence all
    // reach the PTY unchanged, but the arm is gone either way.
    expect(applyStickyCtrl('5', true)).toEqual({ data: '5', armed: false });
    expect(applyStickyCtrl('|', true)).toEqual({ data: '|', armed: false });
    expect(applyStickyCtrl('\x1b[B', true)).toEqual({ data: '\x1b[B', armed: false });
    expect(applyStickyCtrl('pasted text', true)).toEqual({
      data: 'pasted text',
      armed: false,
    });
  });
});
