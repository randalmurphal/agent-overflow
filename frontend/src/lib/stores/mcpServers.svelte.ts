import {
  ListThreadMcpServers,
  ListWorkspaceMcpServers,
  SetThreadMcpServerEnabled,
  SetWorkspaceMcpServerEnabled,
  ReconnectMcpServer,
  GetMcpServerStatus,
  RefreshMcpServerStatus,
  TriggerMcpAuth,
  ThreadMCPServer,
  MCPServerStatus,
  MCPAuthInitResult,
} from './bindings';
import { wailsEventOn } from './events';

// Backend-emitted payload for the `mcp:oauth-completed` channel.
// Mirrors the map shape in app_mcp_bindings.go's
// handleCodexMCPOAuthCompleted.
interface MCPOAuthCompletedPayload {
  threadId: string;
  provider: string;
  serverName: string;
  success: boolean;
  error?: string;
}

// mcpRowKey identifies one server row within a scope. Matches the Go
// side's mcpstatus.Key shape (`<provider>:<name>`).
export function mcpRowKey(provider: string, name: string): string {
  return `${provider}:${name}`;
}

// MCPScope names one backend listing the menu can render: a live/known
// thread (ListThreadMcpServers) or a provider+workspace draft
// (ListWorkspaceMcpServers). Rows are cached per scope so the menu
// renders instantly from the last load while a background refresh runs.
// Thread scopes carry provider + workspacePath too — the chained
// ephemeral status refresh needs them when the thread has no live
// session and its listing fell back to config.
export type MCPScope =
  | { kind: 'thread'; threadId: string; provider: string; workspacePath: string }
  | { kind: 'workspace'; provider: string; workspacePath: string };

function threadScopeKey(threadId: string): string {
  return `thread:${threadId}`;
}

function scopeKey(scope: MCPScope): string {
  return scope.kind === 'thread'
    ? threadScopeKey(scope.threadId)
    : `workspace:${scope.provider}:${scope.workspacePath}`;
}

// mcpScopeFor maps a composer pane's fields onto the scope its MCP
// menu should list: draft placeholders have no thread row yet, so they
// read the workspace config view; everything else asks the thread
// listing. Takes primitives (not the pane) so callers can feed it
// value-stable deriveds — the raw `pane.thread` reference churns on
// every streaming event and would re-trigger dependent effects.
export function mcpScopeFor(
  provider: string,
  threadId: string,
  workspacePath: string,
  isPlaceholder: boolean,
): MCPScope | null {
  if (!provider) return null;
  if (isPlaceholder) {
    return { kind: 'workspace', provider, workspacePath };
  }
  if (threadId) {
    return { kind: 'thread', threadId, provider, workspacePath };
  }
  return null;
}

let rowsByScope = $state(new Map<string, ThreadMCPServer[]>());
let loadingScopes = $state(new Set<string>());
// Single-flight per scope: the trigger and the menu both fire load()
// on open; the second caller shares the first's round-trip. Mutations
// bypass it (fresh: true) so a reload after a toggle can never join a
// pre-toggle fetch; loadSeq makes the newest-started load the only one
// allowed to publish rows, so the superseded fetch can't overwrite it.
const inflightLoads = new Map<string, Promise<ThreadMCPServer[]>>();
const loadSeq = new Map<string, number>();
let eventsSubscribed = false;

function setRows(key: string, next: ThreadMCPServer[]): void {
  const updated = new Map(rowsByScope);
  updated.set(key, next ?? []);
  rowsByScope = updated;
}

function setLoading(key: string, inFlight: boolean): void {
  const next = new Set(loadingScopes);
  if (inFlight) next.add(key);
  else next.delete(key);
  loadingScopes = next;
}

// patchStatus folds a live status-cache update into every cached scope
// that shows the server. Live sessions and ephemeral fetches both feed
// the backend cache, which emits each Put/Invalidate over `mcp:status`
// — so cached rows track provider truth without re-listing.
// Invalidations arrive as status="unknown" and are skipped: the next
// scope load re-reads the authoritative listing, and downgrading a
// rendered row to "unknown" in the meantime is pure flicker.
function patchStatus(status: MCPServerStatus): void {
  if (!status?.provider || !status.name || !status.status) return;
  if ((status.status as string) === 'unknown') return;
  let changed = false;
  const updated = new Map(rowsByScope);
  for (const [key, rows] of updated) {
    const idx = rows.findIndex(
      (r) => r.provider === status.provider && r.name === status.name && !r.disabled,
    );
    if (idx < 0) continue;
    const next = rows.slice();
    next[idx] = new ThreadMCPServer({
      ...next[idx],
      status: status.status as string,
      error: status.error || undefined,
      tools: status.tools?.length ? [...status.tools] : undefined,
      stale: false,
    });
    updated.set(key, next);
    changed = true;
  }
  if (changed) rowsByScope = updated;
}

// needsEphemeralRefresh decides whether a just-loaded config-sourced
// listing should chain a provider status fetch (`claude mcp list` /
// Codex `mcpServerStatus/list`) and re-list. Membership is fully
// config-derived for both providers (Claude enumerates plugin
// manifests and .mcp.json files without spawning), so the fetch is
// only worth it to resolve enabled rows whose connection status is
// unknown or stale. Stale rows (last-known status past the cache TTL)
// keep rendering while the chained refresh replaces them — membership
// never blinks. Session-sourced listings are live truth and never
// chain.
export function needsEphemeralRefresh(rows: ThreadMCPServer[]): boolean {
  if (rows.some((r) => r.source === 'session')) return false;
  return rows.some((r) => !r.disabled && (r.status === 'unknown' || r.stale));
}

function subscribeEvents(): void {
  if (eventsSubscribed) return;
  eventsSubscribed = true;
  wailsEventOn<MCPServerStatus>('mcp:status', (payload) => {
    if (!payload) return;
    patchStatus(payload);
  });
  wailsEventOn<MCPOAuthCompletedPayload>('mcp:oauth-completed', (payload) => {
    if (!payload?.serverName || !payload?.provider) return;
    // Re-list the thread the OAuth flow ran in so users who left the
    // popup open see the new state without reopening. Failures are
    // silent: the next open reloads anyway.
    if (payload.threadId && rowsByScope.has(threadScopeKey(payload.threadId))) {
      const scope: MCPScope = {
        kind: 'thread',
        threadId: payload.threadId,
        provider: payload.provider,
        workspacePath: '',
      };
      void mcpServersStore.load(scope, { fresh: true }).catch(() => undefined);
    }
  });
}

export const mcpServersStore = {
  /**
   * rowsFor returns the last-loaded rows for a scope. Empty until the
   * first load resolves; the menu renders this instantly and lets a
   * background load() replace it.
   */
  rowsFor(scope: MCPScope): ThreadMCPServer[] {
    return rowsByScope.get(scopeKey(scope)) ?? [];
  },

  isLoading(scope: MCPScope): boolean {
    return loadingScopes.has(scopeKey(scope));
  },

  /**
   * load fetches the authoritative listing for a scope. Thread scopes
   * get live session truth when a session is up (config+cache
   * fallback otherwise); workspace scopes get the config+cache view.
   * A config-sourced result whose status cache looks cold or stale
   * chains one ephemeral provider fetch and re-lists, so enabled
   * plugin servers (invisible in provider config files) reach the
   * no-session view; the first listing renders immediately and the
   * chain replaces it. noFetch suppresses the chain — used by the
   * chain's own re-list and by cheap priming loads (trigger badge)
   * that must never spawn a provider health-check.
   */
  async load(
    scope: MCPScope,
    opts?: { fresh?: boolean; noFetch?: boolean },
  ): Promise<ThreadMCPServer[]> {
    subscribeEvents();
    const key = scopeKey(scope);
    if (!opts?.fresh) {
      const inflight = inflightLoads.get(key);
      if (inflight) return inflight;
    }
    const seq = (loadSeq.get(key) ?? 0) + 1;
    loadSeq.set(key, seq);
    const run = (async () => {
      setLoading(key, true);
      try {
        const rows =
          (scope.kind === 'thread'
            ? await ListThreadMcpServers(scope.threadId)
            : await ListWorkspaceMcpServers(scope.provider, scope.workspacePath)) ?? [];
        if (loadSeq.get(key) === seq) setRows(key, rows);
        if (!opts?.noFetch && needsEphemeralRefresh(rows)) {
          await RefreshMcpServerStatus(scope.provider, scope.workspacePath);
          return await mcpServersStore.load(scope, { fresh: true, noFetch: true });
        }
        return rows;
      } finally {
        // Only the newest-started load owns cleanup; a superseded
        // fetch (or this one, after its chained re-list bumped seq)
        // must not evict the fresh in-flight entry or clear its
        // loading flag.
        if (loadSeq.get(key) === seq) {
          inflightLoads.delete(key);
          setLoading(key, false);
        }
      }
    })();
    inflightLoads.set(key, run);
    return run;
  },

  /**
   * setEnabled toggles one server for a scope and re-lists it. Thread
   * scopes with a live Claude session apply the toggle in-session via
   * mcp_toggle (the CLI persists it); everything else writes
   * provider-native config directly.
   */
  async setEnabled(scope: MCPScope, name: string, enabled: boolean): Promise<void> {
    if (!name) return;
    if (scope.kind === 'thread') {
      await SetThreadMcpServerEnabled(scope.threadId, name, enabled);
    } else {
      await SetWorkspaceMcpServerEnabled(scope.provider, scope.workspacePath, name, enabled);
    }
    await this.load(scope, { fresh: true });
  },

  /**
   * reconnect re-runs the connection for one server on a thread's live
   * session (Claude mcp_reconnect / Codex config reload). Requires a
   * live session; the backend refuses otherwise.
   */
  async reconnect(scope: Extract<MCPScope, { kind: 'thread' }>, name: string): Promise<void> {
    await ReconnectMcpServer(scope.threadId, name);
    await this.load(scope, { fresh: true });
  },

  /**
   * refreshStatus forces an ephemeral status fetch for one server.
   * Used on config-sourced rows, where there is no live session to
   * reconnect; the resulting cache Put patches rows via `mcp:status`.
   */
  async refreshStatus(provider: string, name: string): Promise<void> {
    subscribeEvents();
    await GetMcpServerStatus(provider, name, true);
  },

  async triggerAuth(threadId: string, name: string): Promise<MCPAuthInitResult> {
    return TriggerMcpAuth(threadId, name);
  },
};

export function resetMcpServersForTest(): void {
  rowsByScope = new Map();
  loadingScopes = new Set();
  inflightLoads.clear();
  loadSeq.clear();
  eventsSubscribed = false;
}
