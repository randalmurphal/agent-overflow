import { describe, expect, it } from 'vitest';
import {
  asProviderID,
  getProviderDefinition,
  PROVIDER_DEFINITIONS,
  PROVIDER_MODEL_MENU_ORDER,
  PROVIDER_SETTINGS_ORDER,
  providerCliLabel,
} from './catalog';

describe('provider catalog', () => {
  it('normalizes supported provider ids', () => {
    expect(asProviderID('claude')).toBe('claude');
    expect(asProviderID('codex')).toBe('codex');
    expect(asProviderID('other')).toBeNull();
  });

  it('keeps provider-specific defaults in one catalog', () => {
    expect(PROVIDER_SETTINGS_ORDER).toEqual(['claude', 'codex']);
    expect(PROVIDER_MODEL_MENU_ORDER).toEqual(['codex', 'claude']);
    expect(PROVIDER_DEFINITIONS.claude.textGenerationDefaultModel).toBe('claude-haiku-4-5');
    expect(PROVIDER_DEFINITIONS.codex.textGenerationDefaultModel).toBe('gpt-5.4-mini');
    expect(PROVIDER_DEFINITIONS.claude.contextLabels.standard).toBe('200k');
    expect(PROVIDER_DEFINITIONS.codex.contextLabels.standard).toBe('272k');
    expect(PROVIDER_DEFINITIONS.claude.backgroundStop).toBe('claude-task');
    expect(PROVIDER_DEFINITIONS.codex.backgroundStop).toBe('codex-background-terminals');
  });

  it('falls back cleanly for unknown display labels', () => {
    expect(getProviderDefinition('claude')?.label).toBe('Claude');
    expect(getProviderDefinition('unknown')).toBeNull();
    expect(providerCliLabel('unknown')).toBe('unknown');
    expect(providerCliLabel(null)).toBe('Provider');
  });
});
