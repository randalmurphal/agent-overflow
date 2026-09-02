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
