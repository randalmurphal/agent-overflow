import { describe, expect, it } from 'vitest';
import {
  chordMatches,
  encodeChord,
  encodeWhen,
  evaluateWhen,
  parseChord,
  parseWhen,
  tryParseChord,
  tryParseWhen,
} from './keybindingParser';

describe('keybindingParser — chords', () => {
  it('parses a plain key', () => {
    const c = parseChord('k');
    expect(c).toMatchObject({ key: 'k', metaKey: false, ctrlKey: false, shiftKey: false, altKey: false, modKey: false });
  });

  it('parses modifier aliases (cmd/ctrl/alt/option/shift/meta)', () => {
    expect(parseChord('cmd+k')).toMatchObject({ key: 'k', metaKey: true });
    expect(parseChord('meta+k')).toMatchObject({ key: 'k', metaKey: true });
    expect(parseChord('ctrl+k')).toMatchObject({ key: 'k', ctrlKey: true });
    expect(parseChord('control+k')).toMatchObject({ key: 'k', ctrlKey: true });
    expect(parseChord('alt+k')).toMatchObject({ key: 'k', altKey: true });
    expect(parseChord('option+k')).toMatchObject({ key: 'k', altKey: true });
    expect(parseChord('shift+k')).toMatchObject({ key: 'k', shiftKey: true });
  });

  it('parses mod+ as the platform modifier', () => {
    const c = parseChord('mod+j');
    expect(c).toMatchObject({ key: 'j', modKey: true });
  });

  it('treats trailing + as the "+" key', () => {
    const c = parseChord('shift++');
    expect(c).toMatchObject({ key: '+', shiftKey: true });
  });

  it('returns null for unparseable input', () => {
    expect(tryParseChord('')).toBeNull();
    // Two non-modifier tokens.
    expect(tryParseChord('j+k')).toBeNull();
    // Internal whitespace — not a valid single key token.
    expect(tryParseChord('garbage garbage')).toBeNull();
    // Explicit empty middle token via double-plus.
    expect(tryParseChord('cmd++j')).toBeNull();
  });

  it('parses "space" and "esc" aliases', () => {
    expect(parseChord('space')).toMatchObject({ key: ' ' });
    expect(parseChord('esc')).toMatchObject({ key: 'escape' });
  });

  it('encodes back to a canonical form', () => {
    expect(encodeChord(parseChord('mod+shift+p'))).toBe('mod+shift+p');
    expect(encodeChord(parseChord('cmd+k'))).toBe('cmd+k');
    expect(encodeChord(parseChord('space'))).toBe('space');
  });

  it('throws ParseError on invalid chords', () => {
    expect(() => parseChord('')).toThrow(/invalid keybinding shortcut/);
  });
});

describe('keybindingParser — chordMatches', () => {
  const ev = (
    key: string,
    mods: Partial<{ code: string; metaKey: boolean; ctrlKey: boolean; shiftKey: boolean; altKey: boolean }> = {},
  ) => ({
    key,
    code: mods.code,
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
    ...mods,
  });

  it('matches mod+k on macOS against cmd', () => {
    const chord = parseChord('mod+k');
    expect(chordMatches(chord, ev('k', { metaKey: true }), true)).toBe(true);
    expect(chordMatches(chord, ev('k', { ctrlKey: true }), true)).toBe(false);
  });

  it('matches mod+k on non-mac against ctrl', () => {
    const chord = parseChord('mod+k');
    expect(chordMatches(chord, ev('k', { ctrlKey: true }), false)).toBe(true);
    expect(chordMatches(chord, ev('k', { metaKey: true }), false)).toBe(false);
  });

  it('rejects when an unwanted modifier is held', () => {
    const chord = parseChord('mod+k');
    expect(chordMatches(chord, ev('k', { metaKey: true, shiftKey: true }), true)).toBe(false);
  });

  it('matches case-insensitively', () => {
    const chord = parseChord('mod+k');
    expect(chordMatches(chord, ev('K', { metaKey: true }), true)).toBe(true);
  });

  it('matches macOS Option-letter chords by physical key code when event.key is the produced glyph', () => {
    expect(chordMatches(parseChord('alt+h'), ev('˙', { code: 'KeyH', altKey: true }), true))
      .toBe(true);
    expect(chordMatches(parseChord('alt+l'), ev('¬', { code: 'KeyL', altKey: true }), true))
      .toBe(true);
    expect(chordMatches(parseChord('alt+shift+l'), ev('Ò', { code: 'KeyL', altKey: true, shiftKey: true }), true))
      .toBe(true);
  });

  it('does not use physical-key fallback off macOS', () => {
    expect(chordMatches(parseChord('alt+h'), ev('˙', { code: 'KeyH', altKey: true }), false))
      .toBe(false);
  });

  // The default terminal.newPane chord is `mod+shift+~` — the SHIFTED glyph,
  // not the bare backtick. On Chromium platforms Ctrl/Cmd+Shift+` produces
  // event.key="~" (the shift layer) and matches literally. On macOS WebKit
  // the layout's command-modifier table strips Shift, so Cmd+Shift+` arrives
  // as event.key="`" — the chord matches via the code-based Cmd+Shift
  // fallback instead. This pins all three shapes so neither the glyph
  // spelling nor the fallback can regress.
  describe('terminal.newPane glyph (mod+shift+~)', () => {
    it('parses to the tilde glyph with mod+shift and no other modifiers', () => {
      expect(tryParseChord('mod+shift+~')).toMatchObject({
        key: '~',
        modKey: true,
        shiftKey: true,
        metaKey: false,
        ctrlKey: false,
        altKey: false,
      });
    });

    it('matches Ctrl+Shift+Backtick (event.key="~") on non-mac', () => {
      const chord = parseChord('mod+shift+~');
      expect(chordMatches(chord, ev('~', { ctrlKey: true, shiftKey: true }), false)).toBe(true);
    });

    it('matches Cmd+Shift+Backtick (event.key="~") on mac — Chromium shape', () => {
      const chord = parseChord('mod+shift+~');
      expect(chordMatches(chord, ev('~', { metaKey: true, shiftKey: true }), true)).toBe(true);
    });

    it('matches Cmd+Shift+Backtick (event.key="`") on mac — WKWebView shape', () => {
      // The real WKWebView event: Cmd+Shift stripping means key is the
      // UNSHIFTED backtick; only code carries the physical key. Verified
      // against the live layout via UCKeyTranslate (cmd|shift + keycode 50
      // → "`").
      const chord = parseChord('mod+shift+~');
      expect(chordMatches(chord, ev('`', { metaKey: true, shiftKey: true, code: 'Backquote' }), true)).toBe(true);
    });

    it('does NOT match the bare backtick glyph off mac', () => {
      // On Chromium platforms "`" with shift means the layout genuinely
      // produced a backtick — the tilde chord must not fire, and the mac
      // Cmd+Shift fallback must stay unreachable there.
      const chord = parseChord('mod+shift+~');
      expect(chordMatches(chord, ev('`', { ctrlKey: true, shiftKey: true, code: 'Backquote' }), false)).toBe(false);
    });

    it('does NOT use the Cmd+Shift fallback without meta or without shift', () => {
      const chord = parseChord('mod+shift+~');
      // mac, ctrl+shift (no meta): no stripping occurs in reality, and the
      // fallback must not fire for a key the layout says is a backtick.
      expect(chordMatches(chord, ev('`', { ctrlKey: true, shiftKey: true, code: 'Backquote' }), true)).toBe(false);
      // mac, meta without shift: modifier mismatch, and no fallback either.
      expect(chordMatches(chord, ev('`', { metaKey: true, code: 'Backquote' }), true)).toBe(false);
    });
  });

  // Tab switching uses LITERAL ctrl+tab / ctrl+shift+tab — NOT mod+tab. On macOS
  // mod is Cmd, and Cmd+Tab is the OS app switcher (unreachable); ctrl+tab is the
  // cross-platform tab-cycle convention. Pin that the chord parses as literal
  // ctrl (no mod normalization) so a future "tidy to mod" can't silently break
  // it on macOS.
  describe('terminal tab-switch chords (literal ctrl+tab)', () => {
    it('parses ctrl+tab as literal ctrl, not mod', () => {
      expect(tryParseChord('ctrl+tab')).toMatchObject({
        key: 'tab',
        ctrlKey: true,
        modKey: false,
        metaKey: false,
        shiftKey: false,
        altKey: false,
      });
    });

    it('parses ctrl+shift+tab with shift and literal ctrl', () => {
      expect(tryParseChord('ctrl+shift+tab')).toMatchObject({
        key: 'tab',
        ctrlKey: true,
        shiftKey: true,
        modKey: false,
      });
    });

    it('matches Ctrl+Tab on both mac and non-mac, but never Cmd+Tab', () => {
      const chord = parseChord('ctrl+tab');
      expect(chordMatches(chord, ev('Tab', { ctrlKey: true }), false)).toBe(true);
      expect(chordMatches(chord, ev('Tab', { ctrlKey: true }), true)).toBe(true);
      // Cmd+Tab must NOT match a literal-ctrl chord (it's the OS app switcher).
      expect(chordMatches(chord, ev('Tab', { metaKey: true }), true)).toBe(false);
    });
  });
});

describe('keybindingParser — when expressions', () => {
  it('parses a plain identifier', () => {
    expect(parseWhen('terminalOpen')).toEqual({ type: 'identifier', name: 'terminalOpen' });
  });

  it('parses NOT', () => {
    expect(parseWhen('!paletteOpen')).toEqual({
      type: 'not',
      node: { type: 'identifier', name: 'paletteOpen' },
    });
  });

  it('parses AND / OR with correct precedence', () => {
    // a || b && c should parse as (a || (b && c)).
    const ast = parseWhen('a || b && c');
    expect(ast).toEqual({
      type: 'or',
      left: { type: 'identifier', name: 'a' },
      right: {
        type: 'and',
        left: { type: 'identifier', name: 'b' },
        right: { type: 'identifier', name: 'c' },
      },
    });
  });

  it('respects parentheses', () => {
    const ast = parseWhen('(a || b) && c');
    expect(ast).toEqual({
      type: 'and',
      left: {
        type: 'or',
        left: { type: 'identifier', name: 'a' },
        right: { type: 'identifier', name: 'b' },
      },
      right: { type: 'identifier', name: 'c' },
    });
  });

  it('returns null (or throws) on malformed input', () => {
    expect(tryParseWhen('')).toBeNull();
    expect(tryParseWhen('a &&')).toBeNull();
    expect(tryParseWhen('(a')).toBeNull();
    expect(tryParseWhen('!! ')).toBeNull();
    expect(() => parseWhen('a &&')).toThrow(/invalid when/);
  });

  it('evaluateWhen treats missing identifiers as false', () => {
    const ast = parseWhen('!paletteOpen && terminalOpen');
    expect(evaluateWhen(ast, { terminalOpen: true })).toBe(true);
    expect(evaluateWhen(ast, { terminalOpen: true, paletteOpen: true })).toBe(false);
    expect(evaluateWhen(ast, {})).toBe(false);
  });

  it('round-trips through encode/decode', () => {
    const original = '!paletteOpen && (terminalOpen || approvalPending)';
    const ast = parseWhen(original);
    const re = parseWhen(encodeWhen(ast));
    expect(re).toEqual(ast);
  });
});
