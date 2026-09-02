import { describe, expect, it } from 'vitest';

import { clipboardChordFor, isFontZoomChord, type TerminalChordEvent } from './terminalKeys';

function ev(over: Partial<TerminalChordEvent>): TerminalChordEvent {
  return { key: 'a', ctrlKey: false, shiftKey: false, altKey: false, metaKey: false, ...over };
}

describe('clipboardChordFor', () => {
  describe('non-macOS', () => {
    it('Ctrl+Shift+C is copy (lower- and upper-case key glyph)', () => {
      expect(clipboardChordFor(ev({ key: 'c', ctrlKey: true, shiftKey: true }), false)).toBe('copy');
      // With Shift held the browser may report the upper-case glyph.
      expect(clipboardChordFor(ev({ key: 'C', ctrlKey: true, shiftKey: true }), false)).toBe('copy');
    });

    it('Ctrl+Shift+V is paste', () => {
      expect(clipboardChordFor(ev({ key: 'v', ctrlKey: true, shiftKey: true }), false)).toBe('paste');
    });

    it('plain Ctrl+C is copy-if-selected — the caller decides SIGINT vs copy', () => {
      expect(clipboardChordFor(ev({ key: 'c', ctrlKey: true }), false)).toBe('copyIfSelected');
    });

    it('plain Ctrl+V is NOT a chord (Claude Code reads it as image paste)', () => {
      expect(clipboardChordFor(ev({ key: 'v', ctrlKey: true }), false)).toBeNull();
    });

    it('Ctrl+Insert copies and Shift+Insert pastes', () => {
      expect(clipboardChordFor(ev({ key: 'Insert', ctrlKey: true }), false)).toBe('copy');
      expect(clipboardChordFor(ev({ key: 'Insert', shiftKey: true }), false)).toBe('paste');
      expect(clipboardChordFor(ev({ key: 'Insert' }), false)).toBeNull();
      expect(clipboardChordFor(ev({ key: 'Insert', ctrlKey: true, shiftKey: true }), false)).toBeNull();
    });

    it('Cmd+C is not a chord on non-mac', () => {
      expect(clipboardChordFor(ev({ key: 'c', metaKey: true }), false)).toBeNull();
    });

    it('an extra modifier defeats the chord', () => {
      expect(clipboardChordFor(ev({ key: 'c', ctrlKey: true, shiftKey: true, altKey: true }), false)).toBeNull();
      expect(clipboardChordFor(ev({ key: 'c', ctrlKey: true, shiftKey: true, metaKey: true }), false)).toBeNull();
      expect(clipboardChordFor(ev({ key: 'c', ctrlKey: true, altKey: true }), false)).toBeNull();
    });

    it('other letters are not chords', () => {
      expect(clipboardChordFor(ev({ key: 'x', ctrlKey: true, shiftKey: true }), false)).toBeNull();
    });
  });

  describe('macOS', () => {
    it('Cmd+C is copy and Cmd+V is paste', () => {
      expect(clipboardChordFor(ev({ key: 'c', metaKey: true }), true)).toBe('copy');
      expect(clipboardChordFor(ev({ key: 'v', metaKey: true }), true)).toBe('paste');
    });

    it('Ctrl+Shift+C/V still work on mac (same rule everywhere)', () => {
      expect(clipboardChordFor(ev({ key: 'c', ctrlKey: true, shiftKey: true }), true)).toBe('copy');
      expect(clipboardChordFor(ev({ key: 'v', ctrlKey: true, shiftKey: true }), true)).toBe('paste');
    });

    it('plain Ctrl+C is copy-if-selected on mac too', () => {
      expect(clipboardChordFor(ev({ key: 'c', ctrlKey: true }), true)).toBe('copyIfSelected');
    });

    it('Cmd+Shift+C is not a chord (Cmd only, no Shift)', () => {
      expect(clipboardChordFor(ev({ key: 'c', metaKey: true, shiftKey: true }), true)).toBeNull();
    });
  });
});

describe('isFontZoomChord', () => {
  it('matches mod + / = - 0 with ctrl or meta', () => {
    for (const key of ['+', '=', '-', '0']) {
      expect(isFontZoomChord(ev({ key, ctrlKey: true }))).toBe(true);
      expect(isFontZoomChord(ev({ key, metaKey: true }))).toBe(true);
    }
    // Ctrl+Shift+= reports '+' on most layouts; shift must not defeat it.
    expect(isFontZoomChord(ev({ key: '+', ctrlKey: true, shiftKey: true }))).toBe(true);
  });

  it('ignores the bare keys, alt chords and other keys', () => {
    expect(isFontZoomChord(ev({ key: '-' }))).toBe(false);
    expect(isFontZoomChord(ev({ key: '0' }))).toBe(false);
    expect(isFontZoomChord(ev({ key: '-', ctrlKey: true, altKey: true }))).toBe(false);
    expect(isFontZoomChord(ev({ key: '1', ctrlKey: true }))).toBe(false);
    expect(isFontZoomChord(ev({ key: 'c', ctrlKey: true }))).toBe(false);
  });
});
