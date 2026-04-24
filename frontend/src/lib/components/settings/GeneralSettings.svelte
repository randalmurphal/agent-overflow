<script lang="ts">
  import { getSettings, updateSetting } from '../../stores/settings.svelte';
  import type { RuntimeMode, ThreadEnvMode } from '../../types/settings';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import MicroLabel from '../primitives/MicroLabel.svelte';

  let settings = $derived(getSettings());

  const RUNTIME_OPTIONS: Array<{ value: RuntimeMode; label: string }> = [
    { value: 'full-access', label: 'Full access' },
    { value: 'auto-accept-edits', label: 'Auto-edits' },
    { value: 'approval-required', label: 'Approval required' },
  ];

  const ENV_OPTIONS: Array<{ value: ThreadEnvMode; label: string }> = [
    { value: 'local', label: 'Current checkout' },
    { value: 'worktree', label: 'New worktree' },
  ];

  const SELECT_CLASS =
    'min-w-[8rem] text-[12px] rounded-[var(--radius-field)] border border-border-subtle bg-surface-0 ' +
    'px-2.5 py-1 text-fg focus:outline-none focus:border-accent focus-visible:ring-2 focus-visible:ring-accent/40 ' +
    'transition-colors cursor-pointer';

  const ROW_CLASS = 'flex items-center justify-between gap-4 py-2.5';
</script>

<div class="flex flex-col gap-8">
  <section>
    <MicroLabel as="p">Appearance</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">Theme and display</h3>
    <p class="mt-1 text-[12px] text-fg-muted">Choose how Agent Overflow should look across chat, settings, and git views.</p>
    <div class="mt-3 divide-y divide-border-subtle">
      <div class={ROW_CLASS}>
        <div>
          <label for="theme-select" class="text-[13px] text-fg block font-medium">Theme</label>
          <p class="text-[12px] text-fg-muted">Choose your preferred color scheme</p>
        </div>
        <select
          id="theme-select"
          value={settings.theme}
          onchange={(e) => updateSetting('theme', (e.target as HTMLSelectElement).value as 'system' | 'light' | 'dark')}
          class={SELECT_CLASS}
        >
          <option value="system">System</option>
          <option value="light">Light</option>
          <option value="dark">Dark</option>
        </select>
      </div>

      <div class={ROW_CLASS}>
        <div>
          <label for="timestamp-select" class="text-[13px] text-fg block font-medium">Timestamp format</label>
          <p class="text-[12px] text-fg-muted">How timestamps appear in the chat</p>
        </div>
        <select
          id="timestamp-select"
          value={settings.timestampFormat}
          onchange={(e) => updateSetting('timestampFormat', (e.target as HTMLSelectElement).value as 'locale' | '12-hour' | '24-hour')}
          class={SELECT_CLASS}
        >
          <option value="locale">System locale</option>
          <option value="12-hour">12-hour</option>
          <option value="24-hour">24-hour</option>
        </select>
      </div>
    </div>
  </section>

  <section>
    <MicroLabel as="p">Behavior</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">Defaults and live updates</h3>
    <p class="mt-1 text-[12px] text-fg-muted">Tune how new sessions start and how provider output is rendered.</p>
    <div class="mt-3 divide-y divide-border-subtle">
      <div class={ROW_CLASS}>
        <div>
          <label for="provider-select" class="text-[13px] text-fg block font-medium">Default provider</label>
          <p class="text-[12px] text-fg-muted">Provider selected when creating new threads</p>
        </div>
        <select
          id="provider-select"
          value={settings.defaultProvider}
          onchange={(e) => updateSetting('defaultProvider', (e.target as HTMLSelectElement).value as 'claude' | 'codex')}
          class={SELECT_CLASS}
        >
          <option value="claude">Claude</option>
          <option value="codex">Codex</option>
        </select>
      </div>

      <div class={ROW_CLASS}>
        <div>
          <p class="text-[13px] text-fg font-medium">Diff word wrap</p>
          <p class="text-[12px] text-fg-muted">Wrap long lines in diff views</p>
        </div>
        <ToggleSwitch
          checked={settings.diffWordWrap}
          ariaLabel="Toggle diff word wrap"
          onToggle={(value) => updateSetting('diffWordWrap', value)}
        />
      </div>

      <div class={ROW_CLASS}>
        <div>
          <p class="text-[13px] text-fg font-medium">End-of-turn diffs</p>
          <p class="text-[12px] text-fg-muted">Show a compact diff card when an agent turn completes</p>
        </div>
        <ToggleSwitch
          checked={settings.showEndOfTurnDiffs}
          ariaLabel="Toggle end-of-turn diffs"
          onToggle={(value) => updateSetting('showEndOfTurnDiffs', value)}
        />
      </div>

      <div class={ROW_CLASS}>
        <div>
          <p class="text-[13px] text-fg font-medium">Streaming enabled</p>
          <p class="text-[12px] text-fg-muted">Show text as it arrives from the provider</p>
        </div>
        <ToggleSwitch
          checked={settings.streamingEnabled}
          ariaLabel="Toggle streaming"
          onToggle={(value) => updateSetting('streamingEnabled', value)}
        />
      </div>
    </div>
  </section>

  <section data-testid="settings-thread-defaults">
    <MicroLabel as="p">Thread defaults</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">New-thread seeds</h3>
    <p class="mt-1 text-[12px] text-fg-muted">
      New threads always start in chat mode. Chat model, effort, and context
      defaults are remembered from the composer controls.
    </p>
    <div class="mt-3 divide-y divide-border-subtle">
      <div class={ROW_CLASS}>
        <div>
          <label for="default-runtime-mode" class="text-[13px] text-fg block font-medium">Default permissions</label>
          <p class="text-[12px] text-fg-muted">Access mode seeded on new threads</p>
        </div>
        <select
          id="default-runtime-mode"
          data-testid="settings-default-runtime-mode"
          value={settings.defaultRuntimeMode}
          onchange={(e) => updateSetting('defaultRuntimeMode', (e.target as HTMLSelectElement).value as RuntimeMode)}
          class={SELECT_CLASS}
        >
          {#each RUNTIME_OPTIONS as opt}
            <option value={opt.value}>{opt.label}</option>
          {/each}
        </select>
      </div>

      <div class={ROW_CLASS}>
        <div>
          <label for="default-thread-env-mode" class="text-[13px] text-fg block font-medium">Default environment</label>
          <p class="text-[12px] text-fg-muted">Workspace mode seeded on new draft threads</p>
        </div>
        <select
          id="default-thread-env-mode"
          data-testid="settings-default-thread-env-mode"
          value={settings.defaultThreadEnvMode}
          onchange={(e) => updateSetting('defaultThreadEnvMode', (e.target as HTMLSelectElement).value as ThreadEnvMode)}
          class={SELECT_CLASS}
        >
          {#each ENV_OPTIONS as opt}
            <option value={opt.value}>{opt.label}</option>
          {/each}
        </select>
      </div>

      <div class={ROW_CLASS}>
        <div>
          <label for="worktree-branch-prefix" class="text-[13px] text-fg block font-medium">Worktree branch prefix</label>
          <p class="text-[12px] text-fg-muted">Prefix for generated worktree branches</p>
        </div>
        <input
          id="worktree-branch-prefix"
          data-testid="settings-worktree-branch-prefix"
          type="text"
          value={settings.worktreeBranchPrefix}
          onblur={(e) => updateSetting('worktreeBranchPrefix', (e.target as HTMLInputElement).value)}
          class={SELECT_CLASS}
        />
      </div>
    </div>
  </section>

  <section>
    <MicroLabel as="p">Confirmations</MicroLabel>
    <h3 class="mt-1 text-[15px] font-semibold text-fg">Safety checks</h3>
    <p class="mt-1 text-[12px] text-fg-muted">Choose which destructive sidebar actions should stop for confirmation.</p>
    <div class="mt-3 divide-y divide-border-subtle">
      <div class={ROW_CLASS}>
        <div>
          <p class="text-[13px] text-fg font-medium">Confirm before archive</p>
          <p class="text-[12px] text-fg-muted">Show a confirmation dialog when archiving threads</p>
        </div>
        <ToggleSwitch
          checked={settings.confirmArchive}
          ariaLabel="Toggle archive confirmation"
          onToggle={(value) => updateSetting('confirmArchive', value)}
        />
      </div>

      <div class={ROW_CLASS}>
        <div>
          <p class="text-[13px] text-fg font-medium">Confirm before delete</p>
          <p class="text-[12px] text-fg-muted">Show a confirmation dialog when deleting threads</p>
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
