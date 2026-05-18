# Pane Lifecycle

> Creating, closing, reordering panes.

## Decision

### Creation

A pane is created only when a routing gesture asks for one **with a thing in it** (today: a thread). There is no "open empty pane" gesture.

Creation gestures:

- `Ctrl/Cmd+click` on an existing sidebar thread.
- `Ctrl/Cmd+Shift+N` (new thread in new pane).
- Right-click thread → "Open in new pane".
- Drag thread from sidebar onto the pane row.

See [thread-routing.md](./thread-routing.md) and [drag-and-drop.md](./drag-and-drop.md) for the gesture details. The no-duplicates rule applies to all of them.

### Closing

- `Ctrl/Cmd+W` while a pane is focused → closes that pane.
- Each pane header renders an `×` button → closes that pane.
- **Closing is allowed on the last remaining pane.** When the layout becomes 0 panes, the workspace area shows the existing "create a new thread" empty-state screen.

### Post-close focus

Focus moves to the **adjacent left neighbor**. If the closed pane was leftmost, focus moves to the new leftmost pane. If no panes remain, no pane is focused (the empty-state surface holds focus).

### Reorder

- Drag a pane header → moves the pane left or right in the row.
- Drag past the first or last position clamps (no detach-to-new-window in v1 — see [out-of-scope.md](./out-of-scope.md)).
- Keyboard: `Alt+Shift+ArrowLeft/Right` or `Alt+Shift+H/L` → moves focused pane left/right by one position.

## Empty state (0 panes)

The "create a new thread" empty-state surface that the app currently shows at startup (before any thread is loaded) becomes the fallback for **the entire pane row** when no panes exist. Same surface, no new UI.

When the user creates a thread from that empty state, the first pane is created with the new thread, and the layout grows from there.

## Empty-pane safety

The invariant **"a pane always has a thing in it"** holds at every moment of the lifecycle. The only way to have a pane is by creating one via a thread-pairing gesture. Closing a pane removes it entirely, never leaves it empty.

If a future `PaneLayoutKind` is added (e.g. a docs viewer pane), the invariant still holds: the layout item carries its kind and target reference at creation. The "empty pane" state never exists.

## Background behavior

Closing a pane that hosts a running agent (streaming turn, pending approval, etc.) does **not** interrupt the agent. The provider process is the source of truth (per `CLAUDE.md` core principle); the pane is just a view. The user can re-open the thread in any pane to continue watching.

## Implementation notes

- `frontend/src/lib/stores/paneLayout.svelte.ts:28-35` — `removePaneLayoutItem` currently re-inserts a default `main` pane when emptied. **Relax this**: allow the array to be empty.
- `PaneHost.svelte` needs a branch rendering the empty-state surface when `getPaneLayoutItems().length === 0`. Lift the surface out of wherever it currently lives in `ChatView` (or wherever the startup empty state currently renders) so it can be hosted at the pane-row level.
- Pane header `×`: new affordance per pane; calls `removePaneLayoutItem(paneId)` followed by post-close focus logic.
- `Ctrl+W` keybinding: new entry in the keybinding registry, routed to the focused pane's close action.
- Post-close focus: small helper in `panes.svelte.ts` that resolves "adjacent left neighbor" against the current layout-items array.
