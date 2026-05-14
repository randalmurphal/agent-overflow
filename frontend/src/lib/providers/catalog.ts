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
      { value: 'xhigh', label: 'Extra High' },
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
      { value: 'xhigh', label: 'Extra High' },
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
  },
};

export const PROVIDER_SETTINGS_ORDER: ProviderID[] = ['claude', 'codex'];
export const PROVIDER_MODEL_MENU_ORDER: ProviderID[] = ['codex', 'claude'];

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
