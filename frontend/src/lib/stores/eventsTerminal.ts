// Backgrounded-terminal event domain: streaming terminal output into the
// per-pane terminal state and handling terminal-exit teardown (tab
// removal, active-pane refocus, and last-tab thread cleanup). Fan-in
// target of events.ts's setupEventListeners.
import type {
  TerminalExitEventPayload,
  TerminalOutputEventPayload,
} from '../types/terminal';
import { decodeTerminalOutput } from '../types/terminal';
import { closePanesShowingThread, panesShowingThread } from './panes.svelte';
import { getThreadById, removeThread } from './threads.svelte';
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';
import {
  getTerminalFocused,
  getThreadTerminalStateForTerminalEvent,
} from '../components/terminal/terminalStore.svelte';
import { DeleteThread } from './bindings';

export function applyTerminalOutput(payload: TerminalOutputEventPayload): void {
  if (!payload?.threadID || !payload.terminalID) return;
  const decoded = decodeTerminalOutput(payload.data);
  getThreadTerminalStateForTerminalEvent(payload.threadID, payload.terminalID).appendOutput(
    payload.terminalID,
    decoded,
    payload.sequence,
  );
}

export async function applyTerminalExit(payload: TerminalExitEventPayload): Promise<void> {
  if (!payload?.threadID || !payload.terminalID) return;
  const handle = getThreadTerminalStateForTerminalEvent(payload.threadID, payload.terminalID);

  // Removing the ACTIVE tab promotes a sibling, changing activeTerminalID and
  // remounting TerminalBody (keyed on that id) — which blurs the dying xterm.
  // Capture, BEFORE the mutation, which panes had the user actually focused IN
  // this terminal, so focus can follow into the promoted sibling — parity with
  // the close paths (the ✕ button latches pendingFocus directly; Ctrl+Shift+W
  // uses the same pane.requestTerminalFocus channel this handler does). The
  // focus check is the load-bearing guard: a backgrounded shell (`sleep 5; exit`)
  // that exits while the user types in the composer leaves getTerminalFocused()
  // false for that pane, so the cursor is NOT yanked away.
  const exitingActiveTab = handle.activeTerminalID === payload.terminalID;
  const panesToRefocus = exitingActiveTab
    ? panesShowingThread(payload.threadID).filter((pane) => getTerminalFocused(pane.paneId))
    : [];

  handle.removeTab(payload.terminalID);

  // A terminal thread exists only while it has a live shell. When its last
  // session ends — ctrl+D, or the × on the last tab — the terminal is done:
  // tear down any pane showing it and drop it from the sidebar + store. This
  // is the single seam for issue #2 (the pane must close) and #3 (the sidebar
  // must clear). Closing just the *pane* never kills the shell, so no exit
  // fires and the terminal stays backgrounded — exactly the desired split.
  //
  // Guards:
  //  - mode === 'terminal' — a chat thread's bottom-drawer terminal exiting
  //    (ctrl+D) must NOT delete the chat thread; only its tab is removed.
  //  - thread still present — an explicit context-menu delete already removed
  //    it from the store (and emits an exit via CloseThread); the lookup
  //    returns undefined, so the redundant re-delete is skipped.
  //  - app shutdown never reaches here — Go suppresses terminal:exit while
  //    shuttingDown, so backgrounded terminals persist across restart.
  //
  // The shell is already dead, so close any pane showing it immediately (#2).
  // The sidebar row, by contrast, is dropped only once the backend DeleteThread
  // resolves — mirroring the canonical deleteThreadAction order. Removing it
  // first would resurrect a shell-less ghost on the next thread-list refresh if
  // the RPC failed; instead we keep the row and surface the failure, since
  // "errors are user-facing state, not log entries."
  if (handle.tabs.length > 0) {
    // A sibling was promoted to active: follow the cursor into it for the panes
    // where the user was focused in the terminal that just exited.
    // requestTerminalFocus latches the pane's intent; the surface's consume
    // effect lands it on the remounted body within this same flush.
    for (const pane of panesToRefocus) pane.requestTerminalFocus();
    return;
  }
  const thread = getThreadById(payload.threadID);
  if (thread?.mode !== 'terminal') return;
  closePanesShowingThread(payload.threadID);
  try {
    await DeleteThread(payload.threadID);
    removeThread(payload.threadID);
  } catch (err) {
    console.error('terminal: delete thread after last exit failed', err);
    addToast('error', `Could not remove terminal: ${errString(err)}`);
  }
}
