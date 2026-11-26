// The page timers that the end-to-end suite moves a page clock across or
// bounds a wait by, and the line the page logs when its replay watchdog
// fires. Each owner reads its value from here and e2e imports this module,
// so the suite holds no copy of a product value. It imports only the
// generated method budgets, by file name, so Node loads it without the
// bundler.
import { DEFAULT_METHOD_BUDGET_MS, METHOD_ANSWER_MARGIN_MS } from './transport/methodRoutes.ts';

/** How long a toast stays when its caller names no duration (stores/toast.svelte.ts). */
export const TOAST_MS = 5_000;

/**
 * The startup read budget of a computer's catalog rows (stores/computerRows.ts)
 * and of its UI state (stores/appStorage.ts): past it, the answer commits
 * late and the boot proceeds without it.
 */
export const COMPUTER_READ_DEADLINE_MS = 2_500;

/** How long the composer waits after the last edit before it saves the draft (stores/composerDraft.svelte.ts). */
export const DRAFT_SAVE_DEBOUNCE_MS = 500;

/**
 * The age past which a person's act reads a form's held read again
 * (stores/trackerTicketWrites.svelte.ts, "A form read"): the age the Jira
 * client keeps a create screen, so a read past it reaches Jira.
 */
export const FORM_READ_AGE_MS = 60_000;

// The socket's timers (transport/wsClient.ts).
//
// RPC_TIMEOUT_MS is the wait for a method that declares no budget: the
// default server budget (one git, gh or glab subprocess, or one forge
// request) and the answer margin, so the backend's descriptive error
// ("gh ... timed out after 45s") wins the race against this opaque one.
// rpcWaitMs gives a budgeted method its own. The timer is the last-resort
// guard for a live-but-stuck server; dead connections are handled by the
// WS close path rejecting all pending RPCs.
export const RPC_TIMEOUT_MS = DEFAULT_METHOD_BUDGET_MS + METHOD_ANSWER_MARGIN_MS;
export const RECONNECT_INITIAL_MS = 250;
// The shortest rung: a jittered delay never draws below it, so a degenerate
// random source cannot spin a tight reconnect loop.
export const RECONNECT_FLOOR_MS = 50;
// Backoff cap by backend locality (bootstrap `remote` flag). The 30s
// cap is remote-client sizing — polite to a LAN server that may be
// genuinely down. Against a same-machine backend a failed attempt is a
// refused loopback connect (~µs), so a long cap buys nothing and costs
// stale event streams: demand collapse is paced by the rung
// (collapseBackoffForDemand), so a passive viewer would otherwise stare
// at a frozen stream for up to the cap after a relay flap.
export const RECONNECT_MAX_LOCAL_MS = 5_000;
export const RECONNECT_MAX_REMOTE_MS = 30_000;
// Stale-socket watchdog. The server heartbeats every 10s — 3× that
// cadence with no traffic at all means the socket is half-open; the
// full rationale lives in internal/transport/AGENTS.md §Keepalive.
// Armed per connection by the heartbeat capability or its first ping.
// Version/deployment skew must not turn an
// idle-but-healthy connection into a reconnect loop. Check cadence is
// coarse — precision is not the point.
export const STALE_TRAFFIC_THRESHOLD_MS = 30_000;
export const STALE_CHECK_INTERVAL_MS = 10_000;
// Live events and heartbeats can overtake replay; only its completion marker
// proves recovery finished. Bound that wait separately from socket traffic,
// allowing the same full transfer budget as an RPC on a slow remote link.
export const REPLAY_TIMEOUT_MS = RPC_TIMEOUT_MS;
/** What the staleness check logs when it retires a replay past REPLAY_TIMEOUT_MS. */
export const REPLAY_RETIRED_WARNING = 'wsClient: replay did not complete; forcing reconnect';
