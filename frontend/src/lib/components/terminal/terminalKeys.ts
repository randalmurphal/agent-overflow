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
