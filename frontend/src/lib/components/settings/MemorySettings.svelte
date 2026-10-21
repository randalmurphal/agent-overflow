<script lang="ts">
  // Memory (docs/specs/memory.md, "Settings"): the app's shared memory, off
  // by default, and a switch per provider that turns its own memory off for
  // the sessions this app starts. Shared memory beside a provider's own
  // memory gets a warning naming the switch that turns the provider's off.
  import { onMount } from 'svelte';
  import { GetNativeMemoryStatus } from '../../stores/bindings';
  import type { NativeMemoryStatus } from '../../types/models';
  import { errString } from '../../utils/errors';
  import { settingsComputer } from './settingsComputer';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsCallout from './SettingsCallout.svelte';
  import SettingsField from './SettingsField.svelte';

  const { getSettings, updateSetting, call } = settingsComputer();
  let settings = $derived(getSettings());

  // The provider switches' labels, which the overlap warning names.
  const CLAUDE_OFF = "Turn off Claude's own memory";
  const CODEX_OFF = "Turn off Codex's own memory";

  let native = $state<NativeMemoryStatus | null>(null);
  let nativeError = $state('');
  // Reads overlap (the page opening, then shared memory turned on): only
  // the newest read's answer lands, and none after the page closes.
  let nativeRead = 0;

  async function readNative(): Promise<void> {
    const read = ++nativeRead;
    try {
      const status = await call(() => GetNativeMemoryStatus());
      if (read !== nativeRead) return;
      native = status;
      nativeError = '';
    } catch (err) {
      if (read !== nativeRead) return;
      native = null;
      nativeError = errString(err);
    }
  }

  onMount(() => {
    void readNative();
    return () => { nativeRead++; };
  });

  function setShared(on: boolean): void {
    void updateSetting('sharedMemoryEnabled', on);
    // The provider configs may have changed since the page opened.
    if (on) void readNative();
  }

  // The providers whose own memory runs beside shared memory, with the
  // switch that turns it off.
  let overlapping = $derived(
    !settings.sharedMemoryEnabled || native === null
      ? []
      : [
          ...(native.claude && !settings.claudeNativeMemoryDisabled ? [{ name: 'Claude', field: CLAUDE_OFF }] : []),
          ...(native.codex && !settings.codexNativeMemoryDisabled ? [{ name: 'Codex', field: CODEX_OFF }] : []),
        ],
  );
</script>

<div class="settings-sections" data-testid="settings-memory">
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="memory.shared"
        label="Shared memory"
        hint="Give every conversation the memory of its project and the memory of you, which it reads when it starts and may add to. Assistants always have memory."
      >
        <ToggleSwitch
          checked={settings.sharedMemoryEnabled}
          ariaLabel="Toggle shared memory"
          onToggle={setShared}
        />
      </SettingsField>
      {#each overlapping as provider (provider.name)}
        <div data-testid="memory-overlap-warning">
          <SettingsCallout tone="warn">
            {provider.name}'s own memory is on too, so its sessions keep two memories that can disagree.
            Turn it off with "{provider.field}" below.
          </SettingsCallout>
        </div>
      {/each}
      {#if nativeError}
        <div data-testid="memory-native-error">
          <SettingsCallout tone="error">
            Could not read whether Claude's and Codex's own memory is on: {nativeError}
          </SettingsCallout>
        </div>
      {/if}
    </div>
  </section>
  <section>
    <div class="flex flex-col gap-1">
      <SettingsField
        id="memory.claude-off"
        label={CLAUDE_OFF}
        hint="Sessions this app starts send autoMemoryEnabled: false. Your Claude settings files are not changed."
      >
        <ToggleSwitch
          checked={settings.claudeNativeMemoryDisabled}
          ariaLabel="Toggle Claude's own memory off"
          onToggle={(value) => updateSetting('claudeNativeMemoryDisabled', value)}
        />
      </SettingsField>
      <SettingsField
        id="memory.codex-off"
        label={CODEX_OFF}
        hint="Threads this app starts neither use memories nor are mined for them. Your Codex config is not changed."
      >
        <ToggleSwitch
          checked={settings.codexNativeMemoryDisabled}
          ariaLabel="Toggle Codex's own memory off"
          onToggle={(value) => updateSetting('codexNativeMemoryDisabled', value)}
        />
      </SettingsField>
    </div>
    <p class="mt-2 text-[0.75rem] text-fg-muted">
      Each change reaches a conversation's next session start. A running Codex thread keeps the memory
      mode it was loaded with until its session ends.
    </p>
  </section>
</div>
