<script lang="ts">
  // Codex's tool toggles. Codex has no flat disallow list — each toggle id
  // stands for a set of per-thread config keys the backend writes — so this
  // is a fixed curated set, not the free-form field Claude gets.
  //
  // The switch reads as AVAILABILITY (on = the model still has the tool),
  // matching every other switch in Settings; the stored list is the
  // complement, i.e. the ids that are off. Claude's editor states the same
  // thing as a list of disabled names because there is no knowable universe
  // of Claude tool names to render switches for.
  //
  // The switches are not the whole list, though: a stored id this build does
  // not recognise disables nothing (the backend skips it with a log line) and
  // has no switch to sit under. It gets a row of its own, so a hand-edited
  // file or a downgrade leaves something the user can see and delete rather
  // than a setting that is stored, inert, and invisible.

  import { settingsComputer } from './settingsComputer';
  const { getSettings, updateSettingsPatch } = settingsComputer();
  import {
    CODEX_TOOL_TOGGLES,
    disabledToolsFor,
    disabledToolsSettingsKey,
    unknownCodexToggleIds,
    withToolAdded,
    withToolRemoved,
  } from '../../utils/promptOverrides';
  import type { ProviderDefinition } from '../../providers/catalog';
  import ToggleSwitch from '../shared/ToggleSwitch.svelte';
  import SettingsField from './SettingsField.svelte';
  import { GHOST_BUTTON_CLASS } from './styles';

  let { provider }: { provider: ProviderDefinition } = $props();

  let settings = $derived(getSettings());
  let disabled = $derived(disabledToolsFor(settings, provider.id));
  let unknown = $derived(unknownCodexToggleIds(disabled));

  async function write(next: string[]): Promise<void> {
    const key = disabledToolsSettingsKey(provider.id);
    if (!key) return;
    await updateSettingsPatch({ [key]: next });
  }

  function setAvailable(id: string, available: boolean): void {
    void write(available ? withToolRemoved(disabled, id) : withToolAdded(disabled, id));
  }

  function forget(id: string): void {
    void write(withToolRemoved(disabled, id));
  }
</script>

<div data-testid="settings-codex-tools-{provider.id}">
  <SettingsField
    id="codex.builtin-tools"
    label="Built-in tools"
    hint="Turn one off to keep its schema out of the model's context. Shell and file editing are not offered — a session cannot work without them."
    align="start"
    stacked
  >
    <ul class="flex flex-col gap-1">
      {#each CODEX_TOOL_TOGGLES as tool (tool.id)}
        {@const available = !disabled.includes(tool.id)}
        <li
          class="flex items-center justify-between gap-4 rounded-[var(--radius-control)] border border-border-subtle/70 bg-surface-0/40 px-3 py-2"
          data-testid="settings-codex-tool-{tool.id}"
          data-available={available}
        >
          <div class="min-w-0">
            <p class="text-[0.75rem] font-medium text-fg">{tool.label}</p>
            <p class="mt-0.5 text-[0.6875rem] leading-snug text-fg-muted">
              {tool.description}
            </p>
          </div>
          <ToggleSwitch
            checked={available}
            ariaLabel="{tool.label} available to the model"
            onToggle={(value) => setAvailable(tool.id, value)}
          />
        </li>
      {/each}

      {#each unknown as id (id)}
        <li
          class="flex items-center justify-between gap-4 rounded-[var(--radius-control)] border border-dashed border-border-subtle/70 bg-surface-0/40 px-3 py-2"
          data-testid="settings-codex-tool-unknown-{id}"
        >
          <div class="min-w-0">
            <p class="font-mono text-[0.75rem] font-medium text-fg">{id}</p>
            <p class="mt-0.5 text-[0.6875rem] leading-snug text-fg-muted">
              Unknown toggle — has no effect in this version.
            </p>
          </div>
          <button
            type="button"
            class={GHOST_BUTTON_CLASS}
            data-testid="settings-codex-tool-forget-{id}"
            aria-label="Remove unknown toggle {id}"
            onclick={() => forget(id)}
          >
            Remove
          </button>
        </li>
      {/each}
    </ul>
  </SettingsField>
</div>
