// pageHost tells the SPA which shell is hosting this document, and — for
// the shells that own the window — receives the one-time page ticket
// that shell injects instead of writing it into the URL.
//
// A browser can only be told things through the URL it was opened with,
// so a browser's ticket rides `?t=` and bootstrap.ts reads it there. A
// webview window is different: the Go process that mints the ticket also
// owns the window and can evaluate script in the document it just
// loaded. Keeping the credential off that URL is the point — a URL is
// copyable, lands in launcher logs and window diagnostics, and outlives
// its single use in shell history and error reports.
//
// The Go half is internal/pagehost (the marker, the two names below and
// the injected script) and internal/uiwindow.DeliverPageTicket (the
// wiring, in all three window hosts).

// The page-URL marker, mirroring pagehost.Param / pagehost.Webview. Not
// a credential: it says "your ticket is arriving by injection, wait for
// it" and grants nothing. Read once at module load for the reason
// runMode.ts reads `?mode=` once — a different host means a different
// process boot, and the answer must be available synchronously, before
// any fetch resolves. It survives the ticket scrub, which removes only
// the ticket parameter.
const HOST_PARAM = 'host';
const HOST_WEBVIEW = 'webview';

// What the injected script writes, mirroring pagehost.TicketGlobal /
// pagehost.TicketEvent. Two names rather than one because the page and
// the injection race in both directions: the global answers an injection
// that landed before this module ran, the event answers one that lands
// after. Delivering both makes the order irrelevant, and a re-delivery
// is a re-assignment plus an event nobody is listening for.
const TICKET_GLOBAL = '__aoPageTicket';
const TICKET_EVENT = 'ao:page-ticket';

// The message a document sends its Wails host to announce itself.
//
// It is load-bearing rather than cosmetic: WebviewWindow.ExecJS QUEUES
// every script until a document has announced, and this app replaces
// @wailsio/runtime with its own transport shim, so nothing announces
// unless this does. Announcing also RE-triggers delivery, which is what
// the retry cadence below uses to recover a document that finished
// loading before its host subscribed.
const HOST_READY_MESSAGE = 'wails:runtime:ready';

// The event Wails dispatches once it has installed its host bridge on a
// freshly loaded document. Listened for only when the bridge is not
// there yet, so nothing is armed on a page that has no host at all.
const HOST_BRIDGE_READY_EVENT = 'wails:runtime-config-ready';

// How long a webview-hosted page waits for its ticket before reporting
// the delivery failed, and how often it re-announces while waiting.
//
// The normal case resolves in about a millisecond — an announcement, a
// hop through the host's event loop, and a script evaluation — so the
// cadence only ever runs when something is wrong. The deadline is the
// difference between an actionable bootstrap failure and a blank window
// forever; it is long enough that a cold boot competing for the host's
// main thread is not convicted.
const TICKET_RETRY_MS = 300;
const TICKET_TIMEOUT_MS = 10_000;

interface HostBridge {
  invoke?: (message: string) => void;
}

interface HostWindow {
  _wails?: HostBridge;
  chrome?: { webview?: { postMessage?: (message: string) => void } };
  webkit?: { messageHandlers?: { external?: { postMessage?: (message: string) => void } } };
  [TICKET_GLOBAL]?: unknown;
}

function hostWindow(): (Window & HostWindow) | null {
  if (typeof window === 'undefined') return null;
  return window as Window & HostWindow;
}

let cachedWebviewHosted: boolean | null = null;

// isWebviewHosted reports whether this document's shell owns the window
// and will inject the page ticket. Memoised: the value is fixed for the
// document's lifetime.
export function isWebviewHosted(): boolean {
  if (cachedWebviewHosted === null) {
    const w = hostWindow();
    const search = w?.location?.search ?? '';
    cachedWebviewHosted = new URLSearchParams(search).get(HOST_PARAM) === HOST_WEBVIEW;
  }
  return cachedWebviewHosted;
}

// readInjectedPageTicket returns the ticket the host has delivered, or ''
// when none has arrived. Exported for the wait below and for tests; a
// non-string global (a page that ran something odd, a partial write) is
// no ticket rather than an error.
export function readInjectedPageTicket(): string {
  const value = hostWindow()?.[TICKET_GLOBAL];
  return typeof value === 'string' ? value : '';
}

// clearInjectedPageTicket forgets a delivered ticket so the next wait
// asks for a fresh one. The single caller is a REFUSED exchange: a
// spent ticket is normally harmless to re-present (the cookie it bought
// authenticates first), but once the backend has refused this page's
// credential the stale global is the one thing standing between the
// retry ladder and a live ticket.
export function clearInjectedPageTicket(): void {
  const w = hostWindow();
  if (w) delete w[TICKET_GLOBAL];
}

// PageTicketUndeliveredError marks the bounded wait giving up. It is a
// TRANSIENT bootstrap failure, not a refused credential: the reconnect
// ladder retries, each attempt re-announces, and the host answers an
// announcement with a fresh ticket — so a delivery lost to a subscription
// race heals on the next lap instead of latching the page dead.
export class PageTicketUndeliveredError extends Error {
  constructor(timeoutMs: number) {
    super(`page ticket not delivered by the window host within ${timeoutMs}ms`);
    this.name = 'PageTicketUndeliveredError';
  }
}

let awaitingHostBridge = false;

// announceToHost tells the hosting window this document is live and can
// receive injected script, which is also the request for a ticket.
//
// Three carriers, narrowest first. Wails installs `_wails.invoke` on
// every document it loads, but only once that document has finished
// loading; the two platform bridges underneath it exist from document
// creation, so reaching them directly is what lets the connection start
// before `load` rather than after. When none of the three is there yet,
// arm the bridge-ready event once and announce when it fires.
function announceToHost(): void {
  const w = hostWindow();
  if (!w) return;
  const bridge = w._wails;
  if (bridge && typeof bridge.invoke === 'function') {
    bridge.invoke(HOST_READY_MESSAGE);
    return;
  }
  const post = w.chrome?.webview?.postMessage ?? w.webkit?.messageHandlers?.external?.postMessage;
  if (typeof post === 'function') {
    post(HOST_READY_MESSAGE);
    return;
  }
  if (awaitingHostBridge) return;
  awaitingHostBridge = true;
  w.addEventListener(
    HOST_BRIDGE_READY_EVENT,
    () => {
      awaitingHostBridge = false;
      announceToHost();
    },
    { once: true },
  );
}

// awaitInjectedPageTicket resolves with the ticket this document's host
// injected, announcing itself until one arrives.
//
// Race-proof in both directions and idempotent under re-delivery: an
// injection that already landed is read straight off the global, and one
// that lands later arrives as the event. Rejects with
// PageTicketUndeliveredError at the deadline rather than hanging.
export function awaitInjectedPageTicket(
  timeoutMs: number = TICKET_TIMEOUT_MS,
): Promise<string> {
  const immediate = readInjectedPageTicket();
  if (immediate !== '') return Promise.resolve(immediate);
  const w = hostWindow();
  if (!w) return Promise.reject(new PageTicketUndeliveredError(timeoutMs));

  return new Promise<string>((resolve, reject) => {
    let retry: ReturnType<typeof setInterval> | undefined;
    let deadline: ReturnType<typeof setTimeout> | undefined;
    const stop = () => {
      w.removeEventListener(TICKET_EVENT, onDelivered);
      if (retry !== undefined) clearInterval(retry);
      if (deadline !== undefined) clearTimeout(deadline);
    };
    function onDelivered(): void {
      const ticket = readInjectedPageTicket();
      // A dispatch with nothing behind it is not a delivery. Keep
      // waiting rather than resolving with '' and failing the exchange.
      if (ticket === '') return;
      stop();
      resolve(ticket);
    }
    w.addEventListener(TICKET_EVENT, onDelivered);
    deadline = setTimeout(() => {
      stop();
      reject(new PageTicketUndeliveredError(timeoutMs));
    }, timeoutMs);
    // Re-announcing is the recovery for the one race injection cannot
    // cover on its own: a document that finished loading before its host
    // subscribed to the announcement. It costs one postMessage per lap
    // and only ever runs while a boot is already failing.
    retry = setInterval(announceToHost, TICKET_RETRY_MS);
    announceToHost();
  });
}

// __resetPageHostForTest is the test-only escape hatch, matching
// runMode.ts's. Production code never calls it.
export function __resetPageHostForTest(): void {
  cachedWebviewHosted = null;
  awaitingHostBridge = false;
  const w = hostWindow();
  if (w) delete w[TICKET_GLOBAL];
}
