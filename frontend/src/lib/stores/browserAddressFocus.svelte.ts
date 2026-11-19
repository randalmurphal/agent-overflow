import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

// The browser pane's address bar focus, per thread: the focus a user asked
// for with a new tab (openBrowserTab arms it, its own answer settles it, the
// pane takes it once the tab is ready) and the mounted pane's address bar
// for the focus-address command. A leaf: it knows nothing of the companion
// store.

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
export function armAddressFocus(threadId: string): number {
  if (armedAddressFocus.size === 0 && typeof window !== 'undefined') {
    window.addEventListener('pointerdown', dropAllAddressFocus, true);
    window.addEventListener('keydown', dropAllAddressFocus, true);
  }
  const request = ++addressFocusRequests;
  armedAddressFocus.set(threadId, request);
  addressFocusIntents.set(threadId, PENDING_TAB);
  return request;
}

/**
 * Settles request number `request` with the `new` answer's tab, '' when it
 * named none. Only this request's own answer settles it: a newer new tab
 * replaced it, and this answer must neither resolve nor withdraw that one.
 */
export function settleAddressFocus(threadId: string, request: number, pageId: string): void {
  if (armedAddressFocus.get(threadId) !== request || addressFocusIntents.get(threadId) !== PENDING_TAB) return;
  if (pageId) addressFocusIntents.set(threadId, pageId);
  else dropAddressFocus(threadId);
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
  for (const threadId of armedAddressFocus.keys()) dropAddressFocus(threadId);
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

/** Drops every intent and registration; part of resetBrowserCompanionForTest. */
export function resetBrowserAddressFocusForTest(): void {
  dropAllAddressFocus();
  addressFocusIntents.reset();
  addressBars.clear();
}
