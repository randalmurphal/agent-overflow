<script lang="ts">
  // Settings → Agents → Prompts & Tools.
  //
  // Two levers per provider, both read by the backend at spawn:
  //   1. system-prompt replacement, scoped to selected models (Claude
  //      `--system-prompt`, Codex `baseInstructions`);
  //   2. built-in tools kept out of the model's context.
  //
  // Both are spawn-time, so a change never disturbs a running session — the
  // section says so once, here, rather than repeating it per control.
  //
  // Only providers the user has enabled render: a disabled provider starts
  // no sessions, so there is nothing for an override to apply to.

  import { getSettings } from '../../stores/settings.svelte';
  import {
    getProviderDefinition,
    PROVIDER_SETTINGS_ORDER,
    type ProviderDefinition,
  } from '../../providers/catalog';
  import ProviderPromptSection from './ProviderPromptSection.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsHeader from './SettingsHeader.svelte';

  const PROVIDERS: ProviderDefinition[] = PROVIDER_SETTINGS_ORDER.map((provider) =>
    getProviderDefinition(provider),
  );

  let settings = $derived(getSettings());
  let enabledProviders = $derived(
    PROVIDERS.filter((provider) => settings[provider.settings.enabledKey]),
  );
</script>

<div class="flex flex-col gap-6" data-testid="settings-prompts">
  <SettingsHeader
    title="Prompts & Tools"
    description="Replace a provider's system prompt for chosen models, and keep built-in tool schemas out of the model's context. Applies to sessions started after the change; running sessions are unaffected."
  />

  {#if enabledProviders.length === 0}
    <SettingsCallout>
      Enable a provider under Settings → Providers to configure its prompt and tools.
    </SettingsCallout>
  {:else}
    {#each enabledProviders as provider (provider.id)}
      <ProviderPromptSection {provider} />
    {/each}
  {/if}
</div>
