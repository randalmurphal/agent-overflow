// Backgrounded-terminal event domain: streaming terminal output into the
// per-pane terminal state and handling terminal-exit teardown (tab
// removal, active-pane refocus, and last-tab thread cleanup). Fan-in
// target of events.ts's setupEventListeners.
import type {
  TerminalExitEventPayload,
  TerminalHandle,
  TerminalOutputEventPayload,
} from '../types/terminal';
import { decodeTerminalOutput } from '../types/terminal';
import { closePanesShowingThread, panesShowingThread } from './panes.svelte';
import { getThreadById, removeThread } from './threads.svelte';
import { addToast } from './toast.svelte';
import { errString } from '../utils/errors';
import {
  getExistingThreadTerminalState,
  getTerminalFocused,
  getThreadTerminalStateForTerminalEvent,
} from '../components/terminal/terminalStore.svelte';
import { DeleteThread } from './bindings';
import type { ThreadPane } from './thread.svelte';

// Deliberately NOT ThreadPaneIngest (see threadPaneRoles.ts): the two
// members this module touches are a focus request, not event ingest.
// The wrapper narrows at the acquisition point, so any new pane member
// use here fails to compile until this Pick names it.
type TerminalFocusPane = Pick<ThreadPane, 'paneId' | 'requestTerminalFocus'>;

function terminalFocusPanes(threadID: string): TerminalFocusPane[] {
  return panesShowingThread(threadID);
}

/**
 * A PTY this backend just started, opened from any client.
 *
 * The surface reads the set once at mount (`ListTerminals`) and nothing told
 * it about a terminal opened afterwards, so a second device dropped that
 * terminal's output as belonging to an id it had never seen — and, if its own
 * list had come back empty, auto-opened a second shell beside it.
 *
 * Only threads that ALREADY hold terminal state converge: a client with no
 * surface for the thread has nothing to show and would otherwise retain a tab
 * list nobody is looking at, and its next mount reads the list anyway.
 * `addTab` is the same call the opening client makes with the same summary, so
 * the initiator's echo is idempotent. It does NOT take the active tab: which
 * tab a person is typing into is this client's own state, and a shell opened
 * from a phone must not pull the desktop off the one it is using. A surface
 * with no active tab adopts it, since there is nothing to pull away from.
 */
export function applyTerminalOpened(payload: TerminalHandle): void {
  const threadID = payload?.threadID;
  const summary = payload?.summary;
  if (!threadID || !summary?.terminalID) return;
  getExistingThreadTerminalState(threadID)?.addTab(summary, { activate: false });
}

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
    ? terminalFocusPanes(payload.threadID).filter((pane) => getTerminalFocused(pane.paneId))
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
