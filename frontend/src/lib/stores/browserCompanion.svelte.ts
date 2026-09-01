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
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';
import {
  closeCompanion,
  companionForSource,
  getCompanionPane,
  openCompanion,
} from './companionPanes.svelte';
import { addPaneThreadMountedObserver, focusPane, getAllPanes, getFocusedPaneId, getPane } from './panes.svelte';
import { errString } from '../utils/errors';

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

function emptyView(state: BrowserCompanionEvent): BrowserCompanionView {
  return { state, error: '', actionError: '' };
}

const store = createEntityStore<BrowserCompanionView, void>({
  name: 'browserCompanion',
  rawValue: true,
  source: async ({ key, apply }) => {
    const result = await BrowserCompanionPaneAttach(key);
    paneIds.set(key, result.id);
    apply(emptyView(result.state));
    return async () => {
      if (paneIds.get(key) === result.id) paneIds.delete(key);
      await BrowserCompanionPaneDetach(result.id);
    };
  },
});

export function attachBrowserCompanion(threadId: string): EntityAttachment<BrowserCompanionView> {
  return store.attach(threadId, undefined);
}

/**
 * Reports where the mounted pane's host rect sits, already coalesced to one
 * call per changed frame by the pane. A refusal is a teardown race (the mount
 * or its connection just died), and the next attach re-reports — so it is
 * dropped rather than surfaced.
 */
export function reportBrowserPaneRect(
  threadId: string,
  rect: {
    x: number;
    y: number;
    width: number;
    height: number;
    clipX: number;
    clipY: number;
    clipWidth: number;
    clipHeight: number;
    viewportWidth: number;
    viewportHeight: number;
    visible: boolean;
    background: string;
  },
): void {
  const id = paneIds.get(threadId);
  if (!id) return;
  void BrowserCompanionPaneRect(id, new BrowserPaneRect(rect)).catch(() => {});
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
  if (event.kind !== 'state') return;
  hydratedThreads.add(event.threadId);
  if ((event.pages?.length ?? 0) > 0) liveStates.set(event.threadId, event);
  else liveStates.drop(event.threadId);
  const sourcePaneId = sourcePaneIdForThread(event.threadId);
  if (sourcePaneId) {
    const existing = companionForSource(sourcePaneId, 'browser');
    if ((event.pages?.length ?? 0) > 0 && event.visible === true) {
      if (!existing) openCompanion(sourcePaneId, 'browser');
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
 * A bound chord pressed while the thread's NATIVE page view held keyboard
 * focus (spec §7). The engine swallowed it and the backend routed it here;
 * replaying it as a window keydown puts it through the one dispatcher every
 * other keystroke uses, so `when` gates, user rebinds, and the palette all
 * behave exactly as if the SPA had been focused. The companion pane is
 * focused first: the click that gave the page focus moved no DOM focus, and
 * pane-scoped commands (pane.close → this pane's tab) need the logical focus
 * to be where the user is looking.
 */
function replayBrowserAccelerator(event: BrowserCompanionEvent): void {
  const chord = event.accelerator;
  if (!chord) return;
  const sourcePaneId = sourcePaneIdForThread(event.threadId);
  const companion = sourcePaneId ? companionForSource(sourcePaneId, 'browser') : null;
  if (companion) focusPane(companion.paneId);
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
}

// Reactive per-thread read behind the chat header's browser chip: null until
// the thread has live pages. Tracked per key, so a chip re-evaluates only
// when ITS thread's pages change.
export function browserCompanionState(threadId: string): BrowserCompanionEvent | null {
  return liveStates.get(threadId);
}

export function noteBrowserActionError(threadId: string, message: string): void {
  const previous = store.snapshot(threadId);
  if (previous && previous.actionError !== message) store.apply(threadId, { ...previous, actionError: message });
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
): Promise<string | null> {
  noteBrowserActionError(threadId, '');
  try {
    const state = await BrowserCompanionDo(
      threadId,
      new BrowserCompanionAction({
        kind: action.kind,
        pageId: action.pageId ?? '',
        address: action.address ?? '',
        index: action.index ?? 0,
      }),
    );
    applyBrowserCompanionState(state);
    return null;
  } catch (err) {
    const message = errString(err);
    noteBrowserActionError(threadId, message);
    return message;
  }
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
  const focusedId = getFocusedPaneId();
  const companion = focusedId ? getCompanionPane(focusedId) : null;
  if (companion?.kind !== 'browser') return false;
  const threadId = getPane(companion.sourcePaneId)?.threadId;
  const state = threadId ? liveStates.get(threadId) : null;
  const pageId = state?.activePageId || state?.pages?.[0]?.id;
  if (!threadId || !pageId) return false;
  void browserCompanionAct(threadId, { kind: 'close', pageId });
  return true;
}

// The `browser:companion-state` channel is ephemeral (no replay), so a
// freshly loaded UI cannot know a thread already has live pages. One
// backend read per thread fills that in; every later change arrives as a
// push. A push that lands first — or while the read is in flight — is
// newer and wins: it marks the thread hydrated and the read's result is
// dropped.
export function hydrateBrowserCompanionState(threadId: string): void {
  if (!threadId || hydratedThreads.has(threadId)) return;
  hydratedThreads.add(threadId);
  void (async () => {
    // The async wrapper folds a SYNCHRONOUS throw from the binding into the
    // same failure path as a rejection: callers run this from $effect bodies,
    // where an escaping throw tears down the component's effects.
    try {
      const state = await BrowserCompanionThreadState(threadId);
      if (liveStates.get(threadId) === null) applyBrowserCompanionState(state);
    } catch {
      // A failed read (backend restarting, thread gone) must not pin the
      // thread as hydrated forever; the next mount retries.
      hydratedThreads.delete(threadId);
    }
  })();
}

export function reconcileBrowserCompanionForPane(paneId: string, threadId: string): void {
  hydrateBrowserCompanionState(threadId);
  if (
    liveStates.get(threadId)?.visible === true &&
    (liveStates.get(threadId)?.pages?.length ?? 0) > 0 &&
    !companionForSource(paneId, 'browser')
  ) {
    openCompanion(paneId, 'browser');
  }
}

addPaneThreadMountedObserver(reconcileBrowserCompanionForPane);

export function resetBrowserCompanionForTest(): void {
  paneIds.clear();
  liveStates.reset();
  hydratedThreads.clear();
  store.suspend();
  store.resetAll();
}
