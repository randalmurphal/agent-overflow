export interface Settings {
  theme: 'system' | 'light' | 'dark';
  timestampFormat: 'locale' | '12-hour' | '24-hour';
  defaultProvider: 'claude' | 'codex';
  defaultModelClaude: string;
  defaultModelCodex: string;
  recentWorkspaces: string[];
  diffWordWrap: boolean;
  streamingEnabled: boolean;
  confirmArchive: boolean;
  confirmDelete: boolean;
  claudeBinaryPath: string;
  codexBinaryPath: string;
  claudeEnabled: boolean;
  codexEnabled: boolean;
  observabilityTracingEnabled: boolean;
  observabilityOtlpEndpoint: string;
  observabilityEventLogEnabled: boolean;
}

export interface ProviderStatus {
  provider: string;
  installed: boolean;
  version: string;
  binaryPath: string;
  status: 'ready' | 'not_found' | 'error';
  message: string;
}

export interface ModelInfo {
  slug: string;
  name: string;
  provider: string;
  capabilities: string[];
}
