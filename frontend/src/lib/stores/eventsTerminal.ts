// Backgrounded-terminal event domain: streaming terminal output into the
// per-pane terminal state, removing the tab of a terminal that ended (with
// active-pane refocus), and re-reading mounted surfaces' lists after a
// reconnect or a lost frame. A terminal thread whose last shell ends is
// deleted by its computer; its `thread:updated` deletion closes the panes,
// and a failed delete is reported on `terminal:end_failed`.
// Fan-in target of events.ts's setupEventListeners.
import type {
  TerminalEndFailedEventPayload,
  TerminalExitEventPayload,
  TerminalHandle,
  TerminalOutputEventPayload,
} from '../types/terminal';
import { decodeTerminalOutput } from '../types/terminal';
import { panesShowingThread } from './panes.svelte';
import { addToast } from './toast.svelte';
import {
  getExistingThreadTerminalState,
  getMountedTerminalSurfaces,
  getTerminalFocused,
  getThreadTerminalStateForTerminalEvent,
  type ThreadTerminalStateHandle,
} from '../components/terminal/terminalStore.svelte';
import type { ThreadPane } from './thread.svelte';
import type { BackendKey } from '../transport/backendKey';
import { threadMachine } from './attachedBackends.svelte';

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
 * The surface reads the set at mount (`ListTerminals`) and nothing told it
 * about a terminal opened afterwards, so a second device dropped that
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

/**
 * Re-read the terminal list of every mounted surface whose thread is on
 * `backend`, once it reconnects or loses terminal events. Its channels replay
 * independently, so a terminal that opened and exited while this client was
 * away can replay its exit before its open and leave a dead tab; an exit lost
 * to a gap leaves one too, and a lost open hides a running terminal.
 */
export function reconcileTerminalSurfaces(backend: BackendKey): void {
  for (const surface of getMountedTerminalSurfaces()) {
    if (threadMachine(surface.threadID, null) === backend) void surface.relist();
  }
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

/**
 * A terminal of `threadID` has ended: its exit arrived, or a re-read of the
 * thread's list no longer names it. Both land here, so its tab goes the same
 * way either way. Its thread is not this client's to end: a terminal thread
 * whose last shell ended is deleted by its computer, and a chat thread's
 * drawer collapses with its last tab (TerminalSurface).
 *
 * Removing the ACTIVE tab promotes a sibling, changing activeTerminalID and
 * remounting TerminalBody (keyed on that id), which blurs the dying xterm.
 * The panes whose user was focused IN this terminal are captured BEFORE the
 * removal so focus follows into the promoted sibling, as it does on the close
 * paths (the ✕ button latches pendingFocus directly; Ctrl+Shift+W uses the
 * same pane.requestTerminalFocus channel). The focus check is the
 * load-bearing guard: a backgrounded shell (`sleep 5; exit`) that ends while
 * the user types in the composer leaves getTerminalFocused() false for that
 * pane, so the cursor is NOT yanked away.
 */
export function endTerminalTab(
  handle: ThreadTerminalStateHandle,
  threadID: string,
  terminalID: string,
): void {
  const panesToRefocus = handle.activeTerminalID === terminalID
    ? terminalFocusPanes(threadID).filter((pane) => getTerminalFocused(pane.paneId))
    : [];
  handle.removeTab(terminalID);
  // With no sibling there is nothing to focus. Otherwise requestTerminalFocus
  // latches the pane's intent; the surface's consume effect lands it on the
  // remounted body within this same flush.
  if (handle.tabs.length === 0) return;
  for (const pane of panesToRefocus) pane.requestTerminalFocus();
}

/**
 * A terminal thread's last shell exited and its computer could not delete
 * it. No caller waits on that delete, so every client shows the failure. The
 * thread is as the computer left it: still listed and refused new shells
 * (deleting it retries), or already dropped by its `thread:updated` deletion.
 */
export function applyTerminalEndFailed(payload: TerminalEndFailedEventPayload): void {
  const message = typeof payload?.message === 'string' ? payload.message.trim() : '';
  if (message) addToast('error', message);
}

export function applyTerminalExit(payload: TerminalExitEventPayload): void {
  if (!payload?.threadID || !payload.terminalID) return;
  endTerminalTab(
    getThreadTerminalStateForTerminalEvent(payload.threadID, payload.terminalID),
    payload.threadID,
    payload.terminalID,
  );
}
