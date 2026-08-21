<script lang="ts">
  // Settings → Agents → Prompts & Tools.
  //
  // Two levers per provider, both read by the backend at spawn:
  //   1. system-prompt replacement, scoped to selected models (Claude
  //      `--system-prompt`, Codex `baseInstructions`);
  //   2. built-in tools kept out of the model's context.
  //
  // What a save DOES depends on the provider and the axis, so the section
  // says which is which rather than making one claim for all four:
  //
  //   claude prompt, edited or turned on  → live `set_model.system_prompt`
  //                                         on running headless sessions
  //   claude prompt, turned off           → deferred restart (no
  //                                         revert-to-built-in wire form)
  //   codex prompt, claude-tui prompt     → next session only
  //   either tool list                    → next session only (spawn-only
  //                                         on every provider)
  //
  // See app_session_prompt_override.go: applySettingsOwnedAxes is the spawn
  // half of the pair, reconcileSettingsOwnedAxes the live half.
  //
  // Only providers the user has enabled render: a disabled provider starts
  // no sessions, so there is nothing for an override to apply to.

  import { getSettings } from '../../stores/settings.svelte';
  import {
    getProviderDefinition,
    providerIsEnabled,
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
    PROVIDERS.filter((provider) => providerIsEnabled(settings, provider.id)),
  );
</script>

<div class="flex flex-col gap-6" data-testid="settings-prompts">
  <SettingsHeader
    title="Prompts & Tools"
    description="Replace a provider's system prompt for chosen models, and keep built-in tool schemas out of the model's context. A Claude prompt edit reaches running Claude sessions right away; turning one off applies when the session restarts. Codex prompts, Claude TUI sessions, and both tool lists apply to sessions started later."
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
