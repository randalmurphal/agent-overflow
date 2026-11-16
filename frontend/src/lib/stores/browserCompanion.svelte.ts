import { threadBackend } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { requireEntityBackend } from '../transport/backends';
import { isFrontendOnly } from '../transport/runMode';
import { isHostWindow } from '../transport/pageHost';
import {
  BrowserCompanionAction,
  BrowserCompanionDo,
  BrowserCompanionPaneAttach,
  BrowserCompanionPaneDetach,
  BrowserCompanionPaneRect,
  BrowserCompanionThreadState,
  BrowserPaneRect,
  type BrowserCompanionEvent,
} from './bindings';
import { applyBrowserFindEvent } from './browserFind.svelte';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import {
  closeCompanion,
  companionForSource,
  getCompanionPane,
  openCompanion,
} from './companionPanes.svelte';
import { addPaneThreadMountedObserver, focusPane, getAllPanes, getFocusedPaneId, getPane } from './panes.svelte';
import { hasScope } from '../transport/scopes';
import { isCompactLayout } from './layoutMode.svelte';
import { errString } from '../utils/errors';
import { createSequenceGate } from './snapshotStream';
import { getTransportHelloFor } from './transportStatus.svelte';
import {
  browserPaneRectKey,
  createBrowserPaneRectSender,
  type BrowserPaneRectReport,
} from './browserPaneRectSender';
import { paneGeometryBridgeAvailable, postPaneFocusIntent, postPaneGeometry } from '../native/paneGeometryBridge';
import { reportFrontendDiagnostic } from '../utils/frontendErrorCapture';
import { getSettings } from './settings.svelte';
import { addErrorToast } from './toast.svelte';

// The browser pane's state store. The pane surface itself is an empty host
// rect the platform positions a NATIVE browser view over (spec
// docs/specs/embedded-browser.md §7): no pixels cross this store, only the
// thread's page/session state and the host rect reports going the other way.

export interface BrowserCompanionView {
  state: BrowserCompanionEvent;
  /** The pane failed as a whole (attach, backend); replaces its body. */
  error: string;
  /** The last chrome action was refused; the pane's banner. */
  actionError: string;
}

// paneIds maps a thread to its live pane mount, acquired by the entity
// store's source and addressed by every rect report.
const paneIds = new Map<string, string>();

// Last state event per thread with live pages, reactive per key so the chat
// header's browser chip re-evaluates only when ITS thread's pages change.
const liveStates = createKeyedSignalRegistry<BrowserCompanionEvent | null>(null);
const hydratedThreads = new Set<string>();

// A thread's state arrives as pushes and as the answers of attach, action
// and hydrate calls, and the transport can write a push published after a
// call returned ahead of its reply. The backend numbers every snapshot, so
// the newest by that number wins, whatever the arrival order.
const stateOrder = createSequenceGate();

/** Whether `state` is newer than every state of its thread seen so far. */
function admitState(state: BrowserCompanionEvent): boolean {
  return stateOrder.admit(state.threadId, state.seq, getTransportHelloFor(HOME_BACKEND)?.launchId ?? '');
}

/**
 * Whether THIS page can host the thread's native browser pane: the host's
 * own window, which the native view lives in (a browser tab on the host
 * shows what a connected browser shows), in a full app, and the thread
 * lives on the home backend. The chat header's chip, the opener, a
 * mod-clicked link, the entity source and the hydration read all ask this
 * one question, so nothing offers or opens a pane the source would refuse.
 */
export function canUseNativeBrowser(threadId: string): boolean {
  if (!hasScope('host') || !isHostWindow() || isFrontendOnly()) return false;
  try {
    return requireEntityBackend(threadBackend(threadId)) === HOME_BACKEND;
  } catch {
    return false;
  }
}

/**
 * Whether the thread's browser can be opened from this page at all: the
 * pane can host it (canUseNativeBrowser), the home backend has a browser
 * engine (the hello's `browser` capability) and the browser is turned on.
 * The chat header's browser button renders on this, tabs or not.
 */
export function canOpenNativeBrowser(threadId: string): boolean {
  if (!canUseNativeBrowser(threadId)) return false;
  if (!(getTransportHelloFor(HOME_BACKEND)?.capabilities.includes('browser') ?? false)) return false;
  return getSettings(HOME_BACKEND).browserEnabled;
}

/**
 * The only opener of a browser companion. The state push and the pane
 * reconcile on a thread mount call it, and through the push so do the
 * chip's show, a mod-clicked link and startup hydration. Browser
 * companions are never persisted or stashed, so no layout restore
 * reopens one. It opens nothing
 * in a page that cannot host the native view (canUseNativeBrowser), which
 * records the state as a connected browser does, and nothing under
 * compact: opening ends in revealPane, which would pull a phone off its
 * thread list for a native view the compact screen never shows
 * (docs/specs/remote-access.md, "Hidden on the phone").
 */
function openBrowserCompanion(sourcePaneId: string, threadId: string): void {
  if (isCompactLayout() || !canUseNativeBrowser(threadId)) return;
  if (companionForSource(sourcePaneId, 'browser')) return;
  openCompanion(sourcePaneId, 'browser');
}

function emptyView(state: BrowserCompanionEvent): BrowserCompanionView {
  return { state, error: '', actionError: '' };
}

const store = createEntityStore<BrowserCompanionView, void>({
  name: 'browserCompanion',
  // Native views and their mounts belong to this window's local execution host.
  backendForKey: () => HOME_BACKEND,
  rawValue: true,
  source: async ({ key, apply }) => {
    if (!canUseNativeBrowser(key)) throw new Error('A native browser pane is unavailable for this conversation on this device.');
    const result = await BrowserCompanionPaneAttach(key);
    paneIds.set(key, result.id);
    // A push that landed during the attach is newer than its answer.
    const held = store.snapshot(key)?.state;
    apply(emptyView(admitState(result.state) || !held ? result.state : held));
    return async () => {
      if (paneIds.get(key) === result.id) paneIds.delete(key);
      forgetPaneMount(result.id);
      await BrowserCompanionPaneDetach(result.id);
    };
  },
});

export function attachBrowserCompanion(threadId: string): EntityAttachment<BrowserCompanionView> {
  return store.attach(threadId, undefined);
}

/** The backend's id of the thread's mounted pane, null while it has none. */
export function browserPaneMountId(threadId: string): string | null {
  return paneIds.get(threadId) ?? null;
}

// The backend's answer when a rect names a mount it no longer has
// (internal/browser SetPaneRect): a teardown race, which the next attach
// re-reports.
const MOUNT_GONE = 'pane mount not found';

function threadForMount(mount: string): string | null {
  for (const [threadId, id] of paneIds) {
    if (id === mount) return threadId;
  }
  return null;
}

const rectSender = createBrowserPaneRectSender({
  send: (mount, rect) => BrowserCompanionPaneRect(mount, new BrowserPaneRect(rect)),
  isMountGone: (error) => errString(error).includes(MOUNT_GONE),
  onFailure: (mount, error) => {
    const threadId = threadForMount(mount);
    if (threadId) noteBrowserActionError(threadId, `The browser view could not follow the pane: ${errString(error)}`);
  },
});

// Per mount: the newest distinct report, its sequence, and the sequence last
// posted to the launcher (native/paneGeometryBridge.ts).
const paneReports = new Map<string, { key: string; seq: number; postedSeq: number }>();

/**
 * Reports where the mounted pane's host rect sits, already coalesced to one
 * call per frame by the pane. Each distinct report is numbered once per
 * mount. The backend receives it in order through `rectSender`, and its
 * bounds directive carries the number; a launcher that takes geometry
 * directly also receives it at once and moves the view without the backend
 * round trip, placing from whichever of the two is newer.
 */
export function reportBrowserPaneRect(threadId: string, rect: BrowserPaneRectReport): void {
  const mount = paneIds.get(threadId);
  if (!mount) return;
  const key = browserPaneRectKey(rect);
  let state = paneReports.get(mount);
  if (!state) {
    state = { key: '', seq: 0, postedSeq: 0 };
    paneReports.set(mount, state);
  }
  if (state.key !== key) {
    state.key = key;
    state.seq += 1;
  }
  const report = { ...rect, seq: state.seq };
  rectSender.report(mount, report);
  if (state.postedSeq === report.seq || !paneGeometryBridgeAvailable()) return;
  try {
    if (postPaneGeometry({ mount, ...report })) state.postedSeq = report.seq;
  } catch (err) {
    // Until a post lands, the launcher places from the backend's directive,
    // which carries this report's sequence; the next report posts again.
    reportFrontendDiagnostic('Browser pane geometry post to the window host failed', errString(err));
  }
}

/** Ends a mount's reports, local and to the backend. */
function forgetPaneMount(mount: string): void {
  rectSender.forget(mount);
  paneReports.delete(mount);
  // Unmounting is a focus choice; the detach withdraws at the backend too.
  for (const [threadId, record] of [...pageFocusIntents]) {
    if (record.mount === mount) withdrawPageFocus(threadId);
  }
  pageFocusSeqs.delete(mount);
}

function sourcePaneIdForThread(threadId: string): string | null {
  for (const [paneId, pane] of getAllPanes()) {
    if (pane.threadId === threadId) return paneId;
  }
  return null;
}

export function applyBrowserCompanionState(event: BrowserCompanionEvent): void {
  if (!event?.threadId) return;
  if (event.kind === 'error') {
    const previous = store.snapshot(event.threadId);
    if (previous) store.apply(event.threadId, { ...previous, error: event.error || 'Browser pane failed' });
    return;
  }
  if (event.kind === 'accelerator') {
    replayBrowserAccelerator(event);
    return;
  }
  if (event.kind === 'find' || event.kind === 'escape') {
    applyBrowserFindEvent(event);
    return;
  }
  if (event.kind === 'teardown') {
    reportBrowserTeardown(event);
    return;
  }
  if (event.kind === 'focus') {
    settlePageFocus(event);
    return;
  }
  if (event.kind !== 'state') return;
  if (!admitState(event)) return;
  // Another active tab (a switch by chord, by an agent) is another focus
  // choice than the page asked for.
  const focusIntent = pageFocusIntents.get(event.threadId);
  if (focusIntent && (event.activePageId ?? '') !== focusIntent.pageId) withdrawPageFocus(event.threadId);
  hydratedThreads.add(event.threadId);
  if ((event.pages?.length ?? 0) > 0) liveStates.set(event.threadId, event);
  else liveStates.drop(event.threadId);
  // Closing runs on every client: a companion opened on the desktop survives
  // a resize into compact, and its last page closing still has to take it
  // down.
  const sourcePaneId = sourcePaneIdForThread(event.threadId);
  if (sourcePaneId) {
    const existing = companionForSource(sourcePaneId, 'browser');
    if ((event.pages?.length ?? 0) > 0 && event.visible === true) {
      openBrowserCompanion(sourcePaneId, event.threadId);
    } else if (existing) {
      closeCompanion(existing.paneId);
    }
  }
  const previous = store.snapshot(event.threadId);
  store.apply(
    event.threadId,
    previous ? { ...previous, state: event, error: '' } : emptyView(event),
  );
}

/**
 * A page that already left the tab strip failed to close, or its profile
 * failed to release (the backend's "teardown" event). The open pane's banner
 * carries it; with no pane, which is the case once the last tab closed, a
 * toast that stays until dismissed does.
 */
function reportBrowserTeardown(event: BrowserCompanionEvent): void {
  const message = `A closed browser tab did not shut down cleanly: ${event.error || 'unknown error'}`;
  if (browserPaneOpen(event.threadId)) {
    noteBrowserActionError(event.threadId, message);
    return;
  }
  addErrorToast(message, new Error(event.error || message), 0);
}

/** Whether the thread's browser pane is open here, with its banner. */
function browserPaneOpen(threadId: string): boolean {
  const sourcePaneId = sourcePaneIdForThread(threadId);
  return sourcePaneId !== null && !!companionForSource(sourcePaneId, 'browser') && !!store.snapshot(threadId);
}

// True while a chord the page claimed is dispatched (replayBrowserAccelerator).
let replayingPageChord = false;

/**
 * Whether the keydown being dispatched is a chord pressed in a browser page.
 * It was typed with keyboard focus in the page, so it resolves outside any
 * terminal whatever focus the SPA has by the time it arrives: the engine
 * hands focus back to the SPA when it claims the chord, and the terminal
 * caret left there may already have it again.
 */
export function isReplayingBrowserPageChord(): boolean {
  return replayingPageChord;
}

/**
 * A bound chord pressed while the thread's NATIVE page view held keyboard
 * focus (spec §7). The engine swallowed it and the backend routed it here;
 * replaying it as a window keydown puts it through the one dispatcher every
 * other keystroke uses, so `when` gates, user rebinds, and the palette all
 * behave exactly as if the SPA had been focused. The companion pane is
 * focused first: the click that gave the page focus moved no DOM focus, and
 * pane-scoped commands (pane.close → this pane's tab) need the logical focus
 * to be where the user is looking. The engine already handed keyboard focus
 * to the SPA. DOM focus left in the SPA stays where it is, so a surface the
 * chord opens returns focus there on close; it gates nothing for this chord
 * (isReplayingBrowserPageChord).
 *
 * Every host client receives the event; only the one holding the pane
 * mount that showed the page (`event.mount`) replays it.
 */
function replayBrowserAccelerator(event: BrowserCompanionEvent): void {
  const chord = event.accelerator;
  if (!chord || !event.mount || event.mount !== browserPaneMountId(event.threadId)) return;
  const sourcePaneId = sourcePaneIdForThread(event.threadId);
  const companion = sourcePaneId ? companionForSource(sourcePaneId, 'browser') : null;
  if (companion) focusPane(companion.paneId);
  replayingPageChord = true;
  try {
    window.dispatchEvent(
      new KeyboardEvent('keydown', {
        key: chord.key,
        ctrlKey: chord.ctrl === true,
        metaKey: chord.meta === true,
        altKey: chord.alt === true,
        shiftKey: chord.shift === true,
        cancelable: true,
      }),
    );
  } finally {
    replayingPageChord = false;
  }
}

// Reactive per-thread read behind the chat header's browser chip: null until
// the thread has live pages. Tracked per key, so a chip re-evaluates only
// when ITS thread's pages change.
export function browserCompanionState(threadId: string): BrowserCompanionEvent | null {
  return liveStates.get(threadId);
}

// A find stop that could not be delivered, per thread (stores/browserFind):
// the page may keep its highlights. Its own entry, so a browser action
// neither clears it nor is hidden by it.
const findCleanupErrors = createKeyedSignalRegistry<string>('');

/** Records, or with null withdraws, the thread's find cleanup failure. */
export function noteBrowserFindCleanupError(threadId: string, message: string | null): void {
  if (message) findCleanupErrors.set(threadId, message);
  else findCleanupErrors.drop(threadId);
}

/** The thread's find cleanup failure, '' for none. */
export function browserFindCleanupError(threadId: string): string {
  return findCleanupErrors.get(threadId);
}

export function noteBrowserActionError(threadId: string, message: string): void {
  const previous = store.snapshot(threadId);
  if (previous && previous.actionError !== message) store.apply(threadId, { ...previous, actionError: message });
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
  action: { kind: string; pageId?: string; address?: string; index?: number },
): Promise<BrowserActionFailure | null> {
  return (await runBrowserAction(threadId, action)).failure;
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

async function runBrowserAction(
  threadId: string,
  action: { kind: string; pageId?: string; address?: string; index?: number; jar?: BrowserJar },
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

// Page focus intents (internal/browser/focus_intent.go): the user's request,
// by the address bar's Enter or Escape, that one page take the keyboard. It
// reaches the page a round trip later, and on Windows two, so the user's next
// focus choice withdraws it first: a focus anywhere outside the pane's page
// area (the composer, the address bar again), a press outside it, another
// active tab, the pane unmounting. Per thread, the one request in flight;
// numbered per mount, which is how the backend and the launcher tell arms,
// withdrawals and answers apart whatever order they arrive in. A request
// stays here, watched, until one of those choices withdraws it, a newer one
// replaces it, or a Kind "focus" event says how it ended: whatever its arm's
// answer was, and however long it took, since only where focus is applied
// is its age known.
interface PageFocusIntent {
  threadId: string;
  mount: string;
  intent: number;
  pageId: string;
  /** The page area: a focus or press inside it is no other choice. */
  area: HTMLElement;
  /** browserActionCounts for the thread when it was armed. */
  actionsAtArm: number;
}

const pageFocusIntents = new Map<string, PageFocusIntent>();
const pageFocusSeqs = new Map<string, number>();
// Chrome actions started per thread, so a focus refusal that arrives after a
// newer action knows it is older than that action's banner.
const browserActionCounts = new Map<string, number>();

function noteBrowserActionStarted(threadId: string): void {
  browserActionCounts.set(threadId, (browserActionCounts.get(threadId) ?? 0) + 1);
}

/**
 * Gives keyboard focus to `pageId`, the page the thread's pane shows in
 * `area`, as a browser does after the address bar's Enter or Escape: now if
 * it is presented, or once it is (a restore, a layout), unless the user
 * makes another focus choice first. Only this computer's app window holds
 * the native view (canUseNativeBrowser); anywhere else there is nothing to
 * focus. Unlike a chrome action it leaves a banner already up (the
 * navigation it follows may have failed); a refusal, now or when the page
 * is presented, goes in it unless a newer action's banner is up. Null on
 * success or when there is nothing to focus.
 */
export async function focusBrowserPage(
  threadId: string,
  pageId: string,
  area: HTMLElement,
): Promise<BrowserActionFailure | null> {
  if (!canUseNativeBrowser(threadId)) return null;
  const mount = paneIds.get(threadId);
  if (!mount || !pageId) return null;
  // A newer request supersedes the thread's last one wherever it is.
  retirePageFocus(threadId);
  const intent = (pageFocusSeqs.get(mount) ?? 0) + 1;
  pageFocusSeqs.set(mount, intent);
  const record: PageFocusIntent = {
    threadId,
    mount,
    intent,
    pageId,
    area,
    actionsAtArm: browserActionCounts.get(threadId) ?? 0,
  };
  if (pageFocusIntents.size === 0 && typeof window !== 'undefined') {
    document.addEventListener('focusin', onUserFocusChoice, true);
    window.addEventListener('pointerdown', onUserFocusChoice, true);
  }
  pageFocusIntents.set(threadId, record);
  try {
    postPaneFocusIntent({ mount, intent, withdrawn: false });
  } catch (err) {
    // The launcher then applies the focus without hearing of this arm; a
    // withdrawal still reaches it, and the backend.
    reportFrontendDiagnostic('Browser page focus post to the window host failed', errString(err));
  }
  try {
    const state = await BrowserCompanionDo(
      threadId,
      new BrowserCompanionAction({ kind: 'focus-page', pageId, address: '', index: 0, mount, intent }),
    );
    applyBrowserCompanionState(state);
    return null;
  } catch (err) {
    // The arm may still have landed: the request stays watched, and the
    // user's next choice withdraws it.
    const message = errString(err);
    if (pageFocusIntents.get(threadId) === record && bannerable(record)) noteBrowserActionError(threadId, message);
    return { error: message, cause: err };
  }
}

/**
 * Whether a refusal of the intent may take the banner: no chrome action
 * started since it was armed, or the one that did left no banner.
 */
function bannerable(record: PageFocusIntent): boolean {
  if ((browserActionCounts.get(record.threadId) ?? 0) === record.actionsAtArm) return true;
  return !store.snapshot(record.threadId)?.actionError;
}

/** Ends the thread's intent here without telling anyone: it landed, failed or was superseded. */
function retirePageFocus(threadId: string): PageFocusIntent | null {
  const record = pageFocusIntents.get(threadId);
  if (!record) return null;
  pageFocusIntents.delete(threadId);
  if (pageFocusIntents.size === 0 && typeof window !== 'undefined') {
    document.removeEventListener('focusin', onUserFocusChoice, true);
    window.removeEventListener('pointerdown', onUserFocusChoice, true);
  }
  return record;
}

/**
 * The user made another focus choice: the thread's intent is withdrawn at
 * the launcher (at once, in its own process) and at the backend, so a
 * request still on its way moves nothing, and one that landed meanwhile
 * gives the keyboard back.
 */
function withdrawPageFocus(threadId: string): void {
  const record = retirePageFocus(threadId);
  if (!record) return;
  const { mount, intent } = record;
  try {
    postPaneFocusIntent({ mount, intent, withdrawn: true });
  } catch (err) {
    noteBrowserActionError(threadId, `The browser page may still take the keyboard: ${errString(err)}`);
  }
  void (async () => {
    try {
      await BrowserCompanionDo(
        threadId,
        new BrowserCompanionAction({ kind: 'focus-page-withdraw', pageId: '', address: '', index: 0, mount, intent }),
      );
    } catch (err) {
      noteBrowserActionError(threadId, `The browser page may still take the keyboard: ${errString(err)}`);
    }
  })();
}

function onUserFocusChoice(event: Event): void {
  const target = event.target;
  for (const [threadId, record] of [...pageFocusIntents]) {
    if (target instanceof Node && record.area.contains(target)) continue;
    withdrawPageFocus(threadId);
  }
}

/**
 * A Kind "focus" event: how the intent it names ended. Only the thread's
 * current intent settles; a refusal takes the banner unless a newer
 * action's banner is up (bannerable).
 */
function settlePageFocus(event: BrowserCompanionEvent): void {
  const record = pageFocusIntents.get(event.threadId);
  if (!record || record.mount !== event.mount || record.intent !== event.focusIntent) return;
  retirePageFocus(event.threadId);
  if (event.error && bannerable(record)) noteBrowserActionError(event.threadId, event.error);
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
  const { failure, state } = await runBrowserAction(threadId, { kind: 'new', address: options.address });
  // A refusal with no pane to carry its banner (the thread had no tab, so
  // none opened) goes to a toast.
  if (failure && !browserPaneOpen(threadId)) addErrorToast(failure.error, failure.cause);
  // Only this request's own answer settles it: a newer new tab replaced
  // it, and this answer must neither resolve nor withdraw that one.
  if (request && armedAddressFocus.get(threadId) === request && addressFocusIntents.get(threadId) === PENDING_TAB) {
    // The answer names the tab; none when it failed or was closed, and
    // the failed tab shows why.
    const pageId = failure ? '' : (state?.pageId ?? '');
    if (pageId) addressFocusIntents.set(threadId, pageId);
    else dropAddressFocus(threadId);
  }
  return failure;
}

// Address-bar focus the user asked for with a new tab, per thread: null for
// none, PENDING_TAB until the `new` answer names the tab, then its page id.
// Any pointer press or key in the window before the pane takes it withdraws
// every intent (the user moved on), as does the pane finding another tab
// active. Each request is numbered (armedAddressFocus), so only its own
// answer settles it.
const PENDING_TAB = '';
const addressFocusIntents = createKeyedSignalRegistry<string | null>(null);
const armedAddressFocus = new Map<string, number>();
let addressFocusRequests = 0;

/** Arms a focus request for the thread, replacing any, and answers its number. */
function armAddressFocus(threadId: string): number {
  if (armedAddressFocus.size === 0 && typeof window !== 'undefined') {
    window.addEventListener('pointerdown', dropAllAddressFocus, true);
    window.addEventListener('keydown', dropAllAddressFocus, true);
  }
  const request = ++addressFocusRequests;
  armedAddressFocus.set(threadId, request);
  addressFocusIntents.set(threadId, PENDING_TAB);
  return request;
}

function dropAddressFocus(threadId: string): void {
  if (!armedAddressFocus.delete(threadId)) return;
  addressFocusIntents.set(threadId, null);
  if (armedAddressFocus.size === 0 && typeof window !== 'undefined') {
    window.removeEventListener('pointerdown', dropAllAddressFocus, true);
    window.removeEventListener('keydown', dropAllAddressFocus, true);
  }
}

function dropAllAddressFocus(): void {
  for (const threadId of [...armedAddressFocus.keys()]) dropAddressFocus(threadId);
}

/**
 * The tab whose address bar should take focus once it is ready: its page id,
 * PENDING_TAB ('') while the request is unanswered, null for none. Reactive.
 */
export function addressFocusIntent(threadId: string): string | null {
  return addressFocusIntents.get(threadId);
}

/**
 * The pane's answer to an intent for the tab it shows: true, once, when the
 * intended tab is the active one and ready to take an address. Another
 * active tab, or the intended tab failing, withdraws the intent.
 */
export function takeAddressFocus(threadId: string, active: { id: string; creating?: boolean; error?: string } | null): boolean {
  const intent = addressFocusIntents.get(threadId);
  if (intent === null || intent === PENDING_TAB || !active) return false;
  if (active.id !== intent || active.error) {
    dropAddressFocus(threadId);
    return false;
  }
  if (active.creating) return false;
  dropAddressFocus(threadId);
  return true;
}

/**
 * `pane.close` on a focused browser companion closes its ACTIVE TAB; the pane
 * goes only with its last tab (the zero-page state push closes the companion
 * in applyBrowserCompanionState). Routed from the command, not from a keydown
 * handler on the pane: on WebKit a click on the tab strip moves no DOM focus,
 * so a pane-scoped handler missed the chord and the window-level `pane.close`
 * destroyed the whole pane instead. Returns false when nothing browser-shaped
 * holds focus, and the command falls through to its pane close.
 */
export function closeFocusedBrowserTab(): boolean {
  const focused = focusedBrowser();
  const pageId = focused?.state?.activePageId || focused?.state?.pages?.[0]?.id;
  if (!focused || !pageId) return false;
  void browserCompanionAct(focused.threadId, { kind: 'close', pageId });
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
  return { threadId, state: liveStates.get(threadId) };
}

// The mounted pane's address bar, per thread, for the focus-address
// command: the pane registers it while mounted (registerBrowserAddressBar).
const addressBars = new Map<string, () => void>();

/** Registers the thread's mounted address bar focuser; the result removes it. */
export function registerBrowserAddressBar(threadId: string, focus: () => void): () => void {
  addressBars.set(threadId, focus);
  return () => {
    if (addressBars.get(threadId) === focus) addressBars.delete(threadId);
  };
}

/** Focuses the thread's address bar with its text selected; false with no pane mounted. */
export function focusBrowserAddressBar(threadId: string): boolean {
  const focus = addressBars.get(threadId);
  if (!focus) return false;
  focus();
  return true;
}

// The `browser:companion-state` channel is ephemeral (no replay), so a
// freshly loaded UI cannot know a thread already has live pages. One
// backend read per thread fills that in; every later change arrives as a
// push. A push newer than the read's answer, whenever it landed, wins by
// its number (`admitState`).
export function hydrateBrowserCompanionState(threadId: string): void {
  if (!threadId || hydratedThreads.has(threadId)) return;
  // The read rides `host`, and it runs on every pane thread mount, so a
  // session off the host would spend one refusal per thread open on a
  // surface it cannot use anyway: the pane is a NATIVE view this machine
  // paints, which no remote client renders, and no grant can ever open
  // it. The scope read comes BEFORE the thread is marked hydrated: the
  // caller is an $effect, host presence lands with the bootstrap manifest
  // after the first pane mounts, and pinning the thread on the placeholder
  // answer would leave it unhydrated for the life of the page.
  if (!canUseNativeBrowser(threadId)) return;
  hydratedThreads.add(threadId);
  void (async () => {
    // The async wrapper folds a SYNCHRONOUS throw from the binding into the
    // same failure path as a rejection: callers run this from $effect bodies,
    // where an escaping throw tears down the component's effects.
    try {
      applyBrowserCompanionState(await BrowserCompanionThreadState(threadId));
    } catch {
      // A failed read (backend restarting, thread gone) must not pin the
      // thread as hydrated forever; the next mount retries.
      hydratedThreads.delete(threadId);
    }
  })();
}

export function reconcileBrowserCompanionForPane(paneId: string, threadId: string): void {
  hydrateBrowserCompanionState(threadId);
  const state = liveStates.get(threadId);
  if (state?.visible === true && (state.pages?.length ?? 0) > 0) openBrowserCompanion(paneId, threadId);
}

addPaneThreadMountedObserver(reconcileBrowserCompanionForPane);

export function resetBrowserCompanionForTest(): void {
  for (const threadId of [...pageFocusIntents.keys()]) retirePageFocus(threadId);
  browserActionCounts.clear();
  for (const mount of paneIds.values()) forgetPaneMount(mount);
  for (const mount of [...paneReports.keys()]) forgetPaneMount(mount);
  paneIds.clear();
  stateOrder.reset();
  liveStates.reset();
  findCleanupErrors.reset();
  dropAllAddressFocus();
  addressFocusIntents.reset();
  hydratedThreads.clear();
  addressBars.clear();
  store.suspend();
  store.resetAll();
}

/** Records the thread's pane mount as an attach would, for tests. */
export function setBrowserPaneMountForTest(threadId: string, mount: string): void {
  paneIds.set(threadId, mount);
}
