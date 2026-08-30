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
