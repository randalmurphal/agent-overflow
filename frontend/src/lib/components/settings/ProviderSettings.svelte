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
  import type { ProviderStatus, ReasoningEffort } from '../../types/settings';
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
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import { INPUT_CLASS, SELECT_CLASS } from './styles';

  // contextTarget previously plumbed a per-thread compact override into
  // the settings page when the chat-meter "Configure context" button
  // was clicked. The new design treats compact thresholds as
  // per-provider settings (no per-thread mode in this UI), so the prop
  // is intentionally ignored — kept on the Wails dispatch surface so
  // the meter button can still navigate to the providers tab without
  // breaking older clients.
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  let { contextTarget: _ = null }: { contextTarget?: unknown } = $props();

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
</script>

<div class="flex flex-col gap-10">
  {#each PROVIDERS as provider (provider.id)}
    {@const status = getStatus(provider.id)}
    {@const models = getProviderModels(provider.id)}
    <section
      class="rounded-[var(--radius-card)] border border-border-subtle bg-surface-1/30 p-5"
      data-testid="settings-provider-{provider.id}"
    >
      <header class="flex flex-wrap items-start justify-between gap-3">
        <div class="min-w-0">
          <span
            class="text-[0.65625rem] font-medium uppercase tracking-[0.16em] text-fg-hint"
          >
            Provider
          </span>
          <h3 class="mt-1 text-[1rem] font-semibold text-fg leading-tight">
            {provider.label}
          </h3>
          <p class="mt-1 max-w-xl text-[0.75rem] leading-relaxed text-fg-muted">
            {status?.message ||
              `Configure ${provider.label} availability for thread creation, sessions, and context budgets.`}
          </p>
        </div>
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
      </header>

      <div class="mt-4 flex flex-col gap-1">
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
          hint="Models exposed by the provider's catalog."
          align="start"
          stacked={models.length > 3}
        >
          {#if models.length > 0}
            <div
              class="flex flex-wrap gap-1.5"
              data-testid="settings-provider-models"
            >
              {#each models as model (model.slug)}
                <span
                  class="rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 px-2 py-0.5 text-[0.6875rem] text-fg-muted"
                >
                  {model.name || model.slug}
                </span>
              {/each}
            </div>
          {:else}
            <span class="text-[0.75rem] text-fg-muted">No models available.</span>
          {/if}
        </SettingsField>
      </div>

      <div class="mt-5">
        <ProviderContextSettings provider={provider.id} />
      </div>
    </section>
  {/each}

  <section data-testid="settings-text-generation">
    <SettingsHeader
      eyebrow="Text generation"
      title="Commit and PR Message CLI"
      description="Which CLI writes commit messages, PR bodies, and generated thread titles. Independent of the chat provider so Claude users can still spend Codex cycles on short text."
    />

    <div class="mt-4 flex flex-col gap-1">
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
    </div>
  </section>
</div>
