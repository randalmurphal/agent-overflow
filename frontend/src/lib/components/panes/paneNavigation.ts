// Keyboard pane navigation (pane.focusLeft, pane.focusRight): the one
// handoff from the pane focus leaves to the pane it reaches. Pointer input
// and DOM focus observations use plain focusPane instead, which neither
// reveals nor moves the caret.

import { focusAdjacentPane, getPane } from '../../stores/panes.svelte';
import { isThreadHostingPaneKind } from '../../stores/paneLayout.svelte';
import { carryCaretIntoPane, isEditingFocused } from './paneComposerFocus';

/**
 * Moves focus to the pane beside the focused one and reveals it. A caret
 * the user was typing in goes with it, to the pane's composer or its
 * section (carryCaretIntoPane), never staying in the pane left. A
 * terminal-mode thread latches its terminal, which takes focus once its
 * surface is ready and only while focus is still in that pane
 * (TerminalSurface). Returns false at the strip's end.
 */
export function navigateToAdjacentPane(direction: -1 | 1): boolean {
  const carrying = isEditingFocused();
  const next = focusAdjacentPane(direction);
  if (!next) return false;
  if (carrying) carryCaretIntoPane(next.paneId);
  const pane = next.kind === 'thread' || isThreadHostingPaneKind(next.kind) ? getPane(next.paneId) : undefined;
  if (pane?.thread?.mode === 'terminal') pane.requestTerminalFocus();
  return true;
}
