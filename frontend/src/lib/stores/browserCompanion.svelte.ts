import { untrack } from 'svelte';
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
// thread's page/session state. It keeps two things with two lifetimes:
//
// - The thread's accepted snapshot: the newest state admitted for it, which
//   the chat header, the tab strip and the keyboard commands all read. A
//   thread has one whether or not any pane is mounted.
// - The pane's mount: the attachment a mounted pane holds (the entity
//   store), which owns the mount id, the pane's failure and the detach.
//
// Every state, pushed or answered, lands through one admission
// (`admitBrowserState`). The parts that keep their own state beside this
// store follow it through observeBrowserCompanion: the user's actions
// (browserActions.ts), the host rect reports (browserPaneRectReports.ts) and
// the page keyboard focus intent (browserPageFocus.ts). Address bar focus
// is a leaf it imports (browserAddressFocus.svelte.ts).

/** The backend's id of one pane mount, minted from BrowserCompanionPaneAttach's answer. */
export type BrowserMountId = string & { readonly __brand: 'BrowserMountId' };

function mintMountId(id: string): BrowserMountId {
  return id as BrowserMountId;
}

/** A mounted pane's attachment value: the backend's id of its mount. */
export interface BrowserPaneMount {
  /** Null once the mount detached while the attachment re-acquires one. */
  readonly mount: BrowserMountId | null;
}

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
  mountEnded?(mount: BrowserMountId): void;
  /** resetBrowserCompanionForTest: drop all state, telling no one. */
  resetForTest?(): void;
}

const observers: BrowserCompanionObserver[] = [];

/** Registers a part's observer; parts register once, at module setup. */
export function observeBrowserCompanion(observer: BrowserCompanionObserver): void {
  observers.push(observer);
}

// The home connection's generation. The state channel is ephemeral (no
// replay), so a connection edge loses whatever was pushed while the window
// was away: an edge starts a new generation, a snapshot is known only in the
// generation it was admitted in, and an answer to a call started in an older
// one is not admitted.
let connectionGeneration = 0;

interface AcceptedState {
  readonly state: Readonly<BrowserCompanionEvent>;
  readonly generation: number;
}

// The accepted snapshot per thread, reactive per key so a chip re-evaluates
// only when ITS thread's state changes. A known empty snapshot (no pages) is
// data: the thread has no tab. The reads in flight, one per thread.
const snapshots = createKeyedSignalRegistry<AcceptedState | null>(null);
const stateReads = new Map<string, Promise<void>>();

// The transport can write a push published after a call returned ahead of
// its reply. The backend numbers every snapshot, so the newest by that
// number wins within a backend launch, whatever the arrival order.
const stateOrder = createSequenceGate();

/** The home connection's generation; an answer is admitted only in the one its call started in. */
export function browserStateGeneration(): number {
  return connectionGeneration;
}

/**
 * What this connection knows of the thread's state. A known snapshot with
 * no pages is a thread with no tab.
 */
export type BrowserThreadSnapshot =
  | { readonly status: 'unknown' }
  | { readonly status: 'known'; readonly snapshot: Readonly<BrowserCompanionEvent> };

/**
 * The thread's snapshot if this connection has admitted one, else unknown.
 * Not reactive: it answers a decision taken now (a press, a read).
 */
export function browserThreadSnapshot(threadId: string): BrowserThreadSnapshot {
  const accepted = untrack(() => snapshots.get(threadId));
  if (!accepted || accepted.generation !== connectionGeneration) return { status: 'unknown' };
  return { status: 'known', snapshot: accepted.state };
}

/**
 * The thread's last admitted state, known or not (reactive), for what the
 * pane and its chrome show: the newest state there is beats a blank while a
 * reconnect reads it again.
 */
export function browserThreadState(threadId: string): Readonly<BrowserCompanionEvent> | null {
  return snapshots.get(threadId)?.state ?? null;
}

/** The thread's last admitted state while it has pages, else null (reactive). */
export function browserCompanionState(threadId: string): Readonly<BrowserCompanionEvent> | null {
  const state = browserThreadState(threadId);
  return (state?.pages?.length ?? 0) > 0 ? state : null;
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
 * The only opener of a browser companion. The state admission and the
 * reconcile on a thread mount or a layout change call it, and through the
 * admission so do the header's show, a mod-clicked link and startup
 * hydration. Browser companions are never persisted or stashed, so no
 * layout restore reopens one. It opens nothing in a page that cannot host
 * the native view (canUseNativeBrowser), which records the state as a
 * connected browser does, and nothing under compact: opening ends in
 * revealPane, which would pull a phone off its thread list for a native
 * view the compact screen never shows (docs/specs/remote-access.md,
 * "Hidden on the phone").
 */
function openBrowserCompanion(sourcePaneId: string, threadId: string): void {
  if (isCompactLayout() || !canUseNativeBrowser(threadId)) return;
  if (companionForSource(sourcePaneId, 'browser')) return;
  openCompanion(sourcePaneId, 'browser');
}

const store = createEntityStore<BrowserPaneMount, void>({
  name: 'browserCompanion',
  // Native views and their mounts belong to this window's local execution host.
  backendForKey: () => HOME_BACKEND,
  rawValue: true,
  source: async ({ key, apply, signal }) => {
    const refusal = nativeBrowserRefusal(key);
    if (refusal) throw refusal;
    const generation = connectionGeneration;
    const result = await BrowserCompanionPaneAttach(key);
    const mount = mintMountId(result.id);
    // An attach superseded while it was in flight (the pane closed, or
    // closed and opened again) owns nothing here: the mount the thread has
    // now is a later attach's, so this one only detaches its own.
    if (signal.aborted) return () => BrowserCompanionPaneDetach(result.id);
    // The answer's state is admitted like any other; a newer one that
    // already landed (a push during the attach) wins.
    admitBrowserState(result.state, generation);
    apply({ mount });
    return async () => {
      // The entity store keeps the value through a re-acquire; a detached
      // mount is no longer one to address.
      if (store.snapshot(key)?.mount === mount) store.apply(key, { mount: null }, { preserveError: true });
      actionErrors.drop(key);
      for (const observer of observers) observer.mountEnded?.(mount);
      await BrowserCompanionPaneDetach(result.id);
    };
  },
});

export function attachBrowserCompanion(threadId: string): EntityAttachment<BrowserPaneMount> {
  return store.attach(threadId, undefined);
}

/** The backend's id of the thread's mounted pane, null while it has none. Reactive. */
export function browserPaneMountId(threadId: string): BrowserMountId | null {
  return store.peek(threadId)?.mount ?? null;
}

/** The thread whose pane holds `mount`, null for none. */
export function threadForBrowserPaneMount(mount: string): string | null {
  for (const threadId of store.keys()) {
    if (store.snapshot(threadId)?.mount === mount) return threadId;
  }
  return null;
}

function sourcePaneIdForThread(threadId: string): string | null {
  for (const [paneId, pane] of getAllPanes()) {
    if (pane.threadId === threadId) return paneId;
  }
  return null;
}

/** Whether a pane here shows the thread, so its browser has a pane to open beside. */
export function browserSourcePaneShown(threadId: string): boolean {
  return sourcePaneIdForThread(threadId) !== null;
}

/**
 * A pushed event (`browser:companion-state`). A state is admitted in the
 * current connection generation; every other kind is dispatched to its
 * owner.
 */
export function applyBrowserCompanionState(event: BrowserCompanionEvent): void {
  if (!event?.threadId) return;
  if (event.kind === 'error') {
    store.applyError(event.threadId, new Error(event.error || 'Browser pane failed'));
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
  admitBrowserState(event, connectionGeneration);
}

/**
 * The one admission of a thread's state, for a push and for the answer of
 * every call (attach, read, action, page focus), with the connection
 * generation the call started in. An answer from an older generation, or a
 * state older than one already admitted, changes nothing.
 */
export function admitBrowserState(event: BrowserCompanionEvent, generation: number): void {
  if (event?.kind !== 'state' || !event.threadId) return;
  if (generation !== connectionGeneration) return;
  if (!stateOrder.admit(event.threadId, event.seq, getTransportHelloFor(HOME_BACKEND)?.launchId ?? '')) {
    // The held state is at least as new as this answer of the current
    // generation, so it is known in this generation too.
    const held = untrack(() => snapshots.get(event.threadId));
    if (held && held.generation !== generation) snapshots.set(event.threadId, { state: held.state, generation });
    return;
  }
  for (const observer of observers) observer.stateAdmitted?.(event);
  snapshots.set(event.threadId, { state: event, generation });
  presentBrowserState(event.threadId, event);
  // A state is evidence the mounted pane works again: it replaces a failure
  // the backend reported for the pane.
  const mounted = store.snapshot(event.threadId);
  if (mounted && untrack(() => store.peekError(event.threadId))) store.apply(event.threadId, { ...mounted });
}

/**
 * Opens or closes the thread's companion to match its state. Closing runs on
 * every client: a companion opened on the desktop survives a resize into
 * compact, and its last page closing still has to take it down.
 */
function presentBrowserState(threadId: string, state: BrowserCompanionEvent): void {
  const sourcePaneId = sourcePaneIdForThread(threadId);
  if (!sourcePaneId) return;
  if ((state.pages?.length ?? 0) > 0 && state.visible === true) {
    openBrowserCompanion(sourcePaneId, threadId);
    return;
  }
  const existing = companionForSource(sourcePaneId, 'browser');
  if (existing) closeCompanion(existing.paneId);
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

/**
 * Whether the thread's browser pane is in this layout (reactive): what the
 * chat header's button shows as pressed and what its press decides from.
 */
export function browserPaneShown(threadId: string): boolean {
  const sourcePaneId = sourcePaneIdForThread(threadId);
  return sourcePaneId !== null && companionForSource(sourcePaneId, 'browser') !== null;
}

/** Whether the thread's browser pane is open here, with its banner. */
export function browserPaneOpen(threadId: string): boolean {
  return browserPaneShown(threadId) && !!store.snapshot(threadId);
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

// The mounted pane's banner: the last refused chrome action. It lives as
// long as the pane's mount; with no pane mounted there is no banner to write.
const actionErrors = createKeyedSignalRegistry<string>('');

export function noteBrowserActionError(threadId: string, message: string): void {
  if (!store.snapshot(threadId)) return;
  if (message) actionErrors.set(threadId, message);
  else actionErrors.drop(threadId);
}

/** The thread's banner text, '' for none. Reactive. */
export function browserActionError(threadId: string): string {
  return actionErrors.get(threadId);
}

// The `browser:companion-state` channel is ephemeral (no replay), so a
// freshly loaded UI cannot know a thread already has live pages, and a
// reconnected one cannot know what changed while it was away. One backend
// read per thread per connection generation fills that in; every later
// change arrives as a push. Resolves once the thread's snapshot is known,
// reading again when a connection edge passed during a read; rejects when
// the read fails. Resolves at once, the snapshot left unknown, for a page
// that cannot host the thread's browser.
export function readBrowserCompanionState(threadId: string): Promise<void> {
  // The read rides `host`, and it runs on every pane thread mount, so a
  // session off the host would spend one refusal per thread open on a
  // surface it cannot use anyway: the pane is a NATIVE view this machine
  // paints, which no remote client renders, and no grant can ever open it.
  if (!canUseNativeBrowser(threadId) || browserThreadSnapshot(threadId).status === 'known') return Promise.resolve();
  // The read in flight may end in a generation that has since passed; the
  // caller is answered for the current one.
  const pending = stateReads.get(threadId);
  if (pending) return pending.then(() => readBrowserCompanionState(threadId));
  // The async wrapper folds a SYNCHRONOUS throw from the binding into the
  // same failure path as a rejection.
  const read = (async () => {
    while (browserThreadSnapshot(threadId).status === 'unknown') {
      const generation = connectionGeneration;
      const state = await BrowserCompanionThreadState(threadId);
      // Not admitted when older than a state already admitted, which then
      // is the known one; or when an edge passed, and then read again.
      admitBrowserState(state, generation);
      if (generation === connectionGeneration && browserThreadSnapshot(threadId).status === 'unknown') {
        throw new Error('The browser answered with no state for this conversation.');
      }
    }
  })().finally(() => {
    // A failed read (backend restarting, thread gone) leaves the thread
    // unknown; the next mount or click reads again.
    if (stateReads.get(threadId) === read) stateReads.delete(threadId);
  });
  stateReads.set(threadId, read);
  return read;
}

/** Reads the thread's state unless this connection knows it; never throws. */
export function hydrateBrowserCompanionState(threadId: string): void {
  if (!threadId) return;
  // Callers run this from $effect bodies, where an escaping throw tears
  // down the component's effects; a failure is retried by the next caller.
  readBrowserCompanionState(threadId).catch(() => {});
}

/**
 * A connection edge of the home backend: pushes made while this window was
 * away are lost, so a new generation begins and no thread's state is known
 * any more. A read in flight reads again in the new generation. The threads
 * with a pane are read now (the chat header's button reads their state);
 * any other is read when a pane shows it.
 */
export function refreshBrowserCompanionStates(): void {
  connectionGeneration += 1;
  for (const [, pane] of getAllPanes()) {
    if (pane.threadId) hydrateBrowserCompanionState(pane.threadId);
  }
}

/**
 * Whether the thread's state asks for a pane this window can present now:
 * it is visible with tabs, the layout shows browser panes and this page can
 * host the thread's browser. Reactive in the layout and the state; a pane
 * showing the thread follows it (ChatHeaderActions) and reconciles when it
 * turns true, since the push that recorded the state is not sent again.
 */
export function browserPresentationWanted(threadId: string): boolean {
  if (isCompactLayout() || !canUseNativeBrowser(threadId)) return false;
  return browserCompanionState(threadId)?.visible === true;
}

/**
 * Presents the thread's admitted state in `paneId` when this window can:
 * on a thread mount, and when browserPresentationWanted turns true.
 */
export function reconcileBrowserCompanionForPane(paneId: string, threadId: string): void {
  hydrateBrowserCompanionState(threadId);
  if (browserPresentationWanted(threadId)) openBrowserCompanion(paneId, threadId);
}

addPaneThreadMountedObserver(reconcileBrowserCompanionForPane);

export function resetBrowserCompanionForTest(): void {
  for (const observer of observers) observer.resetForTest?.();
  stateOrder.reset();
  snapshots.reset();
  findCleanupErrors.reset();
  actionErrors.reset();
  resetBrowserAddressFocusForTest();
  stateReads.clear();
  store.suspend();
  store.resetAll();
}
