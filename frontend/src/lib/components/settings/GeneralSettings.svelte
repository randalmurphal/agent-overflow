<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import type { ContextWindow, ReasoningEffort, ThreadMode } from '../../types/settings';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';

  let settings = $derived(getSettings());

  // Thread-default options. Discussion is reached via a separate flow,
  // so it isn't offered as a "default mode" — seeding a thread in
  // discussion mode without a deliberation channel would produce an
  // orphan thread.
  const MODE_OPTIONS: Array<{ value: ThreadMode; label: string }> = [
    { value: 'chat', label: 'Chat' },
    { value: 'plan', label: 'Plan' },
    { value: 'design', label: 'Design' },
  ];
  const EFFORT_OPTIONS: Array<{ value: ReasoningEffort; label: string }> = [
    { value: 'low', label: 'Low' },
    { value: 'medium', label: 'Medium' },
    { value: 'high', label: 'High' },
    { value: 'xhigh', label: 'X-High' },
    { value: 'max', label: 'Max' },
  ];
  const CONTEXT_OPTIONS: Array<{ value: ContextWindow; label: string }> = [
    { value: 200000, label: '200k' },
    { value: 1000000, label: '1M' },
  ];
</script>

<div class="space-y-5">
  <section class="rounded-2xl border border-border/70 bg-surface-1/80 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
    <div class="mb-4">
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Appearance</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">Theme and display</h3>
      <p class="mt-1 text-sm text-text-secondary">Choose how Agent Overflow should look across chat, settings, and git views.</p>
    </div>
    <div class="space-y-3">
      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <label for="theme-select" class="text-sm text-text-primary block">Theme</label>
          <p class="text-xs text-text-secondary/60">Choose your preferred color scheme</p>
        </div>
        <select
          id="theme-select"
          value={settings.theme}
          onchange={(e) => updateSetting('theme', (e.target as HTMLSelectElement).value as 'system' | 'light' | 'dark')}
          class="min-w-[8rem] text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
        >
          <option value="system">System</option>
          <option value="light">Light</option>
          <option value="dark">Dark</option>
        </select>
      </div>

      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <label for="timestamp-select" class="text-sm text-text-primary block">Timestamp format</label>
          <p class="text-xs text-text-secondary/60">How timestamps appear in the chat</p>
        </div>
        <select
          id="timestamp-select"
          value={settings.timestampFormat}
          onchange={(e) => updateSetting('timestampFormat', (e.target as HTMLSelectElement).value as 'locale' | '12-hour' | '24-hour')}
          class="min-w-[8rem] text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
        >
          <option value="locale">System locale</option>
          <option value="12-hour">12-hour</option>
          <option value="24-hour">24-hour</option>
        </select>
      </div>
    </div>
  </section>

  <section class="rounded-2xl border border-border/70 bg-surface-1/80 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
    <div class="mb-4">
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Behavior</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">Defaults and live updates</h3>
      <p class="mt-1 text-sm text-text-secondary">Tune how new sessions start and how provider output is rendered.</p>
    </div>
    <div class="space-y-3">
      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <label for="provider-select" class="text-sm text-text-primary block">Default provider</label>
          <p class="text-xs text-text-secondary/60">Provider selected when creating new threads</p>
        </div>
        <select
          id="provider-select"
          value={settings.defaultProvider}
          onchange={(e) => updateSetting('defaultProvider', (e.target as HTMLSelectElement).value as 'claude' | 'codex')}
          class="min-w-[8rem] text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
        >
          <option value="claude">Claude</option>
          <option value="codex">Codex</option>
        </select>
      </div>

      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <p class="text-sm text-text-primary">Diff word wrap</p>
          <p class="text-xs text-text-secondary/60">Wrap long lines in diff views</p>
        </div>
        <ToggleSwitch
          checked={settings.diffWordWrap}
          ariaLabel="Toggle diff word wrap"
          onToggle={(value) => updateSetting('diffWordWrap', value)}
        />
      </div>

      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <p class="text-sm text-text-primary">Streaming enabled</p>
          <p class="text-xs text-text-secondary/60">Show text as it arrives from the provider</p>
        </div>
        <ToggleSwitch
          checked={settings.streamingEnabled}
          ariaLabel="Toggle streaming"
          onToggle={(value) => updateSetting('streamingEnabled', value)}
        />
      </div>
    </div>
  </section>

  <section
    class="rounded-2xl border border-border/70 bg-surface-1/80 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm"
    data-testid="settings-thread-defaults"
  >
    <div class="mb-4">
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Thread defaults</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">New-thread seeds</h3>
      <p class="mt-1 text-sm text-text-secondary">
        Values used when a thread is created without an explicit override. Each
        field is editable per-thread after creation.
      </p>
    </div>
    <div class="space-y-3">
      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <label for="default-mode" class="text-sm text-text-primary block">Default mode</label>
          <p class="text-xs text-text-secondary/60">Mode seeded on new threads (chat / plan / design)</p>
        </div>
        <select
          id="default-mode"
          data-testid="settings-default-mode"
          value={settings.defaultMode}
          onchange={(e) => updateSetting('defaultMode', (e.target as HTMLSelectElement).value as ThreadMode)}
          class="min-w-[8rem] text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
        >
          {#each MODE_OPTIONS as opt}
            <option value={opt.value}>{opt.label}</option>
          {/each}
        </select>
      </div>

      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <label for="default-effort" class="text-sm text-text-primary block">Default reasoning effort</label>
          <p class="text-xs text-text-secondary/60">How hard the provider should think before replying</p>
        </div>
        <select
          id="default-effort"
          data-testid="settings-default-effort"
          value={settings.defaultReasoningEffort}
          onchange={(e) => updateSetting('defaultReasoningEffort', (e.target as HTMLSelectElement).value as ReasoningEffort)}
          class="min-w-[8rem] text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
        >
          {#each EFFORT_OPTIONS as opt}
            <option value={opt.value}>{opt.label}</option>
          {/each}
        </select>
      </div>

      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <p class="text-sm text-text-primary">Default fast mode</p>
          <p class="text-xs text-text-secondary/60">Launch new threads on the small-model tier</p>
        </div>
        <ToggleSwitch
          checked={settings.defaultFastMode}
          ariaLabel="Toggle default fast mode"
          onToggle={(value) => updateSetting('defaultFastMode', value)}
        />
      </div>

      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <label for="default-context" class="text-sm text-text-primary block">Default context window</label>
          <p class="text-xs text-text-secondary/60">Claude uses this; Codex ignores the field</p>
        </div>
        <select
          id="default-context"
          data-testid="settings-default-context"
          value={String(settings.defaultContextWindow)}
          onchange={(e) => updateSetting('defaultContextWindow', Number((e.target as HTMLSelectElement).value) as ContextWindow)}
          class="min-w-[8rem] text-xs rounded-xl border border-border bg-surface-0 px-3 py-2 text-text-primary shadow-sm focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/50 transition-colors cursor-pointer"
        >
          {#each CONTEXT_OPTIONS as opt}
            <option value={String(opt.value)}>{opt.label}</option>
          {/each}
        </select>
      </div>
    </div>
  </section>

  <section class="rounded-2xl border border-border/70 bg-surface-1/80 p-5 shadow-[0_10px_40px_-24px_rgba(0,0,0,0.45)] backdrop-blur-sm">
    <div class="mb-4">
      <p class="text-[11px] font-semibold uppercase tracking-[0.22em] text-text-secondary/70">Confirmations</p>
      <h3 class="mt-1 text-base font-semibold text-text-primary">Safety checks</h3>
      <p class="mt-1 text-sm text-text-secondary">Choose which destructive sidebar actions should stop for confirmation.</p>
    </div>
    <div class="space-y-3">
      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <p class="text-sm text-text-primary">Confirm before archive</p>
          <p class="text-xs text-text-secondary/60">Show a confirmation dialog when archiving threads</p>
        </div>
        <ToggleSwitch
          checked={settings.confirmArchive}
          ariaLabel="Toggle archive confirmation"
          onToggle={(value) => updateSetting('confirmArchive', value)}
        />
      </div>

      <div class="flex items-center justify-between gap-4 rounded-2xl border border-border/55 bg-surface-0/55 px-4 py-3">
        <div>
          <p class="text-sm text-text-primary">Confirm before delete</p>
          <p class="text-xs text-text-secondary/60">Show a confirmation dialog when deleting threads</p>
        </div>
        <ToggleSwitch
          checked={settings.confirmDelete}
          ariaLabel="Toggle delete confirmation"
          onToggle={(value) => updateSetting('confirmDelete', value)}
        />
      </div>
    </div>
  </section>
</div>
