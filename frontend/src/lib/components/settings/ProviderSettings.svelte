<script lang="ts">
  import { onMount } from 'svelte';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { GetProviderStatuses, GetModelsForProvider } from '../../stores/bindings';
  import { addToast } from '../../stores/toast.svelte';
  import type { ProviderStatus, ModelInfo } from '../../types/settings';

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
  {#each [
    { id: 'claude', label: 'Claude', enabledKey: 'claudeEnabled' as const, pathKey: 'claudeBinaryPath' as const, models: claudeModels },
    { id: 'codex', label: 'Codex', enabledKey: 'codexEnabled' as const, pathKey: 'codexBinaryPath' as const, models: codexModels },
  ] as provider}
    {@const status = getStatus(provider.id)}
    <div class="border border-border rounded-lg p-4">
      <div class="flex items-center justify-between mb-4">
        <div class="flex items-center gap-2">
          <h3 class="text-sm font-medium text-text-primary">{provider.label}</h3>
          {#if status}
            <span class="flex items-center gap-1 text-xs text-text-secondary">
              <span class="w-1.5 h-1.5 rounded-full {statusDotColor(status.status)}"></span>
              {status.status}
            </span>
          {/if}
        </div>
        <label class="flex items-center gap-2 cursor-pointer">
          <span class="text-xs text-text-secondary">Enabled</span>
          <input
            type="checkbox"
            checked={settings[provider.enabledKey]}
            onchange={() => updateSetting(provider.enabledKey, !settings[provider.enabledKey])}
            class="w-4 h-4 rounded border-border text-accent focus:ring-accent cursor-pointer"
          />
        </label>
      </div>

      <div class="space-y-3">
        <div>
          <label for="{provider.id}-path" class="text-xs text-text-secondary block mb-1">Binary path</label>
          <input
            id="{provider.id}-path"
            type="text"
            value={settings[provider.pathKey]}
            onchange={(e) => updateSetting(provider.pathKey, (e.target as HTMLInputElement).value)}
            placeholder="Auto-detect"
            class="w-full text-xs rounded border border-border bg-surface-0 px-2 py-1.5 text-text-primary placeholder:text-text-secondary/40 focus:outline-none focus:border-accent"
          />
        </div>

        {#if status?.version}
          <p class="text-xs text-text-secondary">Version: {status.version}</p>
        {/if}
        {#if status?.message}
          <p class="text-xs text-text-secondary/60">{status.message}</p>
        {/if}

        {#if provider.models.length > 0}
          <div>
            <p class="text-xs text-text-secondary mb-1">Known models</p>
            <div class="flex flex-wrap gap-1">
              {#each provider.models as model}
                <span class="text-[10px] px-1.5 py-0.5 rounded bg-surface-2 text-text-secondary">{model.name || model.slug}</span>
              {/each}
            </div>
          </div>
        {/if}
      </div>
    </div>
  {/each}
</div>
