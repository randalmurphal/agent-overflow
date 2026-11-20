import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

// The browser pane's address bar focus, per thread: the focus a user asked
// for with a new tab (openBrowserTab and the chat header's show arm it, the
// answers of the presses that armed it settle it, the pane takes it once
// the tab is ready) and the mounted pane's address bar for the
// focus-address command. A leaf: it knows nothing of the companion store.

/**
 * One thread's request for address bar focus. Presses made while an answer
 * of an earlier one is due join it: the Manager converges overlapping shows
 * on one tab and names it only in the answer of the show that opened it,
 * so the request belongs to the tabs the presses opened, whichever answer
 * names them. `wanted` ends when the user moves on (any pointer press or
 * key in the window before the pane takes it), the pane takes it, or every
 * answer came back naming no tab; the request is kept while answers are
 * due, so a press the user makes meanwhile joins them.
 */
interface FocusRequest {
  /** Answers still due: one per press that armed or joined it. */
  due: number;
  /** The tabs its answers opened. */
  readonly opened: string[];
  wanted: boolean;
}

/** A press's place in its thread's request; settleAddressFocus takes it back. */
export interface AddressFocusTicket {
  readonly threadId: string;
  readonly request: FocusRequest;
}

const requests = new Map<string, FocusRequest>();
// What each thread's wanted request shows the pane: the tabs it opened
// (empty while none is named yet), or null with none wanted.
const addressFocusIntents = createKeyedSignalRegistry<readonly string[] | null>(null);

/** Arms a focus request for the thread, or joins the one whose answers are due, for one press. */
export function armAddressFocus(threadId: string): AddressFocusTicket {
  let request = requests.get(threadId);
  if (!request) {
    request = { due: 0, opened: [], wanted: false };
    requests.set(threadId, request);
  }
  request.due += 1;
  request.wanted = true;
  publish(threadId, request);
  listenForMovingOn();
  return { threadId, request };
}

/**
 * Settles one press's ticket with the tab its answer opened, '' when it
 * named none. A request every answer of which named no tab is withdrawn.
 */
export function settleAddressFocus(ticket: AddressFocusTicket, pageId: string): void {
  const { threadId, request } = ticket;
  if (requests.get(threadId) !== request) return;
  request.due -= 1;
  if (pageId && !request.opened.includes(pageId)) request.opened.push(pageId);
  if (request.wanted && request.due === 0 && request.opened.length === 0) request.wanted = false;
  publish(threadId, request);
}

/** Shows the pane `request` as it stands, and forgets it once it is neither wanted nor due an answer. */
function publish(threadId: string, request: FocusRequest): void {
  addressFocusIntents.set(threadId, request.wanted ? [...request.opened] : null);
  if (!request.wanted && request.due === 0) requests.delete(threadId);
  if (![...requests.values()].some((held) => held.wanted)) stopListeningForMovingOn();
}

function withdraw(threadId: string): void {
  const request = requests.get(threadId);
  if (!request?.wanted) return;
  request.wanted = false;
  publish(threadId, request);
}

function withdrawAll(): void {
  // A Map visits every entry still present, so a withdrawn request deleting
  // itself skips nothing.
  for (const threadId of requests.keys()) withdraw(threadId);
}

let listening = false;

function listenForMovingOn(): void {
  if (listening || typeof window === 'undefined') return;
  listening = true;
  window.addEventListener('pointerdown', withdrawAll, true);
  window.addEventListener('keydown', withdrawAll, true);
}

function stopListeningForMovingOn(): void {
  if (!listening || typeof window === 'undefined') return;
  listening = false;
  window.removeEventListener('pointerdown', withdrawAll, true);
  window.removeEventListener('keydown', withdrawAll, true);
}

/**
 * The tabs a wanted request's answers opened, whose address bar should take
 * focus once ready: empty while none is named yet, null with no request
 * wanted. Reactive.
 */
export function addressFocusIntent(threadId: string): readonly string[] | null {
  return addressFocusIntents.get(threadId);
}

/**
 * The pane's answer to a request for the tab it shows: true, once, when the
 * shown tab is one the request's presses opened and is ready to take an
 * address. The tab failing withdraws the request, and so does another tab
 * shown once no answer is due.
 */
export function takeAddressFocus(threadId: string, active: { id: string; creating?: boolean; error?: string } | null): boolean {
  const request = requests.get(threadId);
  if (!request?.wanted || !active) return false;
  if (!request.opened.includes(active.id)) {
    if (request.due === 0) withdraw(threadId);
    return false;
  }
  if (active.error) {
    withdraw(threadId);
    return false;
  }
  if (active.creating) return false;
  withdraw(threadId);
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

/** Drops every request and registration; part of resetBrowserCompanionForTest. */
export function resetBrowserAddressFocusForTest(): void {
  requests.clear();
  stopListeningForMovingOn();
  addressFocusIntents.reset();
  addressBars.clear();
}
