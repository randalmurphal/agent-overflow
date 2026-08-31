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
//   - Per-channel last-seen seq: the replay-on-reconnect cursor, the
//     dedup check, AND mid-connection drop detection (a forward skip in
//     the seq of a channel we already saw on THIS connection means the
//     server's non-blocking fanout dropped what sat between).
//   - Subscriber fanout (Events.On registers here; the transport pumps
//     incoming event frames into the registered handlers).
//
// Anything that needs the WS goes through this module's exported
// `wsClient` singleton — there is no second path.

import { documentHidden } from '../utils/pageVisibility';
import {
  type Bootstrap,
  BootstrapRejectedError,
  defaultBootstrap,
  pageServedOverLoopback,
} from './bootstrap';
import {
  type ClientFrame,
  type ClientRPCFrame,
  type ServerEventFrame,
  type ServerHelloFrame,
  type ServerFrame,
  clampString,
  extractRpcIdFromOversizedFrame,
} from './frames';
import { getConnectionId, getDeviceId } from './clientIdentity';
import { hasPairedSession, mintDialTicket } from './deviceSession';
import { refreshGrantedScopes } from './scopes';

/**
 * Append this screen's identity to the upgrade URL. Kept as a function rather
 * than a captured constant so a reconnect after a device-id change (the launcher
 * pinning a bucket via ?cid=) uses the current value.
 *
 * Failing to parse the URL is not fatal: the identity is an attribution
 * nicety, and refusing to connect over it would trade a working session for a
 * missing label.
 */
function withClientIdentity(wsUrl: string): string {
  try {
    const url = new URL(wsUrl);
    url.searchParams.set('did', getDeviceId());
    url.searchParams.set('conn', getConnectionId());
    return url.toString();
  } catch {
    return wsUrl;
  }
}

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
// How long a connection must survive before it counts as STABLE and
// earns a backoff reset. Resetting on `open` alone let an
// accept-then-immediately-close server (a backend that upgrades then
// dies on the first frame, a relay that tears the tunnel down right
// after the handshake) pin the delay at the 50ms jitter floor — a 10Hz
// connect storm that looks like a working reconnect ladder. A
// connection that lasted this long proves the far side was actually
// serving, which is what the ladder is supposed to measure.
export const BACKOFF_RESET_AFTER_MS = 30_000;
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

// DisconnectedErrorInit carries the preserved cause of a transport
// failure onto the error the caller sees. Every field is optional: the
// paths that genuinely know nothing (a superseded socket) supply none.
export interface DisconnectedErrorInit {
  /** WebSocket close code, when a close event produced this failure. */
  closeCode?: number;
  /** Close reason verbatim from the peer. Remote text — clamped before
   *  it reaches the message. */
  closeReason?: string;
  /** The error underneath: a socket `error` event, a thrown WebSocket
   *  constructor, or the bootstrap rejection that stopped the attempt. */
  cause?: unknown;
  /** True when the automatic reconnect ladder is STOPPED, so nothing
   *  will retry the work behind this call without user action. False
   *  during an ordinary reconnect, where the entity stores' suspension
   *  re-sources every key on the next connect. */
  terminal?: boolean;
}

// Close reasons are peer-supplied text that lands in logs, toasts, and
// the diagnostics sink. The WS spec caps a reason at 123 UTF-8 bytes, so
// this only ever truncates a peer that does not conform.
const CLOSE_REASON_MAX = 123;

// Distinct kinds of unrecognized wire input tracked for the debug tally.
// Bounded because the kind label can be a frame type the peer chose: a
// backend naming a new type on every frame must not be able to grow the
// map. Past the cap the total still counts.
const MAX_TRACKED_UNKNOWN_KINDS = 8;
// Kind labels come off the wire, so they are clamped before they reach a
// console line or the stats object.
const UNKNOWN_KIND_LABEL_MAX = 64;

// DisconnectedError is what we reject pending RPCs with when the socket
// closes underneath them. Subclassing Error keeps `instanceof` checks
// working at call sites; the `name` field is what most frontend code
// branches on.
//
// The failure's CAUSE travels on the instance and, deliberately, inside
// `message` too. Around 150 call sites render a failure as `err.message`,
// so putting the close code and reason there is what makes the cause
// legible everywhere at once; the alternative — each site reaching for a
// new field — is a sweep that decays the moment someone adds site 151.
// `cause` and the discrete fields remain for callers that branch rather
// than render.
export class DisconnectedError extends Error {
  /** WS close code when a close event produced this, else undefined. */
  readonly closeCode: number | undefined;
  /** Clamped close reason when the peer supplied one. */
  readonly closeReason: string | undefined;
  /** See DisconnectedErrorInit.terminal. */
  readonly terminal: boolean;

  constructor(message = 'transport disconnected', init: DisconnectedErrorInit = {}) {
    super(describeDisconnect(message, init), { cause: init.cause });
    this.name = 'DisconnectedError';
    this.closeCode = init.closeCode;
    this.closeReason = init.closeReason === undefined
      ? undefined
      : clampString(init.closeReason, CLOSE_REASON_MAX);
    this.terminal = init.terminal === true;
  }
}

// describeDisconnect renders the preserved cause into the message. Kept
// out of the constructor body so the no-detail case (the common one)
// returns the caller's own string without building anything.
function describeDisconnect(message: string, init: DisconnectedErrorInit): string {
  const reason = init.closeReason === undefined || init.closeReason === ''
    ? ''
    : clampString(init.closeReason, CLOSE_REASON_MAX);
  // Comma, not colon, between code and reason. `userFacingError` strips
  // everything before the last ": " to unwrap Go-style error chains, so a
  // colon here would render this as "Backend restarting)" — the cause
  // shorn of what it is the cause OF, plus a stray bracket. The formatter
  // is right for wrapped errors; this string just must not look like one.
  if (init.closeCode !== undefined) {
    return reason === ''
      ? `${message} (code ${init.closeCode})`
      : `${message} (code ${init.closeCode}, ${reason})`;
  }
  if (reason !== '') return `${message} (${reason})`;
  // No close detail: fall back to the underlying error's own prose so a
  // thrown constructor or a failed manifest fetch still names itself.
  if (init.cause instanceof Error && init.cause.message !== '') {
    return `${message}: ${clampString(init.cause.message)}`;
  }
  return message;
}

// RetryOnTransientCloseEntry names ONE RPC the client may re-send once
// after a transient socket close instead of rejecting it.
//
// This is a seam, not a policy. A blanket "retry on disconnect" is
// forbidden by construction: an RPC that reached the backend may have
// executed, so retrying a lost ANSWER would duplicate the ACTION —
// exactly the crash-equivalence that `isTransportClassError` exists to
// warn callers about. An entry is admissible only when the call is
// idempotent on the backend AND its loss falls inside a KNOWN transient
// window — the bundle-swap reconnect of docs/specs/remote-access.md §9,
// where a just-updated backend drops every socket seconds after serving
// it.
//
// Match by `methodId` for generated bindings (which never carry a name)
// or by `method` for the by-name call sites. `why` is the decision and is
// mandatory: an entry nobody can justify in one sentence does not belong.
export interface RetryOnTransientCloseEntry {
  methodId?: number;
  method?: string;
  why: string;
}

// The production allowlist. EMPTY, and that is the right answer today:
// every store that must survive a reconnect already does so through the
// suspend/re-source observable (stores/entityStore.svelte.ts), which
// re-asks for CURRENT state rather than replaying a stale request, and
// nothing has been found that needs the weaker guarantee. Declared and
// enforced-empty rather than absent so admitting the first entry is a
// one-line reviewed decision instead of a redesign.
export const RETRY_ON_TRANSIENT_CLOSE: readonly RetryOnTransientCloseEntry[] = Object.freeze([]);

// matchesRetryAllowlist answers whether a dispatch spec is on `list`.
// Consulted once per RPC at DISPATCH time, never on the close path, so
// the empty production list costs one length check — and, decisively, so
// a non-retryable call never retains its frame (see Pending.retry).
function matchesRetryAllowlist(
  list: readonly RetryOnTransientCloseEntry[],
  spec: { methodId?: number; method?: string },
): boolean {
  if (list.length === 0) return false;
  for (const entry of list) {
    if (entry.methodId !== undefined && entry.methodId === spec.methodId) return true;
    if (entry.method !== undefined && entry.method === spec.method) return true;
  }
  return false;
}

// TransportError wraps a server-side FrameError. The `code` is exposed
// so callers can branch on the stable token strings declared in
// internal/transport/frame.go (method_not_found, bad_params,
// temporarily_unavailable, etc).
export class TransportError extends Error {
  code: string;
  // reason is set only on code 'auth_failed' and names which credential
  // check refused the call (internal/identity's closed set). Kept off the
  // message because the message is generic prose for non-loopback callers,
  // so it is the only thing a hint can be derived from — see
  // ./authReason.ts, which is the one place it is translated.
  reason?: string;
  // scope is set only on code 'scope_required' and names the capability
  // this session was not granted (./scopes.ts's set). Same shape and same
  // rule as reason: prose is redacted for a non-loopback caller, so the
  // field is the whole answer, and ./scopeRefusal.ts is the one place it
  // becomes a sentence.
  scope?: string;
  constructor(code: string, message: string, reason?: string, scope?: string) {
    super(message);
    this.name = 'TransportError';
    this.code = code;
    this.reason = reason;
    this.scope = scope;
  }
}

// Pending tracks an outstanding RPC. The timer is cleared on settle so
// the timeout doesn't fire after the response arrives.
interface Pending {
  resolve: (value: unknown) => void;
  reject: (reason: unknown) => void;
  timer: ReturnType<typeof setTimeout>;
  // The frame to re-send if a TRANSIENT close kills this call, or null —
  // which is every call under the empty production allowlist. Holding it
  // costs a reference to the params the caller already owns, so it is
  // populated only for allowlisted calls: an ordinary RPC's arguments
  // (a full prompt, an attachment manifest) stay collectable the moment
  // the send completes. Nulled when the one retry is spent, so a call
  // cannot be re-sent twice.
  retry: ClientRPCFrame | null;
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
// Two states are TERMINAL: the backend answered, the answer will not
// change while this page sits there, and the automatic ladder stops
// rather than burn a device's radio and battery on attempts that cannot
// succeed. They differ in what the person has to do, which is why they
// are two.
//
// 'unauthorized' means the backend positively refused our bootstrap
// credential (BootstrapRejectedError — internal/transport/server.go
// handleBootstrap answers an unrecognised credential with 404) AND this
// session has no way to obtain a fresh one because it was served over
// the network. The page cookie is minted per backend launch, so a
// LAN/remote client whose backend restarted holds one that will be
// refused identically forever. Recovery is re-opening the share link —
// a fresh page load with a fresh ticket.
//
// 'pairing-required' means the opposite half: the manifest SERVES, so
// the credential is fine, but this page's socket would arrive at the
// backend as an off-host peer and this browser holds no paired session
// to name on the upgrade — which that backend refuses (spec §4 "Local
// clients", internal/transport/AGENTS.md). Dialing would produce one
// unfingerprintable 404 per attempt, so the ladder does not start.
// Recovery is pairing this device.
//
// The banner's Retry works out of both (see triggerReconnect), which is
// what recovers a refusal that was a lie from something in the path, and
// what picks up a pairing completed in another tab of this browser.
// A loopback session never enters either state: the embedded webview and
// the --connect stub load a page URL minted by the shell that owns the
// backend, and their sockets reach a backend on this machine.
// 'disconnected' is the zero-value before any connect has been
// attempted; we never re-enter it once a connect cycle starts (we stay
// in 'reconnecting' across attempts) because a still-running loop must
// not present itself as settled.
export type TransportStatus =
  | 'connected'
  | 'reconnecting'
  | 'unauthorized'
  | 'pairing-required'
  | 'disconnected';

/**
 * The subset of TransportStatus the automatic ladder stops on. Exported
 * because ./connectionRefusal.ts is the one module that phrases them and
 * has to be exhaustive over exactly this set.
 */
export type TerminalTransportStatus = Extract<
  TransportStatus,
  'unauthorized' | 'pairing-required'
>;

// TerminalLatch is what the client holds while the ladder is stopped:
// which terminal state, and the sentence every rejection issued from
// under it carries. The message is stored rather than rebuilt per
// rejection because ~150 call sites report a failure as `err.message`
// and they must all say the same thing about the same latch.
interface TerminalLatch {
  status: TerminalTransportStatus;
  /** What a caller awaiting the transport is told. */
  message: string;
  /** The refusal that produced the latch, when there was one. */
  cause?: unknown;
}

// TransportHello is what the connection's opening frame told us about
// the backend on the other end, plus the one thing only the client can
// compute: the clock skew between the two machines.
//
// Null until a hello arrives, which is also the steady state against a
// backend too old to send one. Consumers must read that as "advertises
// nothing" and degrade, never as "assume the feature is there" — which
// is why `hasCapability` is the only accessor and there is deliberately
// no version comparison anywhere in the client.
export interface TransportHello {
  /** The backend's wire dialect. Recorded for logs and bug reports;
   *  nothing branches on it (docs/specs/remote-access.md §9). */
  protocolVersion: number;
  /** Behaviors this backend advertises. Possibly empty. */
  capabilities: readonly string[];
  /** Backend identity, or '' when the store had not opened yet. Empty
   *  means unknown and must never be treated as a wildcard. */
  backendId: string;
  /** The backend's wall clock when it accepted this connection, in Unix
   *  millis. */
  serverTimeMs: number;
  /** serverTimeMs minus the client's clock at receipt. Positive means
   *  the backend is ahead. Captured here because it is only measurable
   *  at the instant the frame lands, and a signed-credential failure
   *  from clock skew is undebuggable without it. Includes one-way
   *  network latency, so it is an indication, not a measurement. */
  clockSkewMs: number;
}

export interface TransportStatusSnapshot {
  status: TransportStatus;
  /** Wall-clock millis when the next reconnect attempt fires. null when
   *  the attempt is already in flight or no attempt is scheduled. */
  nextAttemptAt: number | null;
}

type StatusHandler = (snapshot: TransportStatusSnapshot) => void;
type HelloHandler = (hello: TransportHello | null) => void;

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
// bootstrap without poking at window.location. Every fetch is a real
// round-trip to /bootstrap.json on the page's own origin — there is no
// short-circuit for any mode — which is what makes the refetch after a
// run of failures able to observe that the credential is no longer
// honoured.
type BootstrapFetcher = () => Promise<Bootstrap>;

interface WSClientOptions {
  // For tests: a constructor that yields a fake WSLike.
  WebSocketCtor?: WSConstructor;
  // For tests: a function returning the bootstrap manifest.
  bootstrap?: BootstrapFetcher;
  // For tests: skip the auto-connect on first call so a test can drive
  // events into an explicit harness instead.
  autoConnect?: boolean;
  // For tests: override the "was this page served over loopback?" probe
  // that gates the terminal 'unauthorized' state. Production reads
  // window.location through bootstrap.ts's pageServedOverLoopback.
  loopbackOrigin?: () => boolean;
  // For tests: override MAX_FRAME_BYTES. Production code MUST NOT pass
  // this — the cap matters as a defence and the symmetry with the
  // server's DefaultReadLimit is the contract. Tests pass a small
  // value (~4 KiB) so the oversized-frame regression case can be
  // exercised without allocating tens of MiB per run.
  maxFrameBytes?: number;
  // For tests: override the retry-on-transient-close allowlist. The
  // production list is empty by design (RETRY_ON_TRANSIENT_CLOSE), so
  // the retry path would otherwise be unreachable and untestable — and
  // an untested seam will not work the day someone needs it. Production
  // code MUST NOT pass this.
  retryOnTransientClose?: readonly RetryOnTransientCloseEntry[];
}

// ChannelCursor is one channel's seq bookkeeping: the last seq we
// accepted plus the connection generation that observation belongs to.
// Both are read on the event hot path and must move together — two
// parallel maps would be one forgotten write away from a cursor whose
// epoch lies — so they live in one mutable record per channel.
//
// `epoch` is what scopes drop detection to a SINGLE connection. Across a
// reconnect the server's replay answer is the authority on what was
// missed: it either delivers the missed events, sends an explicit
// `gap:true` marker, or deliberately sends nothing (channels excluded
// from ring retention, and whole-state channels whose newest frame
// supersedes everything before it — see internal/transport/
// event_visibility.go). In those last cases the next live frame's seq
// legitimately jumps far past our cursor, and treating that as a drop
// would fire a spurious resync on every reconnect. Within one
// connection there is no such ambiguity: every event on a visible
// channel is delivered or dropped, so a skip IS a drop.
interface ChannelCursor {
  seq: number;
  epoch: number;
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
  private readonly probeLoopbackOrigin: () => boolean;
  private readonly retryAllowlist: readonly RetryOnTransientCloseEntry[];

  // Cached bootstrap. The socket URL is the manifest's wsUrl verbatim:
  // it names this page's own origin, so the session cookie authenticates
  // the upgrade with nothing added to the URL.
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
  // The terminal latch, null when the ladder is running (see
  // enterTerminal). While set, the automatic reconnect ladder is stopped
  // and the status reads whichever terminal state latched it. Cleared
  // only by an explicit triggerReconnect (the banner's Retry) or by
  // evidence that the condition has lifted — never by the passive loop,
  // which is the whole point.
  //
  // One field rather than one flag per state: every reader asks "is the
  // ladder stopped, and what do I tell the caller", and a second boolean
  // would be a second thing each of the five call sites could forget.
  private terminal: TerminalLatch | null = null;
  // Date.now() of the current socket's open, 0 when no socket is open.
  // The backoff ladder resets from this at close (see
  // BACKOFF_RESET_AFTER_MS) rather than on open.
  private connectedAt = 0;

  // The sink (wired by frontendErrorCapture at install) persists one
  // summary line per outage into the always-on ui-trace error log.
  // `message` is a fixed string (it is the dedupe signature on the
  // other side); the varying numbers travel in `detail`.
  private diagnosticsSink: ((message: string, detail?: string) => void) | null = null;
  private outage: OutageRecord | null = null;

  // The most recent socket `error` event, captured per attempt so the
  // close that follows it can name what killed the connection. The
  // browser event carries no detail by spec, but a `--connect` shell and
  // the test fake both deliver a real Error here, and preserving it is
  // the difference between "socket closed (code 1006)" and a cause.
  private lastSocketError: unknown = null;

  // RPC state.
  private readonly pending = new Map<string, Pending>();
  // Frames held across a transient close for their one allowed re-send
  // (see RetryOnTransientCloseEntry). Always empty under the empty
  // production allowlist. Their Pending entries stay in `pending` — the
  // calls are still outstanding and their RPC timeout still bounds the
  // wait — so the queue holds only what to re-send, never a second
  // settlement path.
  private readonly retryQueue: ClientRPCFrame[] = [];
  private readonly subscribers = new Map<string, Set<EventHandler>>();
  private readonly statusHandlers = new Set<StatusHandler>();
  private readonly helloHandlers = new Set<HelloHandler>();
  // The most recent hello. Survives a disconnect on purpose: the same
  // backend is what the ladder is trying to reach, so clearing it would
  // make every capability read flap to "unsupported" for the length of
  // an outage and back. A reconnect to a DIFFERENT backend overwrites it
  // with that backend's frame, which is the only case where the old
  // answer was wrong.
  private helloSnapshot: TransportHello | null = null;

  // Forward-tolerance accounting: how much wire input this build could
  // not address, and of what kinds. See noteUnknownInput.
  private unknownInputTotal = 0;
  private readonly unknownInputKinds = new Map<string, number>();
  private statusSnapshot: TransportStatusSnapshot = {
    status: 'disconnected',
    nextAttemptAt: null,
  };

  // Per-channel cursor, replayed to the server on reconnect. Map
  // iteration order is insertion-ordered, so we evict the oldest entry
  // once we hit MAX_REPLAY_CHANNELS — the cap mirrors the server's own
  // clamp and stops a hostile remote from blowing the wire frame.
  private readonly lastSeqByChannel: Map<string, ChannelCursor> = new Map();
  // The channel currently at lastSeqByChannel's insertion-order tail —
  // lets recordChannelSeq skip the LRU delete/re-insert for the common
  // consecutive-events-on-one-channel case. Only recordChannelSeq
  // mutates the map, so the hint cannot go stale.
  private lastSeqTailChannel: string | null = null;
  // Bumped by every socket open. Stamped onto a channel's cursor when we
  // accept an event, so handleEventEntry can tell "this channel's last
  // event arrived on the connection I am on now" (a forward skip is a
  // drop) from "…on a previous one" (the replay answer already settled
  // what was missed). See ChannelCursor.
  private connectionEpoch = 0;
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
    this.probeLoopbackOrigin = opts.loopbackOrigin ?? pageServedOverLoopback;
    this.retryAllowlist = opts.retryOnTransientClose ?? RETRY_ON_TRANSIENT_CLOSE;
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

  /** What the backend said about itself in its hello frame, or null if
   *  none has arrived (including against a backend too old to send one). */
  getHello(): TransportHello | null {
    return this.helloSnapshot;
  }

  /**
   * Whether the attached backend advertises `capability`.
   *
   * This is the ONLY sanctioned compatibility question. No hello, or an
   * unrecognised name, answers false, so a feature degrades instead of
   * being attempted against a backend that cannot serve it. There is
   * deliberately no protocol-version comparison to reach for: version
   * gating guesses, flags ask (docs/specs/remote-access.md §9).
   *
   * Never an authorization check. The backend re-checks every RPC; a
   * flag says the behavior EXISTS, not that this caller may use it.
   */
  hasCapability(capability: string): boolean {
    return this.helloSnapshot?.capabilities.includes(capability) ?? false;
  }

  /**
   * Subscribe to hello changes. Fires synchronously with the current
   * value, then whenever a connection reports a different one.
   */
  onHelloChange(handler: HelloHandler): () => void {
    this.helloHandlers.add(handler);
    handler(this.helloSnapshot);
    return () => {
      this.helloHandlers.delete(handler);
    };
  }

  /**
   * Force a reconnect attempt immediately. Cancels any queued backoff
   * timer and kicks off a fresh connect. Safe to call from a UI button
   * — when an attempt is already in flight, this is a no-op.
   *
   * This is also the manual escape hatch out of both terminal latches.
   * Un-latching on an explicit user action keeps the stop-the-ladder
   * decision about the AUTOMATIC loop: one attempt per click can't storm
   * anything, and if the refusal was a lie told by something in the path
   * (a proxy 404-ing while the backend was down, a pairing completed in
   * another tab of this browser) this is what recovers. A condition that
   * still holds re-latches within one round trip.
   */
  triggerReconnect(): void {
    if (this.closed) return;
    this.clearTerminal();
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
   * Re-dial so the upgrade names the session this browser just paired.
   * Called once, when the pairing flow completes: any socket opened
   * before the credential existed dialed without a ticket, so its
   * upgrade named whatever the page cookie did — on a browser that also
   * holds the local page cookie, that is the LOCAL channel, and
   * revoking the new device would never reach it. Closing the socket
   * (or the attempt in flight) routes through the ordinary reconnect
   * path, whose next dial mints a ticket.
   */
  redialAfterPairing(): void {
    // The credential that just landed published the grants it carries, and
    // this page's screens key off them. Re-read before the dial rather
    // than after: the pairing screen unmounts into the ordinary app on the
    // same tick, and a surface that mounted against the pre-pairing answer
    // would sit disabled until something else happened to invalidate it.
    refreshGrantedScopes();
    if (this.closed) return;
    this.clearTerminal();
    this.reconnectAttempt = 0;
    if (this.queuedAttempt !== null) {
      // A queued attempt re-evaluates the session store when it dials —
      // run it now rather than waiting out a backoff that was priced
      // for a failure, not for a credential that just arrived.
      this.queuedAttempt.fire();
      return;
    }
    if (this.connectPromise !== null) {
      // An attempt is in flight and may already be past its mint stage.
      // Let it settle, then close whatever socket it produced; the
      // close event drives the re-dial.
      void this.connectPromise.then(
        () => {
          try {
            this.ws?.close();
          } catch {
            // ignore — already closing; the close event still fires.
          }
        },
        () => {
          // A failed attempt scheduled its own retry.
        },
      );
      return;
    }
    if (this.ws !== null && this.ws.readyState === WS_OPEN) {
      try {
        this.ws.close();
      } catch {
        // ignore — already closing; the close event drives the re-dial.
      }
      return;
    }
    void this.ensureConnected().catch((err) => {
      console.warn('wsClient: redialAfterPairing failed', err);
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
    // A hidden renderer may throttle both this interval and WebSocket message
    // delivery, so silence there says nothing about the socket. The visibility
    // resume path refreshes lastFrameAt before verdicts resume.
    if (documentHidden()) return;
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
    // Terminal by construction: a closed client runs no ladder, so a
    // call parked for a transient re-send is settled here alongside the
    // rest rather than stranded in the retry queue.
    this.retryQueue.length = 0;
    const closedErr = new DisconnectedError('client closed', { terminal: true });
    for (const pending of this.pending.values()) {
      clearTimeout(pending.timer);
      pending.reject(closedErr);
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
      // Allowlist resolved HERE, once, so a call that will never be
      // retried does not pin its params for the RPC's lifetime.
      const retry = matchesRetryAllowlist(this.retryAllowlist, spec) ? frame : null;
      this.pending.set(id, { resolve, reject, timer, retry });

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
    if (this.terminal !== null) {
      // Terminal: refuse without touching the network. Passive demand
      // (a background poll, a subscribe from a remounting pane) must not
      // turn the stopped ladder back into one fetch per caller.
      return Promise.reject(
        new DisconnectedError(this.terminal.message, {
          cause: this.terminal.cause,
          terminal: true,
        }),
      );
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
      // A refused credential is not a transient failure. For a session
      // that can't mint a new token it is terminal — latch it BEFORE
      // scheduleReconnect below, which is what reads the latch and
      // declines to queue another attempt.
      if (err instanceof BootstrapRejectedError && this.isRemoteSession()) {
        this.enterCredentialDead(err);
      }
      console.warn('wsClient: bootstrap failed', err);
      // Bootstrap-stage failures count toward the outage's attempt
      // tally too (when one is open), so the reconnect summary reflects
      // server-unreachable retries and not just WS-stage deaths.
      if (this.outage !== null) this.outage.attempts += 1;
      // Re-raise so the awaiter sees the rejection, but also kick off a
      // reconnect so a transient bootstrap failure recovers without
      // requiring fresh user input.
      this.scheduleReconnect();
      // Wrapped, not re-thrown raw. A failed manifest fetch rejects with
      // whatever fetch threw (a bare TypeError, "Failed to fetch"), and
      // that is a TRANSPORT failure that isTransportClassError could not
      // recognise — so a caller with side effects read it as a definite
      // "nothing happened" when the request may in fact never have been
      // sent. Wrapping puts every connect-stage failure in the one class
      // callers classify on, with the original preserved as `cause`.
      throw new DisconnectedError('transport unreachable', {
        cause: err,
        terminal: this.terminal !== null,
      });
    }
    // Both terminal conditions are decided here, against the manifest
    // that just landed, and in this order. The pairing rule is asked
    // FIRST so a page that is going to latch on it never publishes a
    // moment of 'reconnecting' on the way: clearTerminal below is
    // evidence that a latched condition has lifted, and for an unpaired
    // networked page it has not.
    if (this.pairingRequired(bootstrap.remote === true)) {
      this.connectPromise = null;
      this.enterPairingRequired();
      throw new DisconnectedError('this backend admits paired devices only', {
        terminal: true,
      });
    }
    // A manifest in hand means the credential was accepted, and the
    // pairing rule just answered no, so a latched refusal is history.
    // Republishing here rather than at socket-open means the banner stops
    // naming a cause that no longer holds as soon as we have the
    // evidence.
    this.clearTerminal();
    // A PAIRED device holds its session credential in script (it arrived
    // in the /auth/pair response body, not as a cookie), so the upgrade
    // names its session through the single-use ticket instead
    // (docs/specs/remote-access.md §4). Minted fresh per attempt — a
    // ticket lives seconds and is spent whether or not the upgrade
    // succeeds. Runs before the closed check so a close() during the
    // mint still stops the attempt. The unpaired path (every embedded
    // and local page: their cookie rides the upgrade by itself) stays
    // fully synchronous — no awaited microtask is added to every
    // ordinary dial.
    let dialTicket: string | null = null;
    if (hasPairedSession()) {
      dialTicket = await mintDialTicket();
      if (dialTicket === null && hasPairedSession()) {
        // No ticket, session still held: the mint could not prove the
        // stored session right now (endpoint unreachable, or the owner
        // has not confirmed the pairing yet) and did NOT conclude it is
        // dead — that verdict clears the store, and the next attempt
        // dials unpaired. Dialing anyway would let a page cookie this
        // browser may also hold ride the upgrade and admit this screen
        // as the LOCAL channel — a socket that revoking this device
        // never reaches. Fail the attempt instead; the ladder retries.
        this.connectPromise = null;
        if (this.outage !== null) this.outage.attempts += 1;
        this.scheduleReconnect();
        throw new DisconnectedError('paired session has no dial ticket');
      }
    }
    if (this.closed) {
      this.connectPromise = null;
      throw new DisconnectedError('client closed', { terminal: true });
    }
    // No credential is appended: the upgrade is same-origin, so the
    // browser attaches the session cookie itself. Non-browser clients
    // (the harness client, the e2e suite) present the session token as
    // a query parameter against the same validation instead.
    //
    // What IS appended is this screen's identity, so bound methods can
    // attribute a write and so this client can recognize the echo of its own
    // change. It rides the URL rather than a post-open frame because it has to
    // be in place before the first RPC lands: a draft saved in the window
    // before a handshake completed would echo back into the composer that
    // typed it. Both ids are opaque and the backend re-validates their shape.
    let url = withClientIdentity(bootstrap.wsUrl);
    if (dialTicket !== null) {
      const withTicket = new URL(url);
      withTicket.searchParams.set('ticket', dialTicket);
      url = withTicket.toString();
    }

    // Each attempt starts with no known socket-level cause; a stale one
    // from the previous socket must never be attributed to this close.
    this.lastSocketError = null;

    return await new Promise<void>((resolve, reject) => {
      const attempt: ConnectAttempt = { settled: false, resolve, reject };
      let ws: WSLike;
      try {
        ws = new this.WebSocketCtor(url);
      } catch (err) {
        this.connectPromise = null;
        // Same wrapping rationale as the bootstrap path: a thrown
        // constructor (a malformed URL, a blocked scheme) is a transport
        // failure and must classify as one.
        reject(new DisconnectedError('socket could not be opened', { cause: err }));
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
        //
        // Retained as the close's `cause`: on the browser the event
        // carries no detail, but a `--connect` shell and the test fake
        // deliver a real Error here, and the close code alone (1006 for
        // every abnormal end) cannot distinguish them.
        if (this.ws === ws) this.lastSocketError = errEv;
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
    // New connection generation: every cursor carried across the
    // reconnect now reads as "observed on a previous connection", so the
    // seq jump the replay answer may legitimately produce on this
    // channel isn't mistaken for a mid-connection drop (see ChannelCursor).
    this.connectionEpoch += 1;
    // NOT a backoff reset: reaching OPEN only proves the handshake
    // succeeded, and an accept-then-close server would pin the ladder
    // at its floor. handleSocketClose resets it once the connection
    // proved stable (BACKOFF_RESET_AFTER_MS).
    this.connectedAt = Date.now();
    // First-frame after open: replay any missed events. The server
    // only acts on this if the map is non-empty; it's still cheap
    // to send unconditionally since channel-by-channel reconciliation
    // is exactly what a reconnect needs.
    // Scoped by launch id: the checkpoint is a per-tab dedup for
    // notification replay, and a backend that restarted numbers its
    // events from scratch, so a checkpoint from the previous launch must
    // not suppress the new one's replay.
    const scope = bootstrap.launchId ?? '';
    const replay: Record<string, number> = {};
    // The zero-seeded notification cursor is a LOCAL cold-launch
    // mechanism, not a general one: it exists because a Windows toast can
    // start the desktop window, so the click that launched it landed
    // before this page had a socket to hear it on. A remote page was not
    // launched by a toast on that host, so asking for the channel's
    // retained ring would hand it every activation the desk has produced
    // since boot — and the queue on the other end OPENS each one, which
    // would walk a phone's panes through all of them on every fresh
    // attach. It asks for nothing here and receives live activations
    // only; the ordinary cursor loop below still replays what it actually
    // missed across a reconnect. Scope stays null for the same reason: a
    // checkpoint nothing reads back is only writes to sessionStorage per
    // click.
    if (!this.isRemoteSession(bootstrap.remote === true)) {
      replay[NOTIFICATION_ACTIVATED_CHANNEL] = loadNotificationActivationSeq(scope);
      this.notificationCheckpointScope = scope;
    } else {
      this.notificationCheckpointScope = null;
    }
    this.notificationReplayPending = true;
    this.notificationReplayBuffer = [];
    for (const [channel, cursor] of this.lastSeqByChannel) {
      replay[channel] = cursor.seq;
    }
    this.sendFrame({
      type: 'replay',
      lastSeqByChannel: replay,
    });
    this.flushRetryQueue();
    this.connectPromise = null;
    this.preOpenFailures = 0;
    this.startStaleWatchdog();
    this.setStatus({ status: 'connected', nextAttemptAt: null });
    if (this.outage !== null) {
      const downSeconds = ((Date.now() - this.outage.startedAt) / 1000).toFixed(1);
      const detail =
        `down ${downSeconds}s, close code ${this.outage.closeCode}, ${this.outage.attempts} failed attempts`;
      // Console too, not just the sink: remote clients can't persist
      // through ReportFrontendErrorBatch (host-scoped), and the console
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
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      // Rate-limited, and NOT an error-level log: a peer emitting
      // unparseable frames would otherwise write one console line per
      // frame, and console spam during a wire problem buries the one
      // line that explains it.
      this.noteUnknownInput('unparseable');
      return;
    }
    // A frame is an object with a string `type`. Anything else — a JSON
    // primitive, an array, null — is not addressable by this client and
    // is counted rather than thrown on. `null` in particular would make
    // every property read below throw.
    if (typeof parsed !== 'object' || parsed === null || Array.isArray(parsed)) {
      this.noteUnknownInput('non-object');
      return;
    }
    const type = (parsed as { type?: unknown }).type;
    if (typeof type !== 'string') {
      this.noteUnknownInput('untyped');
      return;
    }
    this.handleFrame(parsed as ServerFrame);
  }

  // handleSocketClose tears down after a socket dies: outage
  // bookkeeping, pending-RPC rejection, bootstrap-cache invalidation,
  // attempt settlement, and the reconnect schedule. A superseded
  // socket's close only settles its own attempt — the live socket's
  // state is not its to touch.
  private handleSocketClose(ws: WSLike, ev: CloseEvent, attempt: ConnectAttempt): void {
    if (this.ws !== ws) {
      attempt.settled = true;
      attempt.reject(new DisconnectedError('socket superseded', {
        closeCode: ev.code,
        closeReason: ev.reason,
      }));
      return;
    }
    this.stopStaleWatchdog();
    this.serverSendsHeartbeats = false;
    // Backoff reset on STABILITY, not on open: a connection that
    // served for BACKOFF_RESET_AFTER_MS proves the far side is
    // healthy, so its eventual drop deserves a fresh ladder. A socket
    // that opened and died immediately keeps climbing — that is the
    // accept-then-close storm the ladder exists for.
    const connectedFor = this.connectedAt === 0 ? 0 : Date.now() - this.connectedAt;
    this.connectedAt = 0;
    if (connectedFor >= BACKOFF_RESET_AFTER_MS) {
      this.reconnectAttempt = 0;
    }
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
    // app layer) — except the explicitly allowlisted few, which
    // failPending holds for one re-send.
    //
    // The close code, the peer's reason, and any preceding socket error
    // ride the rejection: without them every caller in the app reports
    // the same "socket closed" for a relay teardown, a backend restart,
    // and a policy close, and the first question of any triage — WHY did
    // the wire go — has no answer left in the UI or the error log.
    this.failPending(new DisconnectedError('socket closed', {
      closeCode: ev.code,
      closeReason: ev.reason,
      cause: this.lastSocketError ?? undefined,
      terminal: this.terminal !== null,
    }));
    this.notificationReplayPending = false;
    this.notificationReplayBuffer = [];
    this.ws = null;
    if (!attempt.settled) {
      this.outage.attempts += 1;
      this.preOpenFailures += 1;
      if (this.preOpenFailures >= BOOTSTRAP_INVALIDATE_AFTER_FAILURES && this.bootstrap !== null) {
        // Consecutive attempts died before OPEN: the cached bootstrap
        // may be stale — a restarted backend mints a new credential, and
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
      attempt.reject(new DisconnectedError('socket closed before open', {
        closeCode: ev.code,
        closeReason: ev.reason,
        cause: this.lastSocketError ?? undefined,
        terminal: this.terminal !== null,
      }));
    }
    if (this.closed) return;
    this.setReconnecting(null);
    this.scheduleReconnect();
  }

  // isRemoteSession reports whether this session would have to be handed
  // a brand-new credential to reconnect after a backend restart. Two
  // independent signals, OR'd because either one alone leaves a real
  // case uncovered:
  //
  //   - `remoteBackend`, from the last manifest the server served us
  //     (`remote` is the server's own pre-upgrade loopback verdict —
  //     internal/transport/server.go handleBootstrap). Covers a session
  //     that connected fine and then lost its backend.
  //   - a non-loopback document origin. Covers the session that NEVER
  //     connected: a bookmarked share link opened against a rebooted
  //     backend has no manifest to have learned `remote` from.
  //
  // A session tunnelled over SSH satisfies neither (both ends read as
  // loopback, exactly as `loopback.PeerAddress` sees it) and keeps the
  // ordinary retry loop — honest-but-vague beats claiming a share link
  // that may not exist.
  //
  // `remoteBackend` defaults to the latched field, which is what the
  // failure-reporting callers want ("what do we currently believe"). A
  // caller holding the manifest that produced one specific socket passes
  // its verdict instead, so a superseded late fetch cannot decide a
  // question about a different connection.
  private isRemoteSession(remoteBackend: boolean = this.remoteBackend): boolean {
    return remoteBackend || !this.probeLoopbackOrigin();
  }

  // pairingRequired reports that this page's socket would arrive at the
  // backend as an off-host peer while this browser holds no paired
  // session to name on the upgrade — which that backend refuses (spec §4
  // "Local clients"). Asked BEFORE dialing, because the refusal is an
  // unfingerprintable 404 the browser surfaces as a bare 1006: dialing
  // would buy no information and cost one doomed socket per attempt.
  //
  // The AND of the two signals isRemoteSession ORs, and each term
  // excludes a case the other admits wrongly:
  //
  //   - `remote` is the backend's own pre-upgrade loopback verdict on
  //     THIS page's peer (handleWS captures it with the same predicate),
  //     so it is the exact mirror of the rule. It alone would be wrong
  //     for a `--connect` stub page, whose manifest sets `remote` from
  //     the UPSTREAM endpoint while the page's own socket goes to the
  //     stub on this machine.
  //   - a non-loopback document origin, which is false for exactly that
  //     stub page (the stub binds loopback only) and for the embedded
  //     webview. It alone would be wrong for Tailscale Serve or a
  //     same-host proxy, where the page origin is a public name and the
  //     backend still sees a loopback peer.
  private pairingRequired(remoteBackend: boolean): boolean {
    return remoteBackend && !this.probeLoopbackOrigin() && !hasPairedSession();
  }

  // enterCredentialDead latches the terminal state for a refused
  // credential. The backend answered and refused this session's
  // credential, and this session cannot produce another one, so every
  // further attempt is structurally doomed: retrying would be a
  // battery-burning loop against a server that will refuse each attempt
  // identically.
  private enterCredentialDead(err: BootstrapRejectedError): void {
    this.enterTerminal(
      {
        status: 'unauthorized',
        message: 'backend refused this session credential; reopen the share link',
        cause: err,
      },
      `wsClient: backend refused this session's credential (${err.message}); ` +
        'reconnect stopped — the credential is minted per backend launch, so ' +
        'only a freshly-opened share link can restore this session',
    );
  }

  // enterPairingRequired latches the other terminal state. The manifest
  // served, so nothing is wrong with the credential; the socket is what
  // this backend will not open for an unpaired off-host device.
  private enterPairingRequired(): void {
    this.enterTerminal(
      {
        status: 'pairing-required',
        message: 'this backend admits paired devices only',
      },
      'wsClient: this backend admits paired devices only and this browser holds ' +
        'no paired session; reconnect stopped until one is paired',
    );
  }

  // enterTerminal is the ONLY place the automatic reconnect ladder is
  // stopped.
  //
  // The latch is deliberately not self-clearing — no timer un-sets it —
  // because nothing about waiting makes a per-launch credential valid or
  // pairs a device. It clears on exactly two events, both of them
  // evidence rather than hope: a manual triggerReconnect (bounded by the
  // user's finger) and a connect attempt that gets past the condition.
  //
  // A latch of a DIFFERENT status replaces the one held, because the two
  // conditions are answered by different evidence and the newer answer is
  // the one that just came off the wire.
  //
  // Leaves any queued attempt alone: whatever settles that attempt's
  // promise is what awaiting RPCs are parked on, and the attempt itself
  // re-enters this path and stops there.
  private enterTerminal(latch: TerminalLatch, log: string): void {
    if (this.terminal?.status === latch.status) return;
    this.terminal = latch;
    console.warn(log);
    // Nothing is coming back, so a call parked for its one transient
    // re-send has nothing left to wait for. Release it with the refusal
    // as its cause instead of letting it sit out the RPC timeout against
    // a ladder that will never run again.
    this.releaseRetryQueue(new DisconnectedError(latch.message, {
      cause: latch.cause,
      terminal: true,
    }));
    this.setReconnecting(null);
  }

  // clearTerminal is the single un-latch. Both callers are
  // evidence-driven — a connect attempt that got past the condition, or
  // a user asking for one more attempt — and both want the terminal
  // message to stop immediately rather than linger until a socket opens.
  private clearTerminal(): void {
    const held = this.terminal;
    if (held === null) return;
    this.terminal = null;
    if (this.statusSnapshot.status === held.status) this.setReconnecting(null);
  }

  // setReconnecting publishes a between-connections status. The terminal
  // latch wins over anything a caller asks for, and carries no
  // nextAttemptAt — there is no next attempt to count down to.
  private setReconnecting(nextAttemptAt: number | null): void {
    if (this.terminal !== null) {
      this.setStatus({ status: this.terminal.status, nextAttemptAt: null });
      return;
    }
    this.setStatus({ status: 'reconnecting', nextAttemptAt });
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
    if (this.terminal !== null) {
      // Terminal (see enterTerminal): no timer, no ladder, no countdown
      // — just the state that says what the user has to do.
      this.setReconnecting(null);
      return;
    }
    if (this.queuedAttempt !== null) {
      // A reconnect is already queued — let it run; doubling up would
      // drop the pending close-handler attempt and risk synchronising
      // multiple reconnects on a flaky socket.
      return;
    }
    if (this.connectPromise !== null) {
      // An attempt is already in flight — its settlement owns the next
      // step (every failure path nulls connectPromise before it
      // reschedules, and success makes this schedule moot). This arm is
      // reachable when a dying socket's close event lands during the
      // pre-socket stage of a fresh connect: queuing beside that
      // attempt would dial a SECOND socket, and the first one — already
      // past 'open' by then — never re-fires the event the supersede
      // guard reaps on, so both would stay attached.
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
    this.setReconnecting(nextAttemptAt);
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
          reject(new DisconnectedError('client closed', { terminal: true }));
          return;
        }
        // Switch to "in-flight attempt" — clear nextAttemptAt so the UI
        // stops counting down while the connect promise resolves.
        this.setReconnecting(null);
        this.connect().then(resolve, reject);
      };
      this.queuedAttempt = { timer: setTimeout(fire, delay), fire };
    });
    this.connectPromise = promise;
    // Swallow rejections on this branch — see comment above.
    promise.catch(() => {});
  }

  // getBootstrap caches the manifest fetch so a reconnect doesn't re-hit
  // /bootstrap.json on every attempt. The cache is NOT permanent, and
  // must not be: the credential is bound to one server launch, so a client
  // that keeps replaying a cached manifest at a restarted backend never
  // learns it has been refused. handleSocketClose drops the cache every
  // BOOTSTRAP_INVALIDATE_AFTER_FAILURES consecutive pre-open failures,
  // which is what turns a doomed reconnect loop into the observable
  // refusal that latches the terminal state. A rejected fetch is not
  // cached either: nulling the promise on rejection lets the next
  // reconnect retry instead of permanently re-throwing the cached error.
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
          pending.reject(new DisconnectedError('send failed', { cause: err }));
        }
      }
    }
  }

  // handleFrame routes a parsed server frame. RPC responses are matched
  // by id; event pushes fan out to subscribers; batch frames iterate
  // their event array through the same per-event path.
  //
  // FORWARD TOLERANCE (docs/specs/remote-access.md §9): an unknown frame
  // type is ignored and counted, never thrown on and never logged per
  // frame. The swap window — an old bundle live against a just-updated
  // backend for minutes — is a normal operating state, not a fault, so
  // a client of this generation must run correctly against the next
  // one's wire. Unknown FIELDS need no handling at all: nothing here
  // enumerates a frame's properties.
  private handleFrame(frame: ServerFrame): void {
    if (frame.type === 'ping') {
      // Server keepalive heartbeat. The message listener already
      // refreshed lastFrameAt; the first ping additionally proves this
      // connection provides the traffic floor the staleness watchdog
      // assumes, arming it until the socket closes.
      this.serverSendsHeartbeats = true;
      return;
    }
    if (frame.type === 'hello') {
      this.applyHello(frame);
      return;
    }
    if (frame.type === 'rpc') {
      const pending = this.pending.get(frame.id);
      if (!pending) return;
      this.pending.delete(frame.id);
      clearTimeout(pending.timer);
      if (frame.error) {
        pending.reject(
          new TransportError(
            frame.error.code,
            clampString(frame.error.message ?? ''),
            frame.error.reason,
            frame.error.scope,
          ),
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
      // A batch whose `events` is absent or not an array would throw on
      // iteration, and the throw would abandon the rest of the frame —
      // dispatching a prefix and losing the remainder is worse than
      // dropping the whole thing, because the seq cursor then lies.
      if (!Array.isArray(frame.events)) {
        this.noteUnknownInput('batch-without-events');
        return;
      }
      for (const evt of frame.events) {
        // Guard BEFORE building the ServerEventFrame shape: replay
        // buffering is live only during the brief post-reconnect window
        // and only for one rare channel, so the steady streaming state
        // (coalesced batches of up to 50 item deltas) must not pay a
        // throwaway spread copy per event just to probe it.
        if (this.notificationReplayPending && evt?.channel === NOTIFICATION_ACTIVATED_CHANNEL) {
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
      return;
    }
    // A frame type this build has never heard of. Expected, not
    // exceptional: see the forward-tolerance note above.
    //
    // The cast is the point rather than a workaround: ServerFrame
    // enumerates the types THIS build knows, so the compiler narrows to
    // `never` here and would happily let the branch read nothing. The
    // whole reason the branch exists is that the runtime wire is not
    // limited to what the type declares.
    this.noteUnknownInput((frame as { type: string }).type);
  }

  // noteUnknownInput records one piece of wire input this build cannot
  // address, and is the ONLY reaction to it.
  //
  // Counted, so the condition is observable and a future-dialect test can
  // assert on it. Rate-limited to one console line per distinct kind, at
  // debug level, because the alternative — a line per frame — turns a
  // routine version skew into console spam that buries whatever else was
  // going wrong. Never `error`: an unknown frame from a newer backend is
  // the wire working as designed, and routing it to the always-on error
  // log would fill that log with non-faults.
  private noteUnknownInput(kind: string): void {
    this.unknownInputTotal += 1;
    const label = clampString(kind, UNKNOWN_KIND_LABEL_MAX);
    const seen = this.unknownInputKinds.get(label);
    if (seen !== undefined) {
      this.unknownInputKinds.set(label, seen + 1);
      return;
    }
    // Bounded: a peer that names a new type on every frame must not be
    // able to grow this map without limit. Past the cap the total still
    // counts, only the per-kind breakdown stops.
    if (this.unknownInputKinds.size >= MAX_TRACKED_UNKNOWN_KINDS) return;
    this.unknownInputKinds.set(label, 1);
    console.debug(`wsClient: ignoring unrecognized wire input (${label})`);
  }

  /** Tally of wire input this build could not address, by kind. Read by
   *  the future-dialect tests; a running client's counters are also the
   *  quickest way to tell "skewed against a newer backend" from "broken". */
  getUnknownInputStats(): { total: number; kinds: Record<string, number> } {
    return {
      total: this.unknownInputTotal,
      kinds: Object.fromEntries(this.unknownInputKinds),
    };
  }

  // applyHello records the connection's opening frame and publishes it.
  //
  // Every field is validated rather than trusted: this is remote input,
  // and a future backend may send shapes this build has never seen. A
  // malformed field falls back to its neutral value instead of rejecting
  // the frame, because a hello we half-understand is still worth more
  // than none — and refusing it would make an additive server-side change
  // look like a backend with no capabilities at all. Unknown FIELDS are
  // ignored for free: nothing here enumerates the object.
  private applyHello(frame: ServerHelloFrame): void {
    const capabilities = Array.isArray(frame.capabilities)
      ? frame.capabilities.filter((c): c is string => typeof c === 'string')
      : [];
    const serverTimeMs = Number.isFinite(frame.serverTimeMs) ? frame.serverTimeMs : 0;
    const next: TransportHello = {
      protocolVersion: Number.isFinite(frame.protocolVersion) ? frame.protocolVersion : 0,
      capabilities,
      backendId: typeof frame.backendId === 'string' ? frame.backendId : '',
      serverTimeMs,
      clockSkewMs: serverTimeMs === 0 ? 0 : serverTimeMs - Date.now(),
    };
    const previous = this.helloSnapshot;
    this.helloSnapshot = next;
    // Publish only on a real change. A reconnect to the same backend
    // repeats the same answer except for the clock reading, and waking
    // every consumer for a few milliseconds of skew would turn a routine
    // reconnect into a re-render.
    if (previous !== null && sameHello(previous, next)) return;
    for (const handler of [...this.helloHandlers]) {
      try {
        handler(next);
      } catch (err) {
        console.warn('wsClient: hello handler threw', err);
      }
    }
  }

  // handleEventEntry processes a single event entry — used by both
  // the regular event path and the batch iteration path.
  //
  // The shape check is not defensive padding. Everything below writes
  // into `lastSeqByChannel`, and that map is echoed back to the server as
  // the replay cursor on the next reconnect: an entry keyed `undefined`
  // with a NaN seq serializes as `{"undefined": null}`, which the server
  // decodes into map[string]uint64, fails, and answers `bad_params` — so
  // one mis-shaped event from a newer backend would cost the client its
  // entire gap-recovery handshake, silently, from then on. Validate at
  // ingest and the cursor map can only ever hold real values.
  //
  // An event on a channel nobody subscribes to needs no check: dispatch
  // is subscriber-keyed, so an unrecognized channel reaches no handler
  // by construction. That is the steady state today, not an anomaly, and
  // counting it would fire constantly.
  private handleEventEntry(evt: {
    channel: string;
    seq: number;
    data: unknown;
    gap?: boolean;
  }): void {
    if (
      typeof evt !== 'object'
      || evt === null
      || typeof evt.channel !== 'string'
      || !Number.isSafeInteger(evt.seq)
      || evt.seq < 0
    ) {
      this.noteUnknownInput('event-shape');
      return;
    }
    if (evt.gap === true) {
      // A gap marker is a resync instruction, not a data event, so it
      // is honoured BEFORE the dedup check and its seq is adopted in
      // both directions. The server emits one with a seq BELOW our
      // cursor when our cursor sits above its head — a sequence space
      // that isn't ours, i.e. the backend restarted underneath us (see
      // internal/transport/AGENTS.md § Wire frames). Dropping it as a
      // duplicate would strand the cursor above the new head and
      // silently discard every live event on that channel forever.
      this.recordChannelSeq(evt.channel, evt.seq);
      console.warn(
        `wsClient: event gap on ${clampString(evt.channel)} (seq ${evt.seq})`,
      );
      this.diagnosticsSink?.(
        'transport: event gap marker received',
        `${clampString(evt.channel)} seq ${evt.seq}`,
      );
      this.dispatchToSubscribers(TRANSPORT_GAP_CHANNEL, {
        channel: evt.channel,
        seq: evt.seq,
      });
      this.dispatchToSubscribers(evt.channel, evt.data);
      return;
    }
    const cursor = this.lastSeqByChannel.get(evt.channel);
    if (cursor !== undefined && evt.seq <= cursor.seq) return;
    if (
      cursor !== undefined
      && cursor.epoch === this.connectionEpoch
      && evt.seq > cursor.seq + 1
    ) {
      // Forward skip inside one connection: the events between the two
      // seqs existed and never reached us, because the server's fanout
      // drops non-blockingly when a subscriber's buffer fills
      // (internal/transport/eventbus.go Subscriber.deliver). Nothing
      // else will announce it — the explicit `gap:true` marker only
      // covers the reconnect-replay path — so for an edge-triggered
      // channel (one frame per state change) this is the ONLY signal
      // that every consumer of that entity is now stale.
      //
      // The carried event is real data and still dispatches below: this
      // reports what came before it, not what it is.
      console.warn(
        `wsClient: dropped ${evt.seq - cursor.seq - 1} event(s) on `
          + `${clampString(evt.channel)} (seq ${cursor.seq} -> ${evt.seq})`,
      );
      this.diagnosticsSink?.(
        'transport: dropped events detected by seq skip',
        `${clampString(evt.channel)} seq ${cursor.seq} -> ${evt.seq}`,
      );
      this.dispatchToSubscribers(TRANSPORT_GAP_CHANNEL, {
        channel: evt.channel,
        seq: evt.seq,
      });
    }
    this.recordChannelSeq(evt.channel, evt.seq);
    this.dispatchToSubscribers(evt.channel, evt.data);
  }

  // recordChannelSeq updates the per-channel last-seen seq and evicts
  // the oldest entry once the cap is reached. Map iteration order in
  // V8/modern engines is insertion order, so the first entry yielded by
  // .keys() is the oldest — that's what we evict.
  private recordChannelSeq(channel: string, seq: number): void {
    const cursor = this.lastSeqByChannel.get(channel);
    if (cursor !== undefined) {
      cursor.seq = seq;
      cursor.epoch = this.connectionEpoch;
      // Re-insert so the entry moves to the tail and stays "fresh"
      // rather than aging out on the next overflow — but skip the
      // delete/re-insert when the entry already sits at the tail,
      // which is the dominant streaming pattern (back-to-back events
      // on one channel). A plain .set() on an existing key keeps its
      // position, so the LRU order is preserved exactly.
      if (channel !== this.lastSeqTailChannel) {
        this.lastSeqByChannel.delete(channel);
        this.lastSeqByChannel.set(channel, cursor);
      }
    } else {
      if (this.lastSeqByChannel.size >= MAX_TRACKED_REPLAY_CHANNELS) {
        const oldest = this.lastSeqByChannel.keys().next().value;
        if (typeof oldest === 'string') {
          this.lastSeqByChannel.delete(oldest);
        }
      }
      this.lastSeqByChannel.set(channel, { seq, epoch: this.connectionEpoch });
    }
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

  // failPending settles every outstanding RPC with one preserved cause.
  //
  // The exception is the allowlisted few (RetryOnTransientCloseEntry):
  // on a NON-terminal close they keep their Pending entry and their
  // running timeout, and their frame moves to the retry queue for one
  // re-send on the next open. A terminal failure, or a closed client,
  // rejects them like everything else — there is no next open to wait
  // for. `retry` is nulled either way, so a call gets at most one.
  //
  // One error instance is shared across the drained set on purpose: the
  // cause genuinely IS shared (one socket died), and minting N copies of
  // it would allocate per in-flight call at exactly the moment the app
  // is already doing reconnect work.
  private failPending(err: DisconnectedError): void {
    if (this.pending.size === 0) return;
    const holdForRetry = !err.terminal && !this.closed;
    const drained: Pending[] = [];
    for (const [id, p] of this.pending) {
      if (p.retry !== null && holdForRetry) {
        this.retryQueue.push(p.retry);
        p.retry = null;
        continue;
      }
      p.retry = null;
      drained.push(p);
      this.pending.delete(id);
    }
    for (const p of drained) {
      clearTimeout(p.timer);
      p.reject(err);
    }
  }

  // releaseRetryQueue settles calls parked for a re-send that will never
  // happen. Called when the ladder stops (enterTerminal) and when the
  // client shuts down, so a parked call never outlives the transport
  // that owed it an answer.
  private releaseRetryQueue(err: DisconnectedError): void {
    if (this.retryQueue.length === 0) return;
    const parked = this.retryQueue.splice(0, this.retryQueue.length);
    for (const frame of parked) {
      const pending = this.pending.get(frame.id);
      if (!pending) continue;
      this.pending.delete(frame.id);
      clearTimeout(pending.timer);
      pending.reject(err);
    }
  }

  // flushRetryQueue re-sends the parked calls on a fresh connection.
  // Runs after the replay frame so the connection's event cursor is
  // reconciled before new work lands on it. A call whose entry has since
  // gone (its RPC timeout fired during the outage) is dropped.
  private flushRetryQueue(): void {
    if (this.retryQueue.length === 0) return;
    const parked = this.retryQueue.splice(0, this.retryQueue.length);
    for (const frame of parked) {
      if (this.pending.has(frame.id)) this.sendFrame(frame);
    }
  }
}

// sameHello compares the two backends' SUBSTANTIVE answers. Clock skew
// is excluded on purpose: it differs on every connection by definition,
// so including it would defeat the change check entirely.
function sameHello(a: TransportHello, b: TransportHello): boolean {
  return a.protocolVersion === b.protocolVersion
    && a.backendId === b.backendId
    && a.capabilities.length === b.capabilities.length
    && a.capabilities.every((cap, i) => cap === b.capabilities[i]);
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
