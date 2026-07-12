// Long-lived WebSocket client for the Phase B transport. The shim in
// ./runtime.ts re-exports the @wailsio/runtime surface but routes every
// call through this client so the same generated bindings and event-store
// code work whether we ship inside a Wails webview or against a remote
// HTTP+WS server.
//
// Wire protocol is defined in /internal/transport/frame.go. Frames are
// JSON text messages of these shapes:
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

import { setViewOnlySessionFromBootstrap } from './runMode';

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
const RECONNECT_INITIAL_MS = 250;
const RECONNECT_MAX_MS = 30_000;
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
// Logged strings (channel names, error messages) get clamped before
// reaching console / toast surfaces. Caps the worst-case noise from a
// pathological remote without losing the prefix that identifies the
// channel.
const LOG_STRING_MAX = 256;

// RunMode marks how the SPA is attached to its backend:
//   - 'local'    — desktop binary booted a local transport in the same
//                  process. The default whenever the bootstrap omits
//                  the field, since /bootstrap.json on the local
//                  transport doesn't carry mode.
//   - 'client'   — desktop binary launched with --connect; the local
//                  process owns only a stub HTTP server and the SPA
//                  RPCs flow to a remote backend. Local-only settings
//                  panels must hide / placeholder in this mode.
//   - 'headless' — reserved for the WSL launcher path. Not currently
//                  emitted by any boot flow (the Windows-side WebView2
//                  bootstrap-injected page doesn't inject mode), but
//                  defined here so a future Phase D bootstrap can mark
//                  itself without an enum widening.
export type RunMode = 'local' | 'client' | 'headless';

// Bootstrap is the JSON the SPA fetches at /bootstrap.json on first load.
// Mirror the Go-side shape (internal/transport/server.go Bootstrap).
// `mode` is optional on the wire — only the clientmode injection
// emits it today; the local /bootstrap.json path leaves it absent and
// the SPA treats absence as 'local'.
interface Bootstrap {
  wsUrl: string;
  token: string;
  mode?: RunMode;
  remote?: boolean;
  /**
   * Durable UI-state client identity minted by the local shell
   * (--connect injection only; the embedded-webview paths carry it as
   * the ?cid= URL param instead). Consumed by stores/appStorage.ts at
   * module init, not by the transport itself.
   */
  clientId?: string;
}

// Frame shapes — match internal/transport/frame.go ServerFrame /
// ClientFrame. We allow `unknown` for `result` and `data` because the
// generated bindings unpack them via Create.* factories on the caller's
// side, not here.
interface ServerRPCFrame {
  type: 'rpc';
  id: string;
  result?: unknown;
  error?: { code: string; message: string };
}

interface ServerEventFrame {
  type: 'event';
  channel: string;
  seq: number;
  data: unknown;
  gap?: boolean;
}

interface ServerBatchFrame {
  type: 'batch';
  events: Array<{
    channel: string;
    seq: number;
    data: unknown;
    gap?: boolean;
  }>;
}

interface ServerReplayFrame {
  type: 'replay';
  id?: string;
}

type ServerFrame = ServerRPCFrame | ServerEventFrame | ServerBatchFrame | ServerReplayFrame;

interface ClientRPCFrame {
  type: 'rpc';
  id: string;
  methodId?: number;
  method?: string;
  params: unknown[];
}

interface ClientReplayFrame {
  type: 'replay';
  lastSeqByChannel: Record<string, number>;
}

type ClientFrame = ClientRPCFrame | ClientReplayFrame;

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

// extractRpcIdFromOversizedFrame pulls the `id` out of the leading
// portion of a frame too large to JSON.parse safely. ServerFrame's
// `id` is an RPC uuid emitted by call() and is always present at the
// frame's top level; a tolerant regex over the first ~256 chars is
// sufficient to recover it without parsing megabytes of payload.
// Returns null when the prefix doesn't contain an obvious id (frame
// is not an rpc response or shape changed).
const oversizedIdRegex = /"id"\s*:\s*"([^"]{1,128})"/;
function extractRpcIdFromOversizedFrame(text: string): string | null {
  const prefix = text.slice(0, 256);
  const m = oversizedIdRegex.exec(prefix);
  return m ? m[1] : null;
}

type EventHandler = (data: unknown) => void;

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

// clampString truncates noisy / hostile log content before it reaches
// console or toast surfaces. The `…` suffix preserves the visual signal
// that the value was abbreviated.
function clampString(value: string, max = LOG_STRING_MAX): string {
  if (value.length <= max) return value;
  return `${value.slice(0, max)}…`;
}

// Session-scoped stash for the bootstrap token. The token arrives once
// as `?t=` and is immediately scrubbed from the URL (see replaceState
// below), so without this stash any reload — browser F5, the Ctrl+R
// uikeys binding in the embedded webview, a Playwright page.reload() —
// loses the token and every subsequent /bootstrap.json fetch 404s.
// sessionStorage is per-tab and dies with it, matching the token's
// soft-secret posture. Access is fault-tolerant: sandboxed frames and
// some embeddings throw on storage access, and a broken stash must
// degrade to "reload needs the tokened URL again", not a crash.
const TOKEN_STORAGE_KEY = 'ao:bootstrap-token';

function readStoredToken(): string {
  try {
    return window.sessionStorage.getItem(TOKEN_STORAGE_KEY) ?? '';
  } catch {
    return '';
  }
}

function writeStoredToken(token: string): void {
  try {
    window.sessionStorage.setItem(TOKEN_STORAGE_KEY, token);
  } catch {
    // Stash unavailable — reloads will need the tokened URL again.
  }
}

function clearStoredToken(): void {
  try {
    window.sessionStorage.removeItem(TOKEN_STORAGE_KEY);
  } catch {
    // Nothing to clear if the stash itself is unreadable.
  }
}

// Default bootstrap fetcher: read from window.__AO_BOOTSTRAP__ (set by
// Phase F's `--connect` flow) or fall back to `/bootstrap.json?t=<token>`
// where the token comes from `?t=` in window.location.search, or — on a
// reload, after the URL was scrubbed — from sessionStorage. This runs
// the first time anyone calls `ensureConnected`; subsequent calls reuse
// the cached promise.
async function defaultBootstrap(): Promise<Bootstrap> {
  const injected = (globalThis as { __AO_BOOTSTRAP__?: Bootstrap }).__AO_BOOTSTRAP__;
  if (injected && typeof injected.wsUrl === 'string' && typeof injected.token === 'string') {
    validateWsUrl(injected.wsUrl);
    const normalized = { ...injected, mode: normalizeRunMode(injected.mode), remote: injected.remote === true };
    setViewOnlySessionFromBootstrap(normalized.remote);
    return normalized;
  }
  const search = typeof window !== 'undefined' ? window.location.search : '';
  const params = new URLSearchParams(search);
  const urlToken = params.get('t') ?? '';
  const token = urlToken !== '' ? urlToken : readStoredToken();
  const url = `/bootstrap.json?t=${encodeURIComponent(token)}`;
  const resp = await fetch(url, { credentials: 'same-origin' });
  if (!resp.ok) {
    if (urlToken === '' && token !== '') {
      // The stashed token was refused — stale after a backend restart
      // (tokens are minted per boot). Drop it so retries surface the
      // real "no valid token" state instead of re-presenting it.
      clearStoredToken();
    }
    throw new Error(`bootstrap fetch failed: HTTP ${resp.status}`);
  }
  const contentType = resp.headers.get('content-type') ?? '';
  if (!contentType.toLowerCase().startsWith('application/json')) {
    throw new Error(`bootstrap response not JSON: content-type ${clampString(contentType)}`);
  }
  const data = (await resp.json()) as Bootstrap;
  if (!data || typeof data !== 'object') {
    throw new Error('bootstrap response not an object');
  }
  if (typeof data.wsUrl !== 'string' || typeof data.token !== 'string') {
    throw new Error('bootstrap response missing wsUrl/token');
  }
  validateWsUrl(data.wsUrl);
  data.mode = normalizeRunMode(data.mode);
  data.remote = data.remote === true;
  setViewOnlySessionFromBootstrap(data.remote);
  // Stash the server-confirmed token so the tab survives reloads once
  // the URL is scrubbed below.
  writeStoredToken(data.token);
  // Removes the token from history, Referer, and Performance Resource
  // Timing entries. Same-origin redirects and tab-history scrubbing both
  // benefit. Skip when history.replaceState isn't available (older
  // happy-dom builds, weird host pages).
  if (
    typeof window !== 'undefined' &&
    typeof window.history !== 'undefined' &&
    typeof window.history.replaceState === 'function' &&
    urlToken !== ''
  ) {
    try {
      window.history.replaceState(null, '', window.location.pathname + window.location.hash);
    } catch {
      // Some embeddings throw on replaceState; the token-on-URL is
      // already a soft secret, so swallowing is acceptable.
    }
  }
  return data;
}

// normalizeRunMode coerces an incoming mode value to the typed enum.
// Anything outside the known set falls back to 'local' — same as
// absent. Keeping this loose-and-default-safe is intentional: a future
// backend that sends an unrecognised mode shouldn't crash the SPA;
// the worst case is a remote-mode panel rendering when the user is
// actually local, which is benign.
function normalizeRunMode(mode: unknown): RunMode {
  if (mode === 'client' || mode === 'headless' || mode === 'local') return mode;
  return 'local';
}

// validateWsUrl rejects bootstrap responses pointing the client at a
// scheme other than ws:/wss:. A boostrap fetch is over same-origin
// HTTP(S), but defending here means a hijacked bootstrap response can't
// pivot the WS connection to an arbitrary URL handler.
function validateWsUrl(wsUrl: string): void {
  let parsed: URL;
  try {
    parsed = new URL(wsUrl);
  } catch {
    throw new Error(`bootstrap wsUrl invalid: ${clampString(wsUrl)}`);
  }
  if (parsed.protocol !== 'ws:' && parsed.protocol !== 'wss:') {
    throw new Error(`bootstrap wsUrl scheme not ws/wss: ${clampString(parsed.protocol)}`);
  }
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
  // Stored so close() can cancel a pending backoff.
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null;

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
   * — when already in flight, this is a no-op.
   */
  triggerReconnect(): void {
    if (this.closed) return;
    if (this.ws && this.ws.readyState === WS_OPEN) return;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    // Reset the backoff so a manual retry starts at the lowest delay.
    this.reconnectAttempt = 0;
    this.connectPromise = null;
    void this.ensureConnected().catch((err) => {
      console.warn('wsClient: triggerReconnect failed', err);
    });
  }

  // close shuts the client down permanently. After this returns, calls
  // and subscribes reject / no-op. Used by tests; the production
  // singleton is never closed during normal operation.
  close(): void {
    this.closed = true;
    if (this.reconnectTimer !== null) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
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
    let bootstrap: Bootstrap;
    try {
      bootstrap = await this.getBootstrap();
    } catch (err) {
      this.connectPromise = null;
      console.warn('wsClient: bootstrap failed', err);
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
      let settled = false;
      let ws: WSLike;
      try {
        ws = new this.WebSocketCtor(url);
      } catch (err) {
        this.connectPromise = null;
        reject(err);
        return;
      }
      this.ws = ws;

      ws.addEventListener('open', () => {
        settled = true;
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
        this.setStatus({ status: 'connected', nextAttemptAt: null });
        resolve();
      });

      ws.addEventListener('message', (ev: MessageEvent) => {
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
      });

      ws.addEventListener('error', (errEv: Event) => {
        // The browser-spec WebSocket error event fires before close.
        // We don't reject here — the close event delivers the canonical
        // signal and the reconnect path takes over. Logging the event
        // leaves a debug breadcrumb for environments where the close
        // reason is opaque.
        console.warn('wsClient: socket error', errEv);
      });

      ws.addEventListener('close', () => {
        // Drop pending RPCs from this socket; they will not get a
        // response on this connection. The reconnect path resends a
        // replay frame, but RPCs themselves are not retried (the caller
        // sees DisconnectedError and decides whether to retry at the
        // app layer).
        this.failPending(new DisconnectedError('socket closed'));
        this.notificationReplayPending = false;
        this.notificationReplayBuffer = [];
        this.ws = null;
        if (!settled) {
          // First-attempt failure: surface to the awaiter so the call
          // that triggered ensureConnected sees the error rather than
          // hanging on a Promise that never resolves.
          settled = true;
          this.connectPromise = null;
          reject(new DisconnectedError('socket closed before open'));
        }
        if (this.closed) return;
        this.setStatus({ status: 'reconnecting', nextAttemptAt: null });
        this.scheduleReconnect();
      });
    });
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
    if (this.reconnectTimer !== null) {
      // A reconnect is already queued — let it run; doubling up would
      // drop the pending close-handler attempt and risk synchronising
      // multiple reconnects on a flaky socket.
      return;
    }
    const attempt = this.reconnectAttempt;
    this.reconnectAttempt = attempt + 1;
    const base = Math.min(RECONNECT_INITIAL_MS * 2 ** attempt, RECONNECT_MAX_MS);
    // Full jitter — picked uniformly in [0, base]. Floor protects
    // against zero-delay reconnect on Math.random() => 0; without it
    // a degenerate RNG could spin a tight reconnect loop.
    const delay = Math.max(50, Math.floor(Math.random() * base));
    const nextAttemptAt = Date.now() + delay;
    this.setStatus({ status: 'reconnecting', nextAttemptAt });
    const promise = new Promise<void>((resolve, reject) => {
      this.reconnectTimer = setTimeout(() => {
        this.reconnectTimer = null;
        if (this.closed) {
          reject(new DisconnectedError('client closed'));
          return;
        }
        // Switch to "in-flight attempt" — clear nextAttemptAt so the UI
        // stops counting down while the connect promise resolves.
        this.setStatus({ status: 'reconnecting', nextAttemptAt: null });
        this.connect().then(resolve, reject);
      }, delay);
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
        this.bootstrap = b;
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
      if (this.bufferNotificationReplayEvent(frame)) return;
      this.handleEventEntry(frame);
      return;
    }
    if (frame.type === 'batch') {
      for (const evt of frame.events) {
        if (this.bufferNotificationReplayEvent({ type: 'event', ...evt })) continue;
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

  private bufferNotificationReplayEvent(event: ServerEventFrame): boolean {
    if (!this.notificationReplayPending || event.channel !== NOTIFICATION_ACTIVATED_CHANNEL) {
      return false;
    }
    this.notificationReplayBuffer.push(event);
    return true;
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
      // rather than aging out on the next overflow.
      this.lastSeqByChannel.delete(channel);
      this.lastSeqByChannel.set(channel, seq);
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
    if (channel === NOTIFICATION_ACTIVATED_CHANNEL && this.notificationCheckpointScope !== null) {
      storeNotificationActivationSeq(this.notificationCheckpointScope, seq);
    }
  }

  private dispatchToSubscribers(channel: string, data: unknown): void {
    const set = this.subscribers.get(channel);
    if (!set || set.size === 0) return;
    // Copy so a handler that unsubscribes mid-iteration doesn't perturb
    // the loop.
    for (const handler of [...set]) {
      // Re-check membership: a prior handler in this fanout may have
      // unsubscribed `handler`, in which case skipping it preserves the
      // documented unsubscribe contract — once unsubscribed, no further
      // events on this channel.
      if (!set.has(handler)) continue;
      try {
        handler(data);
      } catch (err) {
        console.warn(`wsClient: subscriber on ${clampString(channel)} threw`, err);
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

// appendToken adds `?token=<value>` to the WS URL. Handles URLs that
// already carry query params via the URL constructor.
function appendToken(wsUrl: string, token: string): string {
  try {
    const parsed = new URL(wsUrl);
    parsed.searchParams.set('token', token);
    return parsed.toString();
  } catch {
    // Relative or otherwise un-parseable — fall back to a plain
    // concatenation. We bias toward letting the browser's WS
    // implementation reject a bad URL rather than silently mutating it.
    const sep = wsUrl.includes('?') ? '&' : '?';
    return `${wsUrl}${sep}token=${encodeURIComponent(token)}`;
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
