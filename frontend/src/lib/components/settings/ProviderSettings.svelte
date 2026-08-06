<script lang="ts">
  // ProviderSettings: per-provider provisioning + context window. Each
  // provider (Claude, Codex) renders as its own visually-grouped block
  // containing the binary path, status, model catalog, AND the
  // context-window/auto-compact controls scoped to that provider's
  // models. The text-generation routing controls live below as a
  // separate section since they apply across providers.

  import { GetProviderStatuses } from '../../stores/bindings';
  import {
    getSettings,
    updateSetting,
    updateSettingsPatch,
  } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type {
    CommitMessageStyle,
    ProviderStatus,
    ReasoningEffort,
  } from '../../types/settings';
  import {
    getProviderDefinition,
    PROVIDER_SETTINGS_ORDER,
    type ProviderDefinition,
    type ProviderID,
  } from '../../providers/catalog';
  import {
    getProviderModels,
    refreshProviderModels,
  } from '../../stores/providerModels.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import ProviderContextSettings from './ProviderContextSettings.svelte';
  import ProviderAccountsSettings from './ProviderAccountsSettings.svelte';
  import ProviderCustomEnvSection from './ProviderCustomEnvSection.svelte';
  import ProviderModelChips from './ProviderModelChips.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, SELECT_CLASS } from './styles';

  const PROVIDERS: ProviderDefinition[] = PROVIDER_SETTINGS_ORDER.map(
    (provider) => getProviderDefinition(provider),
  );

  let settings = $derived(getSettings());
  let textGenerationEffortOptions = $derived(
    getProviderDefinition(settings.textGenerationProvider).textGenerationEffortOptions,
  );
  let statuses = $state<ProviderStatus[]>([]);
  let loadGeneration = 0;

  $effect(() => {
    settings.claudeBinaryPath;
    settings.codexBinaryPath;

    const generation = ++loadGeneration;
    void (async () => {
      const [statusResult, ...modelResults] = await Promise.allSettled([
        GetProviderStatuses(),
        ...PROVIDER_SETTINGS_ORDER.map((provider) => refreshProviderModels(provider)),
      ]);
      if (generation !== loadGeneration) return;

      if (statusResult.status === 'fulfilled') {
        statuses = (statusResult.value ?? []) as ProviderStatus[];
      } else {
        console.error('Failed to load provider statuses:', statusResult.reason);
        addToast('error', 'Failed to load provider statuses.');
      }

      for (const [index, result] of modelResults.entries()) {
        if (result.status === 'fulfilled') continue;
        const provider = PROVIDER_SETTINGS_ORDER[index];
        const label = getProviderDefinition(provider).label;
        console.error(`Failed to load ${label} models:`, result.reason);
        addToast('error', `Failed to load ${label} models.`);
      }
    })();
  });

  function getStatus(provider: string): ProviderStatus | undefined {
    return statuses.find((s) => s.provider === provider);
  }

  function isTextGenerationEffortAllowed(
    provider: ProviderID,
    effort: ReasoningEffort,
  ): boolean {
    return getProviderDefinition(provider).textGenerationEffortOptions.some(
      (option) => option.value === effort,
    );
  }

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

  function statusDotColor(status: string): string {
    switch (status) {
      case 'ready':
        return 'bg-success';
      case 'error':
      case 'not_found':
        return 'bg-error';
      case 'version_too_old':
      case 'unauthenticated':
        return 'bg-warning';
      default:
        return 'bg-fg-hint';
    }
  }

  let textGenerationDefaultModel = $derived(
    getProviderDefinition(settings.textGenerationProvider).textGenerationDefaultModel,
  );

  const COMMIT_STYLE_OPTIONS: { value: CommitMessageStyle; label: string }[] = [
    { value: 'conventional', label: 'Conventional Commits' },
    { value: 'repo', label: 'Match repo history' },
    { value: 'custom', label: 'Custom instructions' },
  ];
</script>

<div class="flex flex-col gap-6">
  {#each PROVIDERS as provider (provider.id)}
    {@const status = getStatus(provider.id)}
    {@const models = getProviderModels(provider.id)}
    <section
      class="rounded-[var(--radius-card)] border border-border-subtle bg-surface-1/30 p-5"
      data-testid="settings-provider-{provider.id}"
    >
      <SettingsHeader
        title={provider.label}
        description={status?.message ||
          `Configure ${provider.label} availability for thread creation, sessions, and context budgets.`}
      >
        {#snippet badge()}
          <span
            class="inline-flex items-center gap-1.5 rounded-full border border-border-subtle bg-surface-0 px-2.5 py-0.5 text-[0.6875rem] text-fg-muted"
            data-testid="settings-provider-status-pill"
            data-status={status?.status ?? 'checking'}
          >
            <span
              class="h-1.5 w-1.5 rounded-full {statusDotColor(status?.status ?? 'unknown')}"
              aria-hidden="true"
            ></span>
            {status?.status ?? 'checking'}
            {#if status?.version}
              <span class="text-fg-hint">·</span>
              <span class="font-mono text-[0.65625rem] text-fg-muted">{status.version}</span>
            {/if}
          </span>
        {/snippet}
      </SettingsHeader>

      <div class="flex flex-col gap-1">
        <SettingsField
          label="Enabled"
          hint="Allow new threads to use this provider."
        >
          <ToggleSwitch
            checked={settings[provider.settings.enabledKey]}
            ariaLabel={`Toggle ${provider.label}`}
            onToggle={(value) => updateSetting(provider.settings.enabledKey, value)}
          />
        </SettingsField>

        <SettingsField
          label="Binary path"
          hint="Override the auto-detected CLI binary."
          htmlFor="{provider.id}-path"
        >
          <input
            id="{provider.id}-path"
            type="text"
            value={settings[provider.settings.pathKey]}
            onchange={(e) =>
              updateSetting(provider.settings.pathKey, (e.target as HTMLInputElement).value)}
            placeholder="Auto-detect"
            class="{INPUT_CLASS} max-w-[16rem]"
          />
        </SettingsField>

        <SettingsField
          label="Available models"
          hint="Click a model to show or hide it in model pickers."
          align="start"
          stacked={models.length > 3}
        >
          <ProviderModelChips provider={provider.id} {models} />
        </SettingsField>
      </div>

      <ProviderCustomEnvSection {provider} />

      <ProviderAccountsSettings provider={provider.id} />

      <div class="mt-5">
        <ProviderContextSettings provider={provider.id} />
      </div>
    </section>
  {/each}

  <section data-testid="settings-text-generation">
    <SettingsHeader
      title="Commit and PR Message CLI"
      description="Which CLI writes commit messages, PR bodies, and generated thread titles. Independent of the chat provider so Claude users can still spend Codex cycles on short text."
    />

    <div class="flex flex-col gap-1">
      <SettingsField
        label="Provider"
        hint="CLI that generates non-chat text."
        htmlFor="textgen-provider"
      >
        <select
          id="textgen-provider"
          data-testid="settings-textgen-provider"
          value={settings.textGenerationProvider}
          onchange={(e) =>
            updateTextGenerationProvider(
              (e.target as HTMLSelectElement).value as ProviderID,
            )}
          class={SELECT_CLASS}
        >
          {#each PROVIDERS as provider (provider.id)}
            <option value={provider.id}>{provider.label}</option>
          {/each}
        </select>
      </SettingsField>

      <SettingsField
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
            updateSetting(
              'textGenerationModel',
              (e.target as HTMLInputElement).value,
            )}
          placeholder={`Default: ${textGenerationDefaultModel}`}
          class="{INPUT_CLASS} max-w-[16rem]"
        />
      </SettingsField>

      <SettingsField
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
  </section>
</div>
