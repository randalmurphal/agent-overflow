import { BrowserCompanionAction, BrowserCompanionDo, type BrowserCompanionEvent } from './bindings';
import {
  admitBrowserState,
  browserCompanionState,
  browserPaneMountId,
  browserPaneOpen,
  browserPaneShown,
  browserSourcePaneShown,
  browserStateGeneration,
  canPresentNativeBrowser,
  canUseNativeBrowser,
  nativeBrowserRefusal,
  noteBrowserActionError,
  observeBrowserCompanion,
  type BrowserMountId,
} from './browserCompanion.svelte';
import { armAddressFocus, settleAddressFocus } from './browserAddressFocus.svelte';
import { getCompanionPane } from './companionPanes.svelte';
import { getPane, paneCommandTarget } from './panes.svelte';
import { TransportError } from '../transport/wsClient';
import { errString } from '../utils/errors';
import { addErrorToast } from './toast.svelte';

// The user's actions on a thread's browser: the pane's chrome, the chat
// header's button, the keyboard's commands on the active tab and the page
// focus requests. Each is a BrowserAction; its answer is a state and lands
// through admitBrowserState, and a refusal lands in the pane's banner
// (noteBrowserActionError). The per-mount numbers of the ordered actions
// are kept here and follow the pane mount through observeBrowserCompanion.

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
 * One action against the thread's pages. The backend answers with the next
 * state, which lands like any push; a refusal lands in `actionError` (the
 * mounted pane's banner) and is also returned, for a caller whose pane may
 * not be mounted (the chat header's show/hide chip). Null on success.
 */
export async function browserCompanionAct(threadId: string, action: BrowserAction): Promise<BrowserActionFailure | null> {
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

/** A tab's page id, minted where it enters from a binding or a component. */
export type PageId = string & { readonly __brand: 'PageId' };
/** One of the thread's downloads (ThreadDownload.id). */
export type DownloadId = string & { readonly __brand: 'DownloadId' };

export const toPageId = (raw: string): PageId => raw as PageId;
export const toDownloadId = (raw: string): DownloadId => raw as DownloadId;

/** What a keyboard command does to the tab active at its turn. */
export type ActiveTabOperation = 'close' | 'reload';

/** The commands that act on the tab they name and select nothing. */
export type NamedTabKind = 'back' | 'forward' | 'reload' | 'stop' | 'restore' | 'close' | 'close-others' | 'close-right' | 'devtools';

/** The download rows' commands. */
export type DownloadKind = 'download-cancel' | 'download-reveal' | 'download-open';

/**
 * One user action on a thread's browser. A command on the active tab names
 * no tab and carries its pane mount, so the backend resolves the tab in the
 * user's order (internal/browser/active_commands.go); every other command
 * that acts on a tab names it. Built by the constructors in `browserAction`.
 */
export type BrowserAction =
  | { readonly kind: 'active'; readonly operation: ActiveTabOperation; readonly mount: BrowserMountId }
  | { readonly kind: NamedTabKind; readonly pageId: PageId }
  | { readonly kind: 'navigate'; readonly pageId: PageId; readonly address: string }
  | { readonly kind: 'move'; readonly pageId: PageId; readonly index: number }
  | { readonly kind: 'activate' | 'duplicate'; readonly pageId: PageId }
  | { readonly kind: 'step'; readonly step: 1 | -1 }
  | { readonly kind: 'new'; readonly address: string }
  | { readonly kind: 'reopen' }
  | { readonly kind: 'show-browser' }
  | { readonly kind: 'hide' }
  | { readonly kind: 'jar'; readonly jar: BrowserJar; readonly confirmed: boolean }
  | { readonly kind: DownloadKind; readonly downloadId: DownloadId }
  | { readonly kind: 'downloads-clear' };

/** One constructor per kind of BrowserAction. */
export const browserAction = {
  onActiveTab: (operation: ActiveTabOperation, mount: BrowserMountId): BrowserAction => ({ kind: 'active', operation, mount }),
  onTab: (kind: NamedTabKind, pageId: PageId): BrowserAction => ({ kind, pageId }),
  navigate: (pageId: PageId, address: string): BrowserAction => ({ kind: 'navigate', pageId, address }),
  move: (pageId: PageId, index: number): BrowserAction => ({ kind: 'move', pageId, index }),
  select: (pageId: PageId): BrowserAction => ({ kind: 'activate', pageId }),
  duplicate: (pageId: PageId): BrowserAction => ({ kind: 'duplicate', pageId }),
  step: (step: 1 | -1): BrowserAction => ({ kind: 'step', step }),
  newTab: (address = ''): BrowserAction => ({ kind: 'new', address }),
  reopen: (): BrowserAction => ({ kind: 'reopen' }),
  showBrowser: (): BrowserAction => ({ kind: 'show-browser' }),
  hide: (): BrowserAction => ({ kind: 'hide' }),
  jar: (jar: BrowserJar, confirmed: boolean): BrowserAction => ({ kind: 'jar', jar, confirmed }),
  download: (kind: DownloadKind, downloadId: DownloadId): BrowserAction => ({ kind, downloadId }),
  clearDownloads: (): BrowserAction => ({ kind: 'downloads-clear' }),
};

/** Whether action takes a number in its pane mount's order. */
function ordered(action: BrowserAction): boolean {
  switch (action.kind) {
    case 'active':
    case 'activate':
    case 'step':
    case 'new':
    case 'reopen':
    case 'duplicate':
    case 'download-open':
    case 'show-browser':
      return true;
    case 'download-cancel':
    case 'download-reveal':
    case 'back':
    case 'forward':
    case 'reload':
    case 'stop':
    case 'restore':
    case 'close':
    case 'close-others':
    case 'close-right':
    case 'devtools':
    case 'navigate':
    case 'move':
    case 'hide':
    case 'jar':
    case 'downloads-clear':
      return false;
    default:
      return unhandled(action);
  }
}

function unhandled(action: never): never {
  throw new Error(`browser action ${JSON.stringify(action)} has no wire form`);
}

/** The wire form of action, with its place in the mount's order. */
function wireAction(action: BrowserAction, order: { mount: BrowserMountId; select: number } | null): BrowserCompanionAction {
  const base = { kind: '', pageId: '', address: '', index: 0, ...order };
  switch (action.kind) {
    case 'active':
      return new BrowserCompanionAction({ ...base, kind: action.operation === 'close' ? 'close-active' : 'reload-active' });
    case 'back':
    case 'forward':
    case 'reload':
    case 'stop':
    case 'restore':
    case 'close':
    case 'close-others':
    case 'close-right':
    case 'devtools':
    case 'activate':
    case 'duplicate':
      return new BrowserCompanionAction({ ...base, kind: action.kind, pageId: action.pageId });
    case 'navigate':
      return new BrowserCompanionAction({ ...base, kind: 'navigate', pageId: action.pageId, address: action.address });
    case 'move':
      return new BrowserCompanionAction({ ...base, kind: 'move', pageId: action.pageId, index: action.index });
    case 'step':
      return new BrowserCompanionAction({ ...base, kind: 'step', index: action.step });
    case 'new':
      return new BrowserCompanionAction({ ...base, kind: 'new', address: action.address });
    case 'reopen':
    case 'hide':
    case 'downloads-clear':
    case 'show-browser':
      return new BrowserCompanionAction({ ...base, kind: action.kind });
    case 'jar':
      return new BrowserCompanionAction({ ...base, kind: 'jar', jar: action.jar, confirmed: action.confirmed });
    case 'download-cancel':
    case 'download-reveal':
    case 'download-open':
      return new BrowserCompanionAction({ ...base, kind: action.kind, downloadId: action.downloadId });
    default:
      return unhandled(action);
  }
}

/** What a cookie jar switch answered. */
export type JarSwitchAnswer =
  | { readonly kind: 'switched' }
  | { readonly kind: 'needs-confirmation' }
  | { readonly kind: 'failed'; readonly failure: BrowserActionFailure };

// The backend's refusal of an unconfirmed switch while a tab is live
// (internal/app browserJarUnconfirmedCode): a question for the user, not a
// failure for the banner.
function jarUnconfirmed(err: unknown): boolean {
  return err instanceof TransportError && err.code === 'browser_jar_unconfirmed';
}

/**
 * Sets the cookie jar the thread's browser uses. The backend closes the
 * live tabs and reopens their addresses in the new jar. Unless the user
 * confirmed losing the live tabs' page state, the backend refuses while a tab
 * is live and the answer is 'needs-confirmation': the caller asks and sends
 * again confirmed. Any other refusal lands in the pane's banner.
 */
export async function setBrowserJar(threadId: string, jar: BrowserJar, confirmed: boolean): Promise<JarSwitchAnswer> {
  const { failure } = await runBrowserAction(threadId, browserAction.jar(jar, confirmed), jarUnconfirmed);
  if (!failure) return { kind: 'switched' };
  return jarUnconfirmed(failure.cause) ? { kind: 'needs-confirmation' } : { kind: 'failed', failure };
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

// The last number given in each live mount. The backend counts a mount
// from 1 and waits for every number in turn, so the numbers are consecutive
// per mount and a new mount (a re-attach) starts again at 1.
const selectionSeqs = new Map<BrowserMountId, number>();

function nextNumber(mount: BrowserMountId): { mount: BrowserMountId; select: number } {
  const select = (selectionSeqs.get(mount) ?? 0) + 1;
  selectionSeqs.set(mount, select);
  return { mount, select };
}

/**
 * The place of an ordered action in its mount's order, taken when the user
 * acted: actions run concurrently on the backend, which admits a mount's
 * ordered actions in this order. A command on the active tab carries its
 * own mount; a selection made with no pane mounted carries none and applies
 * in arrival order.
 */
function orderOf(threadId: string, action: BrowserAction): { mount: BrowserMountId; select: number } | null {
  if (action.kind === 'active') return nextNumber(action.mount);
  const mount = browserPaneMountId(threadId);
  return mount && ordered(action) ? nextNumber(mount) : null;
}

// question says whether a refusal is a question for the user, which the
// caller asks instead of the banner showing it.
function runBrowserAction(
  threadId: string,
  action: BrowserAction,
  question: (err: unknown) => boolean = () => false,
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  return sendBrowserAction(threadId, wireAction(action, orderOf(threadId, action)), question);
}

async function sendBrowserAction(
  threadId: string,
  wire: BrowserCompanionAction,
  question: (err: unknown) => boolean,
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  noteBrowserActionStarted(threadId);
  noteBrowserActionError(threadId, '');
  const generation = browserStateGeneration();
  try {
    const state = await BrowserCompanionDo(threadId, wire);
    admitBrowserState(state, generation);
    return { failure: null, state };
  } catch (err) {
    const message = errString(err);
    if (!question(err)) noteBrowserActionError(threadId, message);
    return { failure: { error: message, cause: err }, state: null };
  }
}

/**
 * Opens a new tab in the thread's browser for the user, showing the pane
 * even when the thread had no tab: the backend selects the tab and shows the
 * companion before the engine page exists, so the creating tab arrives with
 * the pane. `address`, when set, is loaded in it (a mod-clicked link).
 * `focusAddress` is the new-tab button's and shortcut's: the
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
    : await runBrowserAction(threadId, browserAction.newTab(options.address));
  // A refusal with no pane to carry its banner (the thread had no tab, so
  // none opened) goes to a toast.
  if (failure && !browserPaneOpen(threadId)) addErrorToast(failure.error, failure.cause);
  // The answer names the tab; none when it failed or was closed, and the
  // failed tab shows why.
  if (request) settleAddressFocus(threadId, request, failure ? '' : (state?.pageId ?? ''));
  return failure;
}

/**
 * A keyboard command on the keyboard's browser pane's active tab. It names
 * no tab: it takes its number in the pane mount's order at the keypress, and
 * the backend acts on the tab active at its turn, after the user's earlier
 * selections and commands and never after their page work. A pane not yet
 * attached has no mount and shows no page, and the command does nothing.
 * The tab menu's and the address bar's commands name their tab.
 */
export function actOnActiveTab(threadId: string, operation: ActiveTabOperation): void {
  const mount = browserPaneMountId(threadId);
  if (mount) void browserCompanionAct(threadId, browserAction.onActiveTab(operation, mount));
}

/**
 * `pane.close` on the keyboard's browser companion closes its ACTIVE TAB;
 * the pane goes only with its last tab (the zero-page state push closes the
 * companion in applyBrowserCompanionState). Routed from the command, not
 * from a keydown handler on the pane: on WebKit a click on the tab strip
 * moves no DOM focus, so a pane-scoped handler missed the chord and the
 * window-level `pane.close` destroyed the whole pane instead. Returns false
 * when the keyboard's pane is no browser, or it shows no tab, and the
 * command falls through to its pane close.
 */
export function closeFocusedBrowserTab(): boolean {
  const focused = focusedBrowser();
  if (!focused || (focused.state?.pages?.length ?? 0) === 0) return false;
  actOnActiveTab(focused.threadId, 'close');
  return true;
}

/**
 * The thread of the keyboard's target pane (paneCommandTarget) when it is
 * a browser companion, with its last state (null before any). Null when
 * the target is another pane or none, so a browser the screen does not
 * show never takes the keyboard's commands. Reactive.
 */
export function focusedBrowser(): { threadId: string; state: Readonly<BrowserCompanionEvent> | null } | null {
  const targetId = paneCommandTarget();
  if (!targetId) return null;
  const companion = getCompanionPane(targetId);
  if (companion?.kind !== 'browser') return null;
  const threadId = getPane(companion.sourcePaneId)?.threadId;
  if (!threadId) return null;
  return { threadId, state: browserCompanionState(threadId) };
}

/**
 * What a press of the chat header's browser button means, chosen from what
 * the button shows when pressed (browserHeaderButton): hide the pane this
 * layout shows, or show the thread's browser.
 */
export type BrowserHeaderIntent =
  | { readonly kind: 'hide'; readonly threadId: string }
  | { readonly kind: 'show'; readonly threadId: string };

/** The chat header's browser button, as it renders now. Reactive. */
export type BrowserHeaderButton =
  | { readonly rendered: false }
  | {
    readonly rendered: true;
    readonly label: 'Open browser' | 'Toggle browser pane';
    /** The pane is in this layout, not merely wanted by the session. */
    readonly pressed: boolean;
    readonly intent: BrowserHeaderIntent;
  };

/**
 * The one answer to how the chat header's browser button renders and what
 * a press does: it renders where this page can present the thread's browser
 * (canPresentNativeBrowser), or while the thread has tabs this page could
 * show; it is pressed while the thread's pane is in this layout, and a
 * press then hides it, else shows the browser.
 */
export function browserHeaderButton(threadId: string): BrowserHeaderButton {
  if (!threadId) return { rendered: false };
  const tabs = canUseNativeBrowser(threadId) && browserCompanionState(threadId) !== null;
  if (!tabs && !canPresentNativeBrowser(threadId)) return { rendered: false };
  const intent = browserHeaderIntent(threadId);
  return {
    rendered: true,
    label: tabs ? 'Toggle browser pane' : 'Open browser',
    pressed: intent.kind === 'hide',
    intent,
  };
}

/** What a header press means now: hide the pane this layout shows, else show the browser. */
export function browserHeaderIntent(threadId: string): BrowserHeaderIntent {
  return browserPaneShown(threadId) ? { kind: 'hide', threadId } : { kind: 'show', threadId };
}

// A header show still unanswered, per thread. A press while the button
// still offers show (the show's state has not landed) is the same press
// repeated and joins it, so a double click arms one address focus and
// raises one toast. A hide press of the thread ends the run: a show after
// it is sent again. The Manager converges two shows on its own
// (ShowCompanion); this is only the UI's.
const headerShows = new Map<string, Promise<void>>();

/**
 * Carries out a header press. A refusal reaches a toast, since a hidden
 * pane has no banner.
 */
export function runBrowserHeaderIntent(intent: BrowserHeaderIntent): Promise<void> {
  switch (intent.kind) {
    case 'hide':
      headerShows.delete(intent.threadId);
      return hideBrowserFromHeader(intent.threadId);
    case 'show': {
      const pending = headerShows.get(intent.threadId);
      if (pending) return pending;
      const show = showBrowserFromHeader(intent.threadId).finally(() => {
        if (headerShows.get(intent.threadId) === show) headerShows.delete(intent.threadId);
      });
      headerShows.set(intent.threadId, show);
      return show;
    }
    default:
      return unhandledIntent(intent);
  }
}

function unhandledIntent(intent: never): never {
  throw new Error(`Unhandled browser header intent: ${JSON.stringify(intent)}`);
}

async function hideBrowserFromHeader(threadId: string): Promise<void> {
  const failure = await browserCompanionAct(threadId, browserAction.hide());
  if (failure) addErrorToast(failure.error, failure.cause);
}

/**
 * Shows the thread's browser with one action that names no tab: at its
 * turn the Manager shows the tab the thread has selected, or opens one
 * blank tab when it has none, which the answer names and whose address bar
 * takes focus (ShowCompanion). The answer's state opens the pane.
 *
 * The show may open a tab, so it is sent only where this layout presents
 * the pane: beside a pane showing the thread, in a layout and page that
 * can present the browser (canPresentNativeBrowser, never compact). It is
 * asked in the press's own task, as the action is sent, so no layout
 * change falls between the check and the send; a tab the show opens
 * before a later change to compact is presented when the layout can be
 * again (browserPresentationWanted).
 */
async function showBrowserFromHeader(threadId: string): Promise<void> {
  const refused = refuseUnhosted(threadId);
  if (refused) {
    addErrorToast(refused.error, refused.cause);
    return;
  }
  if (!canPresentNativeBrowser(threadId) || !browserSourcePaneShown(threadId)) return;
  const request = armAddressFocus(threadId);
  const { failure, state } = await runBrowserAction(threadId, browserAction.showBrowser());
  settleAddressFocus(threadId, request, failure ? '' : (state?.pageId ?? ''));
  // An older backend refuses the kind ("unsupported companion action").
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
    headerShows.clear();
  },
});
