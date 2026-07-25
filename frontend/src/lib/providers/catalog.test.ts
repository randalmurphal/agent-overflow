import { describe, expect, it } from 'vitest';
import {
  asProviderID,
  getProviderDefinition,
  PROVIDER_DEFINITIONS,
  PROVIDER_MODEL_MENU_ORDER,
  PROVIDER_SETTINGS_ORDER,
  providerCliLabel,
  providerSupports,
  type ProviderCapabilities,
} from './catalog';

const ALL_CAPABILITIES: (keyof ProviderCapabilities)[] = [
  'runtimeModes',
  'planMode',
  'fork',
  'attachments',
  'mcp',
];

describe('provider catalog', () => {
  it('normalizes supported provider ids', () => {
    expect(asProviderID('claude')).toBe('claude');
    expect(asProviderID('codex')).toBe('codex');
    expect(asProviderID('claude-tui')).toBe('claude-tui');
    expect(asProviderID('other')).toBeNull();
  });

  it('keeps provider-specific defaults in one catalog', () => {
    expect(PROVIDER_SETTINGS_ORDER).toEqual(['claude', 'codex']);
    // claude-tui is offered in the model picker (it follows claude) but stays
    // out of the settings order (no binary/enable settings of its own).
    expect(PROVIDER_MODEL_MENU_ORDER).toEqual(['codex', 'claude', 'claude-tui']);
    expect(PROVIDER_DEFINITIONS.claude.textGenerationDefaultModel).toBe('claude-haiku-4-5');
    expect(PROVIDER_DEFINITIONS.codex.textGenerationDefaultModel).toBe('gpt-5.6-sol');
    expect(PROVIDER_DEFINITIONS.claude.contextLabels.standard).toBe('200k');
    expect(PROVIDER_DEFINITIONS.codex.contextLabels.standard).toBe('272k');
    expect(PROVIDER_DEFINITIONS.claude.backgroundStop).toBe('claude-task');
    expect(PROVIDER_DEFINITIONS.codex.backgroundStop).toBe('codex-background-terminals');
    expect(PROVIDER_DEFINITIONS['claude-tui'].backgroundStop).toBe('none');
  });

  it('grants every affordance to claude and codex', () => {
    for (const capability of ALL_CAPABILITIES) {
      expect(providerSupports('claude', capability)).toBe(true);
      expect(providerSupports('codex', capability)).toBe(true);
    }
  });

  it('withholds every AO-mediated affordance from claude-tui except attachments', () => {
    // claude-tui reaches most affordances inside the real TUI via take-control, so
    // the UI must hide/disable them — every flag false guards against one flipping
    // back on. attachments is the exception: AO injects an image's file path into
    // the TUI composer, so claude-tui DOES accept composer attachments.
    expect(providerSupports('claude-tui', 'attachments')).toBe(true);
    for (const capability of ALL_CAPABILITIES) {
      if (capability === 'attachments') continue;
      expect(providerSupports('claude-tui', capability)).toBe(false);
    }
  });

  it('defaults unknown / absent providers to supported so the gate only subtracts', () => {
    expect(providerSupports('unknown', 'fork')).toBe(true);
    expect(providerSupports(null, 'mcp')).toBe(true);
    expect(providerSupports(undefined, 'planMode')).toBe(true);
  });

  it('falls back cleanly for unknown display labels', () => {
    expect(getProviderDefinition('claude')?.label).toBe('Claude');
    expect(getProviderDefinition('claude-tui')?.label).toBe('Claude TUI');
    expect(getProviderDefinition('unknown')).toBeNull();
    expect(providerCliLabel('unknown')).toBe('unknown');
    expect(providerCliLabel(null)).toBe('Provider');
  });
});
