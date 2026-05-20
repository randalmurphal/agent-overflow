import {
  ListMcpServers,
  CreateMcpServer,
  UpdateMcpServer,
  DeleteMcpServer,
  SetMcpServerEnabled,
  ProbeMcpServer,
  GetMcpProbeSnapshot,
  TriggerMcpAuth,
  MCPServer,
  MCPProbeResult,
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

// cacheKey is the wire-stable identifier used by the probe cache and
// by the store's internal maps. Matches the Go side's mcp.Spec.CacheKey().
export function mcpCacheKey(provider: string, name: string): string {
  return `${provider}:${name}`;
}

// servers is the unified library across both providers. Keyed by
// cacheKey so a Claude entry and a Codex entry with the same name
// don't collide. The list view consumers filter by provider.
let servers = $state<MCPServer[]>([]);
let probeResults = $state(new Map<string, MCPProbeResult>());
let probesInFlight = $state(new Set<string>());
let oauthSubscribed = false;

function setServers(next: MCPServer[]): void {
  servers = next ?? [];
}

function mergeServers(next: MCPServer[], provider: string, workspaceScoped: boolean): void {
  // Replace every entry that came from the same provider+workspace
  // scope so a workspace-aware reload doesn't leak stale entries from
  // a previous thread. The Disabled flag is workspace-scoped for
  // Claude, so a workspace switch must reset every Claude row to
  // its current scope's state.
  const next2 = (next ?? []).filter((s) => s.provider === provider);
  const others = servers.filter((s) => s.provider !== provider);
  servers = workspaceScoped ? [...others, ...next2] : [...others, ...next2];
}

function setProbeResult(key: string, res: MCPProbeResult): void {
  const m = new Map(probeResults);
  m.set(key, res);
  probeResults = m;
}

function clearProbeResult(key: string): void {
  if (!probeResults.has(key)) return;
  const m = new Map(probeResults);
  m.delete(key);
  probeResults = m;
}

function setProbeInFlight(key: string, inFlight: boolean): void {
  // Treat the Set as immutable so $state reactivity fires on read.
  const next = new Set(probesInFlight);
  if (inFlight) next.add(key);
  else next.delete(key);
  probesInFlight = next;
}

function subscribeOAuth(): void {
  if (oauthSubscribed) return;
  oauthSubscribed = true;
  wailsEventOn<MCPOAuthCompletedPayload>('mcp:oauth-completed', (payload) => {
    if (!payload?.serverName || !payload?.provider) return;
    const key = mcpCacheKey(payload.provider, payload.serverName);
    clearProbeResult(key);
    // Speculatively re-probe so users who left the popup open see the new
    // status without clicking Refresh. Failures are silent: the next
    // explicit probe surfaces the error.
    void mcpServersStore.probeServer(payload.provider, payload.serverName, true).catch(
      () => undefined,
    );
  });
}

export const mcpServersStore = {
  get servers(): readonly MCPServer[] {
    return servers;
  },

  get probeResults(): ReadonlyMap<string, MCPProbeResult> {
    return probeResults;
  },

  get probesInFlight(): ReadonlySet<string> {
    return probesInFlight;
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
    subscribeOAuth();
    const list = (await ListMcpServers(provider, workspacePath)) ?? [];
    mergeServers(list, provider, true);
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
    subscribeOAuth();
    const [claudeList, codexList] = await Promise.all([
      ListMcpServers('claude', '').catch(() => []),
      ListMcpServers('codex', '').catch(() => []),
    ]);
    setServers([...(claudeList ?? []), ...(codexList ?? [])]);
  },

  /**
   * refreshProbeSnapshot seeds probeResults with whatever the backend
   * cache already has so the popup renders instant status on first
   * open. Keyed by cacheKey on the wire.
   */
  async refreshProbeSnapshot(): Promise<void> {
    const snapshot = (await GetMcpProbeSnapshot()) ?? {};
    const m = new Map<string, MCPProbeResult>();
    for (const [key, res] of Object.entries(snapshot)) {
      if (res) m.set(key, res);
    }
    probeResults = m;
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
    clearProbeResult(mcpCacheKey(updated.provider, updated.name));
    return updated;
  },

  async deleteServer(provider: string, name: string): Promise<void> {
    await DeleteMcpServer(provider, name);
    clearProbeResult(mcpCacheKey(provider, name));
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

  async probeServer(provider: string, name: string, force = false): Promise<MCPProbeResult> {
    if (!provider || !name) {
      throw new Error('mcp: probeServer requires provider and name');
    }
    const key = mcpCacheKey(provider, name);
    setProbeInFlight(key, true);
    try {
      const result = await ProbeMcpServer(provider, name, force);
      if (result) setProbeResult(key, result);
      return result;
    } finally {
      setProbeInFlight(key, false);
    }
  },

  async triggerAuth(threadId: string, name: string): Promise<MCPAuthInitResult> {
    return TriggerMcpAuth(threadId, name);
  },
};

export function resetMcpServersForTest(): void {
  servers = [];
  probeResults = new Map();
  probesInFlight = new Set();
  oauthSubscribed = false;
}
