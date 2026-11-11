# components/terminal/

The thread terminal is an xterm.js widget over the backend PTY. `TerminalBody`
owns the xterm instance and every operation on it.

## Input ownership

Every byte sent to the PTY goes through `TerminalBody.writeInput`. Wire
`term.onData` and generated widget input to that function. New input sources
must use `term.input(data, true)` so ordering, connection checks, and sticky
Ctrl handling stay on one path. Do not call `WriteTerminal` elsewhere.

`TerminalKeyRow.svelte` supplies keys missing from compact soft keyboards. It
returns bytes to `TerminalBody`; it never writes directly. Sticky Ctrl is
applied in `terminalKeys.ts` on the shared input path because the following
character usually arrives through `term.onData`.

Key-row pointer presses must retain terminal focus. Keep `tabindex="-1"` and
prevent the default `pointerdown` behavior. In compact mode the terminal fills
the chat column and does not expose the desktop resize handle.

## Find

`terminalFind.svelte.ts` drives the search addon `buildTerminal` loads, for
`TerminalBody` (one find per tab, under `terminalFindKey`) and the
take-control mirror (under its pane id). The tab strip's find button opens
the active tab's bar. mod+f inside a focused xterm stays the shell's
(readline, pagers): never make `find.in-pane` a terminal escape chord. The
addon matches, not `utils/findMatch.ts`: trimmed, case-insensitive, up to
1000 counted. It searches the 4000 lines of scrollback the terminal keeps,
and under a full-screen program (alternate screen) only the visible screen.
New output re-searches once writes pause; until then the count reads as
updating.

Extend the `FakeTerminal` in `TerminalBody.test.ts` for component tests. Reset
`setCompactLayoutForTest` after every test that changes it.
