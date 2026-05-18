# Thread Routing

> How threads enter and switch within panes.

## Decision

| Gesture | Effect |
|---|---|
| Click thread in sidebar | Focused pane switches to that thread. If thread already in another pane, focus jumps to that pane (auto-scrolls into view if off-screen). |
| `Ctrl/Cmd+N` or "new thread" button | New thread created in focused pane (replaces current). |
| `Ctrl/Cmd+click` on existing thread in sidebar | Opens thread in a new pane. **Unless** thread is already in another pane → focuses existing pane instead. |
| `Ctrl/Cmd+Shift+N` | Creates new thread in a **new** pane (always — no existing duplicate to fall back to). |
| Right-click thread in sidebar | Context menu includes "Open in new pane". Same no-duplicates rule as `Ctrl+click`. |
| Drag thread from sidebar onto pane row | Adds a pane at the drop position. See [drag-and-drop.md](./drag-and-drop.md). |

`Ctrl` and `Cmd` are equivalent here; both resolve through the existing keybinding-modifier abstraction.

## No-duplicates rule

**A thread is never displayed in more than one pane simultaneously.** This rule overrides every routing gesture. If a gesture would result in a duplicate, focus instead jumps to the existing pane.

The existing test `frontend/src/lib/stores/panes.svelte.test.ts:80-183` enforces this for the basic single-click case ("focuses the existing pane instead of duplicating a visible thread"). This must extend to the new gestures introduced here: `Ctrl+click`, `Ctrl+Shift+N`, right-click "open in new pane", drag-drop.

## Off-screen focus jump

If a routing gesture causes focus to land on a pane scrolled out of view (due to horizontal overflow in the pane row), the pane row scrolls automatically to bring it into view. Smooth scroll preferred (`scrollIntoView({behavior: 'smooth'})` or controller-owned animated scroll).

## Implementation notes

- `frontend/src/lib/stores/panes.svelte.ts` `commitPanePreview` / `openThreadFromNavigation` already handle the focus-vs-create plumbing. The routing rules above are policy on top.
- "Open in new pane" semantics need the no-duplicates collapse — if the target thread is already in any pane, route to that pane regardless of which gesture asked for a new pane.
- `Ctrl+click` detection in `frontend/src/lib/components/sidebar/ThreadRow.svelte`.
- Right-click context menu on sidebar threads is new; reuse `frontend/src/lib/components/primitives/Menu.svelte`.
- Auto-scroll-into-view: new helper on the pane row, called from any gesture that focuses a pane. The same helper is reused by keyboard nav (see [keyboard-nav.md](./keyboard-nav.md)) and by attention-dot clicks (see [attention-indicators.md](./attention-indicators.md)).
