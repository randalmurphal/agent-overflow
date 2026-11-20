import { createKeyedSignalRegistry } from './keyedSignalRegistry.svelte';

// The browser pane's address bar focus, per thread: the focus a user asked
// for with a new tab (openBrowserTab and the chat header's show arm it, the
// answers of the presses that armed it settle it, the pane takes it once
// that tab is ready) and the mounted pane's address bar for the
// focus-address command. A leaf: it knows nothing of the companion store.

/**
 * A request for address bar focus, belonging to the result of the gesture
 * that made it. Each New Tab press makes its own, for the tab that press
 * opens, so a tab an earlier press opened becoming ready cannot take a
 * later press's focus. The chat header's Show presses converge: the
 * Manager shows one tab for overlapping shows and names it only in the
 * answer of the show that opened it, so a show made while another show's
 * answer is due joins its request, which belongs to the tabs their answers
 * name. `wanted` ends when the user moves on (any pointer press or key in
 * the window before the pane takes it, the next press included), the pane
 * takes it, or every answer came back naming no tab; the request is kept
 * while answers are due, so a show the user presses meanwhile joins it.
 */
interface FocusRequest {
  /** Answers still due: one per press that armed or joined it. */
  due: number;
  /** The tabs its answers opened. */
  readonly opened: string[];
  wanted: boolean;
  /** A header show's, which later shows join while it is due. */
  readonly converges: boolean;
}

/** A press's place in its request; settleAddressFocus takes it back. */
export interface AddressFocusTicket {
  readonly threadId: string;
  readonly request: FocusRequest;
}

/** What made a focus request: a New Tab press, or the chat header's show. */
export type AddressFocusGesture = 'new-tab' | 'show';

const requests = new Map<string, FocusRequest[]>();
// What each thread's wanted requests show the pane: the tabs they opened
// (empty while none is named yet), or null with none wanted.
const addressFocusIntents = createKeyedSignalRegistry<readonly string[] | null>(null);

/**
 * Arms a focus request for one press on the thread: a show joins the
 * request of a show whose answer is due, and anything else makes its own.
 */
export function armAddressFocus(threadId: string, gesture: AddressFocusGesture): AddressFocusTicket {
  const held = requests.get(threadId) ?? [];
  requests.set(threadId, held);
  let request = gesture === 'show' ? held.findLast((candidate) => candidate.converges && candidate.due > 0) : undefined;
  if (!request) {
    request = { due: 0, opened: [], wanted: false, converges: gesture === 'show' };
    held.push(request);
  }
  request.due += 1;
  request.wanted = true;
  publish(threadId);
  listenForMovingOn();
  return { threadId, request };
}

/**
 * Settles one press's ticket with the tab its answer opened, '' when it
 * named none. A request every answer of which named no tab is withdrawn.
 */
export function settleAddressFocus(ticket: AddressFocusTicket, pageId: string): void {
  const { threadId, request } = ticket;
  if (!requests.get(threadId)?.includes(request)) return;
  request.due -= 1;
  if (pageId && !request.opened.includes(pageId)) request.opened.push(pageId);
  if (request.wanted && request.due === 0 && request.opened.length === 0) request.wanted = false;
  publish(threadId);
}

/**
 * Shows the pane the thread's requests as they stand, and forgets each
 * once it is neither wanted nor due an answer.
 */
function publish(threadId: string): void {
  const held = (requests.get(threadId) ?? []).filter((request) => request.wanted || request.due > 0);
  if (held.length > 0) requests.set(threadId, held);
  else requests.delete(threadId);
  const wanted = held.filter((request) => request.wanted);
  addressFocusIntents.set(threadId, wanted.length > 0 ? wanted.flatMap((request) => request.opened) : null);
  if (![...requests.values()].some((list) => list.some((request) => request.wanted))) stopListeningForMovingOn();
}

function withdraw(threadId: string, request: FocusRequest): void {
  if (!request.wanted) return;
  request.wanted = false;
  publish(threadId);
}

function withdrawAll(): void {
  for (const [threadId, held] of [...requests]) {
    for (const request of held) request.wanted = false;
    publish(threadId);
  }
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
 * The tabs the thread's wanted requests opened, whose address bar should
 * take focus once ready: empty while none is named yet, null with no
 * request wanted. Reactive.
 */
export function addressFocusIntent(threadId: string): readonly string[] | null {
  return addressFocusIntents.get(threadId);
}

/**
 * The pane's answer for the tab it shows: true, once, when the shown tab is
 * one a wanted request's presses opened and is ready to take an address,
 * which takes that request. The tab failing withdraws its request; another
 * tab shown withdraws each request no answer is due for.
 */
export function takeAddressFocus(threadId: string, active: { id: string; creating?: boolean; error?: string } | null): boolean {
  const wanted = (requests.get(threadId) ?? []).filter((request) => request.wanted);
  if (wanted.length === 0 || !active) return false;
  const request = wanted.find((candidate) => candidate.opened.includes(active.id));
  if (!request) {
    for (const other of wanted) if (other.due === 0) withdraw(threadId, other);
    return false;
  }
  if (active.error) {
    withdraw(threadId, request);
    return false;
  }
  if (active.creating) return false;
  withdraw(threadId, request);
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
