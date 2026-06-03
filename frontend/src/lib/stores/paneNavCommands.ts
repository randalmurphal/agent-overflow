// The four pane-navigation command ids, shared by the command registrations
// (builtinCommands) and the terminal's xterm key handler (TerminalBody). The
// handler must let chords bound to these commands bubble out of a focused
// terminal — when they are un-gated — instead of writing them to the PTY.
// Kept in its own dependency-free module so TerminalBody doesn't pull in the
// whole command-registry graph just to name them.
export const PANE_NAV_COMMAND_IDS: ReadonlySet<string> = new Set([
  'pane.focusLeft',
  'pane.focusRight',
  'pane.moveLeft',
  'pane.moveRight',
]);
