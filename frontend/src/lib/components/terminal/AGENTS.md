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

`terminalFind.svelte.ts` is the find of `TerminalBody` (one per tab, under
`terminalFindKey`) and of the take-control mirror (under its pane id). The
tab strip's find button opens the active tab's bar. mod+f inside a focused
xterm stays the shell's (readline, pagers): never make `find.in-pane` a
terminal escape chord. `terminalFindScan.ts` matches each logical line
(wrapped rows joined) by `utils/findMatch.ts` through xterm's public buffer
API, over the 4000 lines of scrollback the terminal keeps, and under a
full-screen program (alternate screen) the visible screen only. The newest
1000 matches are counted and painted.

Find paints on its own layer over the grid (`terminalFindLayer.ts`), never
with xterm decorations: xterm 6 leaves a decoration registered for the
terminal's life when the parser disposes its marker out of row order, and
`TerminalFindTerminal` has no `registerDecoration`. The layer draws a
translucent rectangle per run of matched cells from a marker on its row,
after each paint and each xterm render, with a node only per rectangle in
view, and takes no pointer events. The active match is drawn once, in the
current ground.

Find never reads or writes the terminal selection; it is the reader's. Both
surfaces float the bar (`TERMINAL_FIND_OVERLAY_CLASS` inside
`data-terminal-find-host`) over the grid's top right on desktop and, under
compact, over the header row above the grid (the tab strip, the mirror's
control row), which is `inert` while the bar is open there. Opening and
closing it changes neither the rows nor the PTY size, either of which would
drop the selection or signal the program. A query change and a step scroll
the active match clear of the cells the bar covers (`obscured`, measured
from geometry, so none under compact), and so does a change to the bar's
size (`onObscuredChange`), since the bar mounts after a reopen's search. On
desktop, with a buffer too short to scroll, a match on the top two rows from
the bar's left column stays under it. The active match is anchored twice, by its line's first row plus its
offset and by the next line's first row plus its distance from the line's
end, so it survives reflow and trimming of its line's first rows.
Re-searches run from `onBufferChange`, the one notification `buildTerminal`
sends for output, resizes, `clear()` and `reset()`, through the refresh
scheduler: never scan per write. A re-search reads only the rows from the
bottom up to the 1000th match, makes a marker per painted row and two
anchors, and disposes everything the previous scan made
(`terminalFind.browser.test.ts` counts it). A screen switch drops the paint
at once, since the layer draws by row on whichever screen is active.
Register find only after the replay is parsed (the write callback) and a
frame after the grid is fitted to it, or the restored match leaves the
view.

Extend the `FakeTerminal` in `TerminalBody.test.ts` for component tests. Reset
`setCompactLayoutForTest` after every test that changes it.
