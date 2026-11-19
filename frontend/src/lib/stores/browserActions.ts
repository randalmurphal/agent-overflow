import { BrowserCompanionAction, BrowserCompanionDo, type BrowserCompanionEvent } from './bindings';
import {
  admitBrowserState,
  browserCompanionState,
  browserPaneMountId,
  browserPaneOpen,
  browserPaneShown,
  browserSourcePaneShown,
  browserStateGeneration,
  browserThreadSnapshot,
  canPresentNativeBrowser,
  canUseNativeBrowser,
  nativeBrowserRefusal,
  noteBrowserActionError,
  observeBrowserCompanion,
  presentWantedBrowser,
  readBrowserCompanionState,
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
  | { readonly kind: 'show'; readonly pageId: PageId | null }
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
  show: (pageId: PageId | null): BrowserAction => ({ kind: 'show', pageId }),
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
function wireAction(action: BrowserAction, order: TakenPlace['place'] | null): BrowserCompanionAction {
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

// Each live mount's numbering (internal/browser/turnorder). The backend
// admits a mount's numbers in order within an epoch and refuses what
// arrives of an older epoch, so a mount starts its next epoch only with
// the first action it sends while none of its own is unanswered: then
// nothing of the older epoch is still meant to act, and a gap the backend
// recorded there (a number lost or overtaken) no longer holds its keyboard
// commands. A new mount (a re-attach) starts again.
interface MountNumbering {
  epoch: number;
  seq: number;
  unanswered: number;
}

const mountNumberings = new Map<BrowserMountId, MountNumbering>();

/** An ordered action's place in its mount's order, and the end of its wait for an answer. */
interface TakenPlace {
  readonly place: { readonly mount: BrowserMountId; readonly epoch: number; readonly select: number };
  /** The action answered or failed; safe to call more than once. */
  readonly answered: () => void;
}

function takePlace(mount: BrowserMountId): TakenPlace {
  let numbering = mountNumberings.get(mount);
  if (!numbering) {
    numbering = { epoch: 0, seq: 0, unanswered: 0 };
    mountNumberings.set(mount, numbering);
  }
  if (numbering.unanswered === 0) {
    numbering.epoch += 1;
    numbering.seq = 0;
  }
  numbering.seq += 1;
  numbering.unanswered += 1;
  const taken = numbering;
  let waiting = true;
  return {
    place: { mount, epoch: taken.epoch, select: taken.seq },
    answered: () => {
      if (!waiting) return;
      waiting = false;
      taken.unanswered -= 1;
    },
  };
}

/**
 * The place of an ordered action in its mount's order, taken when the user
 * acted: actions run concurrently on the backend, which admits a mount's
 * ordered actions in this order. A command on the active tab carries its
 * own mount; a selection made with no pane mounted carries none and applies
 * in arrival order.
 */
function orderOf(threadId: string, action: BrowserAction): TakenPlace | null {
  if (action.kind === 'active') return takePlace(action.mount);
  const mount = browserPaneMountId(threadId);
  return mount && ordered(action) ? takePlace(mount) : null;
}

// question says whether a refusal is a question for the user, which the
// caller asks instead of the banner showing it.
function runBrowserAction(
  threadId: string,
  action: BrowserAction,
  question: (err: unknown) => boolean = () => false,
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  const taken = orderOf(threadId, action);
  return sendBrowserAction(threadId, wireAction(action, taken?.place ?? null), question).finally(() => taken?.answered());
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

// The header's shows in flight, one per thread: a press while the button
// still shows the browser closed joins the show under way. It stands in for
// a Manager operation that shows the current tab, or reserves one blank tab
// when there is none, in one admission (see showBrowserFromHeader).
const headerShows = new Map<string, Promise<void>>();

/**
 * Carries out a header press. A refusal, or a read that fails, reaches a
 * toast, since a hidden pane has no banner.
 */
export function runBrowserHeaderIntent(intent: BrowserHeaderIntent): Promise<void> {
  switch (intent.kind) {
    case 'hide':
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
 * Shows the thread's browser. A state this connection does not know is
 * read first, so tabs opened while this window was away are shown rather
 * than given another; admitting the read may show the pane already. After
 * the read the pane that asked must still show the thread. Existing tabs
 * are shown; only a state known to have none gets a new tab, its address
 * focused. The owner is checked again as each action is sent.
 *
 * The show and the new tab are two calls, so an agent's tab opened between
 * them still gets a blank tab beside it: closing that needs one Manager
 * operation that shows the active tab, or reserves one blank tab when there
 * is none, in one admission.
 */
async function showBrowserFromHeader(threadId: string): Promise<void> {
  try {
    await readBrowserCompanionState(threadId);
  } catch (err) {
    addErrorToast(`The browser's tabs could not be read: ${errString(err)}`, err);
    return;
  }
  if (!browserSourcePaneShown(threadId)) return;
  const snapshot = browserThreadSnapshot(threadId);
  switch (snapshot.status) {
    case 'unknown': {
      // Only a page that cannot host the browser leaves it unread.
      const refusal = nativeBrowserRefusal(threadId) ?? new Error("The browser's tabs are not known yet.");
      addErrorToast(refusal.message, refusal);
      return;
    }
    case 'known': {
      if ((snapshot.snapshot.pages?.length ?? 0) === 0) {
        // A refusal reaches a toast there (openBrowserTab).
        await openBrowserTab(threadId, { focusAddress: true });
        return;
      }
      if (snapshot.snapshot.visible === true) {
        // The session is shown already; only this layout lacks the pane.
        presentWantedBrowser(threadId);
        return;
      }
      const active = snapshot.snapshot.activePageId;
      const failure = await browserCompanionAct(threadId, browserAction.show(active ? toPageId(active) : null));
      if (failure) addErrorToast(failure.error, failure.cause);
      return;
    }
    default:
      unhandledSnapshot(snapshot);
  }
}

function unhandledSnapshot(snapshot: never): never {
  throw new Error(`Unhandled browser snapshot: ${JSON.stringify(snapshot)}`);
}

observeBrowserCompanion({
  // A new mount (a re-attach) numbers its selections from 1 again.
  mountEnded: (mount) => {
    mountNumberings.delete(mount);
  },
  resetForTest: () => {
    browserActionCounts.clear();
    mountNumberings.clear();
    headerShows.clear();
  },
});
