<script lang="ts">
  // ProviderSettings: provider health, binary paths, and the
  // text-generation routing knobs.
  //
  // Layout aligns with GeneralSettings / EditorSection / NetworkSection
  // — section header (MicroLabel + heading + sentence intro) followed
  // by `divide-y divide-border-subtle` rows. The previous card-heavy
  // styling read as a different app when the user tabbed across
  // sections; the row pattern keeps the visual rhythm consistent.

  import { GetProviderStatuses, GetModelsForProvider } from '../../stores/bindings';
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import { addToast } from '../../stores/toast.svelte';
  import type { ModelInfo, ProviderStatus, ReasoningEffort } from '../../types/settings';
  import MicroLabel from '../primitives/MicroLabel.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';

  // Mirror of internal/settings text-generation defaults so the model-input
  // placeholder tracks the per-provider recommendation without a round-trip.
  const TEXTGEN_DEFAULT_MODEL: Record<'claude' | 'codex', string> = {
    claude: 'claude-haiku-4-5',
    codex: 'gpt-5.4-mini',
  };
  const TEXTGEN_EFFORT_OPTIONS: Array<{ value: ReasoningEffort; label: string }> = [
    { value: 'low', label: 'Low' },
    { value: 'medium', label: 'Medium' },
    { value: 'high', label: 'High' },
    { value: 'xhigh', label: 'X-High' },
    { value: 'max', label: 'Max' },
  ];

  let settings = $derived(getSettings());
  let statuses = $state<ProviderStatus[]>([]);
  let claudeModels = $state<ModelInfo[]>([]);
  let codexModels = $state<ModelInfo[]>([]);
  let loadGeneration = 0;

  $effect(() => {
    settings.claudeBinaryPath;
    settings.codexBinaryPath;

    const generation = ++loadGeneration;
    void (async () => {
      const [statusResult, claudeResult, codexResult] = await Promise.allSettled([
        GetProviderStatuses(),
        GetModelsForProvider('claude'),
        GetModelsForProvider('codex'),
      ]);
      if (generation !== loadGeneration) return;

      if (statusResult.status === 'fulfilled') {
        statuses = (statusResult.value ?? []) as ProviderStatus[];
      } else {
        console.error('Failed to load provider statuses:', statusResult.reason);
        addToast('error', 'Failed to load provider statuses.');
      }
      if (claudeResult.status === 'fulfilled') {
        claudeModels = (claudeResult.value ?? []) as ModelInfo[];
      } else {
        console.error('Failed to load Claude models:', claudeResult.reason);
        addToast('error', 'Failed to load Claude models.');
      }
      if (codexResult.status === 'fulfilled') {
        codexModels = (codexResult.value ?? []) as ModelInfo[];
      } else {
        console.error('Failed to load Codex models:', codexResult.reason);
        addToast('error', 'Failed to load Codex models.');
      }
    })();
  });

  function getStatus(provider: string): ProviderStatus | undefined {
    return statuses.find((s) => s.provider === provider);
  }

  function statusDotColor(status: string): string {
    switch (status) {
      case 'ready': return 'bg-success';
      case 'error': return 'bg-error';
      case 'not_found': return 'bg-error';
      case 'version_too_old': return 'bg-warning';
      case 'unauthenticated': return 'bg-warning';
      default: return 'bg-fg-subtle';
    }
  }

  const SELECT_CLASS =
    'min-w-[8rem] text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
    'px-2.5 py-1 text-fg focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 ' +
    'transition-colors cursor-pointer';

  const INPUT_CLASS =
    'w-full text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
    'px-2.5 py-1.5 text-fg placeholder:text-fg-muted focus:outline-none focus:border-accent ' +
    'focus-visible:ring-2 focus-visible:ring-accent/40 transition-colors';

  const ROW_CLASS = 'flex items-center justify-between gap-4 py-2.5';
</script>

<div class="flex flex-col gap-8">
  {#each [
    { id: 'claude' as const, label: 'Claude', enabledKey: 'claudeEnabled' as const, pathKey: 'claudeBinaryPath' as const, models: claudeModels },
    { id: 'codex' as const, label: 'Codex', enabledKey: 'codexEnabled' as const, pathKey: 'codexBinaryPath' as const, models: codexModels },
  ] as provider}
    {@const status = getStatus(provider.id)}
    <section data-testid="settings-provider-{provider.id}">
      <MicroLabel as="p">Provider</MicroLabel>
      <div class="mt-1 flex items-center gap-2">
        <h3 class="text-[15px] font-semibold text-fg">{provider.label}</h3>
        <span
          class="inline-flex items-center gap-1 rounded-full border border-border-subtle bg-surface-0 px-2 py-0.5 text-[11px] text-fg-muted"
          data-testid="settings-provider-status-pill"
          data-status={status?.status ?? 'checking'}
        >
          <span class="h-1.5 w-1.5 rounded-full {statusDotColor(status?.status ?? 'unknown')}" aria-hidden="true"></span>
          {status?.status ?? 'checking'}
        </span>
      </div>
      <p class="mt-1 max-w-2xl text-[12px] text-fg-muted">
        {status?.message || `Configure ${provider.label} availability for thread creation and session reconnects.`}
      </p>
      <div class="mt-3 divide-y divide-border-subtle">
        <div class={ROW_CLASS}>
          <div>
            <p class="text-[13px] text-fg font-medium">Enabled</p>
            <p class="text-[12px] text-fg-muted">Allow new threads to use this provider.</p>
          </div>
          <ToggleSwitch
            checked={settings[provider.enabledKey]}
            ariaLabel={`Toggle ${provider.label}`}
            onToggle={(value) => updateSetting(provider.enabledKey, value)}
          />
        </div>

        <div class={ROW_CLASS}>
          <div class="flex-1 min-w-0">
            <label for="{provider.id}-path" class="text-[13px] text-fg block font-medium">Binary path</label>
            <p class="text-[12px] text-fg-muted">Override the auto-detected CLI binary.</p>
          </div>
          <input
            id="{provider.id}-path"
            type="text"
            value={settings[provider.pathKey]}
            onchange={(e) => updateSetting(provider.pathKey, (e.target as HTMLInputElement).value)}
            placeholder="Auto-detect"
            class="{INPUT_CLASS} max-w-[16rem]"
          />
        </div>

        <div class={ROW_CLASS}>
          <div>
            <p class="text-[13px] text-fg font-medium">Version</p>
            <p class="text-[12px] text-fg-muted">Reported by the resolved binary.</p>
          </div>
          <span class="text-[12px] text-fg" data-testid="settings-provider-version">
            {status?.version || 'Unavailable'}
          </span>
        </div>

        <div class="py-2.5">
          <p class="text-[13px] text-fg font-medium">Known models</p>
          <p class="text-[12px] text-fg-muted">Models exposed by the provider's catalog.</p>
          {#if provider.models.length > 0}
            <div class="mt-2 flex flex-wrap gap-1.5" data-testid="settings-provider-models">
              {#each provider.models as model}
                <span class="rounded-full border border-border-subtle bg-surface-1 px-2.5 py-0.5 text-[11px] text-fg-muted">
                  {model.name || model.slug}
                </span>
              {/each}
            </div>
          {:else}
            <p class="mt-2 text-[12px] text-fg-muted">No models available.</p>
          {/if}
        </div>
      </div>
    </section>
  {/each}

  <section data-testid="settings-text-generation">
    <MicroLabel as="p">Text generation</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">Commit and PR Message CLI</h3>
    <p class="mt-1 max-w-2xl text-[12px] text-fg-muted">
      Which CLI writes commit messages, PR bodies, and generated thread titles.
      Independent of the chat provider so Claude users can still spend Codex
      cycles on short text.
    </p>
    <div class="mt-3 divide-y divide-border-subtle">
      <div class={ROW_CLASS}>
        <div>
          <label for="textgen-provider" class="text-[13px] text-fg block font-medium">Provider</label>
          <p class="text-[12px] text-fg-muted">CLI that generates non-chat text.</p>
        </div>
        <select
          id="textgen-provider"
          data-testid="settings-textgen-provider"
          value={settings.textGenerationProvider}
          onchange={(e) => updateSetting('textGenerationProvider', (e.target as HTMLSelectElement).value as 'claude' | 'codex')}
          class={SELECT_CLASS}
        >
          <option value="codex">Codex</option>
          <option value="claude">Claude</option>
        </select>
      </div>

      <div class={ROW_CLASS}>
        <div class="flex-1 min-w-0">
          <label for="textgen-model" class="text-[13px] text-fg block font-medium">Model</label>
          <p class="text-[12px] text-fg-muted">Leave empty to use the provider's default small-text model.</p>
        </div>
        <input
          id="textgen-model"
          type="text"
          data-testid="settings-textgen-model"
          value={settings.textGenerationModel}
          onchange={(e) => updateSetting('textGenerationModel', (e.target as HTMLInputElement).value)}
          placeholder={`Default: ${TEXTGEN_DEFAULT_MODEL[settings.textGenerationProvider]}`}
          class="{INPUT_CLASS} max-w-[16rem]"
        />
      </div>

      <div class={ROW_CLASS}>
        <div>
          <label for="textgen-effort" class="text-[13px] text-fg block font-medium">Reasoning effort</label>
          <p class="text-[12px] text-fg-muted">Budget for commit/PR text generation.</p>
        </div>
        <select
          id="textgen-effort"
          data-testid="settings-textgen-effort"
          value={settings.textGenerationReasoningEffort}
          onchange={(e) => updateSetting('textGenerationReasoningEffort', (e.target as HTMLSelectElement).value as ReasoningEffort)}
          class={SELECT_CLASS}
        >
          {#each TEXTGEN_EFFORT_OPTIONS as opt}
            <option value={opt.value}>{opt.label}</option>
          {/each}
        </select>
      </div>
    </div>
  </section>
</div>
