import { SvelteMap } from 'svelte/reactivity';
import { resolveThreadBackend } from '../transport/entityIndex';
import { HOME_BACKEND } from '../transport/backendKey';
import { requireEntityBackend } from '../transport/backends';
import { isFrontendOnly } from '../transport/runMode';
import { isHostWindow } from '../transport/pageHost';
import {
  BrowserCompanionPaneAttach,
  BrowserCompanionPaneDetach,
  BrowserCompanionThreadState,
  type BrowserCompanionEvent,
} from './bindings';
import { applyBrowserFindEvent } from './browserFind.svelte';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import { closeCompanion, companionForSource, openCompanion } from './companionPanes.svelte';
import { addPaneThreadMountedObserver, focusPane, getAllPanes } from './panes.svelte';
import { hasScope } from '../transport/scopes';
import { isCompactLayout } from './layoutMode.svelte';
import { createSequenceGate } from './snapshotStream';
import { getTransportHelloFor } from './transportStatus.svelte';
import { resetBrowserAddressFocusForTest } from './browserAddressFocus.svelte';
import { getSettings } from './settings.svelte';
import { addErrorToast } from './toast.svelte';

// The browser pane's state store. The pane surface itself is an empty host
// rect the platform positions a NATIVE browser view over (spec
// docs/specs/embedded-browser.md §7): no pixels cross this store, only the
// thread's page/session state. It owns the pane attachment and its mount,
// the state events (ordered by their sequence), the chrome action banner,
// and the capability gates. The parts that keep their own state beside it
// import it and follow it through observeBrowserCompanion: the user's
// actions (browserActions.ts), the host rect reports
// (browserPaneRectReports.ts) and the page keyboard focus intent
// (browserPageFocus.ts). Address bar focus is a leaf it imports
// (browserAddressFocus.svelte.ts).

export interface BrowserCompanionView {
  state: BrowserCompanionEvent;
  /** The pane failed as a whole (attach, backend); replaces its body. */
  error: string;
  /** The last chrome action was refused; the pane's banner. */
  actionError: string;
}

// paneIds maps a thread to its live pane mount, acquired by the entity
// store's source and addressed by every rect report. Reactive, so the pane's
// geometry sends its first rect when the mount appears, which can be after
// its state did (a push during the attach).
const paneIds = new SvelteMap<string, string>();

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
// The threads whose state this connection has seen, by a push or a read:
// for one of them, no live state means no tab. A connection edge forgets
// them all (refreshBrowserCompanionStates). The reads in flight, one per
// thread.
const knownThreads = new Set<string>();
const stateReads = new Map<string, Promise<void>>();

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
  return nativeBrowserRefusal(threadId) === null;
}

/**
 * Why this page cannot host the thread's native browser pane, or null when
 * it can (canUseNativeBrowser). Ownership two computers claim is not the
 * home backend's (resolveThreadBackend), whichever claim arrived first.
 * An action asks this again when it is sent, after any wait, so an owner
 * that moved or became contested meanwhile is refused here.
 */
export function nativeBrowserRefusal(threadId: string): Error | null {
  if (!hasScope('host') || !isHostWindow() || isFrontendOnly()) {
    return new Error('A native browser pane is unavailable for this conversation on this device.');
  }
  try {
    if (requireEntityBackend(resolveThreadBackend(threadId)) === HOME_BACKEND) return null;
    return new Error("This conversation is on another computer, whose browser this window cannot show.");
  } catch (err) {
    return err instanceof Error ? err : new Error(String(err));
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
 * Whether a gesture here can open a tab of the thread's browser and show
 * it: the browser can be opened (canOpenNativeBrowser) and the layout shows
 * a browser pane, which compact never does (openBrowserCompanion). The
 * header's open button and a mod-clicked link ask this, so neither makes a
 * tab nobody sees; a link falls back to the ordinary opener.
 */
export function canPresentNativeBrowser(threadId: string): boolean {
  return !isCompactLayout() && canOpenNativeBrowser(threadId);
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
  source: async ({ key, apply, signal }) => {
    const refusal = nativeBrowserRefusal(key);
    if (refusal) throw refusal;
    const result = await BrowserCompanionPaneAttach(key);
    // An attach superseded while it was in flight (the pane closed, or
    // closed and opened again) owns nothing here: the mount the thread has
    // now is a later attach's, so this one only detaches its own.
    if (signal.aborted) return () => BrowserCompanionPaneDetach(result.id);
    paneIds.set(key, result.id);
    // The answer is a state like a push and lands as one, so the keyboard
    // commands (liveStates), the companion's open and close and this pane
    // all see the same revision. A newer state that already landed (a push
    // during the attach) wins; the pane then shows the newest it has.
    applyBrowserCompanionState(result.state);
    if (!store.snapshot(key)) apply(emptyView(liveStates.get(key) ?? result.state));
    return async () => {
      if (paneIds.get(key) === result.id) paneIds.delete(key);
      for (const observer of observers) observer.mountEnded?.(result.id);
      await BrowserCompanionPaneDetach(result.id);
    };
  },
});

export function attachBrowserCompanion(threadId: string): EntityAttachment<BrowserCompanionView> {
  return store.attach(threadId, undefined);
}

/** The backend's id of the thread's mounted pane, null while it has none. Reactive. */
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
  knownThreads.add(event.threadId);
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

// The `browser:companion-state` channel is ephemeral (no replay), so a
// freshly loaded UI cannot know a thread already has live pages, and a
// reconnected one cannot know what changed while it was away. One backend
// read per thread per connection fills that in; every later change arrives
// as a push. A push newer than the read's answer, whenever it landed, wins
// by its number (`admitState`). Rejects when the read fails; the header
// button (toggleBrowserFromHeader) reports that.
export function readBrowserCompanionState(threadId: string): Promise<void> {
  // The read rides `host`, and it runs on every pane thread mount, so a
  // session off the host would spend one refusal per thread open on a
  // surface it cannot use anyway: the pane is a NATIVE view this machine
  // paints, which no remote client renders, and no grant can ever open
  // it. The scope read comes BEFORE the thread is marked known: the
  // caller is an $effect, host presence lands with the bootstrap manifest
  // after the first pane mounts, and pinning the thread on the placeholder
  // answer would leave it unread for the life of the page.
  if (knownThreads.has(threadId) || !canUseNativeBrowser(threadId)) return Promise.resolve();
  const pending = stateReads.get(threadId);
  if (pending) return pending;
  // The async wrapper folds a SYNCHRONOUS throw from the binding into the
  // same failure path as a rejection.
  const read = (async () => {
    applyBrowserCompanionState(await BrowserCompanionThreadState(threadId));
    // Not admitted, the answer is older than a state already applied.
    knownThreads.add(threadId);
  })().finally(() => {
    // A failed read (backend restarting, thread gone) leaves the thread
    // unknown; the next mount or click reads again.
    if (stateReads.get(threadId) === read) stateReads.delete(threadId);
  });
  stateReads.set(threadId, read);
  return read;
}

/** Reads the thread's state unless this connection has seen it; never throws. */
export function hydrateBrowserCompanionState(threadId: string): void {
  if (!threadId) return;
  // Callers run this from $effect bodies, where an escaping throw tears
  // down the component's effects; a failure is retried by the next caller.
  readBrowserCompanionState(threadId).catch(() => {});
}

/**
 * A connection edge of the home backend: pushes made while this window was
 * away are lost, so no thread's state is known any more. The threads with a
 * pane are read again now (the chat header's button reads their state);
 * any other is read when a pane shows it.
 */
export function refreshBrowserCompanionStates(): void {
  knownThreads.clear();
  stateReads.clear();
  for (const [, pane] of getAllPanes()) {
    if (pane.threadId) hydrateBrowserCompanionState(pane.threadId);
  }
}

export function reconcileBrowserCompanionForPane(paneId: string, threadId: string): void {
  hydrateBrowserCompanionState(threadId);
  const state = liveStates.get(threadId);
  if (state?.visible === true && (state.pages?.length ?? 0) > 0) openBrowserCompanion(paneId, threadId);
}

addPaneThreadMountedObserver(reconcileBrowserCompanionForPane);

export function resetBrowserCompanionForTest(): void {
  for (const observer of observers) observer.resetForTest?.();
  paneIds.clear();
  stateOrder.reset();
  liveStates.reset();
  findCleanupErrors.reset();
  resetBrowserAddressFocusForTest();
  knownThreads.clear();
  stateReads.clear();
  store.suspend();
  store.resetAll();
}

/** Records the thread's pane mount as an attach would, for tests. */
export function setBrowserPaneMountForTest(threadId: string, mount: string): void {
  paneIds.set(threadId, mount);
}
