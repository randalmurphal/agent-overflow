// MCP server rows, keyed by the configuration they describe.
//
// Doctrine (frontend/CLAUDE.md → State Boundaries): state is keyed by its
// ENTITY, and the entity a LISTING describes is the WORKSPACE. Claude's
// membership is walked from the workspace outward (`.mcp.json` files from
// that cwd up, plus the enabled plugins' manifests), so two worktrees of one
// project can legitimately list different servers and neither answer is
// stale. Codex enumerates from ~/.codex/config.toml, which is app-global.
//
// So: Codex is one app-global key, Claude is one key per workspace, and
// every composer menu on that workspace renders the same rows and heals
// together.
//
// The TOGGLE is scoped differently from the listing — Claude persists
// `disabledMcpServers` on the canonical PROJECT entry (a worktree shares the
// main checkout's toggles, exactly like the CLI) and Codex's `enabled` flag
// is global — so a toggle in one workspace changes what a sibling worktree
// should render. That fan-out is not this key's job: the backend invalidates
// its status cache for (provider, name), the `mcp:status` sentinel lands
// here, and `invalidateKeysWithServer` re-lists every workspace carrying the
// server. Which is also why the toggle helpers below do NOT invalidate
// locally: two invalidation paths meant the acting pane listed twice.
//
// The listing RPC still needs a thread or a workspace to run against, so the
// attaching pane hands one in as ctx. A key sourced from a draft
// placeholder's ctx gets the config+cache view even when another pane on the
// same workspace has a live session that could have answered — the entity
// store hands the source whichever attacher's ctx it has, and that is the
// price of one shared listing. `mcpRowsSourceThreadId` reports which thread
// (if any) produced the rows, so a pane cannot offer a session-only action
// against someone else's session.

import { untrack } from 'svelte';
import { SvelteMap } from 'svelte/reactivity';
import {
  GetMcpServerStatus,
  ListThreadMcpServers,
  ListWorkspaceMcpServers,
  ReconnectMcpServer,
  RefreshMcpServerStatus,
  SetThreadMcpServerEnabled,
  SetWorkspaceMcpServerEnabled,
  ThreadMCPServer,
  TriggerMcpAuth,
  TriggerWorkspaceMcpAuth,
  type MCPAuthInitResult,
  type MCPServerStatus,
} from './bindings';
import { createEntityStore, type EntityAttachment } from './entityStore.svelte';
import { wailsEventOn } from './wailsEvents';
import { addToast } from './toast.svelte';

// Backend-emitted payload for the `mcp:oauth-completed` channel.
// Mirrors the map shape in app_mcp_auth.go.
interface MCPOAuthCompletedPayload {
  threadId: string;
  provider: string;
  serverName: string;
  success: boolean;
  /**
   * True when the Claude-side poll exhausted its budget without a
   * terminal answer — "not confirmed", not a provider-reported
   * failure; the sign-in may still have landed and the re-list this
   * event triggers will show it. Absent on Codex events and on real
   * terminal outcomes.
   */
  timedOut?: boolean;
  error?: string;
}

/** The app-global Codex key — the backend's `enabled` flag is global. */
export const MCP_CODEX_KEY = 'codex';

/** mcpRowKey identifies one server row. Matches Go's `mcpstatus.Key`. */
export function mcpRowKey(provider: string, name: string): string {
  return `${provider}:${name}`;
}

/**
 * What a source needs from whoever is holding the key. Fields may be
 * declared as GETTERS — an attacher holds one entity key across several
 * thread ids (a thread switch inside one project is the same entity), and a
 * re-list has to run against whichever thread is current when it runs, not
 * the one that happened to be showing at attach time.
 */
export interface MCPCtx {
  readonly provider: string;
  /** Empty for a draft placeholder: no thread row, so no session listing. */
  readonly threadId: string;
  readonly workspacePath: string;
}

/** A consumer's resolved entity key plus the ctx it would attach with. */
export interface MCPTarget extends MCPCtx {
  readonly key: string;
}

// A provider AO does not route MCP for gets no key at all: the backend
// answers every MCP binding for it with ErrMCPProviderUnsupported, and an
// error the user cannot act on is worse than an empty menu.
function mcpEntityKey(provider: string, workspacePath: string): string | null {
  if (provider === 'codex') return MCP_CODEX_KEY;
  if (provider !== 'claude') return null;
  return workspacePath === '' ? null : `claude:${workspacePath}`;
}

/**
 * The entity a composer pane's MCP menu is looking at, or null when the pane
 * has no workspace or its provider has no MCP routing. Takes primitives (not
 * the pane) so callers can feed value-stable deriveds — the raw `pane.thread`
 * reference churns on every streaming event.
 */
export function mcpTargetFor(
  provider: string,
  threadId: string,
  workspacePath: string,
): MCPTarget | null {
  if (!provider) return null;
  const path = workspacePath.trim();
  const key = mcpEntityKey(provider, path);
  if (key === null) return null;
  return { key, provider, threadId, workspacePath: path };
}

// ---------------------------------------------------------------------------
// The store
// ---------------------------------------------------------------------------

// Sourcing is not instant and the menu renders a spinner for it, so the
// in-flight state is tracked alongside the value. A count, not a flag: a
// re-source started before a superseded one unwound must not have its
// spinner cleared by the loser.
const loadCounts = new SvelteMap<string, number>();

function bumpLoad(key: string, delta: number): void {
  // untrack: this runs from source(), which may be reached from an $effect's
  // attach — a tracked SvelteMap read there would make that effect a
  // dependent of its own write.
  const next = untrack(() => loadCounts.get(key) ?? 0) + delta;
  if (next <= 0) loadCounts.delete(key);
  else loadCounts.set(key, next);
}

// Keys whose menu is open. Only those may chain a provider status fetch —
// that spawns `claude mcp list` / a codex app-server, and the trigger's
// badge priming (which runs for every mounted composer) must never do that.
// Refcounted: two panes on one project can have their menus open at once.
const statusFetchHolds = new Map<string, number>();

// Which thread id produced the rows currently applied to a key, or '' when
// they came from the workspace (config + status cache) variant. One key is
// shared by every pane on the workspace, so "these rows came from a live
// session" alone does not tell a pane that ITS session is the live one — a
// session-only action (Reconnect) has to compare against this.
const sourceThreadIds = new SvelteMap<string, string>();

// MCPCtx's fields are GETTERS reading the holder's live pane, so any value
// read out of one after an await may not be the value the RPC ran against.
// Every use here starts from a flat copy taken at the call site.
function ctxSnapshot(ctx: MCPCtx): MCPCtx {
  return { provider: ctx.provider, threadId: ctx.threadId, workspacePath: ctx.workspacePath };
}

async function listRows(ctx: MCPCtx): Promise<ThreadMCPServer[]> {
  const rows = ctx.threadId
    ? await ListThreadMcpServers(ctx.threadId)
    : await ListWorkspaceMcpServers(ctx.provider, ctx.workspacePath);
  return rows ?? [];
}

const store = createEntityStore<ThreadMCPServer[], MCPCtx>({
  name: 'mcpServers',
  source: async ({ key, getCtx, apply, signal }) => {
    bumpLoad(key, 1);
    try {
      const first = ctxSnapshot(getCtx());
      const rows = await listRows(first);
      // apply() from a superseded run is dropped for free; the map write is
      // not, so it takes the same guard or a loser stamps the winner's key.
      if (signal.aborted) return () => {};
      sourceThreadIds.set(key, first.threadId);
      apply(rows);
      // A config-sourced listing whose status cache is cold or stale can
      // only be resolved by asking the provider. Membership is already
      // right, so the first listing renders and this replaces it.
      //
      // signal: opening the menu invalidates the key, and the run that was
      // already in flight would otherwise see the fresh permit AFTER its
      // await and spawn a second `claude mcp list` for the run that
      // replaced it. Superseded work stops here.
      if (statusFetchHolds.has(key) && needsEphemeralRefresh(rows)) {
        const ctx = ctxSnapshot(getCtx());
        await RefreshMcpServerStatus(ctx.provider, ctx.workspacePath);
        if (signal.aborted) return () => {};
        const again = ctxSnapshot(getCtx());
        const refreshed = await listRows(again);
        if (signal.aborted) return () => {};
        sourceThreadIds.set(key, again.threadId);
        apply(refreshed);
      }
    } finally {
      bumpLoad(key, -1);
    }
    // Nothing to release: the listing is a pull, not a subscription.
    return () => {};
  },
  onDrop: (key) => {
    sourceThreadIds.delete(key);
  },
});

/**
 * needsEphemeralRefresh decides whether a just-loaded config-sourced listing
 * should chain a provider status fetch (`claude mcp list` / Codex
 * `mcpServerStatus/list`) and re-list. Membership is fully config-derived for
 * both providers (Claude enumerates plugin manifests and .mcp.json files
 * without spawning), so the fetch is only worth it to resolve enabled rows
 * whose connection status is unknown or stale. Stale rows keep rendering
 * while the chained refresh replaces them — membership never blinks.
 * Session-sourced listings are live truth and never chain.
 */
export function needsEphemeralRefresh(rows: ThreadMCPServer[]): boolean {
  if (rows.some((r) => r.source === 'session')) return false;
  return rows.some((r) => !r.disabled && (r.status === 'unknown' || r.stale));
}

// ---------------------------------------------------------------------------
// Event routing
// ---------------------------------------------------------------------------

// Re-list every key that carries the named server. The backend keys its
// status cache by (provider, name) with no notion of project, so this is the
// same fan-out shape its own emissions have.
//
// A key with no rows YET is still loading, and its in-flight listing was
// issued against pre-toggle config — so it is precisely the key that has to
// re-run, not one to skip. Membership is unknowable for it (there is nothing
// to match the server name against), and invalidating aborts the superseded
// run and re-lists; skipping left a held-but-still-loading workspace showing
// the pre-toggle answer with nothing due to correct it.
function invalidateKeysWithServer(provider: string, name: string): void {
  for (const key of store.keys()) {
    const rows = store.snapshot(key);
    if (rows && !rows.some((row) => row.provider === provider && row.name === name)) continue;
    store.invalidate(key);
  }
}

// A live status update folds into every key that shows the server, so cached
// rows track provider truth without re-listing.
//
// status="unknown" is NOT a status to render: the backend emits it when it
// INVALIDATES a cache entry — a toggle, a reconnect, a completed OAuth — so
// it is the signal that the authoritative listing moved. Rendering it would
// downgrade a live row to "Not checked" for one round-trip; discarding it
// (which this used to do) threw away the one push that says a toggle landed.
function patchStatus(status: MCPServerStatus): void {
  if (!status?.provider || !status.name || !status.status) return;
  if ((status.status as string) === 'unknown') {
    invalidateKeysWithServer(status.provider, status.name);
    return;
  }
  for (const key of store.keys()) {
    const rows = store.snapshot(key);
    if (!rows) continue;
    const idx = rows.findIndex(
      (r) => r.provider === status.provider && r.name === status.name && !r.disabled,
    );
    if (idx < 0) continue;
    // A session row is the thread's own lifecycle truth (the backend
    // merges retained startup state into it — see codexSessionMCPRows).
    // An ephemeral probe is app-global, keyed only (provider, name), and
    // can be fired from any pane; folding it onto a session row would
    // overwrite that merge client-side with the weaker observation.
    // Provider-sourced pushes (notification / live-session) still land.
    if (rows[idx].source === 'session' && status.source === 'ephemeral-fetch') continue;
    const next = rows.slice();
    next[idx] = new ThreadMCPServer({
      ...next[idx],
      status: status.status as string,
      error: status.error || undefined,
      tools: status.tools?.length ? [...status.tools] : undefined,
      stale: false,
    });
    store.apply(key, next);
  }
}

// ONE listener pair for the whole app.
let mcpEventOffs: Array<() => void> = [];

function installMcpEventListeners(): void {
  for (const off of mcpEventOffs) off();
  mcpEventOffs = [
    wailsEventOn<MCPServerStatus>('mcp:status', (payload) => {
      if (payload) patchStatus(payload);
    }),
    wailsEventOn<MCPOAuthCompletedPayload>('mcp:oauth-completed', (payload) => {
      if (!payload?.serverName || !payload?.provider) return;
      // Signing in changes what the next listing reports for that server on
      // every entity that carries it, not just the thread the popup ran in.
      invalidateKeysWithServer(payload.provider, payload.serverName);
      if (!payload.threadId && !payload.success && !payload.timedOut) {
        const detail = payload.error ? `: ${payload.error}` : '';
        addToast('error', `Sign-in failed for ${payload.serverName}${detail}`);
      }
    }),
  ];
}

installMcpEventListeners();

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

/** Refcounted attach for an entity. Release when the consumer unmounts. */
export function attachMcpServers(
  key: string,
  ctx: MCPCtx,
): EntityAttachment<ThreadMCPServer[]> {
  return store.attach(key, ctx);
}

/** Read an entity's rows without attaching. Reactive; empty until loaded. */
export function peekMcpServers(key: string | null): ThreadMCPServer[] {
  return key === null ? [] : (store.peek(key) ?? []);
}

/** Read an entity's listing error without attaching. Reactive. */
export function peekMcpServersError(key: string | null): string | null {
  return key === null ? null : store.peekError(key);
}

/** Whether a listing is in flight for the entity. Reactive. */
export function isMcpServersLoading(key: string | null): boolean {
  return key === null ? false : (loadCounts.get(key) ?? 0) > 0;
}

/**
 * The thread id whose session produced the rows currently held for the key,
 * or '' when they came from the workspace (config + status cache) variant.
 * Reactive. A pane offering a session-only action must match against this:
 * one workspace key is shared by every pane on it, so `row.source ===
 * 'session'` says a session answered, not that THIS pane's did.
 */
export function mcpRowsSourceThreadId(key: string | null): string {
  return key === null ? '' : (sourceThreadIds.get(key) ?? '');
}

/** Re-list an entity now. No-op when nobody is holding it. */
export function refreshMcpServers(key: string): void {
  store.invalidate(key);
}

/**
 * Allow this entity's listing to chain a provider status fetch while the
 * returned release is outstanding. Held by an OPEN menu only — priming the
 * trigger badge must never spawn a provider health-check.
 */
export function permitMcpStatusFetch(key: string): () => void {
  statusFetchHolds.set(key, (statusFetchHolds.get(key) ?? 0) + 1);
  let released = false;
  return () => {
    if (released) return;
    released = true;
    const remaining = (statusFetchHolds.get(key) ?? 1) - 1;
    if (remaining > 0) statusFetchHolds.set(key, remaining);
    else statusFetchHolds.delete(key);
  };
}

/**
 * Toggle one server. A thread with a live Claude session applies the toggle
 * in-session via mcp_toggle (the CLI persists it); everything else writes
 * provider-native config directly.
 *
 * No local re-list: every backend toggle path invalidates its status cache
 * for (provider, name), which lands here as the `mcp:status` unknown
 * sentinel and re-lists every workspace carrying the server — including this
 * one. Invalidating here too made the acting pane list twice for one click.
 */
export async function setMcpServerEnabled(
  target: MCPTarget,
  name: string,
  enabled: boolean,
): Promise<void> {
  if (!name) return;
  if (target.threadId) {
    await SetThreadMcpServerEnabled(target.threadId, name, enabled);
  } else {
    await SetWorkspaceMcpServerEnabled(target.provider, target.workspacePath, name, enabled);
  }
}

/**
 * Re-run the connection for one server on a thread's live session (Claude
 * mcp_reconnect / Codex config reload). Requires a live session; the backend
 * refuses otherwise, and invalidates on success — so, like the toggle, the
 * re-list arrives through the sentinel rather than from here.
 */
export async function reconnectMcpServer(target: MCPTarget, name: string): Promise<void> {
  await ReconnectMcpServer(target.threadId, name);
}

/**
 * Force an ephemeral status fetch for one server. Used on config-sourced
 * rows, where there is no live session to reconnect; the resulting cache Put
 * patches rows via `mcp:status`.
 */
export async function refreshMcpServerStatus(provider: string, name: string): Promise<void> {
  await GetMcpServerStatus(provider, name, true);
}

export async function triggerMcpAuth(target: MCPTarget, name: string): Promise<MCPAuthInitResult> {
  if (target.threadId) {
    return TriggerMcpAuth(target.threadId, name);
  }
  return TriggerWorkspaceMcpAuth(target.provider, target.workspacePath, name);
}

/** Diagnostics / tests: the entities currently held. */
export function mcpServersKeys(): string[] {
  return store.keys();
}

/**
 * Transport-gap recovery: re-list every held entity.
 *
 * `mcp:status` is edge-triggered — one frame per (provider, name) status
 * change, including the `unknown` sentinel that says "the authoritative
 * listing moved" — so a dropped frame leaves every key carrying that server
 * showing a superseded connection state with nothing due to correct it. The
 * gap signal carries no server name, and patchStatus's own fan-out is already
 * "every key that shows this server", so a blanket re-list is the same shape
 * one degree coarser. Live keys are bounded by the mounted panes, and
 * re-sourcing keeps each key's rows, so membership never blinks.
 */
export function resyncMcpServersAfterGap(): void {
  store.invalidateAll();
}

// ---------------------------------------------------------------------------
// Test seam
// ---------------------------------------------------------------------------

/**
 * Drop every entry and permit, as a fresh module load would, and re-arm the
 * event listeners (the shared wails-runtime mock clears every subscriber
 * between tests, and this module registers at load time — which happens once
 * per file).
 *
 * suspend() releases every unheld entry; resetAll() then lifts the
 * suspension. An entry that survives both is one a test attached and never
 * released — resetAll re-sources it against the next test's binding mocks,
 * which is exactly the noise that should make the leak findable.
 */
export function __resetMcpServersStoreForTest(): void {
  store.suspend();
  store.resetAll();
  statusFetchHolds.clear();
  loadCounts.clear();
  sourceThreadIds.clear();
  installMcpEventListeners();
}
