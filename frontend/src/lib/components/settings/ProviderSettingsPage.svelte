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

  import { untrack } from 'svelte';
  import { GetProviderStatuses } from '../../stores/bindings';
  import { HOST_TIER_REASON, settingsComputer } from './settingsComputer';
  const { getSettings, updateSetting, call, backend, hostTierWritable, scopeGrant } = settingsComputer();
  import {
    checkProviderUpdates,
    openProviderUpdateNotes,
    providerUpdate,
    providerUpdatesLoadError,
  } from '../../stores/providerUpdates.svelte';
  import Button from '../primitives/Button.svelte';
  import ProviderUpdateButton from './ProviderUpdateButton.svelte';
  import ProviderUpdateFailure from './ProviderUpdateFailure.svelte';
  import { addErrorToast } from '../../stores/toast.svelte';
  import type { ProviderStatus } from '../../types/settings';
  import {
    dependentProviders,
    getProviderDefinition,
    type ProviderID,
  } from '../../providers/catalog';
  import {
    ensureProviderModels,
    getProviderModels,
    refreshProviderModels,
  } from '../../stores/providerModels.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import ClaudeCommandTimeoutField from './ClaudeCommandTimeoutField.svelte';
  import ClaudeCrossSessionEditor from './ClaudeCrossSessionEditor.svelte';
  import ClaudeDisabledToolsEditor from './ClaudeDisabledToolsEditor.svelte';
  import ClaudeSessionAxesEditor from './ClaudeSessionAxesEditor.svelte';
  import CodexDisabledToolsEditor from './CodexDisabledToolsEditor.svelte';
  import AccountAutoSwitchSettings from './AccountAutoSwitchSettings.svelte';
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
  import { offeredModels } from '../../utils/hiddenModels';

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
  // The enable flags and the binary path are host tier: they decide which
  // binaries THIS backend spawns (internal/settings/tier.go).
  let hostWritable = $derived(hostTierWritable());
  let hostReason = $derived(hostWritable ? undefined : HOST_TIER_REASON);
  let settings = $derived(getSettings());
  let models = $derived(offeredModels(getProviderModels(provider, backend)));
  let dependents = $derived(dependentProviders(provider));
  let status = $state<ProviderStatus | undefined>(undefined);
  let statusGeneration = 0;
  let modelGeneration = 0;

  // The binary path as a $derived primitive: the settings object is replaced
  // wholesale on EVERY save (optimistic patch + server merge), so an effect
  // reading the field off it directly re-ran on any toggle on this page —
  // and each run spawns `claude --version` / `codex --version` via
  // GetProviderStatuses plus a cache-bypassing catalog refresh. The derived
  // only propagates when the path actually changes.
  let binaryPath = $derived(settings[definition.settings.pathKey]);

  // The status and the model catalog load independently, so the pill and
  // the Setup description never wait on the slower catalog refresh.
  function readStatus(): void {
    const generation = ++statusGeneration;
    void (async () => {
      try {
        const all = ((await call(() => GetProviderStatuses())) ?? []) as ProviderStatus[];
        if (generation !== statusGeneration) return;
        status = all.find((s) => s.provider === provider);
      } catch (err) {
        if (generation !== statusGeneration) return;
        console.error('Failed to load provider statuses:', err);
        addErrorToast('Failed to load provider statuses.', err);
      }
    })();
  }

  $effect(() => {
    void binaryPath;
    readStatus();
  });

  let initialBinaryPathSeen = false;
  $effect(() => {
    void binaryPath;
    const isInitialMount = !initialBinaryPathSeen;
    initialBinaryPathSeen = true;
    const generation = ++modelGeneration;
    const loadModels = isInitialMount
      ? ensureProviderModels(provider, backend)
      : refreshProviderModels(provider, backend);
    loadModels.catch((err: unknown) => {
      if (generation !== modelGeneration) return;
      console.error(`Failed to load ${definition.label} models:`, err);
      addErrorToast(`Failed to load ${definition.label} models.`, err);
    });
  });

  // Provider CLI updates (stores/providerUpdates.svelte.ts). Every update
  // RPC is `access:admin`, so without it the page offers none of this.
  let admin = $derived(scopeGrant('access:admin') === 'granted');
  let update = $derived(admin ? providerUpdate(provider, backend) : null);
  let notesOffered = $derived(update !== null && update.available && update.latestVersion !== '');
  let checkFailure = $derived(admin ? update?.checkError || providerUpdatesLoadError(backend) : '');

  // A fresh check whenever the page is shown with the grant.
  $effect(() => {
    if (admin) untrack(() => void checkProviderUpdates(backend));
  });

  // A finished update changes the installed version, and the pill re-reads
  // it. The first version seen is the one the pill already loaded.
  let installedVersion = $derived(update?.installedVersion ?? '');
  let seenInstalledVersion = '';
  $effect(() => {
    const version = installedVersion;
    if (version === '') return;
    const previous = seenInstalledVersion;
    seenInstalledVersion = version;
    if (previous !== '' && previous !== version) untrack(readStatus);
  });

  // Auto-detect found the npm wrapper; the backend runs the native binary it
  // would spawn (internal/app/app_codex_binary.go).
  let pillTitle = $derived(
    status?.wrapperPath
      ? `Runs ${status.binaryPath}, the native binary the npm wrapper ${status.wrapperPath} starts, so threads skip its Node process.`
      : status?.version || undefined,
  );

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
  // A prompt change never reaches a running session: it applies when the
  // thread's session next starts, and a running thread shows a banner that
  // offers the restart (docs/decisions.md, 2026-10-14).
  const promptTiming = "Applies when a thread's session next starts. Running threads show a banner offering a restart.";
</script>

<div class="settings-sections">
  <section data-testid="settings-provider-{provider}">
    <SettingsHeader
      title="Setup"
      description={status?.message ||
        `Whether ${definition.label} is offered to new threads, and which binary and models it runs with.`}
    >
      {#snippet badge()}
        <!-- As tall as the xs buttons from the start, so the update controls
             appearing beside the pill move nothing below. Where the row is
             too narrow for them they wrap under the pill. -->
        <span class="flex min-h-6 min-w-0 flex-wrap items-center justify-end gap-1.5 compact:min-h-9">
          <span
            class="inline-flex min-w-0 items-center gap-1.5 rounded-full border border-border-subtle bg-surface-0 px-2.5 py-0.5 text-[0.6875rem] text-fg-muted"
            data-testid="settings-provider-status-pill"
            data-status={status?.status ?? 'checking'}
            title={pillTitle}
          >
            <span
              class="h-1.5 w-1.5 shrink-0 rounded-full {statusDotColor(status?.status ?? 'unknown')}"
              aria-hidden="true"
            ></span>
            <span class="whitespace-nowrap">{status?.status ?? 'checking'}</span>
            {#if status?.version}
              <span class="text-fg-hint">·</span>
              <span class="min-w-0 max-w-[14rem] truncate font-mono text-[0.65625rem] text-fg-muted">{status.version}</span>
            {/if}
          </span>
          {#if checkFailure}
            <span
              class="shrink-0 whitespace-nowrap text-[0.6875rem] text-warning"
              title={checkFailure}
              data-testid="settings-provider-update-check-error"
            >Update check failed</span>
          {:else if admin && update?.signInRequired}
            <span
              class="shrink-0 whitespace-nowrap text-[0.6875rem] text-warning"
              title="The {definition.label} login has expired. Checking for an update would start {definition.label}, which signs out an expired login, so no check runs until you sign in again."
              data-testid="settings-provider-update-sign-in"
            >Sign in to check for updates</span>
          {/if}
          <ProviderUpdateButton
            {provider}
            {backend}
            variant="tinted"
            size="xs"
            testId="settings-provider-update-button"
          />
          {#if notesOffered}
            <Button
              variant="ghost"
              size="xs"
              class="shrink-0"
              testId="settings-provider-update-notes-button"
              onclick={() => openProviderUpdateNotes(provider, backend)}
            >
              {#snippet children()}What's new{/snippet}
            </Button>
          {/if}
        </span>
      {/snippet}
    </SettingsHeader>

    <ProviderUpdateFailure {provider} {backend} class="mb-3" testId="settings-provider-update-error" />

    <div class="flex flex-col gap-1">
      <SettingsField
        id={providerFieldId(provider, 'enabled')}
        label="Enabled"
        hint={definition.enableHint ?? 'Allow new threads to use this provider.'}
      >
        <ToggleSwitch
          checked={settings[definition.settings.enabledKey]}
          disabled={!hostWritable}
          title={hostReason}
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
              disabled={!settings[definition.settings.enabledKey] || !hostWritable}
              title={hostReason}
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
          disabled={!hostWritable}
          title={hostReason}
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

  {#if definition.features.accounts}
    <section>
      <ProviderAccountsSettings {provider} />
      <AccountAutoSwitchSettings {provider} />
    </section>
  {/if}

  {#if definition.features.autoCompact}
    <section>
      <SettingsHeader title="Context window" />
      <ProviderContextSettings {provider} />
    </section>
  {/if}

  {#if definition.features.promptOverrides}
    <section>
      <SettingsHeader title="System prompt" description={promptTiming} />
      <ProviderPromptSection provider={definition} />
    </section>
  {/if}

  {#if definition.features.disabledTools}
    <section>
      <SettingsHeader title="Tools" description="Applies to sessions started later." />
      {#if provider === 'codex'}
        <CodexDisabledToolsEditor provider={definition} />
      {:else if provider === 'claude'}
        <ClaudeDisabledToolsEditor provider={definition} />
        <!-- Both Claude transports read it: one binary, one variable. -->
        <ClaudeCommandTimeoutField />
      {/if}
    </section>
  {/if}

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
