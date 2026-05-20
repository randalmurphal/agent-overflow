import {
  ListMcpServers,
  CreateMcpServer,
  UpdateMcpServer,
  DeleteMcpServer,
  GetThreadMcpServers,
  UpdateThreadMcpServers,
  ProbeMcpServer,
  GetMcpProbeSnapshot,
  TriggerMcpAuth,
  GetMcpThreadProfile,
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
  serverId: string;
  serverName: string;
  success: boolean;
  error?: string;
}

let library = $state<MCPServer[]>([]);
let probeResults = $state(new Map<string, MCPProbeResult>());
let probesInFlight = $state(new Set<string>());
let threadServerIds = $state(new Map<string, string[]>());
let profileIds = $state<string[]>([]);
let initialized = false;

/**
 * mcpServersStore is a tiny façade over the App bindings so components can
 * read reactive state without each one writing their own load+cache logic.
 * State shapes mirror what the backend returns:
 * - library: full list of MCPServer rows, alphabetical by name
 * - probeResults: per-server probe Result snapshots, populated lazily by
 *   probeServer() and seeded once via GetMcpProbeSnapshot()
 * - threadServerIds: per-thread enable set; populated by loadThreadServers
 *   on first read, kept fresh by setThreadServers writes
 * - profileIds: the "last selected" seed list
 */

function setLibrary(next: MCPServer[]): void {
  library = next ?? [];
}

function setThreadIds(threadId: string, ids: string[]): void {
  const m = new Map(threadServerIds);
  m.set(threadId, ids);
  threadServerIds = m;
}

function setProbeResult(serverId: string, res: MCPProbeResult): void {
  const m = new Map(probeResults);
  m.set(serverId, res);
  probeResults = m;
}

function clearProbeResult(serverId: string): void {
  if (!probeResults.has(serverId)) return;
  const m = new Map(probeResults);
  m.delete(serverId);
  probeResults = m;
}

function setProbeInFlight(serverId: string, inFlight: boolean): void {
  // Treat the Set as immutable so $state reactivity fires on read.
  const next = new Set(probesInFlight);
  if (inFlight) next.add(serverId);
  else next.delete(serverId);
  probesInFlight = next;
}

export const mcpServersStore = {
  get library(): readonly MCPServer[] {
    return library;
  },
  get probeResults(): ReadonlyMap<string, MCPProbeResult> {
    return probeResults;
  },
  get probesInFlight(): ReadonlySet<string> {
    return probesInFlight;
  },
  get profile(): readonly string[] {
    return profileIds;
  },

  threadServers(threadId: string): readonly string[] {
    return threadServerIds.get(threadId) ?? [];
  },

  async ensureInitialized(): Promise<void> {
    if (initialized) return;
    initialized = true;
    // First load is best-effort; the popup re-fetches on open anyway.
    await Promise.allSettled([
      this.refreshLibrary(),
      this.refreshProbeSnapshot(),
      this.refreshProfile(),
    ]);
    wailsEventOn<MCPOAuthCompletedPayload>('mcp:oauth-completed', (payload) => {
      if (!payload?.serverId) return;
      // Drop the cached probe; the next read (whether triggered by the popup
      // or any other subscriber) re-runs the handshake against the freshly
      // authenticated session.
      clearProbeResult(payload.serverId);
      // Speculatively re-probe so users who left the popup open see the new
      // status without clicking Refresh. Failures are silent: the next
      // explicit probe surfaces the error.
      void this.probeServer(payload.serverId, true).catch(() => undefined);
    });
  },

  async refreshLibrary(): Promise<MCPServer[]> {
    const next = (await ListMcpServers()) ?? [];
    setLibrary(next);
    return next;
  },

  async refreshProbeSnapshot(): Promise<void> {
    const snapshot = (await GetMcpProbeSnapshot()) ?? {};
    const m = new Map<string, MCPProbeResult>();
    for (const [id, res] of Object.entries(snapshot)) {
      if (res) m.set(id, res);
    }
    probeResults = m;
  },

  async refreshProfile(): Promise<string[]> {
    const profile = await GetMcpThreadProfile();
    const ids = profile?.serverIds ?? [];
    profileIds = ids;
    return ids;
  },

  async loadThreadServers(threadId: string): Promise<string[]> {
    if (!threadId) return [];
    const ids = (await GetThreadMcpServers(threadId)) ?? [];
    setThreadIds(threadId, ids);
    return ids;
  },

  async setThreadServers(threadId: string, ids: string[]): Promise<void> {
    if (!threadId) return;
    // Optimistic local update so the popup checkbox feels instant. The
    // backend may flag a reconcile failure on the thread error rail; that
    // surfaces independently of this call's success status.
    setThreadIds(threadId, ids);
    try {
      await UpdateThreadMcpServers(threadId, ids);
    } catch (err) {
      // Roll back by re-fetching the authoritative set.
      try {
        await this.loadThreadServers(threadId);
      } catch {
        // Surface the original error; the re-fetch failure is secondary.
      }
      throw err;
    }
  },

  async createServer(input: MCPServer): Promise<MCPServer> {
    const created = await CreateMcpServer(input);
    await this.refreshLibrary();
    await this.refreshProfile();
    return created;
  },

  async updateServer(input: MCPServer): Promise<MCPServer> {
    const updated = await UpdateMcpServer(input);
    clearProbeResult(updated.id);
    await this.refreshLibrary();
    return updated;
  },

  async deleteServer(id: string): Promise<void> {
    await DeleteMcpServer(id);
    clearProbeResult(id);
    await Promise.allSettled([this.refreshLibrary(), this.refreshProfile()]);
  },

  async probeServer(id: string, force = false): Promise<MCPProbeResult> {
    if (!id) {
      throw new Error('mcp: probeServer requires a server id');
    }
    setProbeInFlight(id, true);
    try {
      const result = await ProbeMcpServer(id, force);
      if (result) setProbeResult(id, result);
      return result;
    } finally {
      setProbeInFlight(id, false);
    }
  },

  async triggerAuth(threadId: string, serverId: string): Promise<MCPAuthInitResult> {
    return TriggerMcpAuth(threadId, serverId);
  },
};

// resetMcpServersForTest clears every cached state slice so tests can build
// the store fresh between cases. Not part of the production surface.
export function resetMcpServersForTest(): void {
  library = [];
  probeResults = new Map();
  probesInFlight = new Set();
  threadServerIds = new Map();
  profileIds = [];
  initialized = false;
}
