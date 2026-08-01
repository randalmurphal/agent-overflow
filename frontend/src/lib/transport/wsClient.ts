// Long-lived WebSocket client for the Phase B transport. The shim in
// ./runtime.ts re-exports the @wailsio/runtime surface but routes every
// call through this client so the same generated bindings and event-store
// code work whether we ship inside a Wails webview or against a remote
// HTTP+WS server.
//
// Wire protocol is defined in /internal/transport/frame.go; the frame
// shapes live in ./frames.ts. Frames are JSON text messages of these
// shapes:
//
//   Client → Server:
//     {type:"rpc", id, methodId?, method?, params:[...]}
//     {type:"replay", lastSeqByChannel:{...}}
//
//   Server → Client:
//     {type:"rpc", id, result?}                              // success
//     {type:"rpc", id, error:{code, message}}                // error
//     {type:"event", channel, seq, data, gap?}               // push
//     {type:"batch", events:[{channel,seq,data,gap?},...]}   // coalesced push
//     {type:"replay", id?}                                  // replay complete
//     {type:"ping"}                                         // keepalive heartbeat
//
// The client owns:
//   - The (single) WebSocket connection and its lifecycle.
//   - In-flight RPC tracking, by request id.
//   - Per-channel last-seen seq for replay-on-reconnect.
//   - Subscriber fanout (Events.On registers here; the transport pumps
//     incoming event frames into the registered handlers).
//
// Anything that needs the WS goes through this module's exported
// `wsClient` singleton — there is no second path.

import { documentHidden } from '../utils/pageVisibility';
import { type Bootstrap, defaultBootstrap, appendToken } from './bootstrap';
import {
  type ClientFrame,
  type ClientRPCFrame,
  type ServerEventFrame,
  type ServerFrame,
  clampString,
  extractRpcIdFromOversizedFrame,
} from './frames';

// Test-visible exports for the bound constants. We keep the const
// names for the production code paths (clearer at the call site than
// a member-access) and re-export the values so tests can derive
// boundaries from the same source rather than hard-coding numerals.
// Deliberately ABOVE the Go-side subprocess timeout (defaultTimeout in
// internal/git/core.go, 45s): when a git/gh/glab call hangs, the
// backend's descriptive error ("gh ... timed out after 45s") must win
// the race against this opaque client-side timeout. This timer is the
// last-resort guard for a live-but-stuck server; dead connections are
// handled by the WS close path rejecting all pending RPCs.
export const RPC_TIMEOUT_MS = 60_000;
export const RECONNECT_INITIAL_MS = 250;
// Backoff cap by backend locality (bootstrap `remote` flag). The 30s
// cap is remote-client sizing — polite to a LAN server that may be
// genuinely down. Against a same-machine backend a failed attempt is a
// refused loopback connect (~µs), so a long cap buys nothing and costs
// stale event streams: demand collapse (queuedAttempt.fire) only fires
// when the user acts, so a passive viewer would otherwise stare at a
// frozen stream for up to the cap after a relay flap.
export const RECONNECT_MAX_LOCAL_MS = 5_000;
export const RECONNECT_MAX_REMOTE_MS = 30_000;
// Stale-socket watchdog. The server heartbeats every 10s — 3× that
// cadence with no traffic at all means the socket is half-open; the
// full rationale lives in internal/transport/AGENTS.md §Keepalive.
// Only armed per-connection after its first ping frame proves this
// server heartbeats (version/deployment skew must not turn an
// idle-but-healthy connection into a reconnect loop). Check cadence is
// coarse — precision is not the point.
export const STALE_TRAFFIC_THRESHOLD_MS = 30_000;
export const STALE_CHECK_INTERVAL_MS = 10_000;
// Consecutive close-before-open failures that invalidate the cached
// bootstrap (see preOpenFailures).
export const BOOTSTRAP_INVALIDATE_AFTER_FAILURES = 2;
const TRANSPORT_GAP_CHANNEL = 'transport:gap';
// Cap matches server-side MaxReplayChannels (frame.go) so the replay
// frame can't exceed the wire limit.
export const MAX_REPLAY_CHANNELS = 1024;
// Native notification activation can arrive before the SPA makes its first
// WS connection (notably a cold launch from a Windows toast). Seed this
// channel at sequence zero so the first replay request drains the transport
// ring into eventsNotification's bounded pre-hydration queue.
const NOTIFICATION_ACTIVATED_CHANNEL = 'notification:activated';
const NOTIFICATION_ACTIVATION_SEQ_KEY = 'ao:notification-activation-seq';
let notificationCheckpointLoadWarningLogged = false;
let notificationCheckpointStoreWarningLogged = false;
const MAX_TRACKED_REPLAY_CHANNELS = MAX_REPLAY_CHANNELS - 1;
// Defensive cap on concurrent client RPCs. The server caps at 64 per
// connection — at 10_000 client-side, something pathological is happening.
export const MAX_PENDING_RPCS = 10_000;
// Protects the main thread from a hostile/buggy server flooding huge
// frames. Symmetric with the server's DefaultReadLimit
// (internal/transport/conn.go) so a frame that fits the server cap
// also fits the client cap — keep both values in lockstep. 75 MiB is
// sized for the worst legitimate load: a long thread's
// ListRecentThreadItems response (items.meta + payload metadata across
// hundreds of turns), where spending the memory once per thread switch
// is the right tradeoff vs. forcing pagination on every load.
export const MAX_FRAME_BYTES = 75 * 1024 * 1024;

// DisconnectedError is what we reject pending RPCs with when the socket
// closes underneath them. Subclassing Error keeps `instanceof` checks
// working at call sites; the `name` field is what most frontend code
// branches on.
export class DisconnectedError extends Error {
  constructor(message = 'transport disconnected') {
    super(message);
    this.name = 'DisconnectedError';
  }
}

// TransportError wraps a server-side FrameError. The `code` is exposed
// so callers can branch on the stable token strings declared in
// internal/transport/frame.go (method_not_found, bad_params, etc).
export class TransportError extends Error {
  code: string;
  constructor(code: string, message: string) {
    super(message);
    this.name = 'TransportError';
    this.code = code;
  }
}

// Pending tracks an outstanding RPC. The timer is cleared on settle so
// the timeout doesn't fire after the response arrives.
interface Pending {
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
}

type EventHandler = (data: unknown) => void;

// Reusable subscriber-fanout copy. dispatchToSubscribers snapshots a
// channel's handler set before iterating; doing that into one shared
// scratch array (truncated after each fanout) removes a per-event
// allocation from the streaming drain. Module-level is safe because
// fanouts never nest — dispatch is driven solely by WS onmessage — and
// `fanoutScratchInUse` falls back to a fresh copy if that ever changes.
const fanoutScratch: EventHandler[] = [];
let fanoutScratchInUse = false;

// TransportStatus describes the current connectivity to the wsClient.
// 'connected' means the socket is OPEN and serving traffic.
// 'reconnecting' means a backoff timer is queued or a connect attempt is
// in-flight after a previous close. nextAttemptAt is the wall-clock
// millis when the next attempt is scheduled — null if the attempt is
// already in flight.
// 'disconnected' is the zero-value before any connect has been
// attempted; we never re-enter it once a connect cycle starts (we stay
// in 'reconnecting' across attempts, even after permanent failure)
// because exposing a permanent terminal state would require a manual
// retry control we haven't wired and would lie to the user when the
// loop is still running.
export type TransportStatus = 'connected' | 'reconnecting' | 'disconnected';

export interface TransportStatusSnapshot {
  status: TransportStatus;
  /** Wall-clock millis when the next reconnect attempt fires. null when
   *  the attempt is already in flight or no attempt is scheduled. */
  nextAttemptAt: number | null;
}

type StatusHandler = (snapshot: TransportStatusSnapshot) => void;

// WSConstructor matches the global WebSocket signature plus enough state
// to drive a fake in tests. We keep this typed (not `any`) so the test
// harness can pass a hand-rolled MockWebSocket that satisfies the same
// shape.
type WSConstructor = new (url: string) => WSLike;

interface WSLike {
  readyState: number;
  send(data: string): void;
  close(code?: number, reason?: string): void;
  addEventListener(type: 'open', listener: () => void): void;
  addEventListener(type: 'close', listener: (ev: CloseEvent) => void): void;
  addEventListener(type: 'error', listener: (ev: Event) => void): void;
  addEventListener(type: 'message', listener: (ev: MessageEvent) => void): void;
}

// WS_OPEN is the readyState constant for an open socket. We use the
// numeric literal so MockWebSocket in tests doesn't have to expose the
// global WebSocket constants.
const WS_OPEN = 1;

// BootstrapFetcher is the indirection that lets tests inject a fake
// bootstrap without poking at window.location.
type BootstrapFetcher = () => Promise<Bootstrap>;

interface WSClientOptions {
  // For tests: a constructor that yields a fake WSLike.
  WebSocketCtor?: WSConstructor;
  // For tests: a function returning the bootstrap manifest.
  bootstrap?: BootstrapFetcher;
  // For tests: skip the auto-connect on first call so a test can drive
  // events into an explicit harness instead.
  autoConnect?: boolean;
  // For tests: override MAX_FRAME_BYTES. Production code MUST NOT pass
  // this — the cap matters as a defence and the symmetry with the
  // server's DefaultReadLimit is the contract. Tests pass a small
  // value (~4 KiB) so the oversized-frame regression case can be
  // exercised without allocating tens of MiB per run.
  maxFrameBytes?: number;
}

// ConnectAttempt is the per-attempt settlement state shared by one
// socket's open and close handlers. `settled` flips once the attempt's
// promise has been resolved/rejected; settling an already-settled
// promise is a no-op, so late events on a superseded socket can safely
// re-settle defensively.
interface ConnectAttempt {
  settled: boolean;
  resolve: () => void;
  reject: (reason: unknown) => void;
}

// One outage's bookkeeping for the diagnostics sink: opened by the
// first close after a connected period, settled (formatted + cleared)
// when a reconnect lands. Keeping the three fields in one nullable
// record means "reset every field" is a single assignment.
interface OutageRecord {
  startedAt: number;
  closeCode: number;
  attempts: number;
}

// WSClient is the single transport client. Instances are stateful — the
// module exports a singleton at the bottom; tests can construct a fresh
// instance via `createWSClient` if they want a sandboxed one.
export class WSClient {
  private readonly fetchBootstrap: BootstrapFetcher;
  private readonly WebSocketCtor: WSConstructor;
  private readonly maxFrameBytes: number;

  // Cached bootstrap and the resolved WS URL with token query param.
  private bootstrap: Bootstrap | null = null;
  private bootstrapPromise: Promise<Bootstrap> | null = null;

  // Connection state. `connectPromise` is the in-flight connect; it
  // resolves once the socket reaches OPEN. `ws` is the live socket.
  private ws: WSLike | null = null;
  private connectPromise: Promise<void> | null = null;
  private closed = false;
  private reconnectAttempt = 0;
  // Non-null exactly while a backoff timer is queued (no attempt in
  // flight). fire() cancels the timer and runs the scheduled attempt
  // NOW, settling the same connectPromise the timer would have — so
  // awaiters queued behind the backoff are carried through instead of
  // stranded. Timer and callback live in one field so they cannot fall
  // out of lockstep. Cleared when the attempt fires (from either path)
  // and on close(). This is how fresh demand — a user RPC, a page
  // resume, the banner's Retry — skips the remaining backoff instead
  // of waiting it out.
  private queuedAttempt: { timer: ReturnType<typeof setTimeout>; fire: () => void } | null = null;
  // Wall-clock start of the most recent connect attempt. The RPC
  // demand-collapse refuses to fire a queued attempt sooner than
  // RECONNECT_INITIAL_MS after the previous attempt began, so an
  // RPC-issuing background loop (the diagnostics flush itself is an
  // RPC) can't turn the backoff into a tight retry storm.
  private lastAttemptStartedAt = 0;
  // Detaches the page-lifecycle listeners registered in the
  // constructor; null in non-DOM environments.
  private readonly detachLifecycleListeners: (() => void) | null;

  // Stale-socket watchdog state. lastFrameAt is refreshed on every
  // inbound message (heartbeats included); the timer runs only while a
  // socket is open, and only force-closes once serverSendsHeartbeats
  // has proven the traffic floor exists (see STALE_TRAFFIC_THRESHOLD_MS).
  private lastFrameAt = 0;
  private staleTimer: ReturnType<typeof setInterval> | null = null;
  // Per-connection: set by the first ping frame, reset on close. Each
  // connection re-proves the traffic floor within one heartbeat period,
  // so a backend rollback to a heartbeat-less build can't leave the
  // watchdog armed against a server that will never feed it.
  private serverSendsHeartbeats = false;

  // Consecutive connect attempts that died before reaching OPEN. Every
  // BOOTSTRAP_INVALIDATE_AFTER_FAILURES failures, the cached bootstrap
  // is dropped (and the counter reset) so the next attempt refetches —
  // a backend that restarted (new token) is unreachable forever on the
  // stale credentials, and one refetch per couple of outage-retries is
  // cheap. Reset on open.
  private preOpenFailures = 0;
  // Sticky locality from the last resolved bootstrap. Read by the
  // backoff-cap selection instead of `this.bootstrap` so invalidating
  // the bootstrap cache mid-outage doesn't flip a remote client onto
  // the aggressive local retry cadence.
  private remoteBackend = false;

  // The sink (wired by frontendErrorCapture at install) persists one
  // summary line per outage into the always-on ui-trace error log.
  // `message` is a fixed string (it is the dedupe signature on the
  // other side); the varying numbers travel in `detail`.
  private diagnosticsSink: ((message: string, detail?: string) => void) | null = null;
  private outage: OutageRecord | null = null;

  // RPC state.
  private readonly pending = new Map<string, Pending>();
  private readonly subscribers = new Map<string, Set<EventHandler>>();
  private readonly statusHandlers = new Set<StatusHandler>();
  private statusSnapshot: TransportStatusSnapshot = {
    status: 'disconnected',
    nextAttemptAt: null,
  };

  // Per-channel last-seen seq, replayed to the server on reconnect.
  // Map iteration order is insertion-ordered, so we evict the oldest
  // entry once we hit MAX_REPLAY_CHANNELS — the cap mirrors the server's
  // own clamp and stops a hostile remote from blowing the wire frame.
  private readonly lastSeqByChannel: Map<string, number> = new Map();
  // The channel currently at lastSeqByChannel's insertion-order tail —
  // lets recordChannelSeq skip the LRU delete/re-insert for the common
  // consecutive-events-on-one-channel case. Only recordChannelSeq
  // mutates the map, so the hint cannot go stale.
  private lastSeqTailChannel: string | null = null;
  private notificationReplayPending = false;
  private notificationReplayBuffer: ServerEventFrame[] = [];
  private notificationCheckpointScope: string | null = null;

  constructor(opts: WSClientOptions = {}) {
    this.fetchBootstrap = opts.bootstrap ?? defaultBootstrap;
    // Defer the global WebSocket lookup until first use so tests that
    // never connect don't trip on a missing global.
    this.WebSocketCtor = opts.WebSocketCtor ??
      ((globalThis as { WebSocket?: WSConstructor }).WebSocket as WSConstructor);
    this.maxFrameBytes = opts.maxFrameBytes ?? MAX_FRAME_BYTES;
    this.detachLifecycleListeners = this.attachLifecycleListeners();
  }

  // attachLifecycleListeners wires page thaw/restore signals into the
  // reconnect path. The Windows launcher suspends the WebView2 after
  // 30s minimised (webviewtrim.go); the frozen page's WS dies and its
  // throttled reconnect attempts fail without meaning, so on thaw the
  // client would otherwise sit out a full max backoff (up to 30s)
  // before the next scheduled attempt — the "Ctrl+Shift+` takes 30s
  // after restoring the window" failure. `visibilitychange` covers
  // restore-from-minimise; `resume` (Page Lifecycle API) covers thaw
  // without a visibility flip. Both funnel into the same idempotent
  // handler.
  private attachLifecycleListeners(): (() => void) | null {
    if (typeof document === 'undefined') return null;
    const onLifecycleResume = (): void => {
      this.handleLifecycleResume();
    };
    document.addEventListener('visibilitychange', onLifecycleResume);
    document.addEventListener('resume', onLifecycleResume);
    return () => {
      document.removeEventListener('visibilitychange', onLifecycleResume);
      document.removeEventListener('resume', onLifecycleResume);
    };
  }

  // handleLifecycleResume fires the queued reconnect attempt the moment
  // the page becomes interactive again. Attempts made while hidden or
  // frozen carry no signal about server health, so the ladder restarts
  // from zero — the visible session earns its own backoff. No-ops when
  // nothing is queued: connected needs nothing, and an attempt already
  // in flight is left to finish (racing a second connect against it
  // would clobber `this.ws`).
  private handleLifecycleResume(): void {
    if (this.closed) return;
    if (documentHidden()) return;
    if (this.ws !== null && this.ws.readyState === WS_OPEN) {
      // The watchdog's interval clock froze with the page; judge
      // staleness from fresh post-thaw evidence, otherwise every thaw
      // whose socket survived suspension force-closes it spuriously.
      this.lastFrameAt = Date.now();
    }
    if (this.queuedAttempt === null) return;
    this.reconnectAttempt = 0;
    this.queuedAttempt.fire();
  }

  // callByID sends an `rpc` frame with a numeric methodId and resolves
  // the returned promise with the server's `result`. Rejects with a
  // TransportError on a server-side FrameError, or DisconnectedError if
  // the socket dies before the response arrives.
  callByID(methodId: number, args: unknown[]): Promise<unknown> {
    return this.dispatchRPC({ methodId, params: args });
  }

  // callByName mirrors callByID but identifies the method by FQN. Used
  // by hand-written code paths that don't have a methodId — the
  // generated bindings always use ByID.
  callByName(method: string, args: unknown[]): Promise<unknown> {
    return this.dispatchRPC({ method, params: args });
  }

  // subscribe registers a handler for `channel`. Returns the
  // unsubscribe function; matches Wails' Events.On contract.
  subscribe(channel: string, handler: EventHandler): () => void {
    let set = this.subscribers.get(channel);
    if (!set) {
      set = new Set();
      this.subscribers.set(channel, set);
    }
    set.add(handler);
    // Connect lazily on first subscribe so an event-only listener
    // doesn't have to wait for an explicit RPC to bring the socket up.
    void this.ensureConnected().catch((err) => {
      console.warn('wsClient: ensureConnected failed', err);
    });
    return () => {
      const current = this.subscribers.get(channel);
      if (!current) return;
      current.delete(handler);
      if (current.size === 0) {
        this.subscribers.delete(channel);
      }
    };
  }

  /** Current transport status snapshot. Cheap; safe to call repeatedly. */
  getStatus(): TransportStatusSnapshot {
    return this.statusSnapshot;
  }

  /**
   * Subscribe to transport status changes. Handler is invoked
   * synchronously with the current snapshot so the caller can seed
   * state without a separate `getStatus` race.
   */
  onStatusChange(handler: StatusHandler): () => void {
    this.statusHandlers.add(handler);
    handler(this.statusSnapshot);
    return () => {
      this.statusHandlers.delete(handler);
    };
  }

  /**
   * Force a reconnect attempt immediately. Cancels any queued backoff
   * timer and kicks off a fresh connect. Safe to call from a UI button
   * — when an attempt is already in flight, this is a no-op.
   */
  triggerReconnect(): void {
    if (this.closed) return;
    if (this.ws && this.ws.readyState === WS_OPEN) {
      // A half-open socket also reads as OPEN. An explicit retry
      // deserves the watchdog's staleness verdict now rather than at
      // its next interval; on a genuinely live socket this is a no-op.
      this.checkStaleness();
      return;
    }
    // Reset the backoff so a manual retry starts at the lowest delay.
    this.reconnectAttempt = 0;
    if (this.queuedAttempt !== null) {
      // Run the scheduled attempt now. Going through fire() (rather
      // than cancelling the timer and starting a fresh connect)
      // settles the scheduled connectPromise, so RPCs already queued
      // behind the backoff ride the retried attempt instead of hanging
      // on an abandoned promise until their 60s timeout.
      this.queuedAttempt.fire();
      return;
    }
    if (this.connectPromise !== null) {
      // An attempt is in flight. Racing a second connect against it
      // would mint a parallel socket and orphan one of the two — let
      // it finish; its close path reschedules on failure.
      return;
    }
    void this.ensureConnected().catch((err) => {
      console.warn('wsClient: triggerReconnect failed', err);
    });
  }

  /**
   * Register the sink that persists transport diagnostics (one summary
   * line per outage on reconnect, watchdog force-closes). Wired to the
   * always-on frontend error log by installFrontendErrorCapture;
   * injected rather than imported so the transport package stays free
   * of stores/bindings dependencies (which call back into this client).
   * `message` must be a stable string — it feeds the log's per-signature
   * dedupe — with the varying numbers in `detail`.
   */
  setDiagnosticsSink(sink: ((message: string, detail?: string) => void) | null): void {
    this.diagnosticsSink = sink;
  }

  // The staleness watchdog runs only while a socket is open. It fires
  // only after serverSendsHeartbeats — a server that heartbeats every
  // 10s but has delivered nothing for STALE_TRAFFIC_THRESHOLD_MS is
  // half-open (relay died without a FIN; no close event is coming), so
  // force-closing is the only way the reconnect path ever runs.
  private startStaleWatchdog(): void {
    this.lastFrameAt = Date.now();
    if (this.staleTimer !== null) return;
    this.staleTimer = setInterval(() => {
      this.checkStaleness();
    }, STALE_CHECK_INTERVAL_MS);
  }

  private stopStaleWatchdog(): void {
    if (this.staleTimer !== null) {
      clearInterval(this.staleTimer);
      this.staleTimer = null;
    }
  }

  private checkStaleness(): void {
    if (this.closed || !this.serverSendsHeartbeats) return;
    if (!this.ws || this.ws.readyState !== WS_OPEN) return;
    // A single huge response frame on a slow remote link yields no
    // message event until fully received — and it blocks the
    // heartbeats queued behind it on the wire. While RPCs are in
    // flight against a remote backend, let their own 60s timeout
    // arbitrate instead of killing a socket mid-transfer.
    if (this.remoteBackend && this.pending.size > 0) return;
    const idleMs = Date.now() - this.lastFrameAt;
    if (idleMs <= STALE_TRAFFIC_THRESHOLD_MS) return;
    const idleSeconds = Math.round(idleMs / 1000);
    console.warn(`wsClient: no traffic for ${idleSeconds}s on an open socket; forcing reconnect`);
    this.diagnosticsSink?.(
      'transport: no traffic on an open socket; forcing reconnect',
      `idle ${idleSeconds}s`,
    );
    try {
      this.ws.close();
    } catch {
      // ignore — socket may already be closing; the close event still
      // drives the reconnect path either way.
    }
  }

  // close shuts the client down permanently. After this returns, calls
  // and subscribes reject / no-op. Used by tests; the production
  // singleton is never closed during normal operation.
  close(): void {
    this.closed = true;
    this.detachLifecycleListeners?.();
    this.stopStaleWatchdog();
    if (this.queuedAttempt !== null) {
      clearTimeout(this.queuedAttempt.timer);
      this.queuedAttempt = null;
    }
    if (this.ws) {
      try {
        this.ws.close(1000, 'client close');
      } catch {
        // ignore — socket may already be closed.
      }
      this.ws = null;
    }
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(new DisconnectedError('client closed'));
    }
    this.pending.clear();
    this.subscribers.clear();
  }

  // dispatchRPC is the single path through which both callByID and
  // callByName route. It generates an id, registers the pending entry,
  // and either sends immediately (socket open) or after the in-flight
  // connect resolves (socket connecting).
  private dispatchRPC(spec: { methodId?: number; method?: string; params: unknown[] }): Promise<unknown> {
    if (this.closed) {
      return Promise.reject(new DisconnectedError('client closed'));
    }
    if (this.pending.size >= MAX_PENDING_RPCS) {
      return Promise.reject(
        new TransportError('client_overloaded', `too many concurrent RPCs (>=${MAX_PENDING_RPCS})`),
      );
    }
    const id = generateId();
    return new Promise<unknown>((resolve, reject) => {
      const timer = setTimeout(() => {
        if (this.pending.delete(id)) {
          reject(new TransportError('timeout', `RPC ${id} timed out after ${RPC_TIMEOUT_MS}ms`));
        }
      }, RPC_TIMEOUT_MS);
      this.pending.set(id, { resolve, reject, timer });

      const frame: ClientRPCFrame = {
        type: 'rpc',
        id,
        params: spec.params,
      };
      // Treat methodId === 0 as "fall through to method name" — Wails'
      // generated bindings reserve 0 as a sentinel. Including it on the
      // wire would force the server to look up id 0 and miss the named
      // method.
      if (typeof spec.methodId === 'number' && spec.methodId !== 0) frame.methodId = spec.methodId;
      if (typeof spec.method === 'string') frame.method = spec.method;

      // An RPC is live demand: if reconnection is sitting in a queued
      // backoff, run the attempt now instead of making the caller wait
      // it out (up to the backoff cap of nothing-visibly-happening for
      // whatever UI action issued this call). Rate-floored: an attempt
      // must be at least RECONNECT_INITIAL_MS old before demand starts
      // the next one, so an RPC-issuing background loop can't defeat
      // the backoff entirely. The attempt counter is deliberately NOT
      // reset — against a genuinely down server the ladder keeps its
      // height; this only collapses the idle wait.
      if (Date.now() - this.lastAttemptStartedAt >= RECONNECT_INITIAL_MS) {
        this.queuedAttempt?.fire();
      }
      this.ensureConnected().then(
        () => {
          // The connect may have been replaced by a reconnect that
          // already rejected this id — guard against the double-resolve.
          if (!this.pending.has(id)) return;
          this.sendFrame(frame);
        },
        (err) => {
          if (this.pending.delete(id)) {
            clearTimeout(timer);
            reject(err);
          }
        },
      );
    });
  }

  // ensureConnected returns a promise that resolves once the socket is
  // OPEN. Multiple concurrent callers share the in-flight promise; the
  // promise is replaced each time the socket transitions back to
  // closed/connecting so reconnect attempts are observable.
  private ensureConnected(): Promise<void> {
    if (this.closed) {
      return Promise.reject(new DisconnectedError('client closed'));
    }
    if (this.ws && this.ws.readyState === WS_OPEN) {
      return Promise.resolve();
    }
    if (this.connectPromise) {
      return this.connectPromise;
    }
    this.connectPromise = this.connect();
    return this.connectPromise;
  }

  // connect performs one connection attempt. On open it resolves the
  // outer promise; on close it triggers the reconnect schedule.
  //
  // Failure paths (bootstrap throws, constructor throws, close-before-open)
  // null `this.connectPromise` on the way out so a fresh ensureConnected
  // call after the failure starts a new attempt rather than re-awaiting
  // a permanently-rejected Promise. A bootstrap failure also schedules a
  // reconnect — without that, a transient 5xx on the manifest fetch would
  // leave the client permanently broken until the next user-initiated
  // RPC.
  private async connect(): Promise<void> {
    this.lastAttemptStartedAt = Date.now();
    let bootstrap: Bootstrap;
    try {
      bootstrap = await this.getBootstrap();
    } catch (err) {
      this.connectPromise = null;
      console.warn('wsClient: bootstrap failed', err);
      // Bootstrap-stage failures count toward the outage's attempt
      // tally too (when one is open), so the reconnect summary reflects
      // server-unreachable retries and not just WS-stage deaths.
      if (this.outage !== null) this.outage.attempts += 1;
      // Re-raise so the awaiter sees the rejection, but also kick off a
      // reconnect so a transient bootstrap failure recovers without
      // requiring fresh user input.
      this.scheduleReconnect();
      throw err;
    }
    if (this.closed) {
      this.connectPromise = null;
      throw new DisconnectedError('client closed');
    }
    const url = appendToken(bootstrap.wsUrl, bootstrap.token);

    return await new Promise<void>((resolve, reject) => {
      const attempt: ConnectAttempt = { settled: false, resolve, reject };
      let ws: WSLike;
      try {
        ws = new this.WebSocketCtor(url);
      } catch (err) {
        this.connectPromise = null;
        reject(err);
        return;
      }
      this.ws = ws;
      ws.addEventListener('open', () => this.handleSocketOpen(ws, bootstrap, attempt));
      ws.addEventListener('message', (ev: MessageEvent) => this.handleSocketMessage(ws, ev));
      ws.addEventListener('error', (errEv: Event) => {
        // The browser-spec WebSocket error event fires before close.
        // We don't reject here — the close event delivers the canonical
        // signal and the reconnect path takes over. Logging the event
        // leaves a debug breadcrumb for environments where the close
        // reason is opaque.
        console.warn('wsClient: socket error', errEv);
      });
      ws.addEventListener('close', (ev: CloseEvent) => this.handleSocketClose(ws, ev, attempt));
    });
  }

  // handleSocketOpen finishes a successful connect attempt: replay
  // handshake, watchdog arm, outage settlement, promise resolution.
  // Guarded on socket identity — a socket superseded while CONNECTING
  // (the watchdog force-closed its predecessor and fresh demand minted
  // a successor before the close event landed) must not serve traffic
  // or touch the live connection's state.
  private handleSocketOpen(ws: WSLike, bootstrap: Bootstrap, attempt: ConnectAttempt): void {
    if (this.ws !== ws) {
      try {
        ws.close(1000, 'superseded');
      } catch {
        // ignore — already closing.
      }
      return;
    }
    attempt.settled = true;
    this.reconnectAttempt = 0;
    // First-frame after open: replay any missed events. The server
    // only acts on this if the map is non-empty; it's still cheap
    // to send unconditionally since channel-by-channel reconciliation
    // is exactly what a reconnect needs.
    const replay: Record<string, number> = {
      [NOTIFICATION_ACTIVATED_CHANNEL]: loadNotificationActivationSeq(bootstrap.token),
    };
    this.notificationCheckpointScope = bootstrap.token;
    this.notificationReplayPending = true;
    this.notificationReplayBuffer = [];
    for (const [channel, seq] of this.lastSeqByChannel) {
      replay[channel] = seq;
    }
    this.sendFrame({
      type: 'replay',
      lastSeqByChannel: replay,
    });
    this.connectPromise = null;
    this.preOpenFailures = 0;
    this.startStaleWatchdog();
    this.setStatus({ status: 'connected', nextAttemptAt: null });
    if (this.outage !== null) {
      const downSeconds = ((Date.now() - this.outage.startedAt) / 1000).toFixed(1);
      const detail =
        `down ${downSeconds}s, close code ${this.outage.closeCode}, ${this.outage.attempts} failed attempts`;
      // Console too, not just the sink: remote clients can't persist
      // through ReportFrontendErrorBatch (LocalOnly), and the console
      // line is then the only surviving evidence of the outage.
      console.info(`wsClient: reconnected after outage (${detail})`);
      this.diagnosticsSink?.('transport: reconnected after outage', detail);
      this.outage = null;
    }
    attempt.resolve();
  }

  private handleSocketMessage(ws: WSLike, ev: MessageEvent): void {
    // Frames from a superseded socket must not reach the live
    // connection's state (seq tracking, pending RPCs, watchdog).
    if (this.ws !== ws) return;
    // Any inbound frame is proof of life — refresh the staleness
    // watchdog before any validation can reject the frame.
    this.lastFrameAt = Date.now();
    const text = typeof ev.data === 'string' ? ev.data : '';
    if (!text) return;
    if (text.length > this.maxFrameBytes) {
      // Don't silently drop: previous behavior left the matching
      // RPC pending until its 30 s timeout fired, leaving the UI
      // stuck in `loading=true`. Surface it now — extract the RPC
      // id with a tolerant regex (a full JSON parse of an
      // oversized payload would itself fail), reject the matching
      // pending RPC, and fail loud.
      const id = extractRpcIdFromOversizedFrame(text);
      const err = new TransportError(
        'frame_too_large',
        `frame ${text.length} bytes exceeds cap ${this.maxFrameBytes} (rpc=${id ?? 'unknown'})`,
      );
      console.error('wsClient:', err.message);
      if (id !== null) {
        const pending = this.pending.get(id);
        if (pending) {
          this.pending.delete(id);
          clearTimeout(pending.timer);
          pending.reject(err);
        }
      }
      return;
    }
    try {
      const frame = JSON.parse(text) as ServerFrame;
      this.handleFrame(frame);
    } catch (err) {
      console.warn('wsClient: malformed frame', err);
    }
  }

  // handleSocketClose tears down after a socket dies: outage
  // bookkeeping, pending-RPC rejection, bootstrap-cache invalidation,
  // attempt settlement, and the reconnect schedule. A superseded
  // socket's close only settles its own attempt — the live socket's
  // state is not its to touch.
  private handleSocketClose(ws: WSLike, ev: CloseEvent, attempt: ConnectAttempt): void {
    if (this.ws !== ws) {
      attempt.settled = true;
      attempt.reject(new DisconnectedError('socket superseded'));
      return;
    }
    this.stopStaleWatchdog();
    this.serverSendsHeartbeats = false;
    // Outage bookkeeping: the first close opens the outage record
    // (its code names the original cause — 1006 network death vs
    // 1000/1001 graceful); later closes during the same outage are
    // failed reconnect attempts.
    if (this.outage === null) {
      this.outage = { startedAt: Date.now(), closeCode: ev.code, attempts: 0 };
    }
    // Drop pending RPCs from this socket; they will not get a
    // response on this connection. The reconnect path resends a
    // replay frame, but RPCs themselves are not retried (the caller
    // sees DisconnectedError and decides whether to retry at the
    // app layer).
    this.failPending(new DisconnectedError('socket closed'));
    this.notificationReplayPending = false;
    this.notificationReplayBuffer = [];
    this.ws = null;
    if (!attempt.settled) {
      this.outage.attempts += 1;
      this.preOpenFailures += 1;
      if (this.preOpenFailures >= BOOTSTRAP_INVALIDATE_AFTER_FAILURES && this.bootstrap !== null) {
        // Consecutive attempts died before OPEN: the cached bootstrap
        // may be stale — a restarted backend mints a new token, and
        // reconnecting with the old one is refused forever. Drop the
        // cache so the next attempt refetches; if the server is simply
        // down, the refetch fails into the same backoff it would have
        // anyway. Counter resets so the refetch happens every
        // BOOTSTRAP_INVALIDATE_AFTER_FAILURES failures, not on every
        // failure from here on.
        this.bootstrap = null;
        this.bootstrapPromise = null;
        this.preOpenFailures = 0;
      }
      // First-attempt failure: surface to the awaiter so the call
      // that triggered ensureConnected sees the error rather than
      // hanging on a Promise that never resolves.
      attempt.settled = true;
      this.connectPromise = null;
      attempt.reject(new DisconnectedError('socket closed before open'));
    }
    if (this.closed) return;
    this.setStatus({ status: 'reconnecting', nextAttemptAt: null });
    this.scheduleReconnect();
  }

  // scheduleReconnect waits an exponentially-backoff'd delay and then
  // attempts a new connection. The next ensureConnected call (from a
  // queued RPC or a fresh subscribe) sees the in-flight connectPromise
  // and waits on it.
  //
  // We attach an internal `.catch` to the new promise so a rejection
  // (next attempt also fails / client closes mid-reconnect) doesn't
  // surface as an unhandled rejection — there's no synchronous awaiter
  // when the reconnect was triggered by a passive close. ensureConnected
  // installs its own `.catch` for the active path; this handler is
  // strictly the safety net for the no-awaiter path.
  private scheduleReconnect(): void {
    if (this.closed) return;
    if (this.queuedAttempt !== null) {
      // A reconnect is already queued — let it run; doubling up would
      // drop the pending close-handler attempt and risk synchronising
      // multiple reconnects on a flaky socket.
      return;
    }
    const attempt = this.reconnectAttempt;
    this.reconnectAttempt = attempt + 1;
    const cap = this.remoteBackend ? RECONNECT_MAX_REMOTE_MS : RECONNECT_MAX_LOCAL_MS;
    const base = Math.min(RECONNECT_INITIAL_MS * 2 ** attempt, cap);
    // Full jitter — picked uniformly in [0, base]. Floor protects
    // against zero-delay reconnect on Math.random() => 0; without it
    // a degenerate RNG could spin a tight reconnect loop.
    const delay = Math.max(50, Math.floor(Math.random() * base));
    const nextAttemptAt = Date.now() + delay;
    this.setStatus({ status: 'reconnecting', nextAttemptAt });
    const promise = new Promise<void>((resolve, reject) => {
      // The attempt body is shared between the backoff timer and
      // queuedAttempt.fire so early demand (an RPC, a page resume, the
      // Retry button) runs THIS scheduled attempt — settling this same
      // promise for anyone already awaiting connectPromise — rather
      // than racing a second connect against it.
      const fire = (): void => {
        if (this.queuedAttempt !== null) {
          clearTimeout(this.queuedAttempt.timer);
          this.queuedAttempt = null;
        }
        if (this.closed) {
          reject(new DisconnectedError('client closed'));
          return;
        }
        // Switch to "in-flight attempt" — clear nextAttemptAt so the UI
        // stops counting down while the connect promise resolves.
        this.setStatus({ status: 'reconnecting', nextAttemptAt: null });
        this.connect().then(resolve, reject);
      };
      this.queuedAttempt = { timer: setTimeout(fire, delay), fire };
    });
    this.connectPromise = promise;
    // Swallow rejections on this branch — see comment above.
    promise.catch(() => {});
  }

  // getBootstrap caches the manifest fetch so reconnect doesn't re-hit
  // /bootstrap.json. The bootstrap is stable for the lifetime of the
  // process — the token is bound to the server start and a new server
  // would issue a new one. A rejected fetch is NOT cached: nulling the
  // promise on rejection lets the next reconnect retry instead of
  // permanently re-throwing the cached error.
  private getBootstrap(): Promise<Bootstrap> {
    if (this.bootstrap) return Promise.resolve(this.bootstrap);
    if (!this.bootstrapPromise) {
      const p = this.fetchBootstrap().then((b) => {
        // Only a still-current fetch may populate the cache: the close
        // handler nulls bootstrapPromise mid-flight when it invalidates
        // the cache, and a superseded fetch landing late must not
        // overwrite the newer fetch's result (or flip remoteBackend).
        if (this.bootstrapPromise === p) {
          this.bootstrap = b;
          this.remoteBackend = b.remote === true;
        }
        return b;
      });
      // Null the cached promise on rejection so the next call retries.
      // Without this, a transient 5xx on /bootstrap.json would poison
      // every subsequent connect attempt.
      p.catch(() => {
        if (this.bootstrapPromise === p) this.bootstrapPromise = null;
      });
      this.bootstrapPromise = p;
    }
    return this.bootstrapPromise;
  }

  // sendFrame is the only path that writes to the socket. Marshals to
  // JSON and writes; a synchronous send failure (rare — usually only on
  // a socket that's already closing) immediately fails the matching
  // pending RPC so the caller doesn't block on the RPC timeout.
  private sendFrame(frame: ClientFrame): void {
    if (!this.ws || this.ws.readyState !== WS_OPEN) {
      return;
    }
    try {
      this.ws.send(JSON.stringify(frame));
    } catch (err) {
      console.warn('wsClient: send failed', err);
      // Recovery path: a thrown send means the socket already closed
      // underneath us. The close handler will fail-pending eventually,
      // but failing this RPC now lets the caller see the error before
      // the timeout expires.
      if (frame.type === 'rpc' && frame.id) {
        const pending = this.pending.get(frame.id);
        if (pending) {
          this.pending.delete(frame.id);
          clearTimeout(pending.timer);
          pending.reject(new DisconnectedError('send failed'));
        }
      }
    }
  }

  // handleFrame routes a parsed server frame. RPC responses are matched
  // by id; event pushes fan out to subscribers; batch frames iterate
  // their event array through the same per-event path.
  private handleFrame(frame: ServerFrame): void {
    if (frame.type === 'ping') {
      // Server keepalive heartbeat. The message listener already
      // refreshed lastFrameAt; the first ping additionally proves this
      // connection provides the traffic floor the staleness watchdog
      // assumes, arming it until the socket closes.
      this.serverSendsHeartbeats = true;
      return;
    }
    if (frame.type === 'rpc') {
      const pending = this.pending.get(frame.id);
      if (!pending) return;
      this.pending.delete(frame.id);
      clearTimeout(pending.timer);
      if (frame.error) {
        pending.reject(
          new TransportError(frame.error.code, clampString(frame.error.message ?? '')),
        );
        return;
      }
      pending.resolve(frame.result);
      return;
    }
    if (frame.type === 'event') {
      if (this.notificationReplayPending && frame.channel === NOTIFICATION_ACTIVATED_CHANNEL) {
        this.notificationReplayBuffer.push(frame);
        return;
      }
      this.handleEventEntry(frame);
      return;
    }
    if (frame.type === 'batch') {
      for (const evt of frame.events) {
        // Guard BEFORE building the ServerEventFrame shape: replay
        // buffering is live only during the brief post-reconnect window
        // and only for one rare channel, so the steady streaming state
        // (coalesced batches of up to 50 item deltas) must not pay a
        // throwaway spread copy per event just to probe it.
        if (this.notificationReplayPending && evt.channel === NOTIFICATION_ACTIVATED_CHANNEL) {
          this.notificationReplayBuffer.push({ type: 'event', ...evt });
          continue;
        }
        this.handleEventEntry(evt);
      }
      return;
    }
    if (frame.type === 'replay') {
      const buffered = this.notificationReplayBuffer
        .sort((a, b) => a.seq - b.seq);
      this.notificationReplayBuffer = [];
      this.notificationReplayPending = false;
      for (const event of buffered) this.handleEventEntry(event);
    }
  }

  // handleEventEntry processes a single event entry — used by both
  // the regular event path and the batch iteration path.
  private handleEventEntry(evt: {
    channel: string;
    seq: number;
    data: unknown;
    gap?: boolean;
  }): void {
    const lastSeq = this.lastSeqByChannel.get(evt.channel);
    if (lastSeq !== undefined && evt.seq <= lastSeq) return;
    this.recordChannelSeq(evt.channel, evt.seq);
    if (evt.gap === true) {
      console.warn(
        `wsClient: event gap on ${clampString(evt.channel)} (seq ${evt.seq})`,
      );
      this.dispatchToSubscribers(TRANSPORT_GAP_CHANNEL, {
        channel: evt.channel,
        seq: evt.seq,
      });
    }
    this.dispatchToSubscribers(evt.channel, evt.data);
  }

  // recordChannelSeq updates the per-channel last-seen seq and evicts
  // the oldest entry once the cap is reached. Map iteration order in
  // V8/modern engines is insertion order, so the first entry yielded by
  // .keys() is the oldest — that's what we evict.
  private recordChannelSeq(channel: string, seq: number): void {
    if (this.lastSeqByChannel.has(channel)) {
      // Re-insert so the entry moves to the tail and stays "fresh"
      // rather than aging out on the next overflow — but skip the
      // delete/re-insert when the entry already sits at the tail,
      // which is the dominant streaming pattern (back-to-back events
      // on one channel). A plain .set() on an existing key keeps its
      // position, so the LRU order is preserved exactly.
      if (channel !== this.lastSeqTailChannel) {
        this.lastSeqByChannel.delete(channel);
      }
      this.lastSeqByChannel.set(channel, seq);
      this.lastSeqTailChannel = channel;
      if (channel === NOTIFICATION_ACTIVATED_CHANNEL && this.notificationCheckpointScope !== null) {
        storeNotificationActivationSeq(this.notificationCheckpointScope, seq);
      }
      return;
    }
    if (this.lastSeqByChannel.size >= MAX_TRACKED_REPLAY_CHANNELS) {
      const oldest = this.lastSeqByChannel.keys().next().value;
      if (typeof oldest === 'string') {
        this.lastSeqByChannel.delete(oldest);
      }
    }
    this.lastSeqByChannel.set(channel, seq);
    this.lastSeqTailChannel = channel;
    if (channel === NOTIFICATION_ACTIVATED_CHANNEL && this.notificationCheckpointScope !== null) {
      storeNotificationActivationSeq(this.notificationCheckpointScope, seq);
    }
  }

  private dispatchToSubscribers(channel: string, data: unknown): void {
    const set = this.subscribers.get(channel);
    if (!set || set.size === 0) return;
    // Copy so a handler that unsubscribes mid-iteration doesn't perturb
    // the loop — into a reused module-level scratch rather than a fresh
    // `[...set]` per event, since a streaming drain dispatches here for
    // every event. The scratch is safe because dispatch is only ever
    // driven by WS onmessage (handleFrame/handleEventEntry), so no
    // handler can synchronously re-enter the fanout; the nested-use
    // fallback below keeps a future nested dispatch correct instead of
    // silently corrupting the shared array.
    const usingScratch = !fanoutScratchInUse;
    const handlers = usingScratch ? fanoutScratch : [...set];
    if (usingScratch) {
      fanoutScratchInUse = true;
      for (const handler of set) handlers.push(handler);
    }
    try {
      for (const handler of handlers) {
        // Re-check membership: a prior handler in this fanout may have
        // unsubscribed `handler`, in which case skipping it preserves the
        // documented unsubscribe contract — once unsubscribed, no further
        // events on this channel. (Handlers added mid-fanout are absent
        // from the copy and correctly wait for the next event.)
        if (!set.has(handler)) continue;
        try {
          handler(data);
        } catch (err) {
          console.warn(`wsClient: subscriber on ${clampString(channel)} threw`, err);
        }
      }
    } finally {
      if (usingScratch) {
        // Truncate so the scratch doesn't pin unsubscribed handlers
        // (and their closed-pane closures) until the next dispatch.
        fanoutScratch.length = 0;
        fanoutScratchInUse = false;
      }
    }
  }

  // setStatus updates the snapshot and notifies subscribers. Snapshots
  // are compared by shallow equality so a redundant transition (e.g.
  // 'reconnecting' → 'reconnecting' with the same timer) doesn't spam
  // the handler. Mutating handlers can race during fanout — we copy
  // before iterating to avoid the race.
  private setStatus(next: TransportStatusSnapshot): void {
    if (
      next.status === this.statusSnapshot.status
      && next.nextAttemptAt === this.statusSnapshot.nextAttemptAt
    ) return;
    this.statusSnapshot = next;
    if (this.statusHandlers.size === 0) return;
    for (const handler of [...this.statusHandlers]) {
      try {
        handler(next);
      } catch (err) {
        console.warn('wsClient: status handler threw', err);
      }
    }
  }

  private failPending(err: Error): void {
    if (this.pending.size === 0) return;
    const drained = [...this.pending.values()];
    this.pending.clear();
    for (const p of drained) {
      clearTimeout(p.timer);
      p.reject(err);
    }
  }
}

function loadNotificationActivationSeq(scope: string): number {
  try {
    const value = globalThis.sessionStorage?.getItem(NOTIFICATION_ACTIVATION_SEQ_KEY);
    if (!value) return 0;
    const parsed = JSON.parse(value) as { scope?: unknown; seq?: unknown };
    if (parsed.scope !== scope) return 0;
    return Number.isSafeInteger(parsed.seq) && Number(parsed.seq) >= 0 ? Number(parsed.seq) : 0;
  } catch (error) {
    if (!notificationCheckpointLoadWarningLogged) {
      notificationCheckpointLoadWarningLogged = true;
      console.warn('wsClient: could not load notification activation checkpoint', error);
    }
    return 0;
  }
}

function storeNotificationActivationSeq(scope: string, seq: number): void {
  try {
    globalThis.sessionStorage?.setItem(
      NOTIFICATION_ACTIVATION_SEQ_KEY,
      JSON.stringify({ scope, seq }),
    );
  } catch (error) {
    // Storage is a reload-dedup optimization. Sequence tracking in memory
    // remains authoritative for the live connection when storage is denied.
    if (!notificationCheckpointStoreWarningLogged) {
      notificationCheckpointStoreWarningLogged = true;
      console.warn('wsClient: could not store notification activation checkpoint', error);
    }
  }
}

// generateId returns a random request id via crypto.randomUUID. Every
// environment we run in (modern browsers, happy-dom, the Wails webview)
// ships randomUUID; matches the project precedent in
// frontend/src/lib/components/primitives/Modal.svelte.
function generateId(): string {
  return crypto.randomUUID();
}

// createWSClient is the test entry point. Production code uses the
// singleton at the bottom of this file.
export function createWSClient(opts: WSClientOptions = {}): WSClient {
  return new WSClient(opts);
}

// The production singleton. Module load is side-effect free — the
// connection is established lazily on the first call, subscribe, or
// other public method.
export const wsClient = new WSClient();

// Channel name for the synthetic gap event. Exported so subscribers
// don't have to hard-code the literal.
export const transportGapChannel = TRANSPORT_GAP_CHANNEL;

// Vite HMR re-evaluates this module on edit; without disposing, stale
// clients accumulate with surviving subscribers. dispose() is a no-op
// outside HMR (production builds strip the `import.meta.hot` block).
if (import.meta.hot) {
  import.meta.hot.accept(() => {
    wsClient.close();
  });
}
