# components/terminal/

The thread terminal: an xterm.js widget over the backend PTY, hosted in a
bottom drawer inside the chat column (`ThreadTerminalPlacement` →
`LazyThreadTerminalDrawer` → `ThreadTerminalDrawer` → `TerminalSurface` →
`TerminalBody`). `TerminalBody` owns the xterm instance; everything that
touches `term` lives there.

## One PTY writer

Every byte reaching the PTY goes through `TerminalBody.writeInput`, which
is wired to `term.onData` AND passed to `buildTerminal` as `onInput` for
the bytes the widget produces itself (the Shift+Enter newline). Nothing
else may call `WriteTerminal`. A new source of input goes in through
`term.input(data, true)` so it lands on `onData` like a keystroke, rather
than opening a second path with its own ordering and its own gates.

## The compact key row

`TerminalKeyRow.svelte` is the compact (phone) terminal's modifier
surface, rendered by `TerminalBody` only when `isCompactLayout()`. It
carries the keys a soft keyboard cannot produce: Esc, Tab, Ctrl, the
arrows, and `-` `/` `|` `~`. It writes nothing itself — it hands bytes
back to `TerminalBody`, which delivers them through `term.input`, so the
one-writer rule above still holds.

Sticky Ctrl lives in `terminalKeys.ts` (`controlCodeFor` /
`applyStickyCtrl`), applied inside `writeInput`. It has to sit on the
input path, not on the button: the character it converts usually arrives
from the soft keyboard via `onData`, not from the row. One tap arms it,
the next input chunk spends it — converted if it has a control code,
passed through unchanged if it does not.

A key-row press must never take focus off the terminal, or the soft
keyboard dismisses on every tap. Both defenses are pinned by tests:
`tabindex="-1"` and `preventDefault()` on `pointerdown`.

Under compact the drawer stops being a drawer: `ThreadTerminalDrawer`
covers the chat column with `compact:absolute compact:inset-0` and passes
`fill` to the `Drawer` primitive, which drops the inline height (an
inline style would beat the fill class) and the resize handle. Desktop is
untouched — the `compact:` variant only matches under `.layout-compact`.

## Test notes

The xterm test double lives in `TerminalBody.test.ts`: a `FakeTerminal`
recording writes, resizes and pastes, with a real textarea in `open()` so
focus is observable. Extend it rather than adding a second double.
Compact-only behavior is driven with `setCompactLayoutForTest(true)` and
must be reset in `afterEach`, or it leaks into later files.
