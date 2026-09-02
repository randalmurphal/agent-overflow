// Pure predicate for the terminal's local clipboard chords, kept out of
// TerminalBody so both platform branches are unit-testable without mounting an
// xterm. Copy/paste follow the platform terminal convention: Cmd+C / Cmd+V on
// macOS, Ctrl+Shift+C / Ctrl+Shift+V elsewhere (a bare Ctrl+C is SIGINT, so the
// shifted chord is the copy). A plain Ctrl+C is a non-match, so it still reaches
// the shell as SIGINT.
export type ClipboardChordEvent = Pick<
  KeyboardEvent,
  'key' | 'ctrlKey' | 'shiftKey' | 'altKey' | 'metaKey'
>;

export function isClipboardChord(
  event: ClipboardChordEvent,
  letter: 'c' | 'v',
  isMac: boolean,
): boolean {
  if (event.key.toLowerCase() !== letter) return false;
  return isMac
    ? event.metaKey && !event.ctrlKey && !event.altKey && !event.shiftKey
    : event.ctrlKey && event.shiftKey && !event.altKey && !event.metaKey;
}

// --- Sticky Ctrl (compact key row) ---------------------------------------
//
// Phone keyboards have no Ctrl, so the compact key row offers it as a STICKY
// modifier: one tap arms it, the next character delivered through the
// terminal's input path is converted to its control code, and the arm is
// spent. That "next character" reaches the same `term.onData` stream from
// either source — a key-row button (which goes in via `term.input`) or a letter
// typed on the soft keyboard — so the conversion belongs on the input path, not
// on the button. Kept here as pure functions so the table is testable without
// mounting an xterm.

// The control code a single character maps to under Ctrl, or null when the
// character has none. A character with no mapping spends the arm without
// conversion: there is no meaningful Ctrl+`5`, and swallowing the keystroke
// would be worse than passing it through.
export function controlCodeFor(data: string): string | null {
  if (data.length !== 1) return null;
  const code = data.charCodeAt(0);
  // 'a'..'z' -> \x01..\x1a, and the same for the shifted glyphs 'A'..'Z'.
  if (code >= 0x61 && code <= 0x7a) return String.fromCharCode(code - 0x60);
  if (code >= 0x41 && code <= 0x5a) return String.fromCharCode(code - 0x40);
  switch (data) {
    case '[':
      return '\x1b';
    case '\\':
      return '\x1c';
    case ']':
      return '\x1d';
    case '^':
      return '\x1e';
    case '_':
      return '\x1f';
    case '?':
      return '\x7f';
    default:
      return null;
  }
}

export interface StickyCtrlResult {
  /** The bytes to write to the PTY. */
  data: string;
  /** Whether Ctrl is still armed after this input (always false once spent). */
  armed: boolean;
}

// Apply an armed sticky Ctrl to one chunk of terminal input. Anything reaching
// the input path while armed spends the arm: a convertible character is
// converted, everything else (an arrow's escape sequence, a paste, a digit)
// passes through untouched. Unarmed input is returned verbatim.
export function applyStickyCtrl(data: string, armed: boolean): StickyCtrlResult {
  if (!armed) return { data, armed: false };
  return { data: controlCodeFor(data) ?? data, armed: false };
}
