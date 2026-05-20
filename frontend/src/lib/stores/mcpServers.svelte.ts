import {
  ListMcpServers,
  CreateMcpServer,
  UpdateMcpServer,
  DeleteMcpServer,
  SetMcpServerEnabled,
  GetMcpServerStatus,
  ListMcpServerStatuses,
  RefreshMcpServerStatus,
  TriggerMcpAuth,
  MCPServer,
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

// mcpStatusKey is the wire-stable identifier the store uses to key the
// statuses Map. Matches the Go side's mcpstatus.Key shape
// (`<provider>:<name>`).
export function mcpStatusKey(provider: string, name: string): string {
  return `${provider}:${name}`;
}

// servers is the unified library across both providers. Keyed by
// (provider, name) for de-collision; the list view consumers filter
// by provider.
let servers = $state<MCPServer[]>([]);
let statuses = $state(new Map<string, MCPServerStatus>());
let refreshingProvider = $state(new Set<string>());
let eventsSubscribed = false;

function setServers(next: MCPServer[]): void {
  servers = next ?? [];
}

function mergeServers(next: MCPServer[], provider: string): void {
  // Replace every entry that came from the same provider so a
  // workspace-aware reload doesn't leak stale entries from a previous
  // thread. Workspace scoping is provider-specific (Claude only); the
  // adapter handles that — the store just respects the result.
  const next2 = (next ?? []).filter((s) => s.provider === provider);
  const others = servers.filter((s) => s.provider !== provider);
  servers = [...others, ...next2];
}

function setStatus(status: MCPServerStatus): void {
  if (!status || !status.provider || !status.name) return;
  const key = mcpStatusKey(status.provider, status.name);
  const m = new Map(statuses);
  m.set(key, status);
  statuses = m;
}

function setRefreshing(provider: string, inFlight: boolean): void {
  if (!provider) return;
  const next = new Set(refreshingProvider);
  if (inFlight) next.add(provider);
  else next.delete(provider);
  refreshingProvider = next;
}

function subscribeEvents(): void {
  if (eventsSubscribed) return;
  eventsSubscribed = true;
  // Live cache updates from Go: every Put/Invalidate flows through
  // here, so the popup reflects the provider's authoritative state
  // (live session + notifications + ephemeral fetch) without
  // polling. Invalidations arrive as a ServerStatus with
  // status="unknown".
  wailsEventOn<MCPServerStatus>('mcp:status', (payload) => {
    if (!payload) return;
    setStatus(payload);
  });
  wailsEventOn<MCPOAuthCompletedPayload>('mcp:oauth-completed', (payload) => {
    if (!payload?.serverName || !payload?.provider) return;
    // Speculatively refresh this provider so users who left the popup
    // open see the new status without clicking Refresh. Failures are
    // silent: the next explicit refresh surfaces the error.
    void mcpServersStore.refreshStatuses(payload.provider).catch(() => undefined);
  });
}

export const mcpServersStore = {
  get servers(): readonly MCPServer[] {
    return servers;
  },

  get statuses(): ReadonlyMap<string, MCPServerStatus> {
    return statuses;
  },

  get refreshingProvider(): ReadonlySet<string> {
    return refreshingProvider;
  },

  /**
   * serversForProvider returns the visible servers for one provider.
   * For Claude the rows reflect the workspace scope of the last
   * loadForThread call; for Codex the global enabled flag.
   */
  serversForProvider(provider: string): MCPServer[] {
    return servers.filter((s) => s.provider === provider);
  },

  /**
   * loadForThread fetches the provider's MCP library scoped to the
   * thread's workspace. Used by the composer toolbar popup. Pass an
   * empty workspacePath for Codex (the flag is global).
   */
  async loadForThread(provider: string, workspacePath: string): Promise<MCPServer[]> {
    subscribeEvents();
    const list = (await ListMcpServers(provider, workspacePath)) ?? [];
    mergeServers(list, provider);
    return list;
  },

  /**
   * loadAllProviders fetches both Claude and Codex libraries without
   * a workspace scope. Used by the Settings library view. The
   * Disabled flag here reflects "any workspace has disabled it" for
   * Claude (the adapter returns the cross-workspace library, not a
   * workspace-scoped projection, when workspacePath is empty).
   */
  async loadAllProviders(): Promise<void> {
    subscribeEvents();
    const [claudeList, codexList] = await Promise.all([
      ListMcpServers('claude', '').catch(() => []),
      ListMcpServers('codex', '').catch(() => []),
    ]);
    setServers([...(claudeList ?? []), ...(codexList ?? [])]);
  },

  /**
   * loadStatuses seeds the statuses map from the backend's cached
   * snapshot for one provider. Used on popup open so the UI shows
   * authoritative provider state immediately (live sessions
   * continuously feed the cache; ephemeral fetches kick in via
   * refreshStatuses when no live entry exists).
   */
  async loadStatuses(provider: string): Promise<void> {
    subscribeEvents();
    const snapshot = (await ListMcpServerStatuses(provider)) ?? [];
    const m = new Map(statuses);
    for (const s of snapshot) {
      if (!s) continue;
      m.set(mcpStatusKey(s.provider, s.name), s);
    }
    statuses = m;
  },

  /**
   * refreshStatuses forces an ephemeral fetch for the provider so the
   * UI can re-check connection state on demand (popup refresh button,
   * settings test-connection). Live sessions push their own state via
   * the mcp:status event channel; this is the fallback for inactive
   * threads or post-config-edit re-verification.
   */
  async refreshStatuses(provider: string): Promise<MCPServerStatus[]> {
    if (!provider) return [];
    subscribeEvents();
    setRefreshing(provider, true);
    try {
      const list = (await RefreshMcpServerStatus(provider)) ?? [];
      // Live cache emits over `mcp:status`; explicit assignment here
      // is belt-and-suspenders so callers that don't yet subscribe to
      // the event channel still see the result.
      const m = new Map(statuses);
      for (const s of list) {
        if (!s) continue;
        m.set(mcpStatusKey(s.provider, s.name), s);
      }
      statuses = m;
      return list;
    } finally {
      setRefreshing(provider, false);
    }
  },

  /**
   * fetchStatus forces an ephemeral fetch for a single server. Used
   * when the popup wants to recheck one row without spawning the
   * provider's whole list query. Falls through to the cache on
   * non-force calls; subscribers update via the mcp:status channel.
   */
  async fetchStatus(provider: string, name: string, force = false): Promise<MCPServerStatus> {
    if (!provider || !name) {
      throw new Error('mcp: fetchStatus requires provider and name');
    }
    subscribeEvents();
    const result = await GetMcpServerStatus(provider, name, force);
    if (result) setStatus(result);
    return result;
  },

  async createServer(input: MCPServer): Promise<MCPServer> {
    const created = await CreateMcpServer(input);
    // Refresh the library scope the caller is currently rendering.
    // The settings view calls loadAllProviders; the popup calls
    // loadForThread. Either way the new row is visible next read.
    return created;
  },

  async updateServer(input: MCPServer): Promise<MCPServer> {
    const updated = await UpdateMcpServer(input);
    return updated;
  },

  async deleteServer(provider: string, name: string): Promise<void> {
    await DeleteMcpServer(provider, name);
  },

  /**
   * setEnabled toggles the unified Disabled flag for a server in the
   * scope of the calling thread. Claude: workspace-scoped disabledMcpServers;
   * Codex: global enabled = false. The backend live-reconciles the
   * affected provider session.
   */
  async setEnabled(threadId: string, name: string, enabled: boolean): Promise<void> {
    if (!threadId || !name) return;
    await SetMcpServerEnabled(threadId, name, enabled);
  },

  async triggerAuth(threadId: string, name: string): Promise<MCPAuthInitResult> {
    return TriggerMcpAuth(threadId, name);
  },
};

export function resetMcpServersForTest(): void {
  servers = [];
  statuses = new Map();
  refreshingProvider = new Set();
  eventsSubscribed = false;
}
