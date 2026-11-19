import { BrowserCompanionAction, BrowserCompanionDo, type BrowserCompanionEvent } from './bindings';
import {
  applyBrowserCompanionState,
  browserCompanionState,
  browserPaneMountId,
  browserPaneOpen,
  noteBrowserActionError,
  observeBrowserCompanion,
  readBrowserCompanionState,
} from './browserCompanion.svelte';
import { armAddressFocus, settleAddressFocus } from './browserAddressFocus.svelte';
import { getCompanionPane } from './companionPanes.svelte';
import { getFocusedPaneId, getPane } from './panes.svelte';
import { errString } from '../utils/errors';
import { addErrorToast } from './toast.svelte';

// The user's actions on a thread's browser: the pane's chrome, the chat
// header's button, the keyboard's commands on the active tab and the page
// focus requests. Each is a BrowserAction; its answer is a state and lands
// through applyBrowserCompanionState, and a refusal lands in the pane's
// banner (noteBrowserActionError). The per-mount numbers of the ordered
// actions are kept here and follow the pane mount through
// observeBrowserCompanion.

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
  return (await runBrowserAction(threadId, action)).failure;
}

/** The cookie jars a thread's browser can use (internal/browser/jar.go). */
export type BrowserJar = 'worktree' | 'project' | 'thread' | 'private';

/** A tab's page id, minted where it enters from a binding or a component. */
export type PageId = string & { readonly __brand: 'PageId' };
/** A mounted browser pane's id (BrowserCompanionPaneAttach). */
export type MountId = string & { readonly __brand: 'MountId' };
/** One of the thread's downloads (ThreadDownload.id). */
export type DownloadId = string & { readonly __brand: 'DownloadId' };

export const toPageId = (raw: string): PageId => raw as PageId;
export const toMountId = (raw: string): MountId => raw as MountId;
export const toDownloadId = (raw: string): DownloadId => raw as DownloadId;

/** What a keyboard command does to the tab active at its turn. */
export type ActiveTabOperation = 'close' | 'reload';

/** The commands that act on the tab they name and select nothing. */
type NamedTabKind = 'back' | 'forward' | 'reload' | 'stop' | 'restore' | 'close' | 'close-others' | 'close-right' | 'devtools';

/**
 * One user action on a thread's browser. A command on the active tab names
 * no tab and carries its pane mount, so the backend resolves the tab in the
 * user's order (internal/browser/active_commands.go); every other command
 * that acts on a tab names it. Built by the constructors in `browserAction`.
 */
export type BrowserAction =
  | { readonly kind: 'active'; readonly operation: ActiveTabOperation; readonly mount: MountId }
  | { readonly kind: NamedTabKind; readonly pageId: PageId }
  | { readonly kind: 'navigate'; readonly pageId: PageId; readonly address: string }
  | { readonly kind: 'move'; readonly pageId: PageId; readonly index: number }
  | { readonly kind: 'activate' | 'duplicate'; readonly pageId: PageId }
  | { readonly kind: 'step'; readonly step: 1 | -1 }
  | { readonly kind: 'new'; readonly address: string }
  | { readonly kind: 'reopen' }
  | { readonly kind: 'show'; readonly pageId: PageId | null }
  | { readonly kind: 'hide' }
  | { readonly kind: 'jar'; readonly jar: BrowserJar }
  | { readonly kind: 'download-cancel' | 'download-reveal' | 'download-open'; readonly downloadId: DownloadId }
  | { readonly kind: 'downloads-clear' };

/** One constructor per kind of BrowserAction. */
export const browserAction = {
  onActiveTab: (operation: ActiveTabOperation, mount: MountId): BrowserAction => ({ kind: 'active', operation, mount }),
  onTab: (kind: NamedTabKind, pageId: PageId): BrowserAction => ({ kind, pageId }),
  navigate: (pageId: PageId, address: string): BrowserAction => ({ kind: 'navigate', pageId, address }),
  move: (pageId: PageId, index: number): BrowserAction => ({ kind: 'move', pageId, index }),
  select: (pageId: PageId): BrowserAction => ({ kind: 'activate', pageId }),
  duplicate: (pageId: PageId): BrowserAction => ({ kind: 'duplicate', pageId }),
  step: (step: 1 | -1): BrowserAction => ({ kind: 'step', step }),
  newTab: (address = ''): BrowserAction => ({ kind: 'new', address }),
  reopen: (): BrowserAction => ({ kind: 'reopen' }),
  show: (pageId: PageId | null): BrowserAction => ({ kind: 'show', pageId }),
  hide: (): BrowserAction => ({ kind: 'hide' }),
  jar: (jar: BrowserJar): BrowserAction => ({ kind: 'jar', jar }),
  download: (kind: 'download-cancel' | 'download-reveal' | 'download-open', downloadId: DownloadId): BrowserAction => ({ kind, downloadId }),
  clearDownloads: (): BrowserAction => ({ kind: 'downloads-clear' }),
};

const NAMED_TAB_KINDS: ReadonlySet<string> = new Set<NamedTabKind>([
  'back', 'forward', 'reload', 'stop', 'restore', 'close', 'close-others', 'close-right', 'devtools',
]);

function isNamedTabKind(kind: string): kind is NamedTabKind {
  return NAMED_TAB_KINDS.has(kind);
}

/**
 * The pane chrome's and the download rows' action, which they name by kind
 * string (BrowserPane's `act`, browserDownloads' `act`), as a BrowserAction.
 * A command on the active tab has no chrome form: it is the keyboard's
 * (actOnActiveTab). Null for a kind the chrome does not send, or a tab
 * command with no tab.
 */
export function chromeAction(
  kind: string,
  fields: { pageId?: string; address?: string; index?: number; downloadId?: string },
): BrowserAction | null {
  const page = fields.pageId ? toPageId(fields.pageId) : null;
  if (isNamedTabKind(kind)) return page ? browserAction.onTab(kind, page) : null;
  switch (kind) {
    case 'navigate':
      return page ? browserAction.navigate(page, fields.address ?? '') : null;
    case 'move':
      return page ? browserAction.move(page, fields.index ?? 0) : null;
    case 'activate':
      return page ? browserAction.select(page) : null;
    case 'duplicate':
      return page ? browserAction.duplicate(page) : null;
    case 'reopen':
      return browserAction.reopen();
    case 'show':
      return browserAction.show(page);
    case 'hide':
      return browserAction.hide();
    case 'download-cancel':
    case 'download-reveal':
    case 'download-open':
      return fields.downloadId ? browserAction.download(kind, toDownloadId(fields.downloadId)) : null;
    case 'downloads-clear':
      return browserAction.clearDownloads();
    default:
      return null;
  }
}

/** Whether action takes a number in its pane mount's order. */
function ordered(action: BrowserAction): boolean {
  switch (action.kind) {
    case 'active':
    case 'activate':
    case 'step':
    case 'new':
    case 'reopen':
    case 'duplicate':
      return true;
    case 'download-open':
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
    case 'show':
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
function wireAction(action: BrowserAction, order: { mount: MountId; select: number } | null): BrowserCompanionAction {
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
      return new BrowserCompanionAction({ ...base, kind: action.kind });
    case 'show':
      return new BrowserCompanionAction({ ...base, kind: 'show', pageId: action.pageId ?? '' });
    case 'jar':
      return new BrowserCompanionAction({ ...base, kind: 'jar', jar: action.jar });
    case 'download-cancel':
    case 'download-reveal':
    case 'download-open':
      return new BrowserCompanionAction({ ...base, kind: action.kind, downloadId: action.downloadId });
    default:
      return unhandled(action);
  }
}

/**
 * Sets the cookie jar the thread's browser uses. The backend closes the
 * live tabs and reopens their addresses in the new jar; the caller asks
 * the user first when any are open. A refusal lands in the pane's banner.
 */
export async function setBrowserJar(threadId: string, jar: BrowserJar): Promise<BrowserActionFailure | null> {
  return (await runBrowserAction(threadId, browserAction.jar(jar))).failure;
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
const selectionSeqs = new Map<MountId, number>();

function nextNumber(mount: MountId): { mount: MountId; select: number } {
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
function orderOf(threadId: string, action: BrowserAction): { mount: MountId; select: number } | null {
  if (action.kind === 'active') return nextNumber(action.mount);
  const mount = browserPaneMountId(threadId);
  return mount && ordered(action) ? nextNumber(toMountId(mount)) : null;
}

function runBrowserAction(
  threadId: string,
  action: BrowserAction,
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  return sendBrowserAction(threadId, wireAction(action, orderOf(threadId, action)));
}

async function sendBrowserAction(
  threadId: string,
  wire: BrowserCompanionAction,
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  noteBrowserActionStarted(threadId);
  noteBrowserActionError(threadId, '');
  try {
    const state = await BrowserCompanionDo(threadId, wire);
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
  const { failure, state } = await runBrowserAction(threadId, browserAction.newTab(options.address));
  // A refusal with no pane to carry its banner (the thread had no tab, so
  // none opened) goes to a toast.
  if (failure && !browserPaneOpen(threadId)) addErrorToast(failure.error, failure.cause);
  // The answer names the tab; none when it failed or was closed, and the
  // failed tab shows why.
  if (request) settleAddressFocus(threadId, request, failure ? '' : (state?.pageId ?? ''));
  return failure;
}

/**
 * A keyboard command on the focused browser pane's active tab. It names no
 * tab: it takes its number in the pane mount's order at the keypress, and
 * the backend acts on the tab active at its turn, after the user's earlier
 * selections and commands and never after their page work. A pane not yet
 * attached has no mount and shows no page, and the command does nothing.
 * The tab menu's and the address bar's commands name their tab.
 */
export function actOnActiveTab(threadId: string, operation: ActiveTabOperation): void {
  const mount = browserPaneMountId(threadId);
  if (mount) void browserCompanionAct(threadId, browserAction.onActiveTab(operation, toMountId(mount)));
}

/**
 * `pane.close` on a focused browser companion closes its ACTIVE TAB; the pane
 * goes only with its last tab (the zero-page state push closes the companion
 * in applyBrowserCompanionState). Routed from the command, not from a keydown
 * handler on the pane: on WebKit a click on the tab strip moves no DOM focus,
 * so a pane-scoped handler missed the chord and the window-level `pane.close`
 * destroyed the whole pane instead. Returns false when nothing browser-shaped
 * holds focus, or the pane shows no tab, and the command falls through to
 * its pane close.
 */
export function closeFocusedBrowserTab(): boolean {
  const focused = focusedBrowser();
  if (!focused || (focused.state?.pages?.length ?? 0) === 0) return false;
  actOnActiveTab(focused.threadId, 'close');
  return true;
}

/**
 * The thread of the focused pane when it is a browser companion, with its
 * last state (null before any). Null when another pane, or none, is focused.
 */
export function focusedBrowser(): { threadId: string; state: BrowserCompanionEvent | null } | null {
  const focusedId = getFocusedPaneId();
  const companion = focusedId ? getCompanionPane(focusedId) : null;
  if (companion?.kind !== 'browser') return null;
  const threadId = getPane(companion.sourcePaneId)?.threadId;
  if (!threadId) return null;
  return { threadId, state: browserCompanionState(threadId) };
}

/**
 * The chat header's browser button. With tabs it shows or hides the pane;
 * with none it opens the pane on a new tab with its address focused. Which
 * it is comes from a state this connection has seen: an unknown one is read
 * first, so a thread whose tabs opened while this window was away is shown
 * rather than given another tab. A refusal, or a read that fails, reaches a
 * toast, since a hidden pane has no banner.
 */
export async function toggleBrowserFromHeader(threadId: string): Promise<void> {
  try {
    await readBrowserCompanionState(threadId);
  } catch (err) {
    addErrorToast(`The browser's tabs could not be read: ${errString(err)}`, err);
    return;
  }
  const state = browserCompanionState(threadId);
  if (!state) {
    // A refusal reaches a toast there (openBrowserTab).
    await openBrowserTab(threadId, { focusAddress: true });
    return;
  }
  const visible = state.visible === true;
  const failure = await browserCompanionAct(
    threadId,
    visible ? browserAction.hide() : browserAction.show(state.activePageId ? toPageId(state.activePageId) : null),
  );
  if (failure) addErrorToast(failure.error, failure.cause);
}

observeBrowserCompanion({
  // A new mount (a re-attach) numbers its selections from 1 again.
  mountEnded: (mount) => {
    selectionSeqs.delete(toMountId(mount));
  },
  resetForTest: () => {
    browserActionCounts.clear();
    selectionSeqs.clear();
  },
});
