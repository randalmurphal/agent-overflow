export const PROVIDER_IDS = ['claude', 'codex'] as const;

export type ProviderID = (typeof PROVIDER_IDS)[number];

export function isProviderID(value: unknown): value is ProviderID {
  return value === 'claude' || value === 'codex';
}

export function asProviderID(value: unknown): ProviderID | null {
  return isProviderID(value) ? value : null;
}
