import type { ThreadPane } from '../../stores/thread.svelte';
import { focusPaneComposer } from '../panes/paneComposerFocus';
import { getTerminalFocused } from './terminalStore.svelte';

/**
 * VS Code-style terminal toggle shared by the ⌘`/⌘J chord (`terminal.toggle`)
 * and the chat-header terminal button, so both open AND focus the terminal
 * identically instead of the button being visibility-only.
 *
 * Open: latch the focus intent on the pane, then show the drawer.
 * ThreadTerminalDrawer reads-and-clears that intent in its onMount — whenever
 * the lazily-loaded drawer chunk resolves and mounts — and focuses the xterm
 * once TerminalBody binds (a later frame on a cold first open while
 * OpenTerminal resolves). Latching on the pane rather than firing a one-shot
 * window event removes the cold-open race where the event was dispatched
 * before the lazy drawer had mounted and registered its listener.
 *
 * Close: if the terminal currently holds focus, hand it back to the composer
 * so the drawer unmount doesn't strand the caret on <body>.
 *
 * Callers must materialize the pane's thread first — a placeholder thread has
 * no terminal session to bind to.
 */
export function runTerminalToggle(pane: ThreadPane): void {
  const paneId = pane.paneId;
  if (!pane.showTerminal) {
    pane.requestTerminalFocus();
    pane.setShowTerminal(true);
    return;
  }
  // Scope the focus query to THIS pane so the rescue only fires when the pane
  // being toggled is the one holding terminal focus — never yanking focus from
  // a different pane's terminal. (Previously a module-global query that relied
  // on the close path only ever running for the focused pane; now that
  // terminals can live in their own panes, the pane-scoped read is the correct
  // primitive rather than a happens-to-be-safe global.)
  const terminalHadFocus = getTerminalFocused(paneId);
  pane.setShowTerminal(false);
  if (terminalHadFocus) focusPaneComposer(paneId);
}
