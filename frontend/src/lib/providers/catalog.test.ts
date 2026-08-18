import { describe, expect, it } from 'vitest';
import {
  asProviderID,
  dependentProviders,
  getProviderDefinition,
  PROVIDER_DEFINITIONS,
  PROVIDER_IDS,
  PROVIDER_MODEL_MENU_ORDER,
  PROVIDER_SETTINGS_ORDER,
  providerCliLabel,
  providerIsEnabled,
  providerSupports,
  type ProviderCapabilities,
  type ProviderEnablementSettings,
} from './catalog';

function enablement(
  overrides: Partial<ProviderEnablementSettings> = {},
): ProviderEnablementSettings {
  return {
    claudeEnabled: true,
    codexEnabled: true,
    claudeTuiEnabled: true,
    ...overrides,
  };
}

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
    // claude-tui follows claude in the model picker but stays out of the
    // settings order — it has no binary/account settings of its own, and its
    // enable toggle rides inside claude's section instead.
    expect(PROVIDER_MODEL_MENU_ORDER).toEqual(['codex', 'claude', 'claude-tui']);
    expect(PROVIDER_DEFINITIONS.claude.textGenerationDefaultModel).toBe('claude-haiku-4-5');
    expect(PROVIDER_DEFINITIONS.codex.textGenerationDefaultModel).toBe('gpt-5.6-luna');
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

  it('gates claude-tui on BOTH its own flag and claude', () => {
    // The AND is the whole point: claude-tui runs claude's binary under
    // claude's auth, so neither flag alone can offer it.
    expect(providerIsEnabled(enablement(), 'claude-tui')).toBe(true);
    expect(providerIsEnabled(enablement({ claudeTuiEnabled: false }), 'claude-tui')).toBe(false);
    expect(providerIsEnabled(enablement({ claudeEnabled: false }), 'claude-tui')).toBe(false);
    expect(
      providerIsEnabled(
        enablement({ claudeEnabled: false, claudeTuiEnabled: false }),
        'claude-tui',
      ),
    ).toBe(false);
  });

  it('leaves the independent providers reading their own flag only', () => {
    // claudeTuiEnabled must not leak upward into claude, and the two
    // independent providers must not read each other.
    expect(providerIsEnabled(enablement({ claudeTuiEnabled: false }), 'claude')).toBe(true);
    expect(providerIsEnabled(enablement({ claudeTuiEnabled: false }), 'codex')).toBe(true);
    expect(providerIsEnabled(enablement({ claudeEnabled: false }), 'claude')).toBe(false);
    expect(providerIsEnabled(enablement({ claudeEnabled: false }), 'codex')).toBe(true);
    expect(providerIsEnabled(enablement({ codexEnabled: false }), 'codex')).toBe(false);
    expect(providerIsEnabled(enablement({ codexEnabled: false }), 'claude')).toBe(true);
  });

  it('answers false for a provider the catalog cannot describe', () => {
    // Opposite default to providerSupports, deliberately: there is nothing to
    // OFFER for an id with no definition behind it.
    expect(providerIsEnabled(enablement(), 'unknown')).toBe(false);
    expect(providerIsEnabled(enablement(), null)).toBe(false);
    expect(providerIsEnabled(enablement(), undefined)).toBe(false);
    expect(providerIsEnabled(enablement(), '')).toBe(false);
  });

  it('places every dependent provider under exactly one parent section', () => {
    expect(dependentProviders('claude').map((p) => p.id)).toEqual(['claude-tui']);
    expect(dependentProviders('codex')).toEqual([]);
    expect(dependentProviders('claude-tui')).toEqual([]);

    for (const id of PROVIDER_IDS) {
      const parent = PROVIDER_DEFINITIONS[id].dependsOnProvider;
      if (parent === undefined) {
        // A provider without a parent must own a settings section, or its
        // enable toggle has nowhere to render.
        expect(PROVIDER_SETTINGS_ORDER).toContain(id);
        continue;
      }
      expect(parent).not.toBe(id);
      // One link deep: the parent must itself be independent, so the toggle
      // always lands in a rendered section.
      expect(PROVIDER_DEFINITIONS[parent].dependsOnProvider).toBeUndefined();
      expect(PROVIDER_SETTINGS_ORDER).not.toContain(id);
    }
  });

  it('gives every provider its own enable key', () => {
    const keys = PROVIDER_IDS.map((id) => PROVIDER_DEFINITIONS[id].settings.enabledKey);
    // Two providers sharing a key would make one toggle silently drive the
    // other — which is exactly what claude-tui used to do with claudeEnabled.
    expect(new Set(keys).size).toBe(keys.length);
  });

  it('falls back cleanly for unknown display labels', () => {
    expect(getProviderDefinition('claude')?.label).toBe('Claude');
    expect(getProviderDefinition('claude-tui')?.label).toBe('Claude TUI');
    expect(getProviderDefinition('unknown')).toBeNull();
    expect(providerCliLabel('unknown')).toBe('unknown');
    expect(providerCliLabel(null)).toBe('Provider');
  });
});
