<script lang="ts">
  // Per-provider custom environment variables, applied to the provider
  // processes the backend spawns (sessions, account probes, text generation).
  //
  // Mutations go through the dedicated bindings, not updateSetting: sensitive
  // values come back redacted on every read, so a read-mutate-write patch
  // would persist the redaction. Each call returns the whole (redacted)
  // settings snapshot, which re-seeds the store.
  //
  // Name validation mirrors internal/settings/providerenv.go so the UI never
  // shows "looks fine" for an input the backend would refuse. The reserved
  // names are checked backend-side only — the list lives with the spawn paths
  // that pin them, and its rejection message explains which one and why.

  import {
    SetProviderCustomEnvVar,
    DeleteProviderCustomEnvVar,
  } from '../../stores/bindings';
  import { getSettings, applySettingsSnapshot } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ProviderEnvVar, Settings } from '../../types/settings';
  import type { ProviderDefinition } from '../../providers/catalog';
  import SettingsField from './SettingsField.svelte';
  import type { ProviderFieldId } from './fields';
  import { INPUT_CLASS, PRIMARY_BUTTON_CLASS, GHOST_BUTTON_CLASS } from './styles';
  import { isImeComposingEvent } from '../../utils/imeComposition';

  let { provider }: { provider: ProviderDefinition } = $props();

  // ProviderDefinition.id spans every provider; the field index only covers
  // the two that have a page, and this section only ever renders on one of
  // them. The ternary narrows without a cast.
  let fieldId = $derived<ProviderFieldId>(
    provider.id === 'codex' ? 'codex.env' : 'claude.env',
  );

  let settings = $derived(getSettings());
  let vars = $derived<ProviderEnvVar[]>(settings[provider.settings.customEnvKey] ?? []);

  let draftName = $state('');
  let draftValue = $state('');
  let draftSensitive = $state(false);
  let busy = $state(false);

  let draftError = $derived(validateName(draftName, vars));
  let canAdd = $derived(!busy && draftName.trim() !== '' && draftError === null);

  // The placeholder is an example of a variable this provider actually reads
  // — ANTHROPIC_BASE_URL on a Codex card would suggest a variable Codex
  // ignores.
  let namePlaceholder = $derived(
    provider.settings.customEnvKey === 'codexCustomEnv' ? 'HTTPS_PROXY' : 'ANTHROPIC_BASE_URL',
  );

  function validateName(raw: string, current: ProviderEnvVar[]): string | null {
    const name = raw.trim();
    if (name === '') return null;
    if (name.includes('=')) return 'A variable name cannot contain "=".';
    if (!/^[A-Za-z_][A-Za-z0-9_]*$/.test(name)) {
      return 'Use letters, digits, and underscore, starting with a letter or underscore.';
    }
    if (name.toUpperCase().startsWith('AO_')) {
      return 'Names starting with AO_ are reserved by Agent Overflow.';
    }
    if (current.some((v) => v.name.toUpperCase() === name.toUpperCase())) {
      return 'This variable is already set for this provider.';
    }
    return null;
  }

  // The generated binding returns the Wails model of Settings, whose enum
  // fields are typed as plain strings; the store's cast is where the two
  // shapes meet, same as updateSettingsPatch.
  async function run(
    action: () => PromiseLike<unknown>,
    failure: string,
  ): Promise<boolean> {
    busy = true;
    try {
      const result = await action();
      if (result) applySettingsSnapshot(result as Partial<Settings>);
      return true;
    } catch (err) {
      console.error(failure, err);
      addToast('error', err instanceof Error ? err.message : failure);
      return false;
    } finally {
      busy = false;
    }
  }

  async function addVar(): Promise<void> {
    if (!canAdd) return;
    const ok = await run(
      () =>
        SetProviderCustomEnvVar(
          provider.id,
          draftName.trim(),
          draftValue,
          draftSensitive,
        ),
      'Failed to save the environment variable.',
    );
    if (!ok) return;
    draftName = '';
    draftValue = '';
    draftSensitive = false;
  }

  async function setValue(entry: ProviderEnvVar, value: string): Promise<void> {
    if (!entry.sensitive && value === entry.value) return;
    if (entry.sensitive && value === '') return;
    await run(
      () => SetProviderCustomEnvVar(provider.id, entry.name, value, entry.sensitive ?? false),
      'Failed to save the environment variable.',
    );
  }

  async function setSensitive(entry: ProviderEnvVar): Promise<void> {
    // Only reachable for a visible value: masking one hides it from every read
    // path, so unmasking would have nothing to restore.
    await run(
      () => SetProviderCustomEnvVar(provider.id, entry.name, entry.value, true),
      'Failed to mask the environment variable.',
    );
  }

  async function removeVar(entry: ProviderEnvVar): Promise<void> {
    await run(
      () => DeleteProviderCustomEnvVar(provider.id, entry.name),
      'Failed to remove the environment variable.',
    );
  }

  function handleDraftKeydown(e: KeyboardEvent): void {
    if (e.key === 'Enter' && isImeComposingEvent(e)) return;
    if (e.key === 'Enter' && canAdd) {
      e.preventDefault();
      void addVar();
    }
  }
</script>

<div data-testid="settings-provider-env-{provider.id}">
  <SettingsField
    id={fieldId}
    label="Environment variables"
    hint="Passed to every {provider.label} process this app starts. Takes effect on the next session or account check."
    align="start"
    stacked
  >
    <div class="flex flex-col gap-3">
      {#if vars.length === 0}
        <p class="text-[0.71875rem] text-fg-hint" data-testid="settings-provider-env-empty">
          No environment variables set.
        </p>
      {:else}
        <ul class="flex flex-col gap-1" data-testid="settings-provider-env-list">
          {#each vars as entry (entry.name)}
            <li
              class="flex items-center gap-2 rounded-[var(--radius-field)] border border-border-subtle bg-surface-1/30 px-3 py-1.5"
              data-testid="settings-provider-env-row-{entry.name}"
            >
              <span class="w-[13rem] shrink-0 truncate font-mono text-[0.75rem] text-fg">
                {entry.name}
              </span>
              {#if entry.sensitive}
                <input
                  type="password"
                  data-testid="settings-provider-env-value-{entry.name}"
                  data-masked="true"
                  value=""
                  placeholder="••••••••"
                  autocomplete="off"
                  spellcheck="false"
                  disabled={busy}
                  aria-label={`Replace value for ${entry.name}`}
                  onchange={(e) => void setValue(entry, (e.target as HTMLInputElement).value)}
                  class="{INPUT_CLASS} font-mono"
                />
              {:else}
                <input
                  type="text"
                  data-testid="settings-provider-env-value-{entry.name}"
                  value={entry.value}
                  autocomplete="off"
                  spellcheck="false"
                  disabled={busy}
                  aria-label={`Value for ${entry.name}`}
                  onchange={(e) => void setValue(entry, (e.target as HTMLInputElement).value)}
                  class="{INPUT_CLASS} font-mono"
                />
                <button
                  type="button"
                  data-testid="settings-provider-env-mask-{entry.name}"
                  onclick={() => void setSensitive(entry)}
                  disabled={busy}
                  class={GHOST_BUTTON_CLASS}
                  title="Hide this value everywhere it is displayed"
                >
                  Mask
                </button>
              {/if}
              <button
                type="button"
                data-testid="settings-provider-env-remove-{entry.name}"
                onclick={() => void removeVar(entry)}
                disabled={busy}
                class={GHOST_BUTTON_CLASS}
                aria-label={`Remove ${entry.name}`}
              >
                Remove
              </button>
            </li>
          {/each}
        </ul>
      {/if}

      <div class="flex items-start gap-2">
        <!-- The width lives on a wrapper because INPUT_CLASS carries w-full:
             stacking w-[13rem] onto it is a Tailwind class conflict whose
             winner depends on stylesheet order, and when w-full wins the
             shrink-0 name field swallows the row and crushes the value input
             to a sliver. -->
        <div class="w-[13rem] shrink-0">
          <input
            type="text"
            data-testid="settings-provider-env-name-input"
            value={draftName}
            placeholder={namePlaceholder}
            autocomplete="off"
            spellcheck="false"
            disabled={busy}
            aria-label="New variable name"
            aria-invalid={draftError !== null}
            aria-describedby={draftError ? `env-error-${provider.id}` : undefined}
            oninput={(e) => (draftName = (e.target as HTMLInputElement).value)}
            onkeydown={handleDraftKeydown}
            class="{INPUT_CLASS} font-mono"
          />
        </div>
        <input
          type={draftSensitive ? 'password' : 'text'}
          data-testid="settings-provider-env-value-input"
          value={draftValue}
          placeholder="Value"
          autocomplete="off"
          spellcheck="false"
          disabled={busy}
          aria-label="New variable value"
          oninput={(e) => (draftValue = (e.target as HTMLInputElement).value)}
          onkeydown={handleDraftKeydown}
          class="{INPUT_CLASS} min-w-0 font-mono"
        />
        <label class="flex shrink-0 items-center gap-1.5 py-1.5 text-[0.71875rem] text-fg-muted">
          <input
            type="checkbox"
            data-testid="settings-provider-env-sensitive-input"
            checked={draftSensitive}
            disabled={busy}
            onchange={(e) => (draftSensitive = (e.target as HTMLInputElement).checked)}
            class="cursor-pointer"
          />
          Sensitive
        </label>
        <button
          type="button"
          data-testid="settings-provider-env-add"
          onclick={() => void addVar()}
          disabled={!canAdd}
          class={PRIMARY_BUTTON_CLASS}
        >
          Add
        </button>
      </div>

      {#if draftError}
        <p
          id="env-error-{provider.id}"
          data-testid="settings-provider-env-error"
          class="text-[0.71875rem] text-error"
          role="alert"
        >
          {draftError}
        </p>
      {/if}
    </div>
  </SettingsField>
</div>
