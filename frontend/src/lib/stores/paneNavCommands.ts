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

// terminal.refresh repaints a focused terminal (alt+shift+r → PTY winsize nudge
// → provider redraw). Like the pane-nav chords it must escape a focused xterm
// rather than be encoded to the PTY as a meta sequence, so the xterm key handler
// checks it against the same predicate. Kept as a distinct id from
// PANE_NAV_COMMAND_IDS because that set has pane-nav-only consumers (the Go
// un-gated alt-chord defaults parity test) that must not absorb it.
export const TERMINAL_REFRESH_COMMAND_ID = 'terminal.refresh';

// Every command id the terminal key handler lets bubble out of a focused xterm:
// pane navigation plus terminal.refresh. TerminalBody passes this to
// eventEscapesTerminalToCommand so all of these chords reach App.svelte's window
// keydown handler instead of the PTY.
export const TERMINAL_ESCAPE_COMMAND_IDS: ReadonlySet<string> = new Set([
  ...PANE_NAV_COMMAND_IDS,
  TERMINAL_REFRESH_COMMAND_ID,
]);
