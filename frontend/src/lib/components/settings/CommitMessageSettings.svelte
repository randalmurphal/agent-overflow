<script lang="ts">
  // Commit messages: the CLI that writes commit messages, PR bodies and
  // generated thread titles. It is routed independently of the chat
  // provider, so this is its own page rather than a block on either
  // provider's — a Claude user can spend Codex cycles on short text.
  //
  // SettingsView renders the page title and description, so there is no
  // header here.

  import { updateSetting, updateSettingsPatch, getSettings } from '../../stores/settings.svelte';
  import type { CommitMessageStyle, ReasoningEffort } from '../../types/settings';
  import {
    getProviderDefinition,
    PROVIDER_SETTINGS_ORDER,
    type ProviderDefinition,
    type ProviderID,
  } from '../../providers/catalog';
  import SettingsField from './SettingsField.svelte';
  import { INPUT_CLASS, SELECT_CLASS } from './styles';

  const PROVIDERS: ProviderDefinition[] = PROVIDER_SETTINGS_ORDER.map((provider) =>
    getProviderDefinition(provider),
  );

  const COMMIT_STYLE_OPTIONS: { value: CommitMessageStyle; label: string }[] = [
    { value: 'conventional', label: 'Conventional Commits' },
    { value: 'repo', label: 'Match repo history' },
    { value: 'custom', label: 'Custom instructions' },
  ];

  let settings = $derived(getSettings());
  let textGenerationEffortOptions = $derived(
    getProviderDefinition(settings.textGenerationProvider).textGenerationEffortOptions,
  );
  let textGenerationDefaultModel = $derived(
    getProviderDefinition(settings.textGenerationProvider).textGenerationDefaultModel,
  );

  function isTextGenerationEffortAllowed(
    provider: ProviderID,
    effort: ReasoningEffort,
  ): boolean {
    return getProviderDefinition(provider).textGenerationEffortOptions.some(
      (option) => option.value === effort,
    );
  }

  // The two providers offer different effort tiers, so switching to one that
  // does not carry the stored effort resets it in the same patch — a single
  // write, so the select can never render a value its own option list is
  // missing.
  function updateTextGenerationProvider(provider: ProviderID) {
    const patch: {
      textGenerationProvider: ProviderID;
      textGenerationReasoningEffort?: ReasoningEffort;
    } = { textGenerationProvider: provider };
    if (!isTextGenerationEffortAllowed(provider, settings.textGenerationReasoningEffort)) {
      patch.textGenerationReasoningEffort = 'low';
    }
    void updateSettingsPatch(patch);
  }
</script>

<div class="flex flex-col gap-1" data-testid="settings-text-generation">
  <SettingsField
    id="commit-messages.provider"
    label="Provider"
    hint="CLI that generates non-chat text."
    htmlFor="textgen-provider"
  >
    <select
      id="textgen-provider"
      data-testid="settings-textgen-provider"
      value={settings.textGenerationProvider}
      onchange={(e) =>
        updateTextGenerationProvider((e.target as HTMLSelectElement).value as ProviderID)}
      class={SELECT_CLASS}
    >
      {#each PROVIDERS as provider (provider.id)}
        <option value={provider.id}>{provider.label}</option>
      {/each}
    </select>
  </SettingsField>

  <SettingsField
    id="commit-messages.model"
    label="Model"
    hint="Leave empty to use the provider's default small-text model."
    htmlFor="textgen-model"
  >
    <input
      id="textgen-model"
      type="text"
      data-testid="settings-textgen-model"
      value={settings.textGenerationModel}
      onchange={(e) =>
        updateSetting('textGenerationModel', (e.target as HTMLInputElement).value)}
      placeholder={`Default: ${textGenerationDefaultModel}`}
      class="{INPUT_CLASS} max-w-[16rem]"
    />
  </SettingsField>

  <SettingsField
    id="commit-messages.reasoning-effort"
    label="Reasoning effort"
    hint="Budget for commit/PR text generation."
    htmlFor="textgen-effort"
  >
    <select
      id="textgen-effort"
      data-testid="settings-textgen-effort"
      value={settings.textGenerationReasoningEffort}
      onchange={(e) =>
        updateSetting(
          'textGenerationReasoningEffort',
          (e.target as HTMLSelectElement).value as ReasoningEffort,
        )}
      class={SELECT_CLASS}
    >
      {#each textGenerationEffortOptions as opt (opt.value)}
        <option value={opt.value}>{opt.label}</option>
      {/each}
    </select>
  </SettingsField>

  <SettingsField
    id="commit-messages.style"
    label="Commit message style"
    hint="Phrasing guidance for generated commit messages."
    htmlFor="textgen-commit-style"
  >
    <select
      id="textgen-commit-style"
      data-testid="settings-commit-message-style"
      value={settings.commitMessageStyle}
      onchange={(e) =>
        updateSetting(
          'commitMessageStyle',
          (e.target as HTMLSelectElement).value as CommitMessageStyle,
        )}
      class={SELECT_CLASS}
    >
      {#each COMMIT_STYLE_OPTIONS as opt (opt.value)}
        <option value={opt.value}>{opt.label}</option>
      {/each}
    </select>
  </SettingsField>

  {#if settings.commitMessageStyle === 'custom'}
    <SettingsField
      id="commit-messages.style-instructions"
      label="Style instructions"
      hint="Free-text rules the generated subject and body should follow."
      htmlFor="textgen-commit-style-custom"
      align="start"
    >
      <textarea
        id="textgen-commit-style-custom"
        data-testid="settings-commit-message-style-custom"
        rows={3}
        maxlength={4000}
        value={settings.commitMessageStyleCustom}
        onchange={(e) =>
          updateSetting(
            'commitMessageStyleCustom',
            (e.target as HTMLTextAreaElement).value,
          )}
        placeholder="e.g. Start subjects with a Jira ticket key; keep bodies to one bullet per change."
        class="{INPUT_CLASS} max-w-[24rem] resize-none"
      ></textarea>
    </SettingsField>
  {/if}
</div>
