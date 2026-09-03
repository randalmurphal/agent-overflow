/** The JSON payload of the __AO_HARNESS__ stdout line. */
export interface HarnessBootstrap {
  url: string;
  port: number;
  token: string;
  dataRoot: string;
  dataDir: string;
  homeDir?: string;
  mockProvider: string;
  pid: number;
  version: string;
  /**
   * This instance's durable UI-state identity, already threaded onto `url`
   * as `&cid=`. The harness socket declares the SAME id on its upgrade so
   * its RPCs resolve to the page's ui_state bucket — the backend scopes
   * that bucket by the connection, so a socket declaring nothing has no
   * bucket to read at all.
   */
  clientId?: string;
  startupError?: string;
}

export interface LaunchOptions {
  /** Backend binary. Default: $AO_HARNESS_BIN, else <repo>/bin/agent-overflow. */
  binary?: string;
  /** ao-mockprovider path. Default: $AO_MOCKPROVIDER, else next to the binary. */
  mockProvider?: string;
  /** Data root. Default: a fresh temp dir, removed on close(). */
  dataDir?: string;
  /** Extra environment (merged over process.env). */
  env?: Record<string, string>;
  /** Boot deadline in ms. Default 30s (first boot migrates the DB). */
  timeoutMs?: number;
  /** Hard per-process fallback when the Go launcher cannot install a kernel aggregate limit. */
  memoryLimitBytes?: number;
}
