<script lang="ts">
  import {
    type ContextSettingsProfile,
    type ContextSettingsUpdateInput,
    GetContextSettings,
    UpdateContextSettingsProfile,
    UpdateThreadContextSettings,
  } from '../../stores/bindings';
  import { getMainPane } from '../../stores/panes.svelte';
  import { replaceThread } from '../../stores/threads.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { Thread } from '../../types/models';
  import type { ContextWindowOption, ModelInfo } from '../../types/settings';
  import { errString } from '../../utils/errors';
  import { formatTokens } from '../../utils/format';
  import MicroLabel from '../primitives/MicroLabel.svelte';

  type ContextTarget = {
    threadId?: string;
    provider: string;
    model: string;
    contextWindow?: number;
    autoCompactStandardPercent?: number;
    autoCompactExtendedPercent?: number;
  } | null;

  interface ContextSettingsUpdate extends NonNullable<ContextSettingsUpdateInput> {
    provider: string;
    model: string;
    contextWindow: number;
    autoCompactStandardPercent: number;
    autoCompactExtendedPercent: number;
  }

  let {
    contextTarget = null,
    claudeModels,
    codexModels,
  }: {
    contextTarget?: ContextTarget;
    claudeModels: ModelInfo[];
    codexModels: ModelInfo[];
  } = $props();

  const SELECT_CLASS =
    'min-w-[9rem] text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
    'px-2.5 py-1 text-fg focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 ' +
    'transition-colors cursor-pointer';
  const NUMBER_CLASS =
    'w-16 text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
    'px-2 py-1 text-fg focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40';

  let selectedProvider = $state<'claude' | 'codex'>('claude');
  let selectedModel = $state('');
  let profile = $state<ContextSettingsProfile | null>(null);
  let contextWindow = $state(0);
  let standardPercent = $state(0);
  let extendedPercent = $state(0);
  let loading = $state(false);
  let saving = $state(false);
  let loadGeneration = 0;

  let models = $derived(selectedProvider === 'claude' ? claudeModels : codexModels);
  let contextOptions = $derived(profile?.contextWindows ?? []);
  let hasExtended = $derived(contextOptions.some((option) => option.tier === 'extended'));

  $effect(() => {
    if (!contextTarget) return;
    selectedProvider = contextTarget.provider === 'codex' ? 'codex' : 'claude';
    selectedModel = contextTarget.model;
  });

  $effect(() => {
    if (models.length === 0) {
      selectedModel = '';
      profile = null;
      return;
    }
    if (models.some((model) => model.slug === selectedModel)) return;
    selectedModel = models[0].slug;
    profile = null;
  });

  $effect(() => {
    if (!selectedProvider || !selectedModel) return;
    const generation = ++loadGeneration;
    loading = true;
    void (async () => {
      try {
        const next = await GetContextSettings(selectedProvider, selectedModel) as ContextSettingsProfile;
        if (generation !== loadGeneration) return;
        profile = next;
        const targetApplies = contextTarget?.threadId
          && contextTarget.provider === selectedProvider
          && contextTarget.model === selectedModel;
        contextWindow = targetApplies && contextTarget.contextWindow
          ? contextTarget.contextWindow
          : next.contextWindow;
        standardPercent = targetApplies
          ? (contextTarget.autoCompactStandardPercent ?? 0)
          : (next.autoCompactStandardPercent ?? 0);
        extendedPercent = targetApplies
          ? (contextTarget.autoCompactExtendedPercent ?? 0)
          : (next.autoCompactExtendedPercent ?? 0);
      } catch (err) {
        if (generation !== loadGeneration) return;
        console.error('GetContextSettings failed:', err);
        addToast('error', `Failed to load context settings: ${errString(err)}`);
      } finally {
        if (generation === loadGeneration) loading = false;
      }
    })();
  });

  function setPercent(tier: 'standard' | 'extended', value: number): void {
    const clamped = Math.max(1, Math.min(90, Math.round(value)));
    if (tier === 'standard') {
      standardPercent = clamped;
    } else {
      extendedPercent = clamped;
    }
  }

  function setDefault(tier: 'standard' | 'extended', checked: boolean): void {
    if (tier === 'standard') {
      standardPercent = checked ? 0 : 90;
    } else {
      extendedPercent = checked ? 0 : 90;
    }
  }

  async function save(): Promise<void> {
    if (!profile || !contextWindow) return;
    const update: ContextSettingsUpdate = {
      provider: selectedProvider,
      model: selectedModel,
      contextWindow,
      autoCompactStandardPercent: standardPercent,
      autoCompactExtendedPercent: extendedPercent,
    };
    saving = true;
    try {
      if (contextTarget?.threadId && contextTarget.provider === selectedProvider && contextTarget.model === selectedModel) {
        const updated = await UpdateThreadContextSettings(contextTarget.threadId, update) as Thread;
        const pane = getMainPane();
        if (pane.threadId === updated.id) pane.replaceThread(updated);
        replaceThread(updated);
      } else {
        await UpdateContextSettingsProfile(update);
      }
      addToast('success', 'Context settings saved.');
    } catch (err) {
      console.error('Save context settings failed:', err);
      addToast('error', `Failed to save context settings: ${errString(err)}`);
    } finally {
      saving = false;
    }
  }
</script>

<section id="settings-context" data-testid="settings-context">
  <MicroLabel as="p">Context</MicroLabel>
  <h3 class="mt-1 text-[15px] font-semibold text-fg">Context Window and Auto-Compact</h3>
  <p class="mt-1 max-w-2xl text-[12px] text-fg-muted">
    Per-model defaults for the active context tier. Percentages are capped at 90%.
  </p>

  <div class="mt-3 divide-y divide-border-subtle">
    <div class="flex items-center justify-between gap-4 py-2.5">
      <div>
        <p class="text-[13px] text-fg font-medium">Model</p>
        <p class="text-[12px] text-fg-muted">Settings are stored per provider/model.</p>
      </div>
      <div class="flex items-center gap-2">
        <select bind:value={selectedProvider} class={SELECT_CLASS} aria-label="Context provider">
          <option value="claude">Claude</option>
          <option value="codex">Codex</option>
        </select>
        <select bind:value={selectedModel} class={SELECT_CLASS} aria-label="Context model">
          {#each models as model}
            <option value={model.slug}>{model.name || model.slug}</option>
          {/each}
        </select>
      </div>
    </div>

    <div class="flex items-center justify-between gap-4 py-2.5">
      <div>
        <p class="text-[13px] text-fg font-medium">Active window</p>
        <p class="text-[12px] text-fg-muted">Provider sessions restart when this changes on the current thread.</p>
      </div>
      <div class="inline-flex rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 p-0.5">
        {#each contextOptions as option}
          <button
            type="button"
            onclick={() => contextWindow = option.tokens}
            class="rounded-[calc(var(--radius-field)-2px)] px-2.5 py-1 text-[12px] transition-colors cursor-pointer
              {contextWindow === option.tokens ? 'bg-accent/15 text-fg' : 'text-fg-muted hover:text-fg'}"
          >
            {option.label}
          </button>
        {/each}
      </div>
    </div>

    <div class="py-2.5">
      <div class="flex items-center justify-between gap-4">
        <div>
          <p class="text-[13px] text-fg font-medium">Standard compact</p>
          <p class="text-[12px] text-fg-muted">{formatTokens(contextOptions.find((o) => o.tier === 'standard')?.tokens ?? 0)} window</p>
        </div>
        <label class="flex items-center gap-2 text-[12px] text-fg-muted">
          <input
            type="checkbox"
            checked={standardPercent === 0}
            onchange={(event) => setDefault('standard', (event.target as HTMLInputElement).checked)}
          />
          Default
        </label>
      </div>
      <div class="mt-2 flex items-center gap-3" class:opacity-50={standardPercent === 0}>
        <input
          type="range"
          min="1"
          max="90"
          value={standardPercent || 90}
          disabled={standardPercent === 0}
          oninput={(event) => setPercent('standard', Number((event.target as HTMLInputElement).value))}
          class="w-full accent-accent"
        />
        <input
          type="number"
          min="1"
          max="90"
          value={standardPercent || 90}
          disabled={standardPercent === 0}
          onchange={(event) => setPercent('standard', Number((event.target as HTMLInputElement).value))}
          class={NUMBER_CLASS}
        />
      </div>
    </div>

    {#if hasExtended}
      <div class="py-2.5">
        <div class="flex items-center justify-between gap-4">
          <div>
            <p class="text-[13px] text-fg font-medium">Extended compact</p>
            <p class="text-[12px] text-fg-muted">{formatTokens(contextOptions.find((o) => o.tier === 'extended')?.tokens ?? 0)} window</p>
          </div>
          <label class="flex items-center gap-2 text-[12px] text-fg-muted">
            <input
              type="checkbox"
              checked={extendedPercent === 0}
              onchange={(event) => setDefault('extended', (event.target as HTMLInputElement).checked)}
            />
            Default
          </label>
        </div>
        <div class="mt-2 flex items-center gap-3" class:opacity-50={extendedPercent === 0}>
          <input
            type="range"
            min="1"
            max="90"
            value={extendedPercent || 90}
            disabled={extendedPercent === 0}
            oninput={(event) => setPercent('extended', Number((event.target as HTMLInputElement).value))}
            class="w-full accent-accent"
          />
          <input
            type="number"
            min="1"
            max="90"
            value={extendedPercent || 90}
            disabled={extendedPercent === 0}
            onchange={(event) => setPercent('extended', Number((event.target as HTMLInputElement).value))}
            class={NUMBER_CLASS}
          />
        </div>
      </div>
    {/if}

    <div class="flex items-center justify-end gap-2 py-2.5">
      <span class="mr-auto text-[12px] text-fg-muted">
        {loading ? 'Loading...' : contextTarget?.threadId ? 'Applies to this thread and future threads for the model.' : 'Applies to future threads for the model.'}
      </span>
      <button
        type="button"
        onclick={save}
        disabled={saving || loading || !profile}
        class="rounded-[var(--radius-field)] bg-accent px-3 py-1.5 text-[12px] font-medium text-accent-foreground hover:brightness-105 disabled:opacity-60 disabled:cursor-not-allowed cursor-pointer transition"
      >
        {saving ? 'Saving...' : 'Save'}
      </button>
    </div>
  </div>
</section>
