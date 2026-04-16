<script lang="ts">
  import { onMount } from 'svelte';
  import { GetProviderStatuses, GetModelsForProvider } from '../../stores/bindings';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ModelInfo, ProviderStatus } from '../../types/settings';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';

  let settings = $derived(getSettings());
  let statuses = $state<ProviderStatus[]>([]);
  let claudeModels = $state<ModelInfo[]>([]);
  let codexModels = $state<ModelInfo[]>([]);

  onMount(async () => {
    try {
      const result = await GetProviderStatuses();
      statuses = (result ?? []) as ProviderStatus[];
    } catch (err) {
      console.error('Failed to load provider statuses:', err);
      addToast('error', 'Failed to load provider statuses');
    }
    try {
      claudeModels = (await GetModelsForProvider('claude') ?? []) as ModelInfo[];
    } catch (err) {
      console.error('Failed to load Claude models:', err);
      addToast('error', 'Failed to load Claude models');
    }
    try {
      codexModels = (await GetModelsForProvider('codex') ?? []) as ModelInfo[];
    } catch (err) {
      console.error('Failed to load Codex models:', err);
      addToast('error', 'Failed to load Codex models');
    }
  });

  function getStatus(provider: string): ProviderStatus | undefined {
    return statuses.find((s) => s.provider === provider);
  }

  function statusDotColor(status: string): string {
    switch (status) {
      case 'ready': return 'bg-success';
      case 'error': return 'bg-error';
      default: return 'bg-text-secondary/30';
    }
  }
</script>

<div class="space-y-8">
  <section class="rounded-2xl border border-border/70 bg-surface-1/75 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
    <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Providers</p>
    <h3 class="mt-1 text-base font-semibold text-text-primary">Provider health and binaries</h3>
    <p class="mt-1 text-sm text-text-secondary">Check installation state, override binary paths, and review the model lists each provider exposes.</p>
  </section>
  {#each [
    { id: 'claude', label: 'Claude', enabledKey: 'claudeEnabled' as const, pathKey: 'claudeBinaryPath' as const, models: claudeModels },
    { id: 'codex', label: 'Codex', enabledKey: 'codexEnabled' as const, pathKey: 'codexBinaryPath' as const, models: codexModels },
  ] as provider}
    {@const status = getStatus(provider.id)}
    <div class="rounded-2xl border border-border/70 bg-surface-1/80 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
      <div class="mb-4 flex items-start justify-between gap-4">
        <div class="space-y-1">
          <div class="flex items-center gap-2">
            <h3 class="text-base font-semibold text-text-primary">{provider.label}</h3>
            <span class="inline-flex items-center gap-1 rounded-full border border-border/60 bg-surface-0/70 px-2 py-0.5 text-[11px] text-text-secondary">
              <span class="h-1.5 w-1.5 rounded-full {statusDotColor(status?.status ?? 'unknown')}" aria-hidden="true"></span>
              {status?.status ?? 'checking'}
            </span>
          </div>
          <p class="text-sm text-text-secondary">
            {status?.message || `Configure ${provider.label} availability for thread creation and session reconnects.`}
          </p>
        </div>
        <div class="flex items-center gap-3 rounded-full border border-border/60 bg-surface-0/65 px-3 py-2">
          <span class="text-xs font-medium text-text-secondary">Enabled</span>
          <ToggleSwitch
            checked={settings[provider.enabledKey]}
            ariaLabel={`Toggle ${provider.label}`}
            onToggle={(value) => updateSetting(provider.enabledKey, value)}
          />
        </div>
      </div>

      <div class="space-y-3">
        <div class="rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
          <label for="{provider.id}-path" class="text-xs text-text-secondary block mb-1">Binary path</label>
          <input
            id="{provider.id}-path"
            type="text"
            value={settings[provider.pathKey]}
            onchange={(e) => updateSetting(provider.pathKey, (e.target as HTMLInputElement).value)}
            placeholder="Auto-detect"
            class="w-full text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary placeholder:text-text-secondary/40 shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors"
          />
        </div>

        <div class="grid gap-3 md:grid-cols-2">
          <div class="rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
            <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-text-secondary/70">Version</p>
            <p class="mt-2 text-sm text-text-primary">{status?.version || 'Unavailable'}</p>
          </div>
          <div class="rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
            <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-text-secondary/70">Detected status</p>
            <p class="mt-2 text-sm text-text-primary">{status?.status || 'checking'}</p>
          </div>
        </div>

        <div class="rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
          <p class="text-[11px] font-semibold uppercase tracking-[0.18em] text-text-secondary/70">Known models</p>
          {#if provider.models.length > 0}
            <div class="mt-3 flex flex-wrap gap-2">
              {#each provider.models as model}
                <span class="rounded-full border border-border/60 bg-surface-1 px-2.5 py-1 text-[11px] text-text-secondary">{model.name || model.slug}</span>
              {/each}
            </div>
          {:else}
            <p class="mt-2 text-sm text-text-secondary/70">No models available</p>
          {/if}
        </div>
      </div>
    </div>
  {/each}
</div>
