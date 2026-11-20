import { type BrowserCompanionEvent, type BrowserPageInfo } from './bindings';
import {
  browserCompanionState,
  browserPaneAttachment,
  browserPaneOpen,
  browserPaneShown,
  browserShownPage,
  browserSourcePaneShown,
  canPresentNativeBrowser,
  canUseNativeBrowser,
  isCurrentBrowserPane,
  noteBrowserPaneError,
  observeBrowserCompanion,
  type BrowserMountId,
  type BrowserPaneAttachment,
} from './browserCompanion.svelte';
import {
  browserAction,
  pageIdOf,
  sendForThread,
  sendFromPane,
  type BrowserAction,
  type BrowserActionAnswer,
  type BrowserJar,
  type PageId,
  type ThreadAction,
} from './browserActionWire';
import { browserPageChordPage } from './browserPageChords';
import { armAddressFocus, settleAddressFocus } from './browserAddressFocus.svelte';
import { getCompanionPane } from './companionPanes.svelte';
import { getPane, paneCommandTarget } from './panes.svelte';
import { TransportError } from '../transport/wsClient';
import { addErrorToast, getToasts } from './toast.svelte';

// What the user's gestures on a thread's browser mean: the pane's chrome,
// the chat header's button, the keyboard's commands on the shown tab. Each
// is a BrowserAction sent through browserActionWire.ts, which checks the
// thread's owner and the sending pane as it sends. A pane's action is sent
// from its attachment (actInPane), and its refusal lands in that pane's
// banner and no other; an action with no pane in hand (browserCompanionAct)
// carries the banner of the pane attached when it is sent, if any.

// Chrome actions started per attached mount, the banner they clear and
// write, so a focus refusal that arrives after a newer action knows it is
// older than that action's banner. An action with no pane to show it in
// counts nowhere, and a mount's count ends with the mount.
const browserActionCounts = new Map<BrowserMountId, number>();

/** How many chrome actions have started with `pane`'s banner. */
export function browserActionsStarted(pane: BrowserPaneAttachment): number {
  return browserActionCounts.get(pane.mount) ?? 0;
}

/** Test hook: the mounts an action count is kept for. */
export function browserActionCountMountsForTest(): string[] {
  return [...browserActionCounts.keys()];
}

/** A refused browser action: its banner text and the error behind it. */
export interface BrowserActionFailure {
  error: string;
  cause: unknown;
}

/** A sent action's failure and the state it answered, one of them null. */
interface ActionResult {
  readonly failure: BrowserActionFailure | null;
  readonly state: BrowserCompanionEvent | null;
}

/**
 * Runs one chrome action: counts it, clears the banner of `sink`, sends,
 * and puts a refusal in that banner unless `question` says it is a question
 * for the user, which the caller asks instead.
 */
async function runAction(
  sink: BrowserPaneAttachment | null,
  send: () => Promise<BrowserActionAnswer>,
  question: (err: unknown) => boolean = () => false,
): Promise<ActionResult> {
  if (sink && isCurrentBrowserPane(sink)) {
    browserActionCounts.set(sink.mount, browserActionsStarted(sink) + 1);
    noteBrowserPaneError(sink, '');
  }
  const answer = await send();
  switch (answer.kind) {
    case 'answered':
      return { failure: null, state: answer.state };
    case 'refused':
      if (sink && !question(answer.cause)) noteBrowserPaneError(sink, answer.error);
      return { failure: { error: answer.error, cause: answer.cause }, state: null };
    case 'pane-gone': {
      const gone = new Error('The browser pane closed before this action was sent.');
      return { failure: { error: gone.message, cause: gone }, state: null };
    }
    default:
      return unhandledAnswer(answer);
  }
}

function unhandledAnswer(answer: never): never {
  throw new Error(`Unhandled browser action answer: ${JSON.stringify(answer)}`);
}

/**
 * One action from the pane `pane`, the attachment of the pane the user
 * acted in: numbered in its mount's order when its kind selects or acts on
 * the active tab, sent only while it is the thread's attached mount, and its
 * refusal in its banner. Null on success.
 */
export async function actInPane(pane: BrowserPaneAttachment, action: BrowserAction): Promise<BrowserActionFailure | null> {
  return (await runAction(pane, () => sendFromPane(pane, action))).failure;
}

/**
 * One action on the thread's browser with no pane in hand (the chat
 * header's chip, a download toast). The backend answers with the next
 * state, which lands like any push; a refusal lands in the banner of the
 * pane attached now, if any, and is also returned for the caller to show.
 * Null on success.
 */
export async function browserCompanionAct(threadId: string, action: ThreadAction): Promise<BrowserActionFailure | null> {
  return (await actForThread(threadId, action)).failure;
}

/** browserCompanionAct, answering the state as well. */
function actForThread(threadId: string, action: ThreadAction): Promise<ActionResult> {
  return runAction(browserPaneAttachment(threadId), () => sendForThread(threadId, action));
}

/** What a cookie jar switch answered. */
export type JarSwitchAnswer =
  | { readonly kind: 'switched' }
  | { readonly kind: 'needs-confirmation' }
  /** The pane it was chosen in is not the thread's browser any more: nothing changed. */
  | { readonly kind: 'pane-gone'; readonly failure: BrowserActionFailure }
  | { readonly kind: 'failed'; readonly failure: BrowserActionFailure };

// The backend's refusal of an unconfirmed switch while a tab is live
// (internal/app browserJarUnconfirmedCode): a question for the user, not a
// failure for the banner.
function jarUnconfirmed(err: unknown): boolean {
  return err instanceof TransportError && err.code === 'browser_jar_unconfirmed';
}

// The backend's refusal of a switch whose pane detached, or whose browser
// was closed and opened again, after the user chose it (internal/app
// browserJarPaneGoneCode): that pane has no banner left to show it.
function jarPaneGone(err: unknown): boolean {
  return err instanceof TransportError && err.code === 'browser_jar_pane_gone';
}

/**
 * Sets the cookie jar the thread's browser uses, from its pane. The backend
 * closes the live tabs and reopens their addresses in the new jar. Unless
 * the user confirmed losing the live tabs' page state, the backend refuses
 * while a tab is live and the answer is 'needs-confirmation': the caller
 * asks and sends again confirmed. The switch names `pane`, and the backend
 * switches the browser that pane attached to or nothing: a pane gone by
 * then answers 'pane-gone', which a toast shows. Any other refusal lands in
 * the pane's banner.
 */
export async function setBrowserJar(pane: BrowserPaneAttachment, jar: BrowserJar, confirmed: boolean): Promise<JarSwitchAnswer> {
  const question = (err: unknown) => jarUnconfirmed(err) || jarPaneGone(err);
  const { failure } = await runAction(pane, () => sendFromPane(pane, browserAction.jar(jar, confirmed)), question);
  if (!failure) return { kind: 'switched' };
  if (jarUnconfirmed(failure.cause)) return { kind: 'needs-confirmation' };
  if (jarPaneGone(failure.cause)) {
    addErrorToast(failure.error, failure.cause);
    return { kind: 'pane-gone', failure };
  }
  return { kind: 'failed', failure };
}

/** Options of a new tab the user opens. */
export interface NewTabOptions {
  /** Loaded in the new tab (a mod-clicked link, a picked file). */
  readonly address?: string;
  /**
   * The new-tab button's and shortcut's: the address bar takes focus, its
   * text selected, once the tab is ready, unless the user did anything else
   * first (addressFocusIntent).
   */
  readonly focusAddress?: boolean;
}

/**
 * Opens a new tab in the thread's browser for the user, showing the pane
 * even when the thread had no tab: the backend selects the tab and shows the
 * companion before the engine page exists, so the creating tab arrives with
 * the pane. From a link, with no pane in hand, it applies in arrival order;
 * from a pane, openBrowserTabInPane numbers it.
 */
export function openBrowserTab(threadId: string, options: NewTabOptions = {}): Promise<BrowserActionFailure | null> {
  const action = browserAction.newTab(options.address);
  return openTab(threadId, options, browserPaneAttachment(threadId), () => sendForThread(threadId, action));
}

/** openBrowserTab from the pane `pane`, in its mount's order. */
export function openBrowserTabInPane(pane: BrowserPaneAttachment, options: NewTabOptions = {}): Promise<BrowserActionFailure | null> {
  const action = browserAction.newTab(options.address);
  return openTab(pane.threadId, options, pane, () => sendFromPane(pane, action));
}

async function openTab(
  threadId: string,
  options: NewTabOptions,
  sink: BrowserPaneAttachment | null,
  send: () => Promise<BrowserActionAnswer>,
): Promise<BrowserActionFailure | null> {
  const focus = options.focusAddress ? armAddressFocus(threadId) : null;
  const { failure, state } = await runAction(sink, send);
  // A refusal with no pane to carry its banner (the thread had no tab, so
  // none opened) goes to a toast.
  if (failure && !browserPaneOpen(threadId)) addErrorToast(failure.error, failure.cause);
  // The answer names the tab; none when it failed or was closed, and the
  // failed tab shows why.
  if (focus) settleAddressFocus(focus, failure ? '' : (state?.pageId ?? ''));
  return failure;
}

/**
 * The tab a keyboard command on the keyboard's browser pane acts on: the
 * page a chord pressed in a page came from (browserPageChordPage), else the
 * tab the window shows as active now, which is what the user saw when the
 * key reached the window. Null when it shows none. The command names it,
 * so a tab switch still on its way never moves the command to another tab.
 */
export function keyboardTabTarget(focused: FocusedBrowser): PageId | null {
  return browserPageChordPage() ?? pageIdOf(browserShownPage(focused.state)?.id);
}

/**
 * `pane.close` on the keyboard's browser companion closes the tab it shows
 * (keyboardTabTarget); the pane goes only with its last tab (the zero-page
 * state push closes the companion in applyBrowserCompanionState). The close
 * takes its number in the pane mount's order at the keypress and names the
 * tab, so at its turn the backend closes that tab if it is still there.
 * Routed from the command, not from a keydown handler on the pane: on
 * WebKit a click on the tab strip moves no DOM focus, so a pane-scoped
 * handler missed the chord and the window-level `pane.close` destroyed the
 * whole pane instead. Returns false when the keyboard's pane is no browser,
 * or it shows no tab, and the command falls through to its pane close. A
 * browser pane not attached yet has nothing to send, and `pane.close` is
 * not offered for it (browserPaneUnattached).
 */
export function closeFocusedBrowserTab(): boolean {
  const focused = focusedBrowser();
  const pageId = focused ? keyboardTabTarget(focused) : null;
  if (!focused?.pane || !pageId || (focused.state?.pages?.length ?? 0) === 0) return false;
  void actInPane(focused.pane, browserAction.onTab('close', pageId));
  return true;
}

/** The keyboard's browser pane: its thread, last state (null before any) and attachment (null before the attach answers). */
export interface FocusedBrowser {
  readonly threadId: string;
  readonly state: Readonly<BrowserCompanionEvent> | null;
  readonly pane: BrowserPaneAttachment | null;
}

/**
 * The keyboard's target pane (paneCommandTarget) when it is a browser
 * companion. Null when the target is another pane or none, so a browser the
 * screen does not show never takes the keyboard's commands. Reactive.
 */
export function focusedBrowser(): FocusedBrowser | null {
  const targetId = paneCommandTarget();
  if (!targetId) return null;
  const companion = getCompanionPane(targetId);
  if (companion?.kind !== 'browser') return null;
  const threadId = getPane(companion.sourcePaneId)?.threadId;
  if (!threadId) return null;
  return { threadId, state: browserCompanionState(threadId), pane: browserPaneAttachment(threadId) };
}

/**
 * Whether the keyboard's target is a browser pane with no attachment yet:
 * its tabs may show, but nothing it would send has a place in the order, so
 * its chrome is unavailable and `pane.close` is not offered. Reactive.
 */
export function isBrowserPaneUnattached(): boolean {
  const focused = focusedBrowser();
  return focused !== null && focused.pane === null;
}

/**
 * Whether `page` takes an address: a tab being created, or one that failed
 * to open, has no page to navigate or reload yet. A crashed tab (suspended,
 * with its error) does: it reloads or goes elsewhere.
 */
export function tabTakesAddress(page: Readonly<BrowserPageInfo> | null): boolean {
  return !!page && page.creating !== true && !(page.error && !page.suspended);
}

/**
 * Whether the keyboard's target is a browser pane whose address field
 * cannot take focus: its chrome is inert until the attach answers, and its
 * shown tab may take no address (tabTakesAddress). A reload needs the same.
 * Reactive.
 */
export function isBrowserAddressUnavailable(): boolean {
  const focused = focusedBrowser();
  return focused !== null && (focused.pane === null || !tabTakesAddress(browserShownPage(focused.state)));
}

/** Whether the keyboard's target is a browser pane whose thread has no closed tab to reopen. Reactive. */
export function isBrowserClosedTabMissing(): boolean {
  const focused = focusedBrowser();
  return focused !== null && (focused.state?.closedTabs ?? 0) === 0;
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
 * (canPresentNativeBrowser), or while the thread's pane is in this layout,
 * which a press then hides; otherwise a press shows the browser. A show is
 * sent only where this layout presents the pane (showBrowserFromHeader),
 * so under compact, or with the browser turned off while the thread still
 * has tabs, the button is not offered rather than offered doing nothing.
 */
export function browserHeaderButton(threadId: string): BrowserHeaderButton {
  if (!threadId) return { rendered: false };
  const intent = browserHeaderIntent(threadId);
  if (intent.kind === 'show' && !canPresentNativeBrowser(threadId)) return { rendered: false };
  const tabs = canUseNativeBrowser(threadId) && browserCompanionState(threadId) !== null;
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

/**
 * Carries out a header press. Every press sends: the Manager converges two
 * shows (ShowCompanion), and a repeated press's effects are settled where
 * they are produced. Its address focus request joins one whose answer is
 * still due, so the focus belongs to the tab the presses opened, whichever
 * answer names it (armAddressFocus), and a refusal already on screen in a toast is not
 * raised again (toastHeaderFailure). A refusal reaches a toast, since a
 * hidden pane has no banner.
 */
export function runBrowserHeaderIntent(intent: BrowserHeaderIntent): Promise<void> {
  switch (intent.kind) {
    case 'hide':
      return hideBrowserFromHeader(intent.threadId);
    case 'show':
      return showBrowserFromHeader(intent.threadId);
    default:
      return unhandledIntent(intent);
  }
}

// A header press's refusal, in a toast unless one with the same text is on
// screen: a repeated press refused for the same reason says it once.
function toastHeaderFailure(failure: BrowserActionFailure): void {
  if (getToasts().some((toast) => toast.type === 'error' && toast.message === failure.error)) return;
  addErrorToast(failure.error, failure.cause);
}

function unhandledIntent(intent: never): never {
  throw new Error(`Unhandled browser header intent: ${JSON.stringify(intent)}`);
}

async function hideBrowserFromHeader(threadId: string): Promise<void> {
  const failure = await browserCompanionAct(threadId, browserAction.hide());
  if (failure) toastHeaderFailure(failure);
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
 * again (browserPresentationWanted). A page that cannot host the thread's
 * browser at all is refused where the action is sent (sendForThread), and
 * the refusal reaches a toast.
 */
async function showBrowserFromHeader(threadId: string): Promise<void> {
  if (!browserSourcePaneShown(threadId)) return;
  if (canUseNativeBrowser(threadId) && !canPresentNativeBrowser(threadId)) return;
  const focus = armAddressFocus(threadId);
  const { failure, state } = await actForThread(threadId, browserAction.showBrowser());
  settleAddressFocus(focus, failure ? '' : (state?.pageId ?? ''));
  // An older backend refuses the kind ("unsupported companion action").
  if (failure) toastHeaderFailure(failure);
}

observeBrowserCompanion({
  mountEnded: (mount) => {
    browserActionCounts.delete(mount);
  },
  resetForTest: () => {
    browserActionCounts.clear();
  },
});
