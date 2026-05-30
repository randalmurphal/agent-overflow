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
  // `getTerminalFocused()` is a global query, but the close path only ever runs
  // for the pane whose terminal is (or just was) focused, so it never yanks
  // focus across panes:
  //   • Chord (terminal.toggle): the command context resolves to the focused
  //     pane, and focusing an xterm bubbles up to ChatView's
  //     `onfocusin={() => focusPane(paneId)}` — so the targeted pane IS the one
  //     holding terminal focus.
  //   • Header button: clicking it moves DOM focus to the button, blurring the
  //     xterm first, so `getTerminalFocused()` is already false here and no
  //     composer rescue fires.
  //   • Palette: terminal.toggle can run against the palette's pinned target
  //     pane (CommandPalette resolves it via `contextForPane(targetPaneId)`),
  //     which may differ from the focused pane. Still safe: opening the palette
  //     focuses its list and blurs the xterm, so the counter is already false
  //     before the command runs.
  // If the terminal ever moves out of that focusPane wrapper (e.g. into an RHS
  // panel — a documented future policy), revisit this: the rescue would then
  // need to be pane-scoped (`getTerminalFocused(paneId)`).
  const terminalHadFocus = getTerminalFocused();
  pane.setShowTerminal(false);
  if (terminalHadFocus) focusPaneComposer(paneId);
}
