# Keyboard Navigation

> Hotkeys for pane operations.

## Decision

Two modifier conventions:

- **`Ctrl/Cmd`** = thread operations.
- **`Alt`** = pane operations.

| Hotkey | Effect |
|---|---|
| `Ctrl/Cmd+N` | New thread in focused pane |
| `Ctrl/Cmd+Shift+N` | New thread in a new pane |
| `Ctrl/Cmd+W` | Close focused pane |
| `Alt+ArrowLeft` / `Alt+H` | Focus pane to the left |
| `Alt+ArrowRight` / `Alt+L` | Focus pane to the right |
| `Alt+Shift+ArrowLeft` / `Alt+Shift+H` | Move focused pane left |
| `Alt+Shift+ArrowRight` / `Alt+Shift+L` | Move focused pane right |

`Ctrl` and `Cmd` resolve to the same logical modifier through the existing keybinding system (Cmd on macOS, Ctrl elsewhere).

Vim-style `H` / `L` bindings are alternates to the arrow-key forms; both must be supported.

## Edge behavior

- **Focus nav stops at the row edges.** No wrap-around. `Alt+ArrowRight` from the rightmost pane is a no-op.
- **Move-pane clamps** at the first or last position. `Alt+Shift+ArrowRight` from the rightmost pane is a no-op.
- **Focus onto off-screen pane** (due to overflow scroll) → pane row auto-scrolls to bring the newly focused pane into view. The reveal belongs to the nav command, not to focus itself: `focusPane` never scrolls, and explicit-intent sites (keyboard nav, thread opens, click focus transitions) pair it with `revealPane`. Reuses the same scroll-into-view helper used by [thread-routing.md](./thread-routing.md) and [attention-indicators.md](./attention-indicators.md).

## Why `Alt`

The existing thread-level hotkeys cluster on `Ctrl/Cmd`. Using `Alt` for pane operations keeps the two layers visually distinct: when you press `Alt`, you're talking about the pane shell; when you press `Ctrl`, you're talking about thread content.

This convention also leaves `Ctrl/Cmd+1` … `Ctrl/Cmd+9` available for other uses (or for a future direct-numbered pane focus — see [out-of-scope.md](./out-of-scope.md)).

## Implementation notes

- All bindings register through the existing keybinding registry.
- The handlers all funnel into store actions on `panes.svelte.ts`:
  - `focusAdjacentPane(direction)` for `Alt+arrow/H/L` — every mounted
    pane is a stop, companions and take-control terminals included
  - `moveFocusedPane(direction)` for `Alt+Shift+arrow/H/L`
  - `closeFocusedPaneOrCompanion()` (companionPanes.svelte.ts) for `Ctrl+W`
  - `openThreadInNewPane(...)` for `Ctrl+Shift+N`
- Verify no collisions with existing bindings (e.g. composer's text-input shortcuts, the palette opener). The chat-composer keybindings live in the composer component and are scoped to composer focus — they should not collide.
