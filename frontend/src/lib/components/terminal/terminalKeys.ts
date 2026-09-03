// Pure predicates for the terminal's local keyboard chords, kept out of
// terminalXterm so both platform branches are unit-testable without mounting
// an xterm.
//
// Copy/paste follow the VS Code terminal conventions:
//   copy   Cmd+C (macOS) · Ctrl+Shift+C · Ctrl+Insert · plain Ctrl+C WHEN
//          a selection exists (with no selection Ctrl+C is SIGINT and must
//          reach the shell, so that one is `copyIfSelected`, decided by the
//          caller against the live selection)
//   paste  Cmd+V (macOS) · Ctrl+Shift+V · Shift+Insert
// Plain Ctrl+V is deliberately NOT a paste chord: Claude Code's TUI reads
// Ctrl+V as "paste image from clipboard", so it has to reach the PTY.
export type TerminalChordEvent = Pick<
  KeyboardEvent,
  'key' | 'ctrlKey' | 'shiftKey' | 'altKey' | 'metaKey'
>;

export type ClipboardChord = 'copy' | 'copyIfSelected' | 'paste';

export function clipboardChordFor(
  event: TerminalChordEvent,
  isMac: boolean,
): ClipboardChord | null {
  const key = event.key.toLowerCase();
  const { ctrlKey, shiftKey, altKey, metaKey } = event;
  if (altKey) return null;
  if (key === 'insert') {
    if (ctrlKey && !shiftKey && !metaKey) return 'copy';
    if (shiftKey && !ctrlKey && !metaKey) return 'paste';
    return null;
  }
  if (key !== 'c' && key !== 'v') return null;
  const action: ClipboardChord = key === 'c' ? 'copy' : 'paste';
  if (isMac && metaKey && !ctrlKey && !shiftKey) return action;
  if (ctrlKey && !metaKey) {
    if (shiftKey) return action;
    // Bare Ctrl+C copies only over a selection; bare Ctrl+V is the PTY's.
    return key === 'c' ? 'copyIfSelected' : null;
  }
  return null;
}

// The app's font-scale chords (utils/zoom.ts: mod + / = - 0). They are
// handled by a window keydown listener, so a focused xterm must skip its own
// handling and let the event bubble; the terminal's font size follows the same
// setting, so the chord zooms the terminal too. Mirrors zoom.ts's match
// (either ctrl or meta, any shift so Ctrl+Shift+= → '+' still zooms).
export function isFontZoomChord(event: TerminalChordEvent): boolean {
  if (!(event.ctrlKey || event.metaKey) || event.altKey) return false;
  const key = event.key;
  return key === '+' || key === '=' || key === '-' || key === '0';
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
