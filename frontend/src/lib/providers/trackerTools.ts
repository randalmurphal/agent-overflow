// The providers whose agents get the tracker tools (ao-tracker-tools), as
// Go's provider.Capabilities.SupportsTrackerMCP says; a Go test in
// internal/provider pins this list to it. In the order Ask your agent
// prefers them when the default provider has no tracker tools.

import { PROVIDER_DEFINITIONS, providerIsEnabled, type ProviderEnablementSettings } from './catalog';
import type { ProviderID } from '../types/providers';

export const TRACKER_TOOL_PROVIDERS: readonly ProviderID[] = ['claude', 'codex'];

/** The provider a thread that needs the tracker tools starts on: the
 * default when it has them, else the first of TRACKER_TOOL_PROVIDERS
 * turned on; null when none is. */
export function trackerToolsProvider(defaultProvider: string, settings: ProviderEnablementSettings): string | null {
  const has = (p: string) => (TRACKER_TOOL_PROVIDERS as readonly string[]).includes(p) && providerIsEnabled(settings, p);
  if (has(defaultProvider)) return defaultProvider;
  return TRACKER_TOOL_PROVIDERS.find(has) ?? null;
}

/** Why a thread that needs the tracker tools cannot start on computer
 * `computerName`: '' when a provider with them is turned on. */
export function trackerToolsBlocked(settings: ProviderEnablementSettings, computerName: string): string {
  if (TRACKER_TOOL_PROVIDERS.some((p) => providerIsEnabled(settings, p))) return '';
  const names = TRACKER_TOOL_PROVIDERS.map((p) => PROVIDER_DEFINITIONS[p].label);
  return `Ask needs ${names.join(' or ')}, whose agents get the tracker tools; turn one on in Settings on ${computerName}.`;
}
