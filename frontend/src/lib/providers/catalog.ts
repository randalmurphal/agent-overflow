import type { ReasoningEffort } from '../types/settings';
import {
  asProviderID,
  type ProviderID,
} from '../types/providers';
export {
  asProviderID,
  isProviderID,
  PROVIDER_IDS,
  type ProviderID,
} from '../types/providers';

export type ProviderBackgroundStop =
  | 'claude-task'
  | 'codex-background-terminals'
  | 'none';

export interface ProviderEffortOption {
  value: ReasoningEffort;
  label: string;
}

// Per-provider affordance matrix. Each flag gates a user-facing capability the
// UI must hide or disable when the active thread's provider doesn't support it.
// claude-tui drives the real interactive TUI from outside the process, so the
// affordances AO mediates for the headless providers (runtime-mode selection,
// plan toggle, fork, revert-to-checkpoint, composer attachments, MCP picker)
// have no meaning there — the human reaches them inside the terminal via
// take-control instead. Default (unknown provider) is "supported" so only an
// explicit `false` gates an affordance off; see `providerSupports`.
//
// Distinct from Go's `provider.Capabilities` (internal/provider/capabilities.go):
// that struct holds provider-process plumbing (model-catalog source,
// background-terminal cleaner); this holds UI-affordance gates. They are not
// mirrors of each other — keep them separate.
export interface ProviderCapabilities {
  // Runtime access modes (Supervised / Auto-accept edits / Full access).
  // false → the provider runs full-access only and the AccessToggle is omitted.
  runtimeModes: boolean;
  // chat ↔ plan interaction-mode toggle. false → chat only (no plan mode).
  planMode: boolean;
  // Fork-thread and fork-and-revert affordances.
  fork: boolean;
  // Revert-to-message / restore-checkpoint affordances (viewing a diff is not
  // gated — only the state-mutating restore is).
  revert: boolean;
  // Composer image/file attachments (button, paste, drag-drop).
  attachments: boolean;
  // MCP server selection + status picker.
  mcp: boolean;
}

const FULL_CAPABILITIES: ProviderCapabilities = {
  runtimeModes: true,
  planMode: true,
  fork: true,
  revert: true,
  attachments: true,
  mcp: true,
};

// claude-tui supports none of the AO-mediated affordances — they live inside
// the real TUI, reached through take-control.
const TUI_CAPABILITIES: ProviderCapabilities = {
  runtimeModes: false,
  planMode: false,
  fork: false,
  revert: false,
  attachments: false,
  mcp: false,
};

export interface ProviderDefinition {
  id: ProviderID;
  label: string;
  cliLabel: string;
  shortLabel: string;
  badgeClass: string;
  installActionLabel: string;
  textGenerationDefaultModel: string;
  textGenerationEffortOptions: ProviderEffortOption[];
  contextLabels: {
    standard: string;
    extended: string;
  };
  settings: {
    enabledKey: 'claudeEnabled' | 'codexEnabled';
    pathKey: 'claudeBinaryPath' | 'codexBinaryPath';
    standardCompactKey:
      | 'claudeAutoCompactStandardPercent'
      | 'codexAutoCompactStandardPercent';
    extendedCompactKey:
      | 'claudeAutoCompactExtendedPercent'
      | 'codexAutoCompactExtendedPercent';
  };
  backgroundStop: ProviderBackgroundStop;
  capabilities: ProviderCapabilities;
}

export const PROVIDER_DEFINITIONS: Record<ProviderID, ProviderDefinition> = {
  claude: {
    id: 'claude',
    label: 'Claude',
    cliLabel: 'Claude CLI',
    shortLabel: 'C',
    badgeClass: 'bg-accent/10 text-accent',
    installActionLabel: 'Install Claude CLI',
    textGenerationDefaultModel: 'claude-haiku-4-5',
    textGenerationEffortOptions: [
      { value: 'low', label: 'Low' },
      { value: 'medium', label: 'Medium' },
      { value: 'high', label: 'High' },
      { value: 'xhigh', label: 'xHigh' },
      { value: 'max', label: 'Max' },
    ],
    contextLabels: {
      standard: '200k',
      extended: '1m',
    },
    settings: {
      enabledKey: 'claudeEnabled',
      pathKey: 'claudeBinaryPath',
      standardCompactKey: 'claudeAutoCompactStandardPercent',
      extendedCompactKey: 'claudeAutoCompactExtendedPercent',
    },
    backgroundStop: 'claude-task',
    capabilities: FULL_CAPABILITIES,
  },
  codex: {
    id: 'codex',
    label: 'Codex',
    cliLabel: 'Codex CLI',
    shortLabel: 'X',
    badgeClass: 'bg-provider-codex/10 text-provider-codex',
    installActionLabel: 'Install Codex CLI',
    textGenerationDefaultModel: 'gpt-5.4-mini',
    textGenerationEffortOptions: [
      { value: 'none', label: 'None' },
      { value: 'minimal', label: 'Minimal' },
      { value: 'low', label: 'Low' },
      { value: 'medium', label: 'Medium' },
      { value: 'high', label: 'High' },
      { value: 'xhigh', label: 'xHigh' },
    ],
    contextLabels: {
      standard: '272k',
      extended: '1m',
    },
    settings: {
      enabledKey: 'codexEnabled',
      pathKey: 'codexBinaryPath',
      standardCompactKey: 'codexAutoCompactStandardPercent',
      extendedCompactKey: 'codexAutoCompactExtendedPercent',
    },
    backgroundStop: 'codex-background-terminals',
    capabilities: FULL_CAPABILITIES,
  },
  // claude-tui reuses claude's binary, auth, model catalog, effort set, and
  // context tiers — the only difference is the interactive PTY surface. So its
  // settings keys point at claude's, and text-generation fields mirror claude's
  // (claude-tui is never selected for commit-message / title generation, which
  // is gated to claude + codex, but the type requires the fields). It has no
  // AO-managed background tasks — background work lives inside the TUI, reached
  // through take-control — so backgroundStop is 'none'.
  'claude-tui': {
    id: 'claude-tui',
    label: 'Claude TUI',
    cliLabel: 'Claude TUI',
    shortLabel: 'T',
    badgeClass: 'bg-provider-claude-tui/10 text-provider-claude-tui',
    installActionLabel: 'Install Claude CLI',
    textGenerationDefaultModel: 'claude-haiku-4-5',
    textGenerationEffortOptions: [
      { value: 'low', label: 'Low' },
      { value: 'medium', label: 'Medium' },
      { value: 'high', label: 'High' },
      { value: 'xhigh', label: 'xHigh' },
      { value: 'max', label: 'Max' },
    ],
    contextLabels: {
      standard: '200k',
      extended: '1m',
    },
    settings: {
      enabledKey: 'claudeEnabled',
      pathKey: 'claudeBinaryPath',
      standardCompactKey: 'claudeAutoCompactStandardPercent',
      extendedCompactKey: 'claudeAutoCompactExtendedPercent',
    },
    backgroundStop: 'none',
    capabilities: TUI_CAPABILITIES,
  },
};

export const PROVIDER_SETTINGS_ORDER: ProviderID[] = ['claude', 'codex'];
// claude-tui follows claude in the model picker so the two Claude surfaces read
// as a pair. It is intentionally absent from PROVIDER_SETTINGS_ORDER (it has no
// binary/enable settings of its own — it reuses claude's).
export const PROVIDER_MODEL_MENU_ORDER: ProviderID[] = ['codex', 'claude', 'claude-tui'];

export function getProviderDefinition(provider: ProviderID): ProviderDefinition;
export function getProviderDefinition(
  provider: string | null | undefined,
): ProviderDefinition | null;
export function getProviderDefinition(
  provider: string | null | undefined,
): ProviderDefinition | null {
  const id = asProviderID(provider);
  return id ? PROVIDER_DEFINITIONS[id] : null;
}

export function providerLabel(provider: string | null | undefined): string {
  return getProviderDefinition(provider)?.label ?? provider ?? 'Provider';
}

export function providerCliLabel(provider: string | null | undefined): string {
  return getProviderDefinition(provider)?.cliLabel ?? providerLabel(provider);
}

// Whether a provider supports a given affordance. Unknown providers default to
// supported so the gate only ever subtracts a capability an explicit definition
// has turned off (today: claude-tui). Pass a thread/pane provider string.
export function providerSupports(
  provider: string | null | undefined,
  capability: keyof ProviderCapabilities,
): boolean {
  const definition = getProviderDefinition(provider);
  return definition ? definition.capabilities[capability] : true;
}
