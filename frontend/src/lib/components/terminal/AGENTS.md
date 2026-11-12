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

While the terminal's find bar holds focus nothing the reader does reaches the
PTY: the key row types into the query (Esc closes the bar), and a paste whose
clipboard read finishes then is dropped (`acceptsPaste`). xterm's own replies
to the program also travel through `writeInput`, so never gate that function
on focus.

A terminal has focus while keyboard focus is inside its surface, find bar
included (`terminalFocusRegion`, read by `getTerminalFocused`). Tab end, tab
close and drawer hide move focus by it. The keybinding context's
`terminalFocus` is narrower, the xterm alone, because it hands chords to the
shell.

## Find

`terminalFind.svelte.ts` drives the search addon `buildTerminal` loads, for
`TerminalBody` (one find per tab, under `terminalFindKey`) and the
take-control mirror (under its pane id). The tab strip's find button opens
the active tab's bar. mod+f inside a focused xterm stays the shell's
(readline, pagers): never make `find.in-pane` a terminal escape chord. The
addon matches, not `utils/findMatch.ts`: trimmed, case-insensitive, up to
1000 counted. It searches the 4000 lines of scrollback the terminal keeps,
and under a full-screen program (alternate screen) only the visible screen.

The provider schedules every re-search from `onBufferChange`, the one
notification `buildTerminal` sends for output, resizes, `clear()` and
`reset()`; `terminalSearch.ts` stops the addon's own. Register find only
after the replay is parsed (the write callback) and a frame after the grid is
fitted to it, or the restored match leaves the view. Find owns
only the selection its search made: a re-search keeps it, and waits while the
reader holds a selection of their own. A change of rows drops xterm's
selection; the re-search selects find's match again.

Extend the `FakeTerminal` in `TerminalBody.test.ts` for component tests. Reset
`setCompactLayoutForTest` after every test that changes it.
