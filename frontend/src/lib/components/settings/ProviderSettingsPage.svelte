<script lang="ts">
  // One provider's whole settings page — the body of both ClaudeSettings and
  // CodexSettings, which differ only in the `provider` they pass.
  //
  // Everything that configures Claude or Codex lives here as a flat run of
  // sections: setup (binary, models), environment, accounts, the context
  // window, the system-prompt overrides, the tool list, and — for Claude —
  // the spawn-time session axes and the peer inbox. The page renders every
  // section regardless of the Enabled toggle: a provider is commonly
  // configured before it is switched on, and a disabled provider's stored
  // settings are still the ones its next session will use.
  //
  // SettingsView renders the page title and description from `sections.ts`,
  // so nothing here repeats the provider's name at the top.

  import { GetProviderStatuses } from '../../stores/bindings';
  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting, call, backend } = settingsComputer();
  import { addToast } from '../../stores/toast.svelte';
  import type { ProviderStatus } from '../../types/settings';
  import {
    dependentProviders,
    getProviderDefinition,
    type ProviderID,
  } from '../../providers/catalog';
  import {
    getProviderModels,
    refreshProviderModels,
  } from '../../stores/providerModels.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import ClaudeCrossSessionEditor from './ClaudeCrossSessionEditor.svelte';
  import ClaudeDisabledToolsEditor from './ClaudeDisabledToolsEditor.svelte';
  import ClaudeSessionAxesEditor from './ClaudeSessionAxesEditor.svelte';
  import CodexDisabledToolsEditor from './CodexDisabledToolsEditor.svelte';
  import ProviderAccountsSettings from './ProviderAccountsSettings.svelte';
  import ProviderContextSettings from './ProviderContextSettings.svelte';
  import ProviderCustomEnvSection from './ProviderCustomEnvSection.svelte';
  import ProviderModelChips from './ProviderModelChips.svelte';
  import ProviderPromptSection from './ProviderPromptSection.svelte';
  import SettingsField from './SettingsField.svelte';
  import SettingsHeader from './SettingsHeader.svelte';
  import {
    providerFieldId,
    type SettingsFieldId,
    type SettingsProvider,
  } from './fields';
  import { INPUT_CLASS } from './styles';

  let { provider }: { provider: SettingsProvider } = $props();

  // A dependent provider (today only Claude TUI) has no page of its own, so
  // its enable toggle rides on its parent's Setup section and needs the
  // parent page's field id. The table is the narrowing: a new dependent
  // without an entry renders no row rather than an unsearchable one, which
  // is the same signal `fields.test.ts` gives for a missing registration.
  const DEPENDENT_FIELD_IDS: Partial<Record<ProviderID, SettingsFieldId>> = {
    'claude-tui': 'claude.tui',
  };

  let definition = $derived(getProviderDefinition(provider));
  let settings = $derived(getSettings());
  let models = $derived(getProviderModels(provider, backend));
  let dependents = $derived(dependentProviders(provider));
  let status = $state<ProviderStatus | undefined>(undefined);
  let loadGeneration = 0;

  // The binary path as a $derived primitive: the settings object is replaced
  // wholesale on EVERY save (optimistic patch + server merge), so an effect
  // reading the field off it directly re-ran on any toggle on this page —
  // and each run spawns `claude --version` / `codex --version` via
  // GetProviderStatuses plus a cache-bypassing catalog refresh. The derived
  // only propagates when the path actually changes.
  let binaryPath = $derived(settings[definition.settings.pathKey]);

  $effect(() => {
    void binaryPath;

    const generation = ++loadGeneration;
    void (async () => {
      const [statusResult, modelResult] = await Promise.allSettled([
        call(() => GetProviderStatuses()),
        refreshProviderModels(provider, backend),
      ]);
      if (generation !== loadGeneration) return;

      if (statusResult.status === 'fulfilled') {
        const all = (statusResult.value ?? []) as ProviderStatus[];
        status = all.find((s) => s.provider === provider);
      } else {
        console.error('Failed to load provider statuses:', statusResult.reason);
        addToast('error', 'Failed to load provider statuses.');
      }

      if (modelResult.status === 'rejected') {
        console.error(`Failed to load ${definition.label} models:`, modelResult.reason);
        addToast('error', `Failed to load ${definition.label} models.`);
      }
    })();
  });

  function statusDotColor(state: string): string {
    switch (state) {
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

  // What a system-prompt save DOES depends on the provider, so the section
  // says which is which rather than making one claim for both. See
  // app_session_prompt_override.go: applySettingsOwnedAxes is the spawn half
  // of the pair, reconcileSettingsOwnedAxes the live half.
  let promptTiming = $derived(
    provider === 'claude'
      ? 'An edit, or turning one on, reaches running headless sessions right away; turning one off applies when the session restarts. Claude TUI sessions pick it up on their next start.'
      : 'Applies to sessions started later.',
  );
</script>

<div class="settings-sections">
  <section data-testid="settings-provider-{provider}">
    <SettingsHeader
      title="Setup"
      description={status?.message ||
        `Whether ${definition.label} is offered to new threads, and which binary and models it runs with.`}
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
        id={providerFieldId(provider, 'enabled')}
        label="Enabled"
        hint={definition.enableHint ?? 'Allow new threads to use this provider.'}
      >
        <ToggleSwitch
          checked={settings[definition.settings.enabledKey]}
          ariaLabel={`Toggle ${definition.label}`}
          onToggle={(value) => updateSetting(definition.settings.enabledKey, value)}
        />
      </SettingsField>

      <!--
        Providers spawned through this one's binary (today: Claude TUI)
        have no page of their own, so their enable flag rides here. The
        toggle WRITES its own key — the parent dependency is a read rule,
        owned by providerIsEnabled — and goes inert while the parent is
        off, because the value cannot change what any picker shows until
        the parent comes back.
      -->
      {#each dependents as dependent (dependent.id)}
        {@const fieldId = DEPENDENT_FIELD_IDS[dependent.id]}
        {#if fieldId}
          <SettingsField
            id={fieldId}
            label={dependent.label}
            hint={dependent.enableHint ?? `Allow new threads to use ${dependent.label}.`}
          >
            <ToggleSwitch
              checked={settings[dependent.settings.enabledKey]}
              disabled={!settings[definition.settings.enabledKey]}
              ariaLabel={`Toggle ${dependent.label}`}
              onToggle={(value) => updateSetting(dependent.settings.enabledKey, value)}
            />
          </SettingsField>
        {/if}
      {/each}

      <SettingsField
        id={providerFieldId(provider, 'binary-path')}
        label="Binary path"
        hint="Override the auto-detected CLI binary."
        htmlFor="{provider}-path"
      >
        <input
          id="{provider}-path"
          type="text"
          value={settings[definition.settings.pathKey]}
          onchange={(e) =>
            updateSetting(definition.settings.pathKey, (e.target as HTMLInputElement).value)}
          placeholder="Auto-detect"
          class="{INPUT_CLASS} max-w-[16rem]"
        />
      </SettingsField>

      <SettingsField
        id={providerFieldId(provider, 'models')}
        label="Available models"
        hint="Click a model to show or hide it in model pickers."
        align="start"
        stacked={models.length > 3}
      >
        <ProviderModelChips {provider} {models} />
      </SettingsField>
    </div>
  </section>

  <section>
    <SettingsHeader title="Environment" />
    <ProviderCustomEnvSection provider={definition} />
  </section>

  <section>
    <ProviderAccountsSettings {provider} />
  </section>

  <section>
    <SettingsHeader title="Context window" />
    <ProviderContextSettings {provider} />
  </section>

  <section>
    <SettingsHeader title="System prompt" description={promptTiming} />
    <ProviderPromptSection provider={definition} />
  </section>

  <section>
    <SettingsHeader title="Tools" description="Applies to sessions started later." />
    {#if provider === 'codex'}
      <CodexDisabledToolsEditor provider={definition} />
    {:else}
      <ClaudeDisabledToolsEditor provider={definition} />
    {/if}
  </section>

  <!--
    Headless Claude only. claude-tui launches through a PTY with no
    `--settings` flag, so these axes cannot reach that binary and must not be
    offered under its heading.
  -->
  {#if provider === 'claude'}
    <section>
      <SettingsHeader title="Session" />
      <ClaudeSessionAxesEditor />
    </section>

    <section>
      <SettingsHeader title="Cross-session messaging" />
      <ClaudeCrossSessionEditor />
    </section>
  {/if}
</div>
