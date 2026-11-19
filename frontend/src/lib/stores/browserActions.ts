import { BrowserCompanionAction, BrowserCompanionDo, type BrowserCompanionEvent } from './bindings';
import {
  applyBrowserCompanionState,
  browserCompanionState,
  browserPaneMountId,
  browserPaneOpen,
  browserPaneShown,
  nativeBrowserRefusal,
  noteBrowserActionError,
  observeBrowserCompanion,
  readBrowserCompanionState,
} from './browserCompanion.svelte';
import { armAddressFocus, settleAddressFocus } from './browserAddressFocus.svelte';
import { getCompanionPane } from './companionPanes.svelte';
import { getFocusedPaneId, getPane } from './panes.svelte';
import { isPaneHiddenOnCompact } from './paneLayout.svelte';
import { errString } from '../utils/errors';
import { addErrorToast } from './toast.svelte';

// The user's actions on a thread's browser: the pane's chrome, the chat
// header's button, the keyboard's tab close and the page focus requests. Each
// answer is a state and lands through applyBrowserCompanionState; a refusal
// lands in the pane's banner (noteBrowserActionError). The per-mount
// selection numbers and the in-flight selections are kept here and follow the
// pane mount through observeBrowserCompanion.

// Chrome actions started per thread, so a focus refusal that arrives after a
// newer action knows it is older than that action's banner.
const browserActionCounts = new Map<string, number>();

function noteBrowserActionStarted(threadId: string): void {
  browserActionCounts.set(threadId, (browserActionCounts.get(threadId) ?? 0) + 1);
}

/** How many chrome actions have started for the thread. */
export function browserActionsStarted(threadId: string): number {
  return browserActionCounts.get(threadId) ?? 0;
}

/** A refused browser action: its banner text and the error behind it. */
export interface BrowserActionFailure {
  error: string;
  cause: unknown;
}

/**
 * One chrome action against the thread's pages. The backend answers with the
 * next state, which lands like any push; a refusal lands in `actionError` (the
 * mounted pane's banner) and is also returned, for a caller whose pane may
 * not be mounted (the chat header's show/hide chip). Null on success. The
 * pane's buttons, that chip, and the `pane.close` chord all go through here.
 */
export async function browserCompanionAct(
  threadId: string,
  action: { kind: string; pageId?: string; address?: string; index?: number; downloadId?: string },
): Promise<BrowserActionFailure | null> {
  return refuseUnhosted(threadId) ?? (await runBrowserAction(threadId, action)).failure;
}

/**
 * The refusal of an action on a browser this page cannot host now
 * (nativeBrowserRefusal), in the banner and returned; null when it can.
 * Asked as the action is sent, since one sent after a wait (a state read,
 * an earlier selection) may find the thread's owner moved or contested.
 */
function refuseUnhosted(threadId: string): BrowserActionFailure | null {
  const refusal = nativeBrowserRefusal(threadId);
  if (!refusal) return null;
  noteBrowserActionError(threadId, refusal.message);
  return { error: refusal.message, cause: refusal };
}

/** The cookie jars a thread's browser can use (internal/browser/jar.go). */
export type BrowserJar = 'worktree' | 'project' | 'thread' | 'private';

/**
 * Sets the cookie jar the thread's browser uses. The backend closes the
 * live tabs and reopens their addresses in the new jar; the caller asks
 * the user first when any are open. A refusal lands in the pane's banner.
 */
export async function setBrowserJar(threadId: string, jar: BrowserJar): Promise<BrowserActionFailure | null> {
  return (await runBrowserAction(threadId, { kind: 'jar', jar })).failure;
}

/**
 * Sends a page focus intent's arm ('focus-page') or withdrawal
 * ('focus-page-withdraw') for browserPageFocus.ts, answering the state
 * after it. Unlike a chrome action it neither counts as one nor touches the
 * banner.
 */
export function sendBrowserPageFocus(
  threadId: string,
  kind: 'focus-page' | 'focus-page-withdraw',
  pageId: string,
  mount: string,
  intent: number,
): Promise<BrowserCompanionEvent> {
  return BrowserCompanionDo(threadId, new BrowserCompanionAction({ kind, pageId, address: '', index: 0, mount, intent }));
}

// The action kinds that select a tab: the user's selections, which the
// backend admits per pane mount in the user's order
// (internal/browser/selection.go).
const SELECTING_KINDS = new Set(['activate', 'step', 'new', 'reopen', 'duplicate', 'download-open']);

// The last selection number given in each live mount. The backend counts
// a mount from 1 and waits for every number in turn, so the numbers are
// consecutive per mount and a new mount (a re-attach) starts again at 1.
const selectionSeqs = new Map<string, number>();

/**
 * The mount and number of a selecting action, taken when the user acted:
 * actions run concurrently on the backend, which admits a mount's
 * selections in this order. With no mount the selection carries neither
 * and applies in arrival order.
 */
function selectionOrder(threadId: string, kind: string): { mount: string; select: number } | null {
  const mount = browserPaneMountId(threadId);
  if (!mount || !SELECTING_KINDS.has(kind)) return null;
  const select = (selectionSeqs.get(mount) ?? 0) + 1;
  selectionSeqs.set(mount, select);
  return { mount, select };
}

function runBrowserAction(
  threadId: string,
  action: { kind: string; pageId?: string; address?: string; index?: number; jar?: BrowserJar; downloadId?: string },
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  const order = selectionOrder(threadId, action.kind);
  if (SELECTING_KINDS.has(action.kind)) {
    // A keyboard close made meanwhile waits for this selection's answer.
    return trackOrdered(threadId, sendBrowserAction(threadId, action, order));
  }
  return sendBrowserAction(threadId, action, order);
}

async function sendBrowserAction(
  threadId: string,
  action: { kind: string; pageId?: string; address?: string; index?: number; jar?: BrowserJar; downloadId?: string },
  order: { mount: string; select: number } | null,
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  noteBrowserActionStarted(threadId);
  noteBrowserActionError(threadId, '');
  try {
    const state = await BrowserCompanionDo(
      threadId,
      new BrowserCompanionAction({
        kind: action.kind,
        pageId: action.pageId ?? '',
        address: action.address ?? '',
        index: action.index ?? 0,
        ...(action.jar ? { jar: action.jar } : {}),
        ...(action.downloadId ? { downloadId: action.downloadId } : {}),
        ...order,
      }),
    );
    applyBrowserCompanionState(state);
    return { failure: null, state };
  } catch (err) {
    const message = errString(err);
    noteBrowserActionError(threadId, message);
    return { failure: { error: message, cause: err }, state: null };
  }
}

/**
 * Opens a new tab in the thread's browser for the user, showing the pane
 * even when the thread had no tab: the backend selects the tab and shows the
 * companion before the engine page exists, so the creating tab arrives with
 * the pane. `address`, when set, is loaded in it (a mod-clicked link).
 * `focusAddress` is the new-tab button's, shortcut's and header button's: the
 * address bar takes focus, its text selected, once the tab is ready, unless
 * the user did anything else first (addressFocusIntent).
 */
export async function openBrowserTab(
  threadId: string,
  options: { address?: string; focusAddress?: boolean } = {},
): Promise<BrowserActionFailure | null> {
  const request = options.focusAddress ? armAddressFocus(threadId) : 0;
  const refused = refuseUnhosted(threadId);
  const { failure, state } = refused
    ? { failure: refused, state: null }
    : await runBrowserAction(threadId, { kind: 'new', address: options.address });
  // A refusal with no pane to carry its banner (the thread had no tab, so
  // none opened) goes to a toast.
  if (failure && !browserPaneOpen(threadId)) addErrorToast(failure.error, failure.cause);
  // The answer names the tab; none when it failed or was closed, and the
  // failed tab shows why.
  if (request) settleAddressFocus(threadId, request, failure ? '' : (state?.pageId ?? ''));
  return failure;
}

// The user's tab selections in flight per thread, with the keyboard closes
// queued behind them (closeFocusedBrowserTab).
const orderedActions = new Map<string, Set<Promise<unknown>>>();

function trackOrdered<T>(threadId: string, action: Promise<T>): Promise<T> {
  let set = orderedActions.get(threadId);
  if (!set) {
    set = new Set();
    orderedActions.set(threadId, set);
  }
  set.add(action);
  const settle = (): void => {
    const current = orderedActions.get(threadId);
    current?.delete(action);
    if (current?.size === 0) orderedActions.delete(threadId);
  };
  action.then(settle, settle);
  return action;
}

function activeTabOf(state: BrowserCompanionEvent | null): string {
  return state?.activePageId || state?.pages?.[0]?.id || '';
}

/**
 * `pane.close` on a focused browser companion closes its ACTIVE TAB; the pane
 * goes only with its last tab (the zero-page state push closes the companion
 * in applyBrowserCompanionState). Routed from the command, not from a keydown
 * handler on the pane: on WebKit a click on the tab strip moves no DOM focus,
 * so a pane-scoped handler missed the chord and the window-level `pane.close`
 * destroyed the whole pane instead. Returns false when nothing browser-shaped
 * holds focus, and the command falls through to its pane close.
 *
 * The active tab is the one the user's earlier actions leave active: with a
 * selection (or an earlier keyboard close) still on its way, the close waits
 * for their answers and then closes the tab active by then, so selecting B
 * and pressing Mod+W at once closes B, not the A the last state named. A
 * close from the tab menu names its own tab and does not wait.
 */
export function closeFocusedBrowserTab(): boolean {
  const focused = focusedBrowser();
  if (!focused) return false;
  const threadId = focused.threadId;
  const before = [...(orderedActions.get(threadId) ?? [])];
  if (before.length === 0) {
    const pageId = activeTabOf(focused.state);
    if (!pageId) return false;
    void trackOrdered(threadId, browserCompanionAct(threadId, { kind: 'close', pageId }));
    return true;
  }
  void trackOrdered(threadId, (async () => {
    await Promise.allSettled(before);
    const pageId = activeTabOf(browserCompanionState(threadId));
    if (pageId) await browserCompanionAct(threadId, { kind: 'close', pageId });
  })());
  return true;
}

/**
 * The thread of the focused pane when it is a browser companion, with its
 * last state (null before any). Null when another pane, or none, is focused,
 * and when compact hides the focused pane, so a browser the screen does not
 * show never takes the keyboard's commands. Reactive.
 */
export function focusedBrowser(): { threadId: string; state: BrowserCompanionEvent | null } | null {
  const focusedId = getFocusedPaneId();
  if (!focusedId || isPaneHiddenOnCompact(focusedId)) return null;
  const companion = getCompanionPane(focusedId);
  if (companion?.kind !== 'browser') return null;
  const threadId = getPane(companion.sourcePaneId)?.threadId;
  if (!threadId) return null;
  return { threadId, state: browserCompanionState(threadId) };
}

// The header's open operations in flight, one per thread: a press while the
// button still shows the browser closed joins the open under way.
const headerOpens = new Map<string, Promise<void>>();

/**
 * The chat header's browser button. What a press does is what the button
 * showed when pressed: pressed (the thread's pane is in this layout,
 * browserPaneShown) hides the pane; otherwise it opens the browser, and a
 * press while that open is under way joins it rather than sending another.
 * A refusal, or a read that fails, reaches a toast, since a hidden pane has
 * no banner.
 */
export function toggleBrowserFromHeader(threadId: string): Promise<void> {
  if (browserPaneShown(threadId)) return hideBrowserFromHeader(threadId);
  const pending = headerOpens.get(threadId);
  if (pending) return pending;
  const open = openBrowserFromHeader(threadId).finally(() => {
    if (headerOpens.get(threadId) === open) headerOpens.delete(threadId);
  });
  headerOpens.set(threadId, open);
  return open;
}

async function hideBrowserFromHeader(threadId: string): Promise<void> {
  const failure = await browserCompanionAct(threadId, { kind: 'hide' });
  if (failure) addErrorToast(failure.error, failure.cause);
}

/**
 * Shows the thread's browser. A state this connection has not seen is read
 * first, so tabs opened while this window was away are shown rather than
 * given another; applying the read may show the pane already. Existing tabs
 * are shown; only a state known to have none gets a new tab, its address
 * focused.
 */
async function openBrowserFromHeader(threadId: string): Promise<void> {
  try {
    await readBrowserCompanionState(threadId);
  } catch (err) {
    addErrorToast(`The browser's tabs could not be read: ${errString(err)}`, err);
    return;
  }
  if (browserPaneShown(threadId)) return;
  const state = browserCompanionState(threadId);
  if (!state) {
    // A refusal reaches a toast there (openBrowserTab).
    await openBrowserTab(threadId, { focusAddress: true });
    return;
  }
  const failure = await browserCompanionAct(threadId, { kind: 'show', pageId: state.activePageId ?? '' });
  if (failure) addErrorToast(failure.error, failure.cause);
}

observeBrowserCompanion({
  // A new mount (a re-attach) numbers its selections from 1 again.
  mountEnded: (mount) => {
    selectionSeqs.delete(mount);
  },
  resetForTest: () => {
    browserActionCounts.clear();
    selectionSeqs.clear();
    orderedActions.clear();
    headerOpens.clear();
  },
});
