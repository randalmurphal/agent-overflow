// The fake socket the transport suites drive the real WSClient with.
//
// Shared rather than duplicated: `wsClient.test.ts` drives the state
// machine with it and `transport/stepUp.test.ts` drives the step-up
// interception through the SAME frame-construction path, and a second
// hand-rolled fake would be a second answer to "what does the client
// actually send". No `ws` package and no other dependency — the fake is
// small enough that adding one would cost more than it saves.

// MockWebSocket is a hand-rolled fake that exposes the same shape as
// the WSLike interface the wsClient depends on. Tests drive it via
// `acceptOpen`, `pushFrame`, and `triggerClose`.
export class MockWebSocket {
  static instances: MockWebSocket[] = [];
  static reset(): void {
    MockWebSocket.instances = [];
  }

  url: string;
  readyState = 0; // CONNECTING

  // Frames the client has sent. Captured as parsed JSON for ergonomic
  // assertions.
  sent: Array<Record<string, unknown>> = [];

  // When set, `send` throws this value rather than appending to `sent`.
  // Drives the send-failure branch in tests.
  sendError: Error | null = null;

  private listeners = {
    open: new Set<() => void>(),
    close: new Set<(ev: CloseEvent) => void>(),
    error: new Set<(ev: Event) => void>(),
    message: new Set<(ev: MessageEvent) => void>(),
  };

  constructor(url: string) {
    this.url = url;
    MockWebSocket.instances.push(this);
  }

  send(data: string): void {
    if (this.sendError) throw this.sendError;
    this.sent.push(JSON.parse(data) as Record<string, unknown>);
  }

  // When set, close() only moves to CLOSING and parks the event;
  // flushClose() delivers it. Models the real browser socket, whose
  // close event is always asynchronous — the window in which a fresh
  // connect can start while the old socket is still tearing down.
  deferClose = false;
  private pendingClose: { code: number; reason?: string } | null = null;

  close(code?: number, reason?: string): void {
    if (this.deferClose && this.readyState < 2) {
      this.readyState = 2; // CLOSING
      this.pendingClose = { code: code ?? 1005, reason };
      return;
    }
    this.triggerClose(code ?? 1005, reason);
  }

  flushClose(): void {
    if (this.pendingClose === null) return;
    const { code, reason } = this.pendingClose;
    this.pendingClose = null;
    this.triggerClose(code, reason);
  }

  addEventListener(type: 'open', listener: () => void): void;
  addEventListener(type: 'close', listener: (ev: CloseEvent) => void): void;
  addEventListener(type: 'error', listener: (ev: Event) => void): void;
  addEventListener(type: 'message', listener: (ev: MessageEvent) => void): void;
  addEventListener(type: string, listener: unknown): void {
    if (type === 'open') this.listeners.open.add(listener as () => void);
    else if (type === 'close') this.listeners.close.add(listener as (ev: CloseEvent) => void);
    else if (type === 'error') this.listeners.error.add(listener as (ev: Event) => void);
    else if (type === 'message') this.listeners.message.add(listener as (ev: MessageEvent) => void);
  }

  // Test-driver helpers ----------------------------------------------------

  acceptOpen(): void {
    this.readyState = 1; // OPEN
    for (const fn of [...this.listeners.open]) fn();
  }

  pushFrame(frame: unknown): void {
    const ev = new MessageEvent('message', { data: JSON.stringify(frame) });
    for (const fn of [...this.listeners.message]) fn(ev);
  }

  pushRawText(text: string): void {
    const ev = new MessageEvent('message', { data: text });
    for (const fn of [...this.listeners.message]) fn(ev);
  }

  // Default code 1006 (abnormal closure) — the shape of a network
  // death, which is what most tests simulate and what the outage
  // diagnostics record.
  // `reason` is the peer's close reason. Real deployments supply one on
  // every deliberate close (a relay teardown, a policy close), and it is
  // the single most useful field for telling those apart after the fact,
  // so the fake carries it.
  triggerClose(code = 1006, reason = ''): void {
    if (this.readyState === 3) return;
    this.readyState = 3; // CLOSED
    const ev = new CloseEvent('close', { code, reason });
    for (const fn of [...this.listeners.close]) fn(ev);
  }

  // `detail` stands in for the richer error object a non-browser socket
  // (the `--connect` shell) delivers, where the browser event carries
  // nothing.
  triggerError(detail?: Event): void {
    const ev = detail ?? new Event('error');
    for (const fn of [...this.listeners.error]) fn(ev);
  }
}

// FakeCtor centralises the `as unknown as new (url: string) => MockWebSocket`
// dance. Each test passes it to createWSClient instead of repeating the
// cast.
export const FakeCtor = MockWebSocket as unknown as new (url: string) => MockWebSocket;

// flushMicrotasks yields the event loop a few times so promise-chained
// callbacks (ensureConnected.then(...)) run before the test reads
// state. Two ticks covers our actual chain depth; bumping if a future
// test introduces deeper chaining is fine.
export async function flushMicrotasks(): Promise<void> {
  await Promise.resolve();
  await Promise.resolve();
}
