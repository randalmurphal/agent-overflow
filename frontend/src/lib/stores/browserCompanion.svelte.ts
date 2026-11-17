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
  BrowserCompanionThreadState,
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
import { armAddressFocus, resetBrowserAddressFocusForTest, settleAddressFocus } from './browserAddressFocus.svelte';
import { getSettings } from './settings.svelte';
import { addErrorToast } from './toast.svelte';

// The browser pane's state store. The pane surface itself is an empty host
// rect the platform positions a NATIVE browser view over (spec
// docs/specs/embedded-browser.md §7): no pixels cross this store, only the
// thread's page/session state. It owns the pane attachment and its mount,
// the state events (ordered by their sequence), chrome actions and their
// banner, and the capability gates. The parts that keep their own state
// beside it import it and follow it through observeBrowserCompanion: the
// host rect reports (browserPaneRectReports.ts) and the page keyboard focus
// intent (browserPageFocus.ts). Address bar focus is a leaf it imports
// (browserAddressFocus.svelte.ts).

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

/**
 * A part of the browser pane that keeps its own state beside this store and
 * follows it here, so this store never imports it.
 */
export interface BrowserCompanionObserver {
  /** An admitted Kind "state" event, before it is applied. */
  stateAdmitted?(event: BrowserCompanionEvent): void;
  /** A Kind "focus" event: how a page focus intent ended. */
  focusSettled?(event: BrowserCompanionEvent): void;
  /** The thread's pane mount ended with its attachment. */
  mountEnded?(mount: string): void;
  /** resetBrowserCompanionForTest: drop all state, telling no one. */
  resetForTest?(): void;
}

const observers: BrowserCompanionObserver[] = [];

/** Registers a part's observer; parts register once, at module setup. */
export function observeBrowserCompanion(observer: BrowserCompanionObserver): void {
  observers.push(observer);
}

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
      selectionSeqs.delete(result.id);
      for (const observer of observers) observer.mountEnded?.(result.id);
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

/** The thread whose pane holds `mount`, null for none. */
export function threadForBrowserPaneMount(mount: string): string | null {
  for (const [threadId, id] of paneIds) {
    if (id === mount) return threadId;
  }
  return null;
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
    for (const observer of observers) observer.focusSettled?.(event);
    return;
  }
  if (event.kind !== 'state') return;
  if (!admitState(event)) return;
  for (const observer of observers) observer.stateAdmitted?.(event);
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
export function browserPaneOpen(threadId: string): boolean {
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

/** The thread's banner text, '' for none. */
export function browserActionError(threadId: string): string {
  return store.snapshot(threadId)?.actionError ?? '';
}

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

async function runBrowserAction(
  threadId: string,
  action: { kind: string; pageId?: string; address?: string; index?: number; jar?: BrowserJar; downloadId?: string },
): Promise<{ failure: BrowserActionFailure | null; state: BrowserCompanionEvent | null }> {
  const order = selectionOrder(threadId, action.kind);
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
  const { failure, state } = await runBrowserAction(threadId, { kind: 'new', address: options.address });
  // A refusal with no pane to carry its banner (the thread had no tab, so
  // none opened) goes to a toast.
  if (failure && !browserPaneOpen(threadId)) addErrorToast(failure.error, failure.cause);
  // The answer names the tab; none when it failed or was closed, and the
  // failed tab shows why.
  if (request) settleAddressFocus(threadId, request, failure ? '' : (state?.pageId ?? ''));
  return failure;
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
  for (const observer of observers) observer.resetForTest?.();
  browserActionCounts.clear();
  selectionSeqs.clear();
  paneIds.clear();
  stateOrder.reset();
  liveStates.reset();
  findCleanupErrors.reset();
  resetBrowserAddressFocusForTest();
  hydratedThreads.clear();
  store.suspend();
  store.resetAll();
}

/** Records the thread's pane mount as an attach would, for tests. */
export function setBrowserPaneMountForTest(threadId: string, mount: string): void {
  paneIds.set(threadId, mount);
}
