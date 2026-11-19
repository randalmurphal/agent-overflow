<script lang="ts">
  // Memory (docs/specs/memory.md, "Settings"): the app's shared memory, off
  // by default, and per provider the sessions in which this app turns that
  // provider's own memory off: those that receive Agent Overflow memory (the
  // default), every one, or none. Shared memory beside a provider's own
  // memory, which only "never" allows, gets a warning naming the choice
  // that turns the provider's off. A computer on a release before these
  // keys gets none of it.
  import { providerMemory, watchProviderMemory } from '../../stores/providerMemory.svelte';
  import type { NativeMemoryStatus } from '../../types/models';
  import type { OwnMemoryOff } from '../../types/settings';
  import { settingsComputer } from './settingsComputer';
  import { computerHasSetting } from '../../stores/settings.svelte';
  import { assistantsOn } from '../../stores/assistants.svelte';
  import SettingsOlderComputer from './SettingsOlderComputer.svelte';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';
  import { settingsFieldHint, type SettingsFieldId } from './fields';

  const { backend, getSettings, updateSetting } = settingsComputer();
  let settings = $derived(getSettings());
  const KEYS = ['sharedMemoryEnabled', 'claudeOwnMemoryOff', 'codexOwnMemoryOff'] as const;
  let older = $derived(!KEYS.every((key) => computerHasSetting(key, backend)));

  // Each provider's choice, with what the overlap warning names it by.
  const PROVIDERS = [
    { name: 'Claude', native: 'claude', key: 'claudeOwnMemoryOff', field: 'memory.claude-own', label: "Turn off Claude's own memory" },
    { name: 'Codex', native: 'codex', key: 'codexOwnMemoryOff', field: 'memory.codex-own', label: "Turn off Codex's own memory" },
  ] as const satisfies ReadonlyArray<{
    name: string;
    native: keyof NativeMemoryStatus;
    key: 'claudeOwnMemoryOff' | 'codexOwnMemoryOff';
    field: SettingsFieldId;
    label: string;
  }>;

  // The choices, each with one line saying what it does. Assistants are
  // named only while this computer has them on.
  let assistants = $derived(assistantsOn(backend));
  function choices(provider: string): Array<{ value: OwnMemoryOff; label: string; description: string }> {
    return [
      {
        value: 'agentOverflowMemory',
        label: 'Where Agent Overflow memory is used',
        description: (assistants
          ? 'Off for assistants, the threads they start, and every conversation while shared memory is on. '
          : 'Off in every conversation while shared memory is on. ')
          + `Other conversations keep ${provider}'s own memory as you set it up.`,
      },
      {
        value: 'always',
        label: 'In every conversation',
        description: 'Off in every conversation this app starts.',
      },
      {
        value: 'never',
        label: 'Never',
        description: `${provider}'s own memory stays as you set it up in every conversation`
          + (assistants ? ', so assistants keep it beside Agent Overflow memory.' : '.'),
      },
    ];
  }

  // This computer's answer for each provider's own memory, read while the
  // page shows and again on each change to its settings.
  $effect(() => {
    if (!older) return watchProviderMemory(backend);
  });
  let answer = $derived(providerMemory(backend));

  // The providers whose own memory runs beside shared memory: only where
  // their choice is "never", since otherwise every conversation that shares
  // memory runs with the provider's own off.
  let overlapping = $derived(
    !settings.sharedMemoryEnabled
      ? []
      : PROVIDERS.filter((provider) => answer?.status?.[provider.native].on && settings[provider.key] === 'never'),
  );
  // The providers whose configuration this computer could not read.
  let unreadable = $derived(PROVIDERS.filter((provider) => answer?.status?.[provider.native].error));
</script>

<div class="settings-sections" data-testid="settings-memory">
  {#if older}
  <section>
    <SettingsOlderComputer {backend} lacks="memory settings" />
  </section>
  {:else}
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="memory.shared"
        label="Shared memory"
        hint={settingsFieldHint('memory.shared', backend)}
      >
        <ToggleSwitch
          checked={settings.sharedMemoryEnabled}
          ariaLabel="Toggle shared memory"
          onToggle={(on) => void updateSetting('sharedMemoryEnabled', on)}
        />
      </SettingsField>
      {#each overlapping as provider (provider.name)}
        <div data-testid="memory-overlap-warning">
          <SettingsCallout tone="warn">
            {provider.name}'s own memory is on too, so its sessions keep two memories that can disagree.
            Turn it off with "{provider.label}" below.
          </SettingsCallout>
        </div>
      {/each}
      {#if answer?.error}
        <div data-testid="memory-native-error">
          <SettingsCallout tone="error">
            Could not read whether Claude's and Codex's own memory is on: {answer.error}
          </SettingsCallout>
        </div>
      {/if}
      {#each unreadable as provider (provider.name)}
        <div data-testid="memory-native-error">
          <SettingsCallout tone="error">
            Could not read whether {provider.name}'s own memory is on: {answer?.status?.[provider.native].error}
          </SettingsCallout>
        </div>
      {/each}
    </div>
  </section>
  <section>
    <div class="flex flex-col gap-3">
      {#each PROVIDERS as provider (provider.key)}
        <SettingsField id={provider.field} label={provider.label} hint={settingsFieldHint(provider.field, backend)} stacked>
          <div
            class="grid gap-2"
            role="radiogroup"
            aria-label={provider.label}
            data-testid={`own-memory-${provider.key}`}
          >
            {#each choices(provider.name) as option (option.value)}
              {@const checked = settings[provider.key] === option.value}
              <label
                class={[
                  'flex cursor-pointer items-start gap-3 rounded-[var(--radius-field)] border px-3 py-2 transition-colors',
                  checked
                    ? 'border-accent/50 bg-accent/10 text-fg'
                    : 'border-border-subtle bg-surface-1/30 text-fg-muted hover:border-border hover:text-fg',
                ].join(' ')}
                data-testid={`own-memory-${provider.key}-${option.value}`}
              >
                <input
                  type="radio"
                  name={provider.key}
                  value={option.value}
                  {checked}
                  onchange={() => void updateSetting(provider.key, option.value)}
                  class="mt-1 h-3.5 w-3.5 accent-accent"
                />
                <span class="min-w-0">
                  <span class="text-[0.8125rem] font-medium">{option.label}</span>
                  <span class="mt-0.5 block text-[0.75rem] leading-5 text-fg-muted">{option.description}</span>
                </span>
              </label>
            {/each}
          </div>
        </SettingsField>
      {/each}
    </div>
    <p class="mt-2 text-[0.75rem] text-fg-muted">
      Each change reaches a conversation's next session start. A running Codex thread keeps the memory
      mode it was loaded with until its session ends.
    </p>
  </section>
  {/if}
</div>
